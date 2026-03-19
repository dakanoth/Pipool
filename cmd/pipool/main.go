package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dakota/pipool/internal/asic"
	"github.com/dakota/pipool/internal/config"
	"github.com/dakota/pipool/internal/ctl"
	"github.com/dakota/pipool/internal/dashboard"
	"github.com/dakota/pipool/internal/discord"
	"github.com/dakota/pipool/internal/merge"
	"github.com/dakota/pipool/internal/metrics"
	"github.com/dakota/pipool/internal/mining"
	"github.com/dakota/pipool/internal/rpc"
	"github.com/dakota/pipool/internal/quai"
	"github.com/dakota/pipool/internal/stratum"
)

const version = "1.0.0"

// ─── Live coin price cache (CoinGecko) ────────────────────────────────────────

var (
	priceMu    sync.RWMutex
	priceCache = make(map[string]float64) // symbol → USD
)

// geckoIDs maps pool coin symbols to CoinGecko IDs.
var geckoIDs = map[string]string{
	"BTC":  "bitcoin",
	"LTC":  "litecoin",
	"DOGE": "dogecoin",
	"BCH":  "bitcoin-cash",
	"DGB":  "digibyte",
	"DGBS": "digibyte", // DGB Scrypt — same underlying coin, different algo port
	"PEP":  "pepecoin-network",
}

func coinPrice(symbol string) float64 {
	priceMu.RLock()
	defer priceMu.RUnlock()
	return priceCache[symbol]
}

// fetchPricesLoop polls CoinGecko every 60 s and updates priceCache.
func fetchPricesLoop(ctx context.Context) {
	ids := make([]string, 0, len(geckoIDs))
	for _, id := range geckoIDs {
		ids = append(ids, id)
	}
	url := "https://api.coingecko.com/api/v3/simple/price?ids=" +
		strings.Join(ids, ",") + "&vs_currencies=usd"

	client := &http.Client{Timeout: 10 * time.Second}
	do := func() {
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("[prices] fetch error: %v", err)
			return
		}
		defer resp.Body.Close()
		var result map[string]map[string]float64
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			log.Printf("[prices] decode error: %v", err)
			return
		}
		priceMu.Lock()
		for sym, geckoID := range geckoIDs {
			if data, ok := result[geckoID]; ok {
				priceCache[sym] = data["usd"]
			}
		}
		priceMu.Unlock()
	}

	do() // fetch immediately on start
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			do()
		}
	}
}

// ─── Difficulty adjustment epoch cache ───────────────────────────────────────

var (
	epochCacheMu     sync.RWMutex
	epochStartTime   = make(map[string]int64)  // coin → epoch start unix timestamp
	epochStartHeight = make(map[string]int64)  // coin → epoch start height
)

// computeDiffAdjustment returns difficulty retarget info for BTC/LTC (which have fixed 2016-block epochs).
// Returns nil for other coins.
func computeDiffAdjustment(sym string, height int64, cli *rpc.Client) *dashboard.DiffAdjustment {
	const epochSize = 2016
	var targetBlockSec int
	switch sym {
	case "BTC", "BCH":
		targetBlockSec = 600
	case "LTC":
		targetBlockSec = 150
	default:
		return nil // other coins don't have fixed epochs
	}
	blocksInEpoch := height % epochSize
	blocksRemaining := int64(epochSize) - blocksInEpoch
	epochStartH := height - blocksInEpoch

	// Check cache
	epochCacheMu.RLock()
	cachedHeight := epochStartHeight[sym]
	cachedTime := epochStartTime[sym]
	epochCacheMu.RUnlock()

	var epochStartUnix int64
	if cachedHeight == epochStartH && cachedTime > 0 {
		epochStartUnix = cachedTime
	} else {
		// Fetch from node
		hash, err := cli.GetBlockHash(epochStartH)
		if err == nil {
			bh, err2 := cli.GetBlockHeader(hash)
			if err2 == nil {
				epochStartUnix = bh.Time
				epochCacheMu.Lock()
				epochStartHeight[sym] = epochStartH
				epochStartTime[sym] = epochStartUnix
				epochCacheMu.Unlock()
			}
		}
	}

	var projectedPct float64
	var estimatedDate string
	if epochStartUnix > 0 && blocksInEpoch > 0 {
		elapsedSec := time.Now().Unix() - epochStartUnix
		expectedSec := int64(blocksInEpoch) * int64(targetBlockSec)
		// difficulty increases if blocks came faster than expected (elapsedSec < expectedSec)
		// projected % change: (expectedSec/elapsedSec - 1) * 100
		projectedPct = (float64(expectedSec)/float64(elapsedSec) - 1) * 100
		// ETA: average block time so far, extrapolated to remaining blocks
		avgBlockSec := elapsedSec / blocksInEpoch
		etaSec := blocksRemaining * avgBlockSec
		etaTime := time.Now().Add(time.Duration(etaSec) * time.Second)
		estimatedDate = etaTime.Format("Jan 2, 15:04")
	} else if blocksInEpoch == 0 {
		// Just started epoch
		estimatedDate = "~" + fmtUptime(time.Duration(epochSize)*time.Duration(targetBlockSec)*time.Second)
	}

	return &dashboard.DiffAdjustment{
		EpochSize:      epochSize,
		BlocksInEpoch:  blocksInEpoch,
		BlocksRemaining: blocksRemaining,
		ProjectedPct:   projectedPct,
		EstimatedDate:  estimatedDate,
		TargetBlockSec: targetBlockSec,
	}
}

// fmtUptime formats a duration as "2d 3h 14m" or "47m 12s"
func fmtUptime(d time.Duration) string {
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, mins, secs)
	}
	return fmt.Sprintf("%dm %ds", mins, secs)
}

// fmtWorkerUptime formats a session duration for the worker table.
func fmtWorkerUptime(d time.Duration) string { return fmtUptime(d) }

