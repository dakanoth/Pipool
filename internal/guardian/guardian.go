// Package guardian provides an autonomous, rule-based monitoring and self-healing
// system for PiPool. It runs entirely inside the pool process — no external APIs,
// no tokens, no cloud dependencies. Think of it as a tireless operator watching
// every metric and taking corrective action in real time.
package guardian

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// ─── Interfaces ──────────────────────────────────────────────────────────────

// StratumServer is the read interface the guardian uses to inspect a stratum chain.
type StratumServer interface {
	Coin() string
	Stats() StratumStats
	Diag() DiagStats
	AllWorkers() []WorkerInfo
	KickWorker(name string) bool
}

// StratumStats mirrors stratum.Stats (avoid import cycle).
type StratumStats struct {
	Symbol              string
	Algorithm           string
	ConnectedMiners     int32
	TotalShares         uint64
	ValidShares         uint64
	BlocksFound         uint64
	ValidWorkSinceBlock float64
	LastNetworkDiff     float64
}

// DiagStats mirrors stratum.DiagStats.
type DiagStats struct {
	Symbol         string
	TotalShares    uint64
	ValidShares    uint64
	StaleShares    uint64
	RejectedShares uint64
	CurrentJobID   string
	CurrentJobAge  int64
	WorkerCount    int
	HasJob         bool
}

// WorkerInfo mirrors stratum.WorkerInfo (fields guardian cares about).
type WorkerInfo struct {
	Name           string
	DeviceName     string
	HashrateKHs    float64
	SharesAccepted uint64
	SharesRejected uint64
	SharesStale    uint64
	Difficulty     float64
	Online         bool
	RemoteAddr     string
	WattsEstimate  float64
	LastShareAt    time.Time
}

// Alerter sends alerts (typically Discord webhook).
type Alerter interface {
	GuardianAlert(severity, title, detail string)
}

// SystemMetrics provides host-level stats.
type SystemMetrics interface {
	CPUTemp() float64
	CPUUsage() float64
	RAMUsedGB() float64
}

// ─── Configuration ───────────────────────────────────────────────────────────

// Config tunes the guardian's thresholds. All have sensible defaults.
type Config struct {
	Enabled  bool `json:"enabled"`
	TickSec  int  `json:"tick_sec"`  // evaluation interval (default 30)
	LogLevel int  `json:"log_level"` // 0=errors only, 1=actions, 2=verbose

	// Worker health
	MaxStaleRatePct   float64 `json:"max_stale_rate_pct"`   // kick if stale% exceeds (default 40)
	MaxRejectRatePct  float64 `json:"max_reject_rate_pct"`  // kick if reject% exceeds (default 25)
	MinSharesForKick  int     `json:"min_shares_for_kick"`  // need this many shares before kick (default 50)
	SilentWorkerMin   int     `json:"silent_worker_min"`    // alert if no shares for N minutes (default 15)

	// Job freshness
	MaxJobAgeSec int `json:"max_job_age_sec"` // alert if job older than this (default 300 = 5 min)

	// Temperature
	TempWarnC     float64 `json:"temp_warn_c"`     // warn threshold (default 75)
	TempCritC     float64 `json:"temp_crit_c"`     // critical threshold (default 82)
	TempTrendWarn float64 `json:"temp_trend_warn"` // °C/min rise rate to warn (default 2.0)

	// Hashrate
	HashrateDropPct float64 `json:"hashrate_drop_pct"` // alert if hashrate drops >N% (default 50)
}

// DefaultConfig returns production-tuned defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:          true,
		TickSec:          30,
		LogLevel:         1,
		MaxStaleRatePct:  40,
		MaxRejectRatePct: 25,
		MinSharesForKick: 50,
		SilentWorkerMin:  15,
		MaxJobAgeSec:     300,
		TempWarnC:        75,
		TempCritC:        82,
		TempTrendWarn:    2.0,
		HashrateDropPct:  50,
	}
}

// ─── Guardian ────────────────────────────────────────────────────────────────

