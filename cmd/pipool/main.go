package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dakota/pipool/internal/config"
	"github.com/dakota/pipool/internal/ctl"
	"github.com/dakota/pipool/internal/dashboard"
	"github.com/dakota/pipool/internal/discord"
	"github.com/dakota/pipool/internal/merge"
	"github.com/dakota/pipool/internal/metrics"
	"github.com/dakota/pipool/internal/mining"
	"github.com/dakota/pipool/internal/rpc"
	"github.com/dakota/pipool/internal/stratum"
)

const version = "1.0.0"

func main() {
	// ─── Flags ────────────────────────────────────────────────────────────────
	cfgPath := flag.String("config", "configs/pipool.json", "Path to pipool.json config file")
	genConfig := flag.Bool("init", false, "Generate a default config file and exit")
	metricsPort := flag.Int("metrics-port", 9100, "Prometheus metrics port (0 to disable)")
	flag.Parse()

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
	notifier := discord.NewNotifier(cfg.Discord)

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

	// ─── Metrics registry ─────────────────────────────────────────────────────
	reg := metrics.NewRegistry()

	// ─── Control server (pipoolctl) ───────────────────────────────────────────
	ctlServer := ctl.NewServer()
	registerCtlHandlers(ctlServer, cfg, *cfgPath, servers, sysmon, reg, notifier)
	if err := ctlServer.Start(); err != nil {
		log.Printf("[ctl] warning: could not start control socket: %v", err)
	} else {
		log.Printf("[ctl] Unix socket ready at /var/run/pipool/pipool.sock")
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
	if cfg.Discord.Alerts.HashrateIntervalMin > 0 {
		reportInterval := time.Duration(cfg.Discord.Alerts.HashrateIntervalMin) * time.Minute
		go func() {
			ticker := time.NewTicker(reportInterval)
			defer ticker.Stop()
			for range ticker.C {
				notifier.HashrateReport(buildReportData(servers, sysmon))
			}
		}()
	}

	// ─── Web dashboard (always on) ───────────────────────────────────────────
	{
		dashPort := cfg.Dashboard.Port
		if dashPort == 0 {
			dashPort = 8080
		}
		pushInterval := time.Duration(cfg.Dashboard.PushIntervalS) * time.Second
		if pushInterval == 0 {
			pushInterval = 5 * time.Second
		}
		// Pre-build one RPC client per coin — reused every dashboard snapshot
		// instead of allocating a new http.Client every 5 seconds
		dashClients := make(map[string]*rpc.Client, len(cfg.Coins))
		for sym, coinCfg := range cfg.Coins {
			dashClients[sym] = rpc.NewClient(coinCfg.Node.Host, coinCfg.Node.Port,
				coinCfg.Node.User, coinCfg.Node.Password, sym)
		}
		dash := dashboard.New(dashPort, pushInterval, func() dashboard.StatsSnapshot {
			return buildDashboardSnapshot(cfg, servers, sysmon, dashClients)
		})
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
		Uptime:      sysmon.Uptime().String(),
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

	for sym, coinCfg := range cfg.Coins {
		cs := dashboard.CoinStats{
			Symbol:      sym,
			Enabled:     coinCfg.Enabled,
			IsMergeAux:  coinCfg.MergeParent != "",
			MergeParent: coinCfg.MergeParent,
		}

		// Use cached RPC client — avoids allocating a new http.Client every snapshot
		cli, ok := dashClients[sym]
		if !ok {
			cli = rpc.NewClient(coinCfg.Node.Host, coinCfg.Node.Port,
				coinCfg.Node.User, coinCfg.Node.Password, sym)
		}
		if err := cli.Ping(); err == nil {
			cs.DaemonOnline = true
			if h, err := cli.GetBlockCount(); err == nil {
				cs.Height = h
			}
		}

		if srv, ok := srvMap[sym]; ok {
			stats := srv.Stats()
			cs.Miners = stats.ConnectedMiners
			cs.Blocks = stats.BlocksFound
			upSecs := sysmon.Uptime().Seconds()
			if upSecs > 0 {
				cs.HashrateKHs = float64(stats.ValidShares) * coinCfg.Stratum.Vardiff.MinDiff / upSecs * 1000
			}
			totalKHs += cs.HashrateKHs
			totalBlocks += cs.Blocks
		}
		snap.Coins = append(snap.Coins, cs)
	}

	snap.TotalKHs = totalKHs
	snap.BlocksFound = totalBlocks

	// Workers across all servers
	for _, srv := range servers {
		sym := srv.Stats().Symbol
		for _, w := range srv.ConnectedWorkers() {
			snap.Workers = append(snap.Workers, dashboard.WorkerStat{
				Name:       w.Name,
				Coin:       sym,
				Device:     w.DeviceName,
				Difficulty: w.Difficulty,
				Shares:     w.ShareCount,
				RemoteAddr: w.RemoteAddr,
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
		khs := float64(stats.ValidShares) / sysmon.Uptime().Seconds() * 1000
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
					"shares": w.ShareCount,
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
			out = append(out, map[string]interface{}{
				"symbol":   stats.Symbol,
				"enabled":  true,
				"miners":   stats.ConnectedMiners,
				"hashrate": fmt.Sprintf("%.2f KH/s", float64(stats.ValidShares)/sysmon.Uptime().Seconds()*1000),
				"blocks":   stats.BlocksFound,
				"height":   "--",
			})
		}
		return ctl.Response{OK: true, Data: out}
	})

	ctlSrv.Register("coin", func(args []string) ctl.Response {
		if len(args) < 2 {
			return ctl.Response{OK: false, Message: "usage: coin <enable|disable> <SYMBOL>"}
		}
		action, symbol := args[0], args[1]
		coinCfg, ok := cfg.Coins[symbol]
		if !ok {
			return ctl.Response{OK: false, Message: fmt.Sprintf("unknown coin: %s", symbol)}
		}
		switch action {
		case "enable":
			coinCfg.Enabled = true
			cfg.Coins[symbol] = coinCfg
			return ctl.Response{OK: true, Message: fmt.Sprintf("%s enabled (restart stratum to take full effect)", symbol)}
		case "disable":
			coinCfg.Enabled = false
			cfg.Coins[symbol] = coinCfg
			return ctl.Response{OK: true, Message: fmt.Sprintf("%s disabled", symbol)}
		}
		return ctl.Response{OK: false, Message: fmt.Sprintf("unknown action: %s", action)}
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