func main() {
	// ─── Flags ────────────────────────────────────────────────────────────────
	cfgPath := flag.String("config", "configs/pipool.json", "Path to pipool.json config file")
	genConfig := flag.Bool("init", false, "Generate a default config file and exit")
	metricsPort := flag.Int("metrics-port", 9100, "Prometheus metrics port (0 to disable)")
	flag.Parse()
	go func() { log.Println(http.ListenAndServe("localhost:6060", nil)) }()

	fmt.Printf(`
  ╔══════════════════════════════════════════════════════╗
  ║   ⛏️  PiPool v%s                                    ║
  ║   Raspberry Pi 5 Solo Mining Pool                    ║
  ║   LTC+DOGE · BTC+BCH · DGB · PEP (opt-in)           ║
  ╚══════════════════════════════════════════════════════╝
`, version)

	// ─── Init mode ────────────────────────────────────────────────────────────
	if *genConfig {
		cfg := config.DefaultConfig()
		if err := config.Save(cfg, *cfgPath); err != nil {
			log.Fatalf("Could not write default config: %v", err)
		}
		fmt.Printf("✔ Default config written to %s\n", *cfgPath)
		fmt.Println("  Edit it to set your wallet addresses, RPC credentials, and Discord webhook.")
		fmt.Println("  Then run: ./pipool -config configs/pipool.json")
		os.Exit(0)
	}

	// ─── Load config ──────────────────────────────────────────────────────────
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v\n\n  Run with -init to generate a default config.", *cfgPath, err)
	}

	// ─── Logging setup ────────────────────────────────────────────────────────
	if cfg.Logging.ToFile {
		os.MkdirAll("/var/log/pipool", 0755)
		lf, err := os.OpenFile(cfg.Logging.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("[warn] could not open log file %s: %v — logging to stdout only", cfg.Logging.Path, err)
		} else {
			log.SetOutput(lf)
			log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
		}
	}

	log.Printf("PiPool v%s starting — pool: %s", version, cfg.Pool.Name)

	// cfgMu protects cfg fields that are mutated by dashboard HTTP handlers
	// while background goroutines read them.
	var cfgMu sync.RWMutex

	// ─── System Monitor ───────────────────────────────────────────────────────
	sysmon := mining.NewSystemMonitor()

	// ─── Discord Notifier ─────────────────────────────────────────────────────
	notifier := discord.NewNotifier(&cfg.Discord)

	// Hook temp alerts into Discord
	sysmon.OnHighTemp = func(tempC float64) {
		notifier.HighTemp(tempC, cfg.Pool.TempLimitC)
	}

	sysmon.Start(cfg.Pool.TempLimitC)

	// ─── Merge Mining Coordinator ─────────────────────────────────────────────
	coord := merge.NewCoordinator(cfg.Coins)
	coord.Start()
	defer coord.Stop()

	// ─── Stratum Servers ──────────────────────────────────────────────────────
	var servers []*stratum.Server

	enabledCoins := cfg.EnabledCoins()
	for _, coin := range enabledCoins {
		// Aux coins (DOGE, BCH) are merged via AuxPoW — no separate stratum port.
		// Probe their daemon and warn if unavailable; coordinator retries automatically.
		if coin.MergeParent != "" {
			if err := probeDaemon(coin); err != nil {
				log.Printf("[%s] daemon offline (%v) — merge mining paused, will retry automatically", coin.Symbol, err)
				notifier.NodeUnreachable(coin.Symbol, err)
			} else {
				log.Printf("[%s] daemon online — merge mining active via %s", coin.Symbol, coin.MergeParent)
			}
			continue
		}

		// Probe primary coin daemon — start stratum regardless so miners can
		// connect immediately. Jobs are served as soon as the daemon comes online.
		if err := probeDaemon(coin); err != nil {
			log.Printf("[%s] daemon offline (%v) — stratum port open, jobs paused until daemon is reachable", coin.Symbol, err)
			notifier.NodeUnreachable(coin.Symbol, err)
		} else {
			log.Printf("[%s] daemon online", coin.Symbol)
		}

		srv := stratum.NewServer(coin, cfg, coord)

		// Wire up Discord + dashboard callbacks
		srv.OnMinerConnect = func(worker, addr string) {
			total := srv.Stats().ConnectedMiners
			notifier.MinerConnected(coin.Symbol, worker, addr, total)
		}
		srv.OnMinerDisconnect = func(worker string) {
			total := srv.Stats().ConnectedMiners
			notifier.MinerDisconnected(coin.Symbol, worker, total)
		}
		srv.OnNodeUnreachable = func(err error) {
			notifier.NodeUnreachable(coin.Symbol, err)
		}
		srv.OnNodeOnline = func() {
			notifier.NodeBackOnline(coin.Symbol)
		}
		srv.OnNodeWatchdogRestart = func(service string) {
			notifier.DaemonRestarted(coin.Symbol, service)
		}
		srv.OnBlockFound = func(sym, hash, worker string, reward, luck float64) {
			notifier.BlockFound(sym, hash, 0, reward, luck, worker)
		}
		srv.OnBlockMature = func(coin, hash string, height int64, reward string) {
			notifier.BlockMatured(coin, hash, height, reward)
		}
		srv.OnStaleKick = func(workerName string, kickCount int) {
			notifier.StaleKick(coin.Symbol, workerName, kickCount)
		}
		srv.OnAuxBlockFound = func(info stratum.AuxBlockInfo) {
			// Build the allHashes slice in sorted symbol order (same order used when
			// building the coinbase commitment so the merkle tree is identical).
			syms := info.AuxSortedSyms
			allHashes := make([][32]byte, len(syms))
			for i, sym := range syms {
				if aw, ok := info.AuxWorkSnap[sym]; ok {
					copy(allHashes[i][:], aw.Hash)
				}
			}

			children := cfg.MergeChildren(coin.Symbol)
			for _, child := range children {
				child := child // capture
				go func() {
					// Use the snapshotted aux work — it must match exactly what was
					// committed in the parent coinbase when the job was built.
					// Using coord.GetAuxWork() here would be wrong: if the aux chain
					// found a new block after job creation, the hash would differ from
					// the coinbase commitment and submitauxblock would fail.
					auxWork, ok := info.AuxWorkSnap[child.Symbol]
					if !ok {
						// This child had no work when the job was built — not committed.
						return
					}

					// Find this child's index in the sorted symbol list used at job creation.
					chainIdx := -1
					for i, sym := range syms {
						if sym == child.Symbol {
							chainIdx = i
							break
						}
					}
					if chainIdx < 0 {
						log.Printf("[%s] child symbol not found in aux snapshot syms — skipping submitauxblock", child.Symbol)
						return
					}

					auxPoWHex := merge.BuildAuxPoWHex(info.CoinbaseTx, info.MerkleBranch, info.HeaderBytes, allHashes, chainIdx)
					if err := coord.SubmitAuxBlock(child.Symbol, auxWork.HashHex, auxPoWHex); err != nil {
						log.Printf("[%s] submitauxblock failed: %v", child.Symbol, err)
					} else {
						log.Printf("[%s] aux block submitted successfully! (merged via %s)", child.Symbol, coin.Symbol)
						notifier.BlockFound(child.Symbol, auxWork.HashHex, 0, child.BlockReward, -1, "merged")
					}
				}()
			}
		}

		if err := srv.Start(); err != nil {
			log.Printf("[%s] FAILED to open stratum port: %v", coin.Symbol, err)
			continue
		}

		servers = append(servers, srv)
		log.Printf("[%s] Stratum listening on port %d", coin.Symbol, coin.Stratum.Port)
	}

	if len(servers) == 0 {
		log.Fatal("No stratum ports could start — check for port conflicts or config errors")
	}

	// ASIC health cache — populated by the polling goroutine below
	var (
		asicMu     sync.RWMutex
		asicHealth = make(map[string]*asic.Health)
	)

	// ─── Quai Network stratum servers ─────────────────────────────────────────
	var quaiServers []*quai.Server
	if cfg.Quai.Enabled {
		log.Printf("[quai] Quai integration enabled — node at %s:%d",
			cfg.Quai.Node.Host, cfg.Quai.Node.HTTPPort)

		quaiNode := quai.NewNodeClient(cfg.Quai.Node.Host, cfg.Quai.Node.HTTPPort)

		// SHA-256 server (for Goldshell, Bitmain, etc.)
		if cfg.Quai.SHA256.Enabled {
			qSHA := quai.NewServer(quai.ServerConfig{
				Algo:         "sha256d",
				ListenAddr:   fmt.Sprintf("0.0.0.0:%d", cfg.Quai.SHA256.Port),
				CoinbaseAddr: cfg.Quai.SHA256.Wallet,
				MinDiff:      cfg.Quai.SHA256.MinDiff,
				MaxDiff:      cfg.Quai.SHA256.MaxDiff,
				TargetTime:   cfg.Quai.SHA256.TargetTime,
			}, quaiNode)
			qSHA.SetCallbacks(
				func(height uint64, hash, worker string) {
					notifier.BlockFound("QUAI", hash, int64(height), 0, -1, worker)
				},
				nil,
			)
			if err := qSHA.Start(); err != nil {
				log.Printf("[quai/sha256d] failed to start: %v", err)
			} else {
				quaiServers = append(quaiServers, qSHA)
				log.Printf("[quai/sha256d] stratum listening on port %d", cfg.Quai.SHA256.Port)
			}
		}

		// Scrypt server (for Elphapex DG Home 1, Goldshell Mini-DOGE, etc.)
		if cfg.Quai.Scrypt.Enabled {
			qScrypt := quai.NewServer(quai.ServerConfig{
				Algo:         "scrypt",
				ListenAddr:   fmt.Sprintf("0.0.0.0:%d", cfg.Quai.Scrypt.Port),
				CoinbaseAddr: cfg.Quai.Scrypt.Wallet,
				MinDiff:      cfg.Quai.Scrypt.MinDiff,
				MaxDiff:      cfg.Quai.Scrypt.MaxDiff,
				TargetTime:   cfg.Quai.Scrypt.TargetTime,
			}, quaiNode)
			qScrypt.SetCallbacks(
				func(height uint64, hash, worker string) {
					notifier.BlockFound("QUAI", hash, int64(height), 0, -1, worker)
				},
				nil,
			)
			if err := qScrypt.Start(); err != nil {
				log.Printf("[quai/scrypt] failed to start: %v", err)
			} else {
				quaiServers = append(quaiServers, qScrypt)
				log.Printf("[quai/scrypt] stratum listening on port %d", cfg.Quai.Scrypt.Port)
			}
		}
	}
	// quaiServers passed to buildDashboardSnapshot

	// Shutdown context — cancelled on SIGINT/SIGTERM so background goroutines exit cleanly.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	// ─── ASIC health polling ──────────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		asicAlertedAt := make(map[string]time.Time)
		for {
			select {
			case <-shutdownCtx.Done():
				return
			case <-ticker.C:
			}
			for _, srv := range servers {
				for _, w := range srv.AllWorkers() {
					if !w.Online || w.DeviceName == "" {
						continue
					}
					ip := w.RemoteAddr
					if idx := strings.LastIndex(ip, ":"); idx >= 0 {
						ip = ip[:idx]
					}
					h, err := asic.Poll(ip, w.DeviceName, 5*time.Second)
					if err != nil {
						continue
					}
					asicMu.Lock()
					asicHealth[w.Name] = h
					asicMu.Unlock()
					// Discord alert for high ASIC temp (10-min dedup)
					if h.TempC > 0 && cfg.Pool.TempLimitC > 0 && int(h.TempC) >= cfg.Pool.TempLimitC {
						key := "asic:" + w.Name
						asicMu.RLock()
						last := asicAlertedAt[key]
						asicMu.RUnlock()
						if time.Since(last) > 10*time.Minute {
							asicMu.Lock()
							asicAlertedAt[key] = time.Now()
							asicMu.Unlock()
							if notifier != nil {
								notifier.HighTemp(float64(h.TempC), cfg.Pool.TempLimitC)
							}
						}
					}
				}
			}
		}
	}()

	// ─── Live power polling (miner HTTP API) ─────────────────────────────────
	go func() {
		powerClient := &http.Client{Timeout: 3 * time.Second}
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-shutdownCtx.Done():
				return
			case <-ticker.C:
			}
			for _, srv := range servers {
				for _, w := range srv.AllWorkers() {
					if !w.Online {
						continue
					}
					w := w // capture for goroutine
					srv := srv
					go func() {
						host, _, err := net.SplitHostPort(w.RemoteAddr)
						if err != nil {
							return
						}
						url := "http://" + host + "/api/system/info"
						resp, err := powerClient.Get(url)
						if err != nil {
							return
						}
						defer resp.Body.Close()
						var info struct {
							Power float64 `json:"power"`
							Temp  float64 `json:"temp"`
						}
						if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
							return
						}
						srv.SetLiveWatts(w.Name, info.Power, info.Temp)
					}()
				}
			}
		}
	}()

	// ─── Metrics registry ─────────────────────────────────────────────────────
	reg := metrics.NewRegistry()

	// ─── Control server (pipoolctl) ───────────────────────────────────────────
	ctlServer := ctl.NewServer()
	registerCtlHandlers(ctlServer, cfg, *cfgPath, servers, sysmon, reg, notifier)
	if err := ctlServer.Start(); err != nil {
		log.Printf("[ctl] warning: could not start control socket: %v", err)
	} else {
		log.Printf("[ctl] Unix socket ready at /run/pipool/pipool.sock")
		log.Printf("[ctl] Use: pipoolctl status | workers | coins | reload | ...")
		defer ctlServer.Stop()
	}

	// ─── Prometheus metrics endpoint ──────────────────────────────────────────
	if *metricsPort > 0 {
		metricsMux := http.NewServeMux()
		metricsMux.HandleFunc("/metrics", reg.Handler())
		metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			fmt.Fprint(w, "ok")
		})
		go func() {
			addr := fmt.Sprintf(":%d", *metricsPort)
			log.Printf("[metrics] Prometheus endpoint at http://0.0.0.0%s/metrics", addr)
			if err := http.ListenAndServe(addr, metricsMux); err != nil {
				log.Printf("[metrics] server error: %v", err)
			}
		}()
	}

	// ─── Pre-build RPC clients (shared by metrics updater and dashboard) ───────
	dashClients := make(map[string]*rpc.Client, len(cfg.Coins))
	for sym, coinCfg := range cfg.Coins {
		dashClients[sym] = rpc.NewClient(coinCfg.Node.Host, coinCfg.Node.Port,
			coinCfg.Node.User, coinCfg.Node.Password, sym)
	}

	// ─── Metrics updater goroutine ────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-shutdownCtx.Done():
				return
			case <-ticker.C:
			}
			reg.CPUTempC.Set(sysmon.CurrentTemp())
			reg.CPUUsagePct.Set(sysmon.ReadCPUUsage())
			reg.RAMUsedGB.Set(sysmon.ReadRAMUsage())
			for _, srv := range servers {
				stats := srv.Stats()
				sym := stats.Symbol
				reg.ConnectedMiners.Set(sym, float64(stats.ConnectedMiners))
				reg.ValidShares.Add(sym, 0) // ensure label exists
				reg.TotalShares.Add(sym, 0)
				reg.BlocksFound.Add(sym, 0)
				// Per-worker hashrate metrics
				for _, w := range srv.AllWorkers() {
					if w.Online && w.HashrateKHs > 0 {
						reg.WorkerHashrateKHs.Set(w.Name, w.HashrateKHs)
					}
				}
				// Rejected and stale shares from seenWorkers
				diag := srv.Diag()
				reg.RejectedShares.Add(sym, 0) // ensure label exists
				reg.StaleShares.Add(sym, 0)
				_ = diag
			}
			// Wallet balances from dash clients
			for sym, cli := range dashClients {
				if wi, err := cli.GetWalletInfo(); err == nil {
					reg.WalletBalance.Set(sym, wi.Balance)
					reg.WalletImmature.Set(sym, wi.ImmatureBalance)
				}
			}
		}
	}()
	var activeSymbols []string
	for _, c := range enabledCoins {
		label := c.Symbol
		if c.MergeParent != "" {
			label += " (merge → " + c.MergeParent + ")"
		}
		activeSymbols = append(activeSymbols, label)
	}
	notifier.PoolStarted(activeSymbols)

	// ─── Live coin price fetcher ──────────────────────────────────────────────
	go fetchPricesLoop(shutdownCtx)

	// ─── Periodic hashrate report ─────────────────────────────────────────────
	// Polls every minute; checks live config so dashboard interval changes take effect without restart.
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		var minutesSinceReport int
		for {
			select {
			case <-shutdownCtx.Done():
				return
			case <-ticker.C:
			}
			minutesSinceReport++
			cfgMu.RLock()
			interval := cfg.Discord.Alerts.HashrateIntervalMin
			cfgMu.RUnlock()
			if interval > 0 && minutesSinceReport >= interval {
				notifier.HashrateReport(buildReportData(servers, sysmon))
				minutesSinceReport = 0
			}
		}
	}()

	// ─── Worker last-share alert ──────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-shutdownCtx.Done():
				return
			case <-ticker.C:
			}
			cfgMu.RLock()
			alertMin := cfg.Pool.LastShareAlertMin
			cfgMu.RUnlock()
			if alertMin <= 0 {
				continue
			}
			threshold := time.Duration(alertMin) * time.Minute
			for _, srv := range servers {
				sym := srv.Stats().Symbol
				for _, w := range srv.AllWorkers() {
					if !w.Online {
						continue
					}
					if w.LastShareAt.IsZero() {
						continue
					}
					if time.Since(w.LastShareAt) >= threshold {
						notifier.WorkerStale(sym, w.Name, alertMin)
					}
				}
			}
		}
	}()

	// ─── Wallet balance alert ────────────────────────────────────────────────
	go func() {
		// Wait a bit for daemons to warm up before first check
		select {
		case <-shutdownCtx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			cfgMu.RLock()
			checkMin := cfg.Discord.Alerts.WalletCheckMin
			thresholds := cfg.Discord.Alerts.WalletThresholds
			cfgMu.RUnlock()

			if checkMin <= 0 {
				checkMin = 60 // default: check every hour
			}

			var alerts []discord.WalletBalance
			for sym, cli := range dashClients {
				wi, err := cli.GetWalletInfo()
				if err != nil || (wi.Balance == 0 && wi.ImmatureBalance == 0) {
					continue
				}
				threshold := 0.0
				if thresholds != nil {
					threshold = thresholds[sym]
				}
				if wi.Balance >= threshold {
					alerts = append(alerts, discord.WalletBalance{
						Symbol:    sym,
						Confirmed: wi.Balance,
						Immature:  wi.ImmatureBalance,
						PriceUSD:  coinPrice(sym),
					})
				}
			}
			if len(alerts) > 0 {
				notifier.WalletBalanceAlert(alerts)
			}

			// Sleep for the configured interval
			wait := time.Duration(checkMin) * time.Minute
			select {
			case <-shutdownCtx.Done():
				return
			case <-time.After(wait):
			}
			// Also drain the ticker to avoid buildup
			select {
			case <-ticker.C:
			default:
			}
		}
	}()

	// ─── Web dashboard (always on) ───────────────────────────────────────────
	{
		dashPort := cfg.Dashboard.Port
		if dashPort == 0 {
			dashPort = 8080
		}
		pushInterval := time.Duration(cfg.Dashboard.PushIntervalS) * time.Second
		if pushInterval == 0 {
			pushInterval = 15 * time.Second
		}
		// Pre-build one RPC client per coin — reused every dashboard snapshot
		// instead of allocating a new http.Client every 5 seconds
		dash := dashboard.New(dashPort, pushInterval,
			func() dashboard.StatsSnapshot {
				return buildDashboardSnapshot(cfg, servers, quaiServers, sysmon, dashClients, &asicMu, asicHealth)
			},
			func() dashboard.NotifSettings {
				cfgMu.RLock()
				a := cfg.Discord.Alerts
				cfgMu.RUnlock()
				return dashboard.NotifSettings{
					BlockFound:          a.BlockFound,
					MinerConnected:      a.MinerConnected,
					MinerDisconnect:     a.MinerDisconnect,
					HighTemp:            a.HighTemp,
					HashrateReport:      a.HashrateReport,
					HashrateIntervalMin: a.HashrateIntervalMin,
					HashrateDropPct:     a.HashrateDropPct,
					NodeUnreachable:     a.NodeUnreachable,
				}
			},
			func(ns dashboard.NotifSettings) {
				cfgMu.Lock()
				cfg.Discord.Alerts.BlockFound          = ns.BlockFound
				cfg.Discord.Alerts.MinerConnected      = ns.MinerConnected
				cfg.Discord.Alerts.MinerDisconnect     = ns.MinerDisconnect
				cfg.Discord.Alerts.HighTemp            = ns.HighTemp
				cfg.Discord.Alerts.HashrateReport      = ns.HashrateReport
				cfg.Discord.Alerts.HashrateIntervalMin = ns.HashrateIntervalMin
				cfg.Discord.Alerts.HashrateDropPct     = ns.HashrateDropPct
				cfg.Discord.Alerts.NodeUnreachable     = ns.NodeUnreachable
				if err := config.Save(cfg, *cfgPath); err != nil {
					log.Printf("[dashboard] notif save failed: %v", err)
				}
				cfgMu.Unlock()
			},
			func() string {
				cfgMu.RLock()
				defer cfgMu.RUnlock()
				return cfg.Pool.CoinbaseTag
			},
			func(tag string) {
				cfgMu.Lock()
				cfg.Pool.CoinbaseTag = tag
				cfgMu.Unlock()
				for _, srv := range servers {
					srv.SetCoinbaseTag(tag)
				}
				cfgMu.RLock()
				if err := config.Save(cfg, *cfgPath); err != nil {
					log.Printf("[dashboard] tag save failed: %v", err)
				}
				cfgMu.RUnlock()
				log.Printf("[dashboard] coinbase tag updated to: %s", tag)
			},
			// Config editor: GET current editable settings
			func() dashboard.ConfigEditorData {
				cfgMu.RLock()
				defer cfgMu.RUnlock()
				wallets := make(map[string]string)
				vmin := make(map[string]float64)
				vmax := make(map[string]float64)
				nodeHosts := make(map[string]string)
				nodePorts := make(map[string]int)
				nodeUsers := make(map[string]string)
				nodePws := make(map[string]string)
				bkEnabled := make(map[string]bool)
				bkHosts := make(map[string]string)
				bkPorts := make(map[string]int)
				bkUsers := make(map[string]string)
				bkPws := make(map[string]string)
				upEnabled := make(map[string]bool)
				upHosts := make(map[string]string)
				upPorts := make(map[string]int)
				upUsers := make(map[string]string)
				for sym, cc := range cfg.Coins {
					wallets[sym] = cc.Wallet
					vmin[sym] = cc.Stratum.Vardiff.MinDiff
					vmax[sym] = cc.Stratum.Vardiff.MaxDiff
					nodeHosts[sym] = cc.Node.Host
					nodePorts[sym] = cc.Node.Port
					nodeUsers[sym] = cc.Node.User
					nodePws[sym] = cc.Node.Password
					if cc.BackupNode != nil {
						bkEnabled[sym] = true
						bkHosts[sym] = cc.BackupNode.Host
						bkPorts[sym] = cc.BackupNode.Port
						bkUsers[sym] = cc.BackupNode.User
						bkPws[sym] = cc.BackupNode.Password
					} else {
						bkEnabled[sym] = false
					}
					upEnabled[sym] = cc.UpstreamPool.Enabled
					upHosts[sym] = cc.UpstreamPool.Host
					upPorts[sym] = cc.UpstreamPool.Port
					upUsers[sym] = cc.UpstreamPool.User
				}
				if cfg.Quai.Enabled {
					if cfg.Quai.SHA256.Enabled {
						wallets["QUAI"] = cfg.Quai.SHA256.Wallet
						vmin["QUAI"] = cfg.Quai.SHA256.MinDiff
						vmax["QUAI"] = cfg.Quai.SHA256.MaxDiff
					}
					if cfg.Quai.Scrypt.Enabled {
						wallets["QUAIS"] = cfg.Quai.Scrypt.Wallet
						vmin["QUAIS"] = cfg.Quai.Scrypt.MinDiff
						vmax["QUAIS"] = cfg.Quai.Scrypt.MaxDiff
					}
				}
				return dashboard.ConfigEditorData{
					TempLimitC:      cfg.Pool.TempLimitC,
					WorkerTimeoutS:  cfg.Pool.WorkerTimeout,
					DiscordWebhook:  cfg.Discord.WebhookURL,
					Wallets:         wallets,
					VardiffMin:      vmin,
					VardiffMax:      vmax,
					AutoKickPct:     cfg.Pool.AutoKickRejectPct,
					AutoKickMin:     cfg.Pool.AutoKickMinShares,
					WorkerFixedDiff: cfg.Pool.WorkerFixedDiff,
					KwhRateUSD:      cfg.Pool.KwhRateUSD,
					NodeHosts: nodeHosts, NodePorts: nodePorts,
					NodeUsers: nodeUsers, NodePasswords: nodePws,
					BackupEnabled: bkEnabled, BackupHosts: bkHosts, BackupPorts: bkPorts,
					BackupUsers: bkUsers, BackupPasswords: bkPws,
					UpstreamEnabled: upEnabled, UpstreamHosts: upHosts, UpstreamPorts: upPorts,
					UpstreamUsers: upUsers,
					QuaiNodeHost: cfg.Quai.Node.Host,
					QuaiNodePort: cfg.Quai.Node.HTTPPort,
				}
			},
			// Config editor: SET and persist
			func(cd dashboard.ConfigEditorData) {
				cfgMu.Lock()
				defer cfgMu.Unlock()
				cfg.Pool.TempLimitC      = cd.TempLimitC
				cfg.Pool.WorkerTimeout   = cd.WorkerTimeoutS
				cfg.Discord.WebhookURL   = cd.DiscordWebhook
				cfg.Pool.AutoKickRejectPct = cd.AutoKickPct
				cfg.Pool.AutoKickMinShares = cd.AutoKickMin
				cfg.Pool.KwhRateUSD      = cd.KwhRateUSD
				if cd.WorkerFixedDiff != nil {
					if cfg.Pool.WorkerFixedDiff == nil {
						cfg.Pool.WorkerFixedDiff = make(map[string]float64)
					}
					for k, v := range cd.WorkerFixedDiff {
						if v <= 0 {
							delete(cfg.Pool.WorkerFixedDiff, k)
						} else {
							cfg.Pool.WorkerFixedDiff[k] = v
						}
					}
				}
				for sym, addr := range cd.Wallets {
					if cc, ok := cfg.Coins[sym]; ok {
						cc.Wallet = addr
						cfg.Coins[sym] = cc
					}
				}
				for sym, vd := range cd.VardiffMin {
					if cc, ok := cfg.Coins[sym]; ok {
						cc.Stratum.Vardiff.MinDiff = vd
						cfg.Coins[sym] = cc
					}
				}
				for sym, vd := range cd.VardiffMax {
					if cc, ok := cfg.Coins[sym]; ok {
						cc.Stratum.Vardiff.MaxDiff = vd
						cfg.Coins[sym] = cc
					}
				}
				// Apply node settings
				for sym, host := range cd.NodeHosts {
					if cc, ok := cfg.Coins[sym]; ok {
						cc.Node.Host = host
						if p, ok2 := cd.NodePorts[sym]; ok2 { cc.Node.Port = p }
						if u, ok2 := cd.NodeUsers[sym]; ok2 { cc.Node.User = u }
						if pw, ok2 := cd.NodePasswords[sym]; ok2 && pw != "" { cc.Node.Password = pw }
						cfg.Coins[sym] = cc
					}
				}
				for sym, enabled := range cd.BackupEnabled {
					if cc, ok := cfg.Coins[sym]; ok {
						if enabled {
							bkHost := cd.BackupHosts[sym]
							bkPort := cd.BackupPorts[sym]
							if bkHost != "" && bkPort > 0 {
								if cc.BackupNode == nil {
									cc.BackupNode = &config.NodeConf{}
								}
								cc.BackupNode.Host = bkHost
								cc.BackupNode.Port = bkPort
								if u, ok2 := cd.BackupUsers[sym]; ok2 { cc.BackupNode.User = u }
								if pw, ok2 := cd.BackupPasswords[sym]; ok2 && pw != "" { cc.BackupNode.Password = pw }
							}
						} else {
							cc.BackupNode = nil
						}
						cfg.Coins[sym] = cc
					}
				}
				for sym, enabled := range cd.UpstreamEnabled {
					if cc, ok := cfg.Coins[sym]; ok {
						cc.UpstreamPool.Enabled = enabled
						if h, ok2 := cd.UpstreamHosts[sym]; ok2 { cc.UpstreamPool.Host = h }
						if p, ok2 := cd.UpstreamPorts[sym]; ok2 { cc.UpstreamPool.Port = p }
						if u, ok2 := cd.UpstreamUsers[sym]; ok2 { cc.UpstreamPool.User = u }
						cfg.Coins[sym] = cc
					}
				}
				// Apply Quai settings
				if cd.QuaiNodeHost != "" {
					cfg.Quai.Node.Host = cd.QuaiNodeHost
				}
				if cd.QuaiNodePort > 0 {
					cfg.Quai.Node.HTTPPort = cd.QuaiNodePort
				}
				if w, ok := cd.Wallets["QUAI"]; ok && w != "" {
					cfg.Quai.SHA256.Wallet = w
				}
				if w, ok := cd.Wallets["QUAIS"]; ok && w != "" {
					cfg.Quai.Scrypt.Wallet = w
				}
				if v, ok := cd.VardiffMin["QUAI"]; ok && v > 0 { cfg.Quai.SHA256.MinDiff = v }
				if v, ok := cd.VardiffMax["QUAI"]; ok && v > 0 { cfg.Quai.SHA256.MaxDiff = v }
				if v, ok := cd.VardiffMin["QUAIS"]; ok && v > 0 { cfg.Quai.Scrypt.MinDiff = v }
				if v, ok := cd.VardiffMax["QUAIS"]; ok && v > 0 { cfg.Quai.Scrypt.MaxDiff = v }
				if err := config.Save(cfg, *cfgPath); err != nil {
					log.Printf("[dashboard] config save failed: %v", err)
				} else {
					log.Printf("[dashboard] config updated and saved")
				}
			},
		)
		// Wire restart callback
		dash.TriggerRestart = func() {
			log.Printf("[dashboard] restart requested via node picker")
			syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}
		// Wire GetWorkerDetail callback
		dash.GetWorkerDetail = func(coin, name string) *dashboard.WorkerDetail {
			for _, srv := range servers {
				if srv.Coin() != coin {
					continue
				}
				workers := srv.AllWorkers()
				for _, w := range workers {
					if w.Name != name {
						continue
					}
					ws := dashboard.WorkerStat{
						Name:           w.Name,
						Coin:           coin,
						Device:         w.DeviceName,
						Difficulty:     w.Difficulty,
						HashrateKHs:    w.HashrateKHs,
						SharesAccepted: w.SharesAccepted,
						SharesRejected: w.SharesRejected,
						SharesStale:    w.SharesStale,
						BestShare:      w.BestShare,
						RemoteAddr:     w.RemoteAddr,
						Online:         w.Online,
						ReconnectCount: w.ReconnectCount,
						UserAgent:      w.UserAgent,
						WattsEstimate:  w.WattsEstimate,
					}
					if !w.ConnectedAt.IsZero() {
						ws.ConnectedAt = w.ConnectedAt.Format(time.RFC3339)
					}
					if !w.LastSeenAt.IsZero() {
						ws.LastSeenAt = w.LastSeenAt.Format(time.RFC3339)
					}
					if w.Online && !w.SessionStartedAt.IsZero() {
						ws.SessionDuration = time.Since(w.SessionStartedAt).Round(time.Second).String()
					}
					asicMu.RLock()
					ah := asicHealth[w.Name]
					asicMu.RUnlock()
					if ah != nil {
						ws.ASICTempC       = ah.TempC
						ws.ASICFanRPM      = ah.FanRPM
						ws.ASICFanPct      = ah.FanPct
						ws.ASICPowerW      = ah.PowerW
						ws.ASICHashrateKHs = ah.HashrateKHs
					}
					// Convert DiffHistory
					dh := make([]dashboard.DiffEvent, len(w.DiffHistory))
					for i, e := range w.DiffHistory {
						dh[i] = dashboard.DiffEvent{Diff: e.Diff, AtMS: e.AtMS}
					}
					return &dashboard.WorkerDetail{
						WorkerStat:     ws,
						DiffHistory:    dh,
						ShareSparkline: srv.WorkerShareSparkline(name),
					}
				}
			}
			return nil
		}

		go func() {
			if err := dash.Start(); err != nil {
				log.Printf("[dashboard] error: %v", err)
			}
		}()
		log.Printf("[dashboard] http://0.0.0.0:%d — open in any browser on your network", dashPort)
	}

	// ─── Graceful shutdown ────────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	log.Printf("Received signal %v — shutting down PiPool...", sig)

	// Cancel background goroutines first so they don't pile up RPC calls during shutdown
	shutdownCancel()

	for _, srv := range servers {
		srv.Stop()
	}
	for _, qs := range quaiServers {
		qs.Stop()
	}
	sysmon.Stop()

	log.Printf("PiPool stopped cleanly. Goodbye!")
}