// Guardian is the autonomous rule engine.
type Guardian struct {
	cfg     Config
	servers []StratumServer
	alerter Alerter
	sysmet  SystemMetrics

	mu sync.Mutex
	// Rolling state for trend detection
	tempHistory    []tempSample        // last 10 readings for trend calc
	hrHistory      map[string]float64  // coin → last known total hashrate
	actionCooldown map[string]time.Time // "action:key" → earliest next fire
	workerWarned   map[string]time.Time // "coin:worker" → last warning time
	eventLog       []Event             // last 200 events (ring buffer)
	eventIdx       int

	stopCh chan struct{}
}

type tempSample struct {
	t    time.Time
	temp float64
}

// Event is a single guardian decision for the audit log.
type Event struct {
	Time     time.Time `json:"time"`
	Severity string    `json:"severity"` // INFO, WARN, ACTION, CRITICAL
	Rule     string    `json:"rule"`
	Coin     string    `json:"coin,omitempty"`
	Worker   string    `json:"worker,omitempty"`
	Detail   string    `json:"detail"`
	Action   string    `json:"action,omitempty"` // what was done (empty = alert only)
}

// New creates a guardian. Call Start() to begin the evaluation loop.
func New(cfg Config, servers []StratumServer, alerter Alerter, sysmet SystemMetrics) *Guardian {
	if cfg.TickSec <= 0 {
		cfg.TickSec = 30
	}
	return &Guardian{
		cfg:            cfg,
		servers:        servers,
		alerter:        alerter,
		sysmet:         sysmet,
		hrHistory:      make(map[string]float64),
		actionCooldown: make(map[string]time.Time),
		workerWarned:   make(map[string]time.Time),
		eventLog:       make([]Event, 200),
		stopCh:         make(chan struct{}),
	}
}

// Start begins the autonomous evaluation loop.
func (g *Guardian) Start() {
	go g.run()
	log.Printf("[guardian] started — tick every %ds, %d chains monitored", g.cfg.TickSec, len(g.servers))
}

// Stop halts the guardian.
func (g *Guardian) Stop() {
	close(g.stopCh)
}

// Events returns the last N events for the dashboard/ctl.
func (g *Guardian) Events(n int) []Event {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n > 200 {
		n = 200
	}
	out := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		idx := (g.eventIdx - 1 - i + 200) % 200
		ev := g.eventLog[idx]
		if ev.Time.IsZero() {
			break
		}
		out = append(out, ev)
	}
	return out
}

func (g *Guardian) run() {
	ticker := time.NewTicker(time.Duration(g.cfg.TickSec) * time.Second)
	defer ticker.Stop()

	// Run first evaluation immediately
	g.evaluate()

	for {
		select {
		case <-g.stopCh:
			log.Println("[guardian] stopped")
			return
		case <-ticker.C:
			g.evaluate()
		}
	}
}

// ─── Core evaluation loop ────────────────────────────────────────────────────

func (g *Guardian) evaluate() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[guardian] PANIC in evaluate: %v", r)
		}
	}()

	g.evalTemperature()
	g.evalSystem()

	for _, srv := range g.servers {
		g.evalChain(srv)
	}
}

// ─── Rule: Temperature trending ──────────────────────────────────────────────

func (g *Guardian) evalTemperature() {
	if g.sysmet == nil {
		return
	}
	temp := g.sysmet.CPUTemp()
	if temp <= 0 {
		return
	}

	g.mu.Lock()
	g.tempHistory = append(g.tempHistory, tempSample{time.Now(), temp})
	if len(g.tempHistory) > 20 {
		g.tempHistory = g.tempHistory[len(g.tempHistory)-20:]
	}
	history := make([]tempSample, len(g.tempHistory))
	copy(history, g.tempHistory)
	g.mu.Unlock()

	// Trend: °C per minute over last 5 readings
	if len(history) >= 5 {
		oldest := history[len(history)-5]
		newest := history[len(history)-1]
		dt := newest.t.Sub(oldest.t).Minutes()
		if dt > 0 {
			trend := (newest.temp - oldest.temp) / dt
			if trend >= g.cfg.TempTrendWarn && temp >= g.cfg.TempWarnC-5 {
				g.emit(Event{
					Severity: "WARN",
					Rule:     "temp_trend",
					Detail:   fmt.Sprintf("Temperature rising %.1f°C/min (%.1f°C → %.1f°C), currently %.1f°C", trend, oldest.temp, newest.temp, temp),
				})
				g.alert("WARN", "Temperature Rising Fast",
					fmt.Sprintf("Rate: **%.1f°C/min** — currently **%.1f°C**\nTrend: %.1f°C → %.1f°C over %.0fs", trend, temp, oldest.temp, newest.temp, dt*60))
			}
		}
	}

	if temp >= g.cfg.TempCritC {
		g.emit(Event{
			Severity: "CRITICAL",
			Rule:     "temp_critical",
			Detail:   fmt.Sprintf("CPU temperature CRITICAL: %.1f°C (limit %.1f°C)", temp, g.cfg.TempCritC),
		})
	}
}

