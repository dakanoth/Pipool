package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"sync"

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
  ║   LTC+DOGE · BTC+BCH · PEP (opt-in)                 ║
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
		srv.OnBlockFound = func(sym, hash string, reward float64) {
			notifier.BlockFound(sym, hash, 0, reward, "unknown")
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

	// ─── Quai Network stratum servers ─────────────────────────────────────────
	var quaiServers []*quai.Server
	if cfg.Quai.Enabled {
		log.Printf("[quai] Quai integration enabled — connecting to node at %s:%d",
			cfg.Quai.Node.Host, cfg.Quai.Node.WSPort)

		quaiNode := quai.NewNodeClient(cfg.Quai.Node.Host, cfg.Quai.Node.WSPort)
		quaiCtx, quaiCancel := context.WithTimeout(context.Background(), 10*time.Second)
		quaiErr := quaiNode.Connect(quaiCtx)
		quaiCancel()
		if quaiErr != nil {
			log.Printf("[quai] WARNING: could not connect to Quai node: %v", quaiErr)
		} else {
			quaiNode.StartPolling(context.Background(), time.Second, nil) // callbacks set per-server below

			// SHA-256 server (for Goldshell, Bitmain, etc.)
			if cfg.Quai.SHA256.Enabled {
				qSHA := quai.NewServer(quai.ServerConfig{
					Algo:       "sha256d",
					ListenAddr: fmt.Sprintf("0.0.0.0:%d", cfg.Quai.SHA256.Port),
					MinDiff:    cfg.Quai.SHA256.MinDiff,
					MaxDiff:    cfg.Quai.SHA256.MaxDiff,
					TargetTime: cfg.Quai.SHA256.TargetTime,
				}, quaiNode)
				qSHA.SetCallbacks(
					func(height uint64, hash, worker string) {
						notifier.BlockFound("QUAI", hash, int64(height), 0, worker)
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
					Algo:       "scrypt",
					ListenAddr: fmt.Sprintf("0.0.0.0:%d", cfg.Quai.Scrypt.Port),
					MinDiff:    cfg.Quai.Scrypt.MinDiff,
					MaxDiff:    cfg.Quai.Scrypt.MaxDiff,
					TargetTime: cfg.Quai.Scrypt.TargetTime,
				}, quaiNode)
				qScrypt.SetCallbacks(
					func(height uint64, hash, worker string) {
						notifier.BlockFound("QUAI", hash, int64(height), 0, worker)
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
	}
	_ = quaiServers // used in dashboard snapshot below

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

	// ─── Metrics updater goroutine ────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
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

	// ─── Periodic hashrate report ─────────────────────────────────────────────
	// Polls every minute; checks live config so dashboard interval changes take effect without restart.
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		var minutesSinceReport int
		for range ticker.C {
			minutesSinceReport++
			interval := cfg.Discord.Alerts.HashrateIntervalMin
			if interval > 0 && minutesSinceReport >= interval {
				notifier.HashrateReport(buildReportData(servers, sysmon))
				minutesSinceReport = 0
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
		dashClients := make(map[string]*rpc.Client, len(cfg.Coins))
		for sym, coinCfg := range cfg.Coins {
			dashClients[sym] = rpc.NewClient(coinCfg.Node.Host, coinCfg.Node.Port,
				coinCfg.Node.User, coinCfg.Node.Password, sym)
		}
		dash := dashboard.New(dashPort, pushInterval,
			func() dashboard.StatsSnapshot {
				return buildDashboardSnapshot(cfg, servers, sysmon, dashClients)
			},
			func() dashboard.NotifSettings {
				a := cfg.Discord.Alerts
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
			},
			func() string {
				return cfg.Pool.CoinbaseTag
			},
			func(tag string) {
				cfg.Pool.CoinbaseTag = tag
				// propagate to all stratum servers
				for _, srv := range servers {
					srv.SetCoinbaseTag(tag)
				}
				if err := config.Save(cfg, *cfgPath); err != nil {
					log.Printf("[dashboard] tag save failed: %v", err)
				}
				log.Printf("[dashboard] coinbase tag updated to: %s", tag)
			},
		)
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

	for _, srv := range servers {
		srv.Stop()
	}

	log.Printf("PiPool stopped cleanly. Goodbye!")
}

// ─── Stats aggregators ────────────────────────────────────────────────────────

func buildDashboardSnapshot(cfg *config.PoolConfig, servers []*stratum.Server, sysmon *mining.SystemMonitor, dashClients map[string]*rpc.Client) dashboard.StatsSnapshot {
	snap := dashboard.StatsSnapshot{
		Timestamp:   time.Now(),
		Uptime:      fmtUptime(sysmon.Uptime()),
		CPUTemp:     sysmon.CurrentTemp(),
		CPUUsage:    sysmon.ReadCPUUsage(),
		RAMUsedGB:   sysmon.ReadRAMUsage(),
		Throttling:  sysmon.IsThrottling(),
		CoinbaseTag: cfg.Pool.CoinbaseTag,
	}

	srvMap := make(map[string]*stratum.Server)
	for _, s := range servers {
		srvMap[s.Stats().Symbol] = s
	}

	var totalKHs float64
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
			// Estimate hashrate from recent share difficulty samples (more accurate than
			// using MinDiff which would be stale once vardiff ramps up)
			samples := srv.ShareSamples()
			if len(samples) >= 2 {
				var diffSum float64
				for _, ss := range samples {
					diffSum += ss.Difficulty
				}
				spanSec := float64(samples[len(samples)-1].TimeMS-samples[0].TimeMS) / 1000.0
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
			totalBlocks += cs.Blocks
		} else if coinCfg.MergeParent != "" {
			// Merge aux coins (DOGE, BCH) share the parent's stratum server.
			// Reflect the parent's connected miner count so the coin card isn't misleadingly 0.
			if parentSrv, ok := srvMap[coinCfg.MergeParent]; ok {
				cs.Miners = parentSrv.Stats().ConnectedMiners
			}
		}
		snap.Coins = append(snap.Coins, cs)
	}

	snap.TotalKHs = totalKHs
	snap.BlocksFound = totalBlocks

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
				SharesAccepted: w.SharesAccepted,
				SharesRejected: w.SharesRejected,
				SharesStale:    w.SharesStale,
				BestShare:      w.BestShare,
				RemoteAddr:     w.RemoteAddr,
				Online:         w.Online,
			}
			if !w.ConnectedAt.IsZero() {
				ws.ConnectedAt = w.ConnectedAt.Format("Jan 2 15:04")
			}
			if !w.LastSeenAt.IsZero() {
				ws.LastSeenAt = w.LastSeenAt.Format("Jan 2 15:04:05")
			}
			snap.Workers = append(snap.Workers, ws)
		}
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

	// Block log across all servers
	for _, srv := range servers {
		for _, b := range srv.BlockLog() {
			snap.BlockLog = append(snap.BlockLog, dashboard.BlockEvent{
				Coin:    b.Coin,
				Height:  b.Height,
				Hash:    b.Hash,
				Reward:  b.Reward,
				Worker:  b.Worker,
				FoundAt: b.FoundAt.Format("Jan 2 15:04:05"),
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
		if d.TotalShares > 0 {
			cd.StalePct = float64(d.StaleShares) / float64(d.TotalShares) * 100
			cd.RejectPct = float64(d.RejectedShares) / float64(d.TotalShares) * 100
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