// ─── Stats aggregators ────────────────────────────────────────────────────────

// defaultExplorers maps coin symbols to block explorer base URLs.
// The block hash is appended directly after the base URL.
var defaultExplorers = map[string]string{
	"BTC":  "https://blockchair.com/bitcoin/block/",
	"BCH":  "https://blockchair.com/bitcoin-cash/block/",
	"LTC":  "https://blockchair.com/litecoin/block/",
	"DOGE": "https://blockchair.com/dogecoin/block/",
	"DGB":  "https://digiexplorer.info/block/",
	"DGBS": "https://digiexplorer.info/block/",
	"PEP":  "https://pepenomics.com/explorer/block/",
}

// explorerURL returns the block explorer base URL for a coin, checking coin config first.
func explorerURL(cfg *config.PoolConfig, symbol string) string {
	if coinCfg, ok := cfg.Coins[symbol]; ok && coinCfg.BlockExplorer != "" {
		return coinCfg.BlockExplorer
	}
	return defaultExplorers[symbol]
}

func buildDashboardSnapshot(cfg *config.PoolConfig, servers []*stratum.Server, quaiServers []*quai.Server, sysmon *mining.SystemMonitor, dashClients map[string]*rpc.Client, asicMu *sync.RWMutex, asicHealth map[string]*asic.Health) dashboard.StatsSnapshot {
	snap := dashboard.StatsSnapshot{
		Timestamp:   time.Now(),
		Uptime:      fmtUptime(sysmon.Uptime()),
		CPUTemp:     sysmon.CurrentTemp(),
		CPUUsage:    sysmon.ReadCPUUsage(),
		RAMUsedGB:   sysmon.ReadRAMUsage(),
		Throttling:  sysmon.IsThrottling(),
		CoinbaseTag: cfg.Pool.CoinbaseTag,
		PoolName:    cfg.Pool.Name,
	}

	// Build stratum connection endpoints for the Connect section.
	// Build a map of primary coin → list of merge children for display.
	mergeMap := make(map[string][]string)
	for sym, coinCfg := range cfg.Coins {
		if coinCfg.MergeParent != "" {
			mergeMap[coinCfg.MergeParent] = append(mergeMap[coinCfg.MergeParent], sym)
		}
	}
	for _, srv := range servers {
		stats := srv.Stats()
		sym := stats.Symbol
		coinCfg, ok := cfg.Coins[sym]
		if !ok || !coinCfg.Enabled {
			continue
		}
		host := cfg.Pool.Host
		if host == "" {
			host = "YOUR_POOL_IP"
		}
		mergeDesc := sym
		if children, ok := mergeMap[sym]; ok && len(children) > 0 {
			mergeDesc = sym + "+" + strings.Join(children, "+")
		}
		snap.Endpoints = append(snap.Endpoints, dashboard.StratumEndpoint{
			Symbol:    sym,
			MergeDesc: mergeDesc,
			Algorithm: coinCfg.Algorithm,
			Host:      host,
			Port:      coinCfg.Stratum.Port,
		})
	}

	// Quai Network stratum endpoints
	if cfg.Quai.Enabled {
		host := cfg.Pool.Host
		if host == "" {
			host = "YOUR_POOL_IP"
		}
		if cfg.Quai.SHA256.Enabled {
			snap.Endpoints = append(snap.Endpoints, dashboard.StratumEndpoint{
				Symbol:    "QUAI",
				MergeDesc: "QUAI",
				Algorithm: "sha256d",
				Host:      host,
				Port:      cfg.Quai.SHA256.Port,
			})
		}
		if cfg.Quai.Scrypt.Enabled {
			snap.Endpoints = append(snap.Endpoints, dashboard.StratumEndpoint{
				Symbol:    "QUAIS",
				MergeDesc: "QUAI-S",
				Algorithm: "scrypt",
				Host:      host,
				Port:      cfg.Quai.Scrypt.Port,
			})
		}
	}

	srvMap := make(map[string]*stratum.Server)
	for _, s := range servers {
		srvMap[s.Stats().Symbol] = s
	}

	var totalKHs float64
	var sha256KHs float64
	var scryptKHs float64
	var totalBlocks uint64

	// Fetch per-coin RPC data concurrently — avoids serial blocking over LAN
	type coinResult struct {
		sym string
		cs  dashboard.CoinStats
	}
	resultCh := make(chan coinResult, len(cfg.Coins))
	for sym, coinCfg := range cfg.Coins {
		sym, coinCfg := sym, coinCfg // capture loop vars
		go func() {
			cs := dashboard.CoinStats{
				Symbol:        sym,
				Enabled:       coinCfg.Enabled,
				IsMergeAux:    coinCfg.MergeParent != "",
				MergeParent:   coinCfg.MergeParent,
				NodeHost:      coinCfg.Node.Host,
				NodeLatencyMs: -1,
				SyncPct:       -1, // -1 = unknown
			}
			cli, ok := dashClients[sym]
			if !ok {
				cli = rpc.NewClient(coinCfg.Node.Host, coinCfg.Node.Port,
					coinCfg.Node.User, coinCfg.Node.Password, sym)
			}
			t0 := time.Now()
			if info, err := cli.GetBlockchainInfo(); err == nil {
				cs.DaemonOnline = true
				cs.NodeLatencyMs = time.Since(t0).Milliseconds()
				cs.Height = info.Blocks
				cs.Headers = info.Headers
				cs.IBD = info.InitialBlockDownload
				cs.SyncPct = info.VerificationProgress * 100.0
				if cs.SyncPct > 100.0 {
					cs.SyncPct = 100.0
				}
				if mi, err2 := cli.GetMiningInfo(); err2 == nil {
					cs.Difficulty = mi.Difficulty
					// DGB multi-algo: getmininginfo.difficulty reflects the last-mined algo (any of 5).
					// Use the per-algo difficulty from the difficulties map for the correct value.
					if sym == "DGB" {
						if d, ok := mi.Difficulties["sha256d"]; ok && d > 0 {
							cs.Difficulty = d
						}
					} else if sym == "DGBS" {
						if d, ok := mi.Difficulties["scrypt"]; ok && d > 0 {
							cs.Difficulty = d
						}
					}
				}
				if wi, err3 := cli.GetWalletInfo(); err3 == nil {
					cs.BalanceConfirmed = wi.Balance
					cs.BalanceImmature = wi.ImmatureBalance
				}
			} else {
				// Daemon offline — try ping for latency at least
				t1 := time.Now()
				if err2 := cli.Ping(); err2 == nil {
					cs.DaemonOnline = true
					cs.NodeLatencyMs = time.Since(t1).Milliseconds()
				}
			}
			resultCh <- coinResult{sym, cs}
		}()
	}

	// Collect results — timeout safety so a hung node can't block the whole snapshot
	coinMap := make(map[string]dashboard.CoinStats, len(cfg.Coins))
	collectTimeout := time.After(4 * time.Second)
collect:
	for i := 0; i < len(cfg.Coins); i++ {
		select {
		case r := <-resultCh:
			coinMap[r.sym] = r.cs
		case <-collectTimeout:
			break collect
		}
	}

	for sym, coinCfg := range cfg.Coins {
		cs, ok := coinMap[sym]
		if !ok {
			// timed out — use offline placeholder
			cs = dashboard.CoinStats{
				Symbol:        sym,
				Enabled:       coinCfg.Enabled,
				IsMergeAux:    coinCfg.MergeParent != "",
				MergeParent:   coinCfg.MergeParent,
				NodeHost:      coinCfg.Node.Host,
				NodeLatencyMs: -1,
				SyncPct:       -1,
			}
		}
		if srv, ok := srvMap[sym]; ok {
			stats := srv.Stats()
			cs.Miners = stats.ConnectedMiners
			cs.Blocks = stats.BlocksFound
			// Session effort: how much work done relative to expected block cost
			if stats.ValidWorkSinceBlock > 0 && stats.LastNetworkDiff > 0 {
				cs.SessionEffortPct = stats.ValidWorkSinceBlock / stats.LastNetworkDiff * 100
			}
			// Estimate hashrate from recent share difficulty samples.
			// Only use samples from the last 5 minutes so hashrate decays to 0
			// when a miner goes quiet instead of staying stuck at an old value.
			samples := srv.ShareSamples()
			cutoffMS := (time.Now().Add(-5 * time.Minute)).UnixMilli()
			var recentSamples []stratum.ShareSample
			for _, ss := range samples {
				if ss.TimeMS >= cutoffMS {
					recentSamples = append(recentSamples, ss)
				}
			}
			if len(recentSamples) >= 2 {
				var diffSum float64
				for _, ss := range recentSamples {
					diffSum += ss.Difficulty
				}
				spanSec := float64(recentSamples[len(recentSamples)-1].TimeMS-recentSamples[0].TimeMS) / 1000.0
				if spanSec > 0 {
					// diff1 constants: scrypt=65536, sha256d≈2^32
					diff1 := 65536.0
					if coinCfg.Algorithm == "sha256d" {
						diff1 = 4294967296.0
					}
					cs.HashrateKHs = diffSum * diff1 / spanSec / 1000.0
				}
			}
			totalKHs += cs.HashrateKHs
			if coinCfg.Algorithm == "sha256d" {
				sha256KHs += cs.HashrateKHs
			} else if coinCfg.Algorithm == "scrypt" || coinCfg.Algorithm == "scryptn" {
				scryptKHs += cs.HashrateKHs
			}
			totalBlocks += cs.Blocks
			// Share difficulty histogram
			for _, hb := range srv.ShareHistogram() {
				cs.DiffHistogram = append(cs.DiffHistogram, dashboard.HistoBucket{
					Min:   hb.Min,
					Max:   hb.Max,
					Count: hb.Count,
				})
			}
		} else if coinCfg.MergeParent != "" {
			// Merge aux coins (DOGE, BCH) share the parent's stratum server.
			// Reflect the parent's connected miner count so the coin card isn't misleadingly 0.
			if parentSrv, ok := srvMap[coinCfg.MergeParent]; ok {
				cs.Miners = parentSrv.Stats().ConnectedMiners
			}
		}
		// Price, block reward, estimated daily earnings, and expected time to block
		cs.BlockReward = coinCfg.BlockReward
		cs.PriceUSD = coinPrice(sym)
		if cs.Difficulty > 0 && cs.HashrateKHs > 0 {
			hashPerSec := cs.HashrateKHs * 1000.0
			// diff1 constant: scrypt uses 65536 (2^16), sha256d uses 2^32
			diff1Const := 65536.0
			if coinCfg.Algorithm == "sha256d" {
				diff1Const = 4294967296.0
			}
			cs.ExpectedBlockSec = cs.Difficulty * diff1Const / hashPerSec
			if cs.PriceUSD > 0 {
				expectedBlocksPerDay := 86400.0 / cs.ExpectedBlockSec
				cs.EarningsPerDayUSD = expectedBlocksPerDay * coinCfg.BlockReward * cs.PriceUSD
			}
		}
		// Difficulty adjustment countdown (BTC and LTC only)
		if (sym == "BTC" || sym == "LTC") && cs.Height > 0 {
			if cli, ok := dashClients[sym]; ok {
				if adj := computeDiffAdjustment(sym, cs.Height, cli); adj != nil {
					cs.DiffAdjust = adj
				}
			}
		}
		snap.Coins = append(snap.Coins, cs)
	}

	snap.TotalKHs = totalKHs
	snap.Sha256KHs = sha256KHs
	snap.ScryptKHs = scryptKHs
	snap.BlocksFound = totalBlocks

	// ─── Quai Network coin stats ──────────────────────────────────────────────
	for _, qs := range quaiServers {
		stats := qs.Stats()
		sym := qs.Symbol()
		cs := dashboard.CoinStats{
			Symbol:        sym,
			Enabled:       true,
			DaemonOnline:  qs.NodeOnline(),
			NodeHost:      cfg.Quai.Node.Host,
			NodeLatencyMs: -1,
			Height:        int64(qs.CurrentHeight()),
			Miners:        stats.ConnectedMiners,
			Blocks:        stats.BlocksFound,
			SyncPct:       100.0,
		}
		// Estimate hashrate from accepted shares in the last 5 minutes
		samples := qs.ShareSamples()
		cutoffMS := time.Now().Add(-5 * time.Minute).UnixMilli()
		var recentSamples []quai.ShareSample
		for _, ss := range samples {
			if ss.TimeMS >= cutoffMS {
				recentSamples = append(recentSamples, ss)
			}
		}
		if len(recentSamples) >= 2 {
			var diffSum float64
			for _, ss := range recentSamples {
				diffSum += ss.Difficulty
			}
			spanSec := float64(recentSamples[len(recentSamples)-1].TimeMS-recentSamples[0].TimeMS) / 1000.0
			if spanSec > 0 {
				diff1 := 65536.0
				if stats.Algo == "sha256d" {
					diff1 = 4294967296.0
				}
				cs.HashrateKHs = diffSum * diff1 / spanSec / 1000.0
			}
		}
		snap.Coins = append(snap.Coins, cs)
		snap.TotalKHs += cs.HashrateKHs
		if stats.Algo == "sha256d" {
			snap.Sha256KHs += cs.HashrateKHs
		} else {
			snap.ScryptKHs += cs.HashrateKHs
		}
		snap.BlocksFound += cs.Blocks
	}

	// Storage stats
	snap.Disks = collectDiskStats(cfg)

	// Workers across all servers — include seen but offline workers for history
	for _, srv := range servers {
		sym := srv.Stats().Symbol
		for _, w := range srv.AllWorkers() {
			ws := dashboard.WorkerStat{
				Name:           w.Name,
				Coin:           sym,
				Device:         w.DeviceName,
				Difficulty:     w.Difficulty,
				HashrateKHs:    w.HashrateKHs,
				SharesAccepted: w.SharesAccepted,
				SharesRejected: w.SharesRejected,
				SharesStale:    w.SharesStale,
				BestShare:      w.BestShare,
				RemoteAddr:     w.RemoteAddr,
				Online:         w.Online,
				ReconnectCount: w.ReconnectCount,
				UserAgent:      w.UserAgent,
			}
			if !w.ConnectedAt.IsZero() {
				ws.ConnectedAt = w.ConnectedAt.Format("Jan 2 15:04")
			}
			if !w.LastSeenAt.IsZero() {
				ws.LastSeenAt = w.LastSeenAt.Format("Jan 2 15:04:05")
			}
			if w.Online && !w.SessionStartedAt.IsZero() {
				ws.SessionDuration = fmtWorkerUptime(time.Since(w.SessionStartedAt))
			}
			// Electricity cost and profit estimation
			kwhRate := cfg.Pool.KwhRateUSD
			if kwhRate > 0 && w.WattsEstimate > 0 {
				ws.WattsEstimate = w.WattsEstimate
				ws.CostPerDayUSD = w.WattsEstimate / 1000.0 * 24.0 * kwhRate
				// Find this worker's coin's earnings per day
				for _, cs := range snap.Coins {
					if cs.Symbol == sym && cs.HashrateKHs > 0 && cs.EarningsPerDayUSD > 0 {
						fraction := w.HashrateKHs / cs.HashrateKHs
						ws.ProfitPerDayUSD = fraction*cs.EarningsPerDayUSD - ws.CostPerDayUSD
						break
					}
				}
			}
			// ASIC health
			asicMu.RLock()
			ah := asicHealth[w.Name]
			asicMu.RUnlock()
			if ah != nil {
				ws.ASICTempC       = ah.TempC
				ws.ASICFanRPM      = ah.FanRPM
				ws.ASICFanPct      = ah.FanPct
				ws.ASICPowerW      = ah.PowerW
				ws.ASICHashrateKHs = ah.HashrateKHs
			}
			snap.Workers = append(snap.Workers, ws)
		}
	}

	// Quai Network workers
	for _, qs := range quaiServers {
		sym := qs.Symbol()
		for _, w := range qs.Workers() {
			ws := dashboard.WorkerStat{
				Name:           w.Name,
				Coin:           sym,
				Difficulty:     w.Difficulty,
				HashrateKHs:    w.HashrateKHs,
				SharesAccepted: w.SharesAccepted,
				SharesRejected: w.SharesRejected,
				SharesStale:    w.SharesStale,
				BestShare:      w.BestShare,
				RemoteAddr:     w.RemoteAddr,
				Online:         true,
			}
			if !w.ConnectedAt.IsZero() {
				ws.ConnectedAt = w.ConnectedAt.Format("Jan 2 15:04")
			}
			if !w.LastShareAt.IsZero() {
				ws.LastSeenAt = w.LastShareAt.Format("Jan 2 15:04:05")
				ws.SessionDuration = fmtWorkerUptime(time.Since(w.ConnectedAt))
			}
			snap.Workers = append(snap.Workers, ws)
		}
	}

	// After workers loop, compute totals and per-coin electrical cost (online workers only)
	{
		coinCost := make(map[string]float64)
		var totalCost, totalRevenue float64
		for _, ws := range snap.Workers {
			if ws.Online {
				totalCost += ws.CostPerDayUSD
				coinCost[ws.Coin] += ws.CostPerDayUSD
			}
		}
		for i := range snap.Coins {
			snap.Coins[i].CostPerDayUSD = coinCost[snap.Coins[i].Symbol]
			totalRevenue += snap.Coins[i].EarningsPerDayUSD
		}
		snap.TotalCostPerDayUSD = totalCost
		snap.TotalProfitPerDayUSD = totalRevenue - totalCost
		snap.KwhRateUSD = cfg.Pool.KwhRateUSD
	}

	// Share difficulty history for telemetry chart
	for _, srv := range servers {
		sym := srv.Stats().Symbol
		for _, ss := range srv.ShareSamples() {
			snap.ShareHistory = append(snap.ShareHistory, dashboard.ShareSample{
				Coin:       sym,
				Difficulty: ss.Difficulty,
				TimeMS:     ss.TimeMS,
				Accepted:   ss.Accepted,
			})
		}
		// Hashrate history — use the already-computed HashrateKHs for this coin
		// (stats.HashrateKHs from Stats() is never set; look up the value we computed above)
		var khs float64
		for _, cs := range snap.Coins {
			if cs.Symbol == sym {
				khs = cs.HashrateKHs
				break
			}
		}
		srv.RecordHashrateSample(khs)
		for _, hs := range srv.HashrateSamples() {
			snap.HashrateHistory = append(snap.HashrateHistory, dashboard.HashrateSample{
				Coin:   sym,
				KHs:    hs.KHs,
				TimeMS: hs.TimeMS,
			})
		}
	}

	// Quai share and hashrate history
	for _, qs := range quaiServers {
		sym := qs.Symbol()
		for _, ss := range qs.ShareSamples() {
			snap.ShareHistory = append(snap.ShareHistory, dashboard.ShareSample{
				Coin:       sym,
				Difficulty: ss.Difficulty,
				TimeMS:     ss.TimeMS,
				Accepted:   ss.Accepted,
			})
		}
		var quaiKHs float64
		for _, cs := range snap.Coins {
			if cs.Symbol == sym {
				quaiKHs = cs.HashrateKHs
				break
			}
		}
		qs.RecordHashrateSample(quaiKHs)
		for _, hs := range qs.HashrateSamples() {
			snap.HashrateHistory = append(snap.HashrateHistory, dashboard.HashrateSample{
				Coin:   sym,
				KHs:    hs.KHs,
				TimeMS: hs.TimeMS,
			})
		}
	}

	// Block log across all servers
	for _, srv := range servers {
		for _, b := range srv.BlockLog() {
			snap.BlockLog = append(snap.BlockLog, dashboard.BlockEvent{
				Coin:           b.Coin,
				Height:         b.Height,
				Hash:           b.Hash,
				Reward:         b.Reward,
				Worker:         b.Worker,
				FoundAt:        b.FoundAt.Format("Jan 2 15:04:05"),
				FoundAtMS:      b.FoundAt.UnixMilli(),
				Luck:           b.BlockLuck,
				ExplorerURL:    explorerURL(cfg, b.Coin),
				Confirmations:  b.Confirmations,
				IsOrphaned:     b.IsOrphaned,
				MaturityTarget: stratum.CoinMaturityThreshold(b.Coin),
			})
		}
	}

	// Quai block log
	for _, qs := range quaiServers {
		sym := qs.Symbol()
		for _, b := range qs.BlockLog() {
			snap.BlockLog = append(snap.BlockLog, dashboard.BlockEvent{
				Coin:      sym,
				Height:    int64(b.Height),
				Hash:      b.Hash,
				Worker:    b.Worker,
				FoundAt:   b.FoundAt.Format("Jan 2 15:04:05"),
				FoundAtMS: b.FoundAt.UnixMilli(),
				Luck:      -1,
			})
		}
	}

	// Per-chain diagnostics
	for _, srv := range servers {
		d := srv.Diag()
		cd := dashboard.ChainDiag{
			Symbol:         d.Symbol,
			TotalShares:    d.TotalShares,
			ValidShares:    d.ValidShares,
			StaleShares:    d.StaleShares,
			RejectedShares: d.RejectedShares,
			CurrentJobID:   d.CurrentJobID,
			CurrentJobAge:  d.CurrentJobAge,
			WorkerCount:    d.WorkerCount,
			HasJob:         d.HasJob,
		}
		// Use consistent denominator: valid+stale+rejected from seenWorkers.
		// TotalShares (atomic) counts all submissions including unauthorized
		// workers, making stale% and reject% appear artificially low.
		trackTotal := d.ValidShares + d.StaleShares + d.RejectedShares
		if trackTotal > 0 {
			cd.StalePct = float64(d.StaleShares) / float64(trackTotal) * 100
			cd.RejectPct = float64(d.RejectedShares) / float64(trackTotal) * 100
		}
		// Issue detection
		if !d.HasJob {
			cd.Issues = append(cd.Issues, "No active job — node may be unreachable or syncing")
		}
		// Warn only when job age exceeds ~4× the coin's expected block time.
		// BTC/BCH average 600s, LTC 150s, DOGE/PEP ~60s. Normal variance can
		// double these, so the threshold is generous to avoid false alarms.
		jobAgeWarnSec := map[string]int64{
			"BTC": 2400, "BCH": 2400,
			"LTC": 600,
			"DGB": 300, "DGBS": 300, // DGB per-algo block ~75s; 4× = 300s
			"DOGE": 300, "PEP": 300,
		}
		warnSec, ok := jobAgeWarnSec[d.Symbol]
		if !ok {
			warnSec = 600 // safe default for unknown coins
		}
		if d.CurrentJobAge > warnSec {
			cd.Issues = append(cd.Issues, fmt.Sprintf("Job is %ds old — no new block seen in a long time (node may be stalled)", d.CurrentJobAge))
		}
		// Only flag rate-based issues after enough shares to be meaningful
		if d.TotalShares >= 20 {
			if cd.StalePct >= 10 {
				cd.Issues = append(cd.Issues, fmt.Sprintf("High stale rate: %.1f%% — check network latency or job broadcast timing", cd.StalePct))
			} else if cd.StalePct >= 3 {
				cd.Issues = append(cd.Issues, fmt.Sprintf("Elevated stale rate: %.1f%%", cd.StalePct))
			}
			if cd.RejectPct >= 5 {
				cd.Issues = append(cd.Issues, fmt.Sprintf("High reject rate: %.1f%% — possible difficulty mismatch or wrong algorithm", cd.RejectPct))
			}
		}
		if d.WorkerCount == 0 {
			cd.Issues = append(cd.Issues, "No miners connected to this chain")
		}
		// Cross-check: node height vs job height
		for _, cs := range snap.Coins {
			if cs.Symbol == d.Symbol && d.HasJob {
				coinCfg, ok := cfg.Coins[d.Symbol]
				_ = coinCfg
				_ = ok
				if cs.IBD {
					cd.Issues = append(cd.Issues, "Node is in Initial Block Download — shares will be stale until sync completes")
				}
				if cs.NodeLatencyMs > 500 {
					cd.Issues = append(cd.Issues, fmt.Sprintf("High node latency: %dms — may cause stale jobs", cs.NodeLatencyMs))
				}
			}
		}
		snap.ChainDiags = append(snap.ChainDiags, cd)
	}

	// Quai chain diagnostics
	for _, qs := range quaiServers {
		stats := qs.Stats()
		sym := qs.Symbol()
		total := stats.ValidShares + stats.StaleShares + stats.RejectedShares
		var stalePct, rejectPct float64
		if total > 0 {
			stalePct = float64(stats.StaleShares) / float64(total) * 100
			rejectPct = float64(stats.RejectedShares) / float64(total) * 100
		}
		cd := dashboard.ChainDiag{
			Symbol:      sym,
			ValidShares: stats.ValidShares,
			StaleShares: stats.StaleShares,
			RejectedShares: stats.RejectedShares,
			TotalShares: total,
			StalePct:    stalePct,
			RejectPct:   rejectPct,
			WorkerCount: int(stats.ConnectedMiners),
			HasJob:      qs.NodeOnline(),
			CurrentJobAge: 0,
		}
		if !qs.NodeOnline() {
			cd.Issues = append(cd.Issues, "Quai node offline — check go-quai is running with --rpc.http-addr 0.0.0.0")
		}
		if total >= 20 {
			if stalePct >= 10 {
				cd.Issues = append(cd.Issues, fmt.Sprintf("High stale rate: %.1f%%", stalePct))
			}
			if rejectPct >= 5 {
				cd.Issues = append(cd.Issues, fmt.Sprintf("High reject rate: %.1f%%", rejectPct))
			}
		}
		if stats.ConnectedMiners == 0 {
			cd.Issues = append(cd.Issues, "No miners connected to this chain")
		}
		snap.ChainDiags = append(snap.ChainDiags, cd)
	}

	// Worker groups
	if len(cfg.Pool.WorkerGroups) > 0 {
		// Build worker lookup from snap.Workers
		workerByName := make(map[string]dashboard.WorkerStat)
		for _, ws := range snap.Workers {
			workerByName[ws.Name] = ws
		}
		for groupName, memberNames := range cfg.Pool.WorkerGroups {
			gs := dashboard.GroupStat{Name: groupName}
			for _, memberName := range memberNames {
				gs.WorkerCount++
				if ws, ok := workerByName[memberName]; ok {
					if ws.Online {
						gs.OnlineCount++
					}
					gs.HashrateKHs += ws.HashrateKHs
					gs.SharesAccepted += ws.SharesAccepted
					gs.CostPerDayUSD += ws.CostPerDayUSD
					gs.ProfitPerDayUSD += ws.ProfitPerDayUSD
				}
			}
			snap.Groups = append(snap.Groups, gs)
		}
		// Sort groups by name for stable ordering
		sort.Slice(snap.Groups, func(i, j int) bool {
			return snap.Groups[i].Name < snap.Groups[j].Name
		})
	}

	snap.Notifs = dashboard.NotifSettings{
		BlockFound:          cfg.Discord.Alerts.BlockFound,
		MinerConnected:      cfg.Discord.Alerts.MinerConnected,
		MinerDisconnect:     cfg.Discord.Alerts.MinerDisconnect,
		HighTemp:            cfg.Discord.Alerts.HighTemp,
		HashrateReport:      cfg.Discord.Alerts.HashrateReport,
		HashrateIntervalMin: cfg.Discord.Alerts.HashrateIntervalMin,
		HashrateDropPct:     cfg.Discord.Alerts.HashrateDropPct,
		NodeUnreachable:     cfg.Discord.Alerts.NodeUnreachable,
	}

	return snap
}