// ─── Rule: System resources ──────────────────────────────────────────────────

func (g *Guardian) evalSystem() {
	if g.sysmet == nil {
		return
	}
	ram := g.sysmet.RAMUsedGB()
	cpu := g.sysmet.CPUUsage()

	// High RAM — potential OOM risk on Pi (8 GB total, but digibyted can eat 6+)
	if ram >= 7.0 {
		g.emitThrottled("sys_ram_critical", 5*time.Minute, Event{
			Severity: "CRITICAL",
			Rule:     "sys_ram",
			Detail:   fmt.Sprintf("RAM usage critical: %.2f GB / 8 GB — OOM kill risk", ram),
		})
	} else if ram >= 6.0 {
		g.emitThrottled("sys_ram_warn", 10*time.Minute, Event{
			Severity: "WARN",
			Rule:     "sys_ram",
			Detail:   fmt.Sprintf("RAM usage high: %.2f GB / 8 GB", ram),
		})
	}

	// Sustained high CPU
	if cpu >= 95 {
		g.emitThrottled("sys_cpu_critical", 5*time.Minute, Event{
			Severity: "WARN",
			Rule:     "sys_cpu",
			Detail:   fmt.Sprintf("CPU usage sustained at %.1f%%", cpu),
		})
	}
}

// ─── Rule: Per-chain evaluation ──────────────────────────────────────────────

func (g *Guardian) evalChain(srv StratumServer) {
	coin := srv.Coin()
	diag := srv.Diag()
	stats := srv.Stats()
	workers := srv.AllWorkers()

	g.evalJobFreshness(coin, diag)
	g.evalChainHealth(coin, diag, stats)
	g.evalHashrate(coin, stats, workers)

	for _, w := range workers {
		if !w.Online {
			continue
		}
		g.evalWorker(srv, coin, w)
	}
}

// ─── Rule: Job freshness ─────────────────────────────────────────────────────

func (g *Guardian) evalJobFreshness(coin string, diag DiagStats) {
	if !diag.HasJob {
		g.emitThrottled("no_job:"+coin, 2*time.Minute, Event{
			Severity: "CRITICAL",
			Rule:     "job_missing",
			Coin:     coin,
			Detail:   "No active job — miners have no work! Check daemon connectivity.",
		})
		return
	}

	maxAge := int64(g.cfg.MaxJobAgeSec)
	if diag.CurrentJobAge > maxAge {
		g.emitThrottled("job_stale:"+coin, 5*time.Minute, Event{
			Severity: "WARN",
			Rule:     "job_stale",
			Coin:     coin,
			Detail:   fmt.Sprintf("Job %s is %ds old (limit %ds) — daemon may be stuck or disconnected", diag.CurrentJobID, diag.CurrentJobAge, maxAge),
		})
	}
}

// ─── Rule: Chain share health ────────────────────────────────────────────────

func (g *Guardian) evalChainHealth(coin string, diag DiagStats, stats StratumStats) {
	total := diag.TotalShares
	if total < 100 {
		return // not enough data
	}

	stalePct := float64(diag.StaleShares) / float64(total) * 100
	rejectPct := float64(diag.RejectedShares) / float64(total) * 100

	if stalePct >= 25 {
		g.emitThrottled("chain_stale:"+coin, 10*time.Minute, Event{
			Severity: "WARN",
			Rule:     "chain_stale_high",
			Coin:     coin,
			Detail:   fmt.Sprintf("Chain-wide stale rate %.1f%% (%d/%d) — check block time vs share target", stalePct, diag.StaleShares, total),
		})
	}

	if rejectPct >= 10 {
		g.emitThrottled("chain_reject:"+coin, 10*time.Minute, Event{
			Severity: "WARN",
			Rule:     "chain_reject_high",
			Coin:     coin,
			Detail:   fmt.Sprintf("Chain-wide reject rate %.1f%% (%d/%d) — check miner firmware or vardiff settings", rejectPct, diag.RejectedShares, total),
		})
	}
}