func buildReportData(servers []*stratum.Server, sysmon *mining.SystemMonitor) discord.HashrateReportData {
	data := discord.HashrateReportData{
		Uptime:    sysmon.Uptime(),
		CPU:       sysmon.ReadCPUUsage(),
		TempC:     sysmon.CurrentTemp(),
		RAMUsedGB: sysmon.ReadRAMUsage(),
	}
	for _, srv := range servers {
		stats := srv.Stats()
		// Use the same diff1-weighted formula as the dashboard for accuracy.
		var khs float64
		samples := srv.ShareSamples()
		if len(samples) >= 2 {
			var diffSum float64
			for _, ss := range samples {
				if ss.Accepted {
					diffSum += ss.Difficulty
				}
			}
			spanSec := float64(samples[len(samples)-1].TimeMS-samples[0].TimeMS) / 1000.0
			if spanSec > 0 {
				diff1 := 65536.0
				if stats.Algorithm == "sha256d" {
					diff1 = 4294967296.0
				}
				khs = diffSum * diff1 / spanSec / 1000.0
			}
		}
		data.ActiveCoins = append(data.ActiveCoins, discord.CoinStat{
			Symbol:      stats.Symbol,
			HashrateKHs: khs,
			Miners:      stats.ConnectedMiners,
			Blocks:      stats.BlocksFound,
		})
		data.TotalKHs += khs
		data.BlocksFound += stats.BlocksFound
	}
	return data
}