// ─── Rule: Hashrate anomaly ──────────────────────────────────────────────────

func (g *Guardian) evalHashrate(coin string, stats StratumStats, workers []WorkerInfo) {
	var totalKHs float64
	for _, w := range workers {
		if w.Online {
			totalKHs += w.HashrateKHs
		}
	}

	g.mu.Lock()
	prev, hasPrev := g.hrHistory[coin]
	g.hrHistory[coin] = totalKHs
	g.mu.Unlock()

	if !hasPrev || prev <= 0 || totalKHs <= 0 {
		return
	}

	dropPct := (1.0 - totalKHs/prev) * 100
	if dropPct >= g.cfg.HashrateDropPct {
		g.emitThrottled("hr_drop:"+coin, 5*time.Minute, Event{
			Severity: "WARN",
			Rule:     "hashrate_drop",
			Coin:     coin,
			Detail:   fmt.Sprintf("Hashrate dropped %.0f%% on %s (%.0f KH/s → %.0f KH/s)", dropPct, coin, prev, totalKHs),
		})
		g.alert("WARN", fmt.Sprintf("%s Hashrate Drop", coin),
			fmt.Sprintf("**%.0f%%** drop detected\n%.0f KH/s → %.0f KH/s", dropPct, prev, totalKHs))
	}

	// Also detect miners going to zero
	onlineCount := 0
	for _, w := range workers {
		if w.Online {
			onlineCount++
		}
	}
	if stats.ConnectedMiners > 0 && onlineCount == 0 {
		g.emitThrottled("no_miners:"+coin, 5*time.Minute, Event{
			Severity: "WARN",
			Rule:     "no_miners",
			Coin:     coin,
			Detail:   fmt.Sprintf("All miners disconnected from %s", coin),
		})
	}
}

// ─── Rule: Individual worker health ──────────────────────────────────────────