// ─── Storage stats ────────────────────────────────────────────────────────────

func collectDiskStats(cfg *config.PoolConfig) []dashboard.DiskStat {
	// With PC nodes, blockchain data lives on the Windows PC — not locally.
	// We show the Pi's own storage: root (SD card) always, plus any other
	// real mounted volumes (skip /mnt/external if it isn't actually mounted).
	type mountEntry struct {
		label string
		dirs  map[string]string // symbol → local datadir (only if DataDir is set)
	}
	mounts := make(map[string]*mountEntry)

	// Always include root (SD card)
	mounts["/"] = &mountEntry{label: "SD Card", dirs: make(map[string]string)}

	// Include any coin datadirs that are explicitly configured AND local
	// (DataDir == "" means data is on the PC — skip it)
	for sym, coinCfg := range cfg.Coins {
		if !coinCfg.Enabled || coinCfg.DataDir == "" {
			continue
		}
		// Only include dirs that actually exist on this machine
		if _, err := os.Stat(coinCfg.DataDir); err != nil {
			continue
		}
		mount := mountPointOf(coinCfg.DataDir)
		if _, ok := mounts[mount]; !ok {
			label := "Storage"
			if mount == "/" {
				label = "SD Card"
			}
			mounts[mount] = &mountEntry{label: label, dirs: make(map[string]string)}
		}
		mounts[mount].dirs[sym] = coinCfg.DataDir
	}

	// Include any real extra mounted volumes (skip if not actually mounted)
	for _, extra := range []string{"/mnt/ssd", "/mnt/nvme", "/mnt/data"} {
		if _, err := os.Stat(extra); err != nil {
			continue // not mounted — skip
		}
		mount := mountPointOf(extra)
		if _, ok := mounts[mount]; !ok {
			mounts[mount] = &mountEntry{label: "SSD", dirs: make(map[string]string)}
		}
	}

	var stats []dashboard.DiskStat
	for mount, entry := range mounts {
		ds := statfsToStat(mount, entry.label)
		ds.ChainSizes = make(map[string]float64)
		for sym, dir := range entry.dirs {
			ds.ChainSizes[sym] = dirSizeGB(dir)
		}
		if ds.TotalGB > 0 { // skip mounts that returned no data (permission error etc.)
			stats = append(stats, ds)
		}
	}
	return stats
}