func (g *Guardian) evalWorker(srv StratumServer, coin string, w WorkerInfo) {
	total := w.SharesAccepted + w.SharesRejected + w.SharesStale
	if total < uint64(g.cfg.MinSharesForKick) {
		return // not enough data to judge
	}

	stalePct := float64(w.SharesStale) / float64(total) * 100
	rejectPct := float64(w.SharesRejected) / float64(total) * 100
	key := coin + ":" + w.Name

	// Auto-kick for extreme stale rate
	if stalePct >= g.cfg.MaxStaleRatePct {
		if g.tryAction("kick_stale:"+key, 5*time.Minute) {
			kicked := srv.KickWorker(w.Name)
			action := "KICKED"
			if !kicked {
				action = "kick failed (already disconnected?)"
			}
			g.emit(Event{
				Severity: "ACTION",
				Rule:     "worker_stale_kick",
				Coin:     coin,
				Worker:   w.Name,
				Detail:   fmt.Sprintf("Stale rate %.1f%% exceeds limit %.1f%% (%d stale / %d total)", stalePct, g.cfg.MaxStaleRatePct, w.SharesStale, total),
				Action:   action,
			})
			g.alert("WARN", fmt.Sprintf("Guardian Kicked %s", w.Name),
				fmt.Sprintf("**%s** on **%s**: stale rate **%.1f%%** (limit %.1f%%)\nAction: %s", w.Name, coin, stalePct, g.cfg.MaxStaleRatePct, action))
		}
		return
	}

	// Auto-kick for extreme reject rate
	if rejectPct >= g.cfg.MaxRejectRatePct {
		if g.tryAction("kick_reject:"+key, 5*time.Minute) {
			kicked := srv.KickWorker(w.Name)
			action := "KICKED"
			if !kicked {
				action = "kick failed"
			}
			g.emit(Event{
				Severity: "ACTION",
				Rule:     "worker_reject_kick",
				Coin:     coin,
				Worker:   w.Name,
				Detail:   fmt.Sprintf("Reject rate %.1f%% exceeds limit %.1f%% (%d rejected / %d total)", rejectPct, g.cfg.MaxRejectRatePct, w.SharesRejected, total),
				Action:   action,
			})
			g.alert("WARN", fmt.Sprintf("Guardian Kicked %s", w.Name),
				fmt.Sprintf("**%s** on **%s**: reject rate **%.1f%%** (limit %.1f%%)\nAction: %s", w.Name, coin, rejectPct, g.cfg.MaxRejectRatePct, action))
		}
		return
	}

	// Warn for elevated rates (below kick threshold)
	if stalePct >= 15 || rejectPct >= 5 {
		g.emitThrottled("worker_health:"+key, 15*time.Minute, Event{
			Severity: "WARN",
			Rule:     "worker_health",
			Coin:     coin,
			Worker:   w.Name,
			Detail:   fmt.Sprintf("Elevated rates — stale: %.1f%%, reject: %.1f%% (%d total shares)", stalePct, rejectPct, total),
		})
	}

	// Silent worker detection
	if !w.LastShareAt.IsZero() && w.Online {
		silentMin := time.Since(w.LastShareAt).Minutes()
		if silentMin >= float64(g.cfg.SilentWorkerMin) {
			g.emitThrottled("silent:"+key, 30*time.Minute, Event{
				Severity: "WARN",
				Rule:     "worker_silent",
				Coin:     coin,
				Worker:   w.Name,
				Detail:   fmt.Sprintf("Worker connected but no shares for %.0f minutes (limit %d min)", silentMin, g.cfg.SilentWorkerMin),
			})
		}
	}

	// Zero hashrate while connected
	if w.Online && w.HashrateKHs <= 0 && total > 10 {
		g.emitThrottled("zero_hr:"+key, 10*time.Minute, Event{
			Severity: "WARN",
			Rule:     "worker_zero_hashrate",
			Coin:     coin,
			Worker:   w.Name,
			Detail:   fmt.Sprintf("Worker online but 0 hashrate (has %d historical shares) — may be stuck", total),
		})
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func (g *Guardian) emit(ev Event) {
	ev.Time = time.Now()
	g.mu.Lock()
	g.eventLog[g.eventIdx%200] = ev
	g.eventIdx++
	g.mu.Unlock()

	if g.cfg.LogLevel >= 1 || ev.Severity == "CRITICAL" || ev.Severity == "ACTION" {
		prefix := "[guardian]"
		if ev.Coin != "" {
			prefix = fmt.Sprintf("[guardian/%s]", ev.Coin)
		}
		if ev.Action != "" {
			log.Printf("%s %s: %s → %s", prefix, ev.Rule, ev.Detail, ev.Action)
		} else {
			log.Printf("%s %s: %s", prefix, ev.Rule, ev.Detail)
		}
	}
}

func (g *Guardian) emitThrottled(key string, cooldown time.Duration, ev Event) {
	g.mu.Lock()
	last, ok := g.actionCooldown[key]
	if ok && time.Since(last) < cooldown {
		g.mu.Unlock()
		return
	}
	g.actionCooldown[key] = time.Now()
	// Prune old cooldowns (prevent unbounded map growth)
	if len(g.actionCooldown) > 500 {
		cutoff := time.Now().Add(-1 * time.Hour)
		for k, t := range g.actionCooldown {
			if t.Before(cutoff) {
				delete(g.actionCooldown, k)
			}
		}
	}
	g.mu.Unlock()
	g.emit(ev)
}

func (g *Guardian) tryAction(key string, cooldown time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	last, ok := g.actionCooldown[key]
	if ok && time.Since(last) < cooldown {
		return false
	}
	g.actionCooldown[key] = time.Now()
	return true
}

func (g *Guardian) alert(severity, title, detail string) {
	if g.alerter == nil {
		return
	}
	g.alerter.GuardianAlert(severity, title, detail)
}

// Unused is a compile-time assertion for the math import.
var _ = math.Abs