// statfsToStat reads disk usage for a mount point
func statfsToStat(mount, label string) dashboard.DiskStat {
	var st syscall.Statfs_t
	ds := dashboard.DiskStat{Label: label, Mount: mount}
	if err := syscall.Statfs(mount, &st); err != nil {
		return ds
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	free := float64(st.Bavail) * float64(st.Bsize)
	used := total - free
	gb := 1024 * 1024 * 1024
	ds.TotalGB = total / float64(gb)
	ds.UsedGB = used / float64(gb)
	ds.FreeGB = free / float64(gb)
	if total > 0 {
		ds.UsedPct = (used / total) * 100
	}
	return ds
}

// mountPointOf returns the mount point that contains the given path
func mountPointOf(path string) string {
	// Walk up until we find a path where the device changes — simplified version
	// Just return the path itself if it exists, otherwise "/"
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return "/"
}

// dirSizeGB cache — recalculate at most every 5 minutes to avoid walking
// hundreds of thousands of blockchain files on every dashboard push tick.
var (
	dirSizeMu    sync.Mutex
	dirSizeCache = map[string]float64{}
	dirSizeTime  = map[string]time.Time{}
)

// dirSizeGB returns the total size of a directory tree in GB (cached 5 min).
func dirSizeGB(path string) float64 {
	dirSizeMu.Lock()
	if t, ok := dirSizeTime[path]; ok && time.Since(t) < 5*time.Minute {
		v := dirSizeCache[path]
		dirSizeMu.Unlock()
		return v
	}
	dirSizeMu.Unlock()

	var total int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	gb := float64(total) / (1024 * 1024 * 1024)

	dirSizeMu.Lock()
	dirSizeCache[path] = gb
	dirSizeTime[path] = time.Now()
	dirSizeMu.Unlock()
	return gb
}

// ─── Control server handler registration ─────────────────────────────────────

func registerCtlHandlers(
	ctlSrv *ctl.Server,
	cfg *config.PoolConfig,
	cfgPath string,
	servers []*stratum.Server,
	sysmon *mining.SystemMonitor,
	reg *metrics.Registry,
	notifier *discord.Notifier,
) {
	ctlSrv.Register("status", func(args []string) ctl.Response {
		var totalMiners int32
		for _, s := range servers {
			totalMiners += s.Stats().ConnectedMiners
		}
		return ctl.Response{OK: true, Data: map[string]interface{}{
			"uptime":       sysmon.Uptime().String(),
			"cpu_temp_c":   fmt.Sprintf("%.1f°C", sysmon.CurrentTemp()),
			"cpu_usage":    fmt.Sprintf("%.1f%%", sysmon.ReadCPUUsage()),
			"ram_used_gb":  fmt.Sprintf("%.2f GB", sysmon.ReadRAMUsage()),
			"throttling":   sysmon.IsThrottling(),
			"miners_total": totalMiners,
		}}
	})

	ctlSrv.Register("workers", func(args []string) ctl.Response {
		var out []map[string]interface{}
		for _, srv := range servers {
			for _, w := range srv.ConnectedWorkers() {
				out = append(out, map[string]interface{}{
					"name":   w.Name,
					"coin":   srv.Stats().Symbol,
					"device": w.DeviceName,
					"diff":   fmt.Sprintf("%.4f", w.Difficulty),
					"shares_accepted": w.SharesAccepted,
					"shares_rejected": w.SharesRejected,
					"addr":   w.RemoteAddr,
				})
			}
		}
		return ctl.Response{OK: true, Data: out}
	})

	ctlSrv.Register("coins", func(args []string) ctl.Response {
		var out []map[string]interface{}
		for _, srv := range servers {
			stats := srv.Stats()
			sym := stats.Symbol
			nodeHost := "--"
			if coinCfg, ok := cfg.Coins[sym]; ok {
				nodeHost = fmt.Sprintf("%s:%d", coinCfg.Node.Host, coinCfg.Node.Port)
			}
			uptimeSec := sysmon.Uptime().Seconds()
			var khs float64
			if uptimeSec > 0 {
				khs = float64(stats.ValidShares) / uptimeSec * 1000
			}
			out = append(out, map[string]interface{}{
				"symbol":    sym,
				"enabled":   true,
				"miners":    stats.ConnectedMiners,
				"hashrate":  fmt.Sprintf("%.2f KH/s", khs),
				"blocks":    stats.BlocksFound,
				"node":      nodeHost,
			})
		}
		return ctl.Response{OK: true, Data: out}
	})

	ctlSrv.Register("coin", func(args []string) ctl.Response {
		if len(args) < 2 {
			return ctl.Response{OK: false, Message: "usage: coin <enable|disable> <SYMBOL>"}
		}
		action, symbol := args[0], strings.ToUpper(args[1])
		coinCfg, ok := cfg.Coins[symbol]
		if !ok {
			return ctl.Response{OK: false, Message: fmt.Sprintf("unknown coin: %s", symbol)}
		}
		switch action {
		case "enable":
			coinCfg.Enabled = true
			cfg.Coins[symbol] = coinCfg
			if err := config.Save(cfg, cfgPath); err != nil {
				log.Printf("[ctl] failed to save config after enabling %s: %v", symbol, err)
			}
			return ctl.Response{OK: true, Message: fmt.Sprintf("%s enabled and saved (restart pipool to open stratum port)", symbol)}
		case "disable":
			coinCfg.Enabled = false
			cfg.Coins[symbol] = coinCfg
			if err := config.Save(cfg, cfgPath); err != nil {
				log.Printf("[ctl] failed to save config after disabling %s: %v", symbol, err)
			}
			return ctl.Response{OK: true, Message: fmt.Sprintf("%s disabled and saved", symbol)}
		}
		return ctl.Response{OK: false, Message: fmt.Sprintf("unknown action: %s (use enable or disable)", action)}
	})

	ctlSrv.Register("kick", func(args []string) ctl.Response {
		if len(args) < 1 {
			return ctl.Response{OK: false, Message: "usage: kick <worker_name>"}
		}
		name := args[0]
		kicked := false
		for _, srv := range servers {
			if srv.KickWorker(name) {
				kicked = true
			}
		}
		if kicked {
			return ctl.Response{OK: true, Message: fmt.Sprintf("worker %s disconnected", name)}
		}
		return ctl.Response{OK: false, Message: fmt.Sprintf("worker %s not found", name)}
	})

	ctlSrv.Register("vardiff", func(args []string) ctl.Response {
		if len(args) < 3 {
			return ctl.Response{OK: false, Message: "usage: vardiff <SYMBOL> <min_diff> <max_diff>"}
		}
		return ctl.Response{OK: true, Message: fmt.Sprintf(
			"vardiff for %s updated: min=%s max=%s (takes effect on next share)", args[0], args[1], args[2])}
	})

	ctlSrv.Register("reload", func(args []string) ctl.Response {
		newCfg, err := config.Load(cfgPath)
		if err != nil {
			return ctl.Response{OK: false, Message: fmt.Sprintf("reload failed: %v", err)}
		}
		cfg.Discord = newCfg.Discord
		cfg.Dashboard = newCfg.Dashboard
		cfg.Logging = newCfg.Logging
		cfg.Pool.TempLimitC = newCfg.Pool.TempLimitC
		cfg.Pool.MaxConnections = newCfg.Pool.MaxConnections
		return ctl.Response{OK: true, Message: fmt.Sprintf("Config reloaded from %s", cfgPath)}
	})

	ctlSrv.Register("stop", func(args []string) ctl.Response {
		log.Printf("[ctl] stop requested via pipoolctl")
		// Send SIGTERM to ourselves — systemd will restart if Restart=always
		go func() {
			time.Sleep(100 * time.Millisecond) // let the response reach the client first
			syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}()
		return ctl.Response{OK: true, Message: "PiPool stopping... (systemd will restart it automatically)"}
	})

	ctlSrv.Register("restart", func(args []string) ctl.Response {
		log.Printf("[ctl] restart requested via pipoolctl")
		go func() {
			time.Sleep(100 * time.Millisecond)
			syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}()
		return ctl.Response{OK: true, Message: "PiPool restarting... (watch: sudo journalctl -u pipool -f)"}
	})

	ctlSrv.Register("loglevel", func(args []string) ctl.Response {
		if len(args) < 1 {
			return ctl.Response{OK: false, Message: "usage: loglevel <debug|info|warn|error>"}
		}
		cfg.Logging.Level = args[0]
		return ctl.Response{OK: true, Message: fmt.Sprintf("Log level set to %s", args[0])}
	})

	ctlSrv.Register("discord", func(args []string) ctl.Response {
		if len(args) > 0 && args[0] == "test" {
			notifier.PoolStarted([]string{"TEST"})
			return ctl.Response{OK: true, Message: "Test notification sent to Discord"}
		}
		return ctl.Response{OK: false, Message: "usage: discord test"}
	})

	ctlSrv.Register("version", func(args []string) ctl.Response {
		return ctl.Response{OK: true, Message: fmt.Sprintf("PiPool v%s", version)}
	})

	// fixdiff <worker> <diff>  — pin a worker to a fixed vardiff target (0 = remove pin)
	ctlSrv.Register("fixdiff", func(args []string) ctl.Response {
		if len(args) < 2 {
			return ctl.Response{OK: false, Message: "usage: fixdiff <worker> <diff>  (0 = remove pin)"}
		}
		workerName := args[0]
		var diff float64
		if _, err := fmt.Sscanf(args[1], "%f", &diff); err != nil || diff < 0 {
			return ctl.Response{OK: false, Message: "diff must be a non-negative number"}
		}
		if cfg.Pool.WorkerFixedDiff == nil {
			cfg.Pool.WorkerFixedDiff = make(map[string]float64)
		}
		if diff == 0 {
			delete(cfg.Pool.WorkerFixedDiff, workerName)
		} else {
			cfg.Pool.WorkerFixedDiff[workerName] = diff
		}
		if err := config.Save(cfg, cfgPath); err != nil {
			log.Printf("[ctl] fixdiff: save config: %v", err)
			return ctl.Response{OK: false, Message: fmt.Sprintf("config save failed: %v", err)}
		}
		if diff == 0 {
			return ctl.Response{OK: true, Message: fmt.Sprintf("fixed diff removed for %s — reverting to vardiff", workerName)}
		}
		return ctl.Response{OK: true, Message: fmt.Sprintf("worker %s pinned to difficulty %.4f (takes effect on next share)", workerName, diff)}
	})

	// calc [SYMBOL] — solo mining luck calculator
	ctlSrv.Register("calc", func(args []string) ctl.Response {
		const powerball = 292201338.0
		const megamillions = 302575350.0
		type calcResult struct {
			Symbol      string  `json:"symbol"`
			Algorithm   string  `json:"algorithm"`
			HashrateKHs float64 `json:"hashrate_khs"`
			NetworkDiff float64 `json:"network_diff"`
			ExpectedSec float64 `json:"expected_sec"`
			OddsPerHour float64 `json:"odds_per_hour"` // 1-in-X per hour
			PctPerHour  float64 `json:"pct_per_hour"`
			VsPowerball float64 `json:"vs_powerball"` // >1 means X times better than Powerball
		}
		var results []calcResult
		filter := ""
		if len(args) > 0 {
			filter = strings.ToUpper(args[0])
		}
		for _, srv := range servers {
			stats := srv.Stats()
			if filter != "" && stats.Symbol != filter {
				continue
			}
			if stats.LastNetworkDiff <= 0 {
				continue
			}
			// Sum hashrate from all online workers
			var totalKHs float64
			for _, w := range srv.AllWorkers() {
				if w.Online {
					totalKHs += w.HashrateKHs
				}
			}
			if totalKHs <= 0 {
				continue
			}
			hashPerSec := totalKHs * 1000
			diff1 := 4294967296.0
			if stats.Algorithm == "scrypt" || stats.Algorithm == "scryptn" {
				diff1 = 65536.0
			}
			expectedSec := (stats.LastNetworkDiff * diff1) / hashPerSec
			pctPerHour := (1 - math.Exp(-3600/expectedSec)) * 100
			oddsPerHour := 100 / pctPerHour
			results = append(results, calcResult{
				Symbol:      stats.Symbol,
				Algorithm:   stats.Algorithm,
				HashrateKHs: totalKHs,
				NetworkDiff: stats.LastNetworkDiff,
				ExpectedSec: expectedSec,
				OddsPerHour: oddsPerHour,
				PctPerHour:  pctPerHour,
				VsPowerball: powerball / oddsPerHour,
			})
		}
		if len(results) == 0 {
			return ctl.Response{OK: true, Message: "No active coins with known network difficulty. Are any miners connected?"}
		}
		return ctl.Response{OK: true, Data: results}
	})
}

// probeDaemon does a lightweight ping to a coin daemon to check availability.
// Returns nil if reachable, an error describing the failure otherwise.
// This is non-fatal — callers decide what to do if offline.
func probeDaemon(coin config.CoinConfig) error {
	cli := rpc.NewClient(
		coin.Node.Host, coin.Node.Port,
		coin.Node.User, coin.Node.Password,
		coin.Symbol,
	)
	return cli.Ping()
}
