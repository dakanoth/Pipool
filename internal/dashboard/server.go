package dashboard

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// StatsSnapshot is the data pushed to dashboard clients every N seconds
type StatsSnapshot struct {
	Timestamp   time.Time      `json:"timestamp"`
	Uptime      string         `json:"uptime"`
	CPUTemp     float64        `json:"cpu_temp_c"`
	CPUUsage    float64        `json:"cpu_usage_pct"`
	RAMUsedGB   float64        `json:"ram_used_gb"`
	Throttling  bool           `json:"throttling"`
	TotalKHs    float64        `json:"total_khs"`
	BlocksFound uint64         `json:"blocks_found"`
	CoinbaseTag string         `json:"coinbase_tag"`
	Coins       []CoinStats    `json:"coins"`
	Workers     []WorkerStat   `json:"workers"`
	BlockLog    []BlockEvent   `json:"block_log"`
	Disks       []DiskStat     `json:"disks"`
	Notifs      NotifSettings  `json:"notifs"`
}

// DiskStat holds usage info for one mount point
type DiskStat struct {
	Label      string             `json:"label"`
	Mount      string             `json:"mount"`
	TotalGB    float64            `json:"total_gb"`
	UsedGB     float64            `json:"used_gb"`
	FreeGB     float64            `json:"free_gb"`
	UsedPct    float64            `json:"used_pct"`
	ChainSizes map[string]float64 `json:"chain_sizes"`
}

type CoinStats struct {
	Symbol        string  `json:"symbol"`
	Enabled       bool    `json:"enabled"`
	DaemonOnline  bool    `json:"daemon_online"`
	NodeHost      string  `json:"node_host"`
	NodeLatencyMs int64   `json:"node_latency_ms"`
	HashrateKHs   float64 `json:"hashrate_khs"`
	Miners        int32   `json:"miners"`
	Blocks        uint64  `json:"blocks"`
	Height        int64   `json:"height"`
	Headers       int64   `json:"headers"`       // chain tip headers seen (may be ahead of Height)
	SyncPct       float64 `json:"sync_pct"`      // 0.0–100.0; 100 = fully synced
	IBD           bool    `json:"ibd"`           // true while in initial block download
	Difficulty    float64 `json:"difficulty"`
	IsMergeAux    bool    `json:"is_merge_aux"`
	MergeParent   string  `json:"merge_parent,omitempty"`
}

type WorkerStat struct {
	Name           string  `json:"name"`
	Coin           string  `json:"coin"`
	Device         string  `json:"device"`
	Difficulty     float64 `json:"difficulty"`
	SharesAccepted uint64  `json:"shares_accepted"`
	SharesRejected uint64  `json:"shares_rejected"`
	SharesStale    uint64  `json:"shares_stale"`
	RemoteAddr     string  `json:"addr"`
	ConnectedAt    string  `json:"connected_at"`
	LastSeenAt     string  `json:"last_seen_at"`
	Online         bool    `json:"online"`
}

type BlockEvent struct {
	Coin    string `json:"coin"`
	Height  int64  `json:"height"`
	Hash    string `json:"hash"`
	Reward  string `json:"reward"`
	Worker  string `json:"worker"`
	FoundAt string `json:"found_at"`
}

// NotifSettings mirrors DiscordAlerts — lives here to avoid import cycle
type NotifSettings struct {
	BlockFound          bool `json:"block_found"`
	MinerConnected      bool `json:"miner_connected"`
	MinerDisconnect     bool `json:"miner_disconnect"`
	HighTemp            bool `json:"high_temp"`
	HashrateReport      bool `json:"hashrate_report"`
	HashrateIntervalMin int  `json:"hashrate_interval_min"`
	HashrateDropPct     int  `json:"hashrate_drop_pct"`
	NodeUnreachable     bool `json:"node_unreachable"`
}

// StatsFn returns the current pool snapshot
type StatsFn func() StatsSnapshot

// Server is the optional web dashboard
type Server struct {
	port         int
	pushInterval time.Duration
	getStats     StatsFn
	getNotifs    func() NotifSettings
	setNotifs    func(NotifSettings)

	mu      sync.Mutex
	clients map[chan string]struct{}
}

// New creates a new dashboard server
func New(port int, pushInterval time.Duration, stats StatsFn,
	getNotifs func() NotifSettings, setNotifs func(NotifSettings)) *Server {
	return &Server{
		port:         port,
		pushInterval: pushInterval,
		getStats:     stats,
		getNotifs:    getNotifs,
		setNotifs:    setNotifs,
		clients:      make(map[chan string]struct{}),
	}
}

// Start begins serving the dashboard
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/discord", s.handleDiscord)

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[dashboard] http://0.0.0.0%s", addr)

	go s.broadcastLoop()

	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(s.getStats())
}

func (s *Server) handleDiscord(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.getNotifs())
	case http.MethodPost:
		var ns NotifSettings
		if err := json.NewDecoder(r.Body).Decode(&ns); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.setNotifs(ns)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 8)
	s.mu.Lock()
	s.clients[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, ch)
		s.mu.Unlock()
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (s *Server) broadcastLoop() {
	ticker := time.NewTicker(s.pushInterval)
	defer ticker.Stop()
	for range ticker.C {
		snap := s.getStats()
		data, err := json.Marshal(snap)
		if err != nil {
			continue
		}
		s.mu.Lock()
		for ch := range s.clients {
			select {
			case ch <- string(data):
			default:
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>PIPOOL // TERMINAL</title>
<link href="https://fonts.googleapis.com/css2?family=VT323&family=Share+Tech+Mono&display=swap" rel="stylesheet">
<style>
:root {
  --bg:      #000800;
  --surf:    #000f00;
  --surf2:   #001500;
  --bdr:     #004400;
  --bdr2:    #007700;
  --hi:      #00ff41;
  --hi2:     #00cc33;
  --dim:     #008822;
  --dim2:    #005514;
  --off:     #002200;
  --red:     #ff4400;
  --amber:   #ffaa00;
  --scan:    'Share Tech Mono', monospace;
  --vt:      'VT323', monospace;
  --glow:    0 0 8px rgba(0,255,65,0.6);
  --glow2:   0 0 20px rgba(0,255,65,0.3);
}
* { margin:0; padding:0; box-sizing:border-box; }
html { scrollbar-width:thin; scrollbar-color:var(--bdr2) var(--bg); }

body {
  background:var(--bg); color:var(--hi2);
  font-family:var(--scan); min-height:100vh;
  overflow-x:hidden;
}

/* CRT scanlines */
body::after {
  content:''; position:fixed; inset:0; pointer-events:none; z-index:999;
  background: repeating-linear-gradient(
    0deg,
    transparent,
    transparent 3px,
    rgba(0,0,0,0.18) 3px,
    rgba(0,0,0,0.18) 4px
  );
}

/* CRT vignette */
body::before {
  content:''; position:fixed; inset:0; pointer-events:none; z-index:998;
  background: radial-gradient(ellipse at center,
    transparent 60%,
    rgba(0,0,0,0.55) 100%);
}

@keyframes flicker {
  0%,100%{opacity:1} 92%{opacity:1} 93%{opacity:.92} 94%{opacity:1} 97%{opacity:.96} 98%{opacity:1}
}
@keyframes blink { 0%,49%{opacity:1} 50%,100%{opacity:0} }
@keyframes pulse { 0%,100%{box-shadow:0 0 4px rgba(0,255,65,0.8)} 50%{box-shadow:0 0 12px rgba(0,255,65,0.2)} }
@keyframes scanin { from{opacity:0;transform:translateY(-4px)} to{opacity:1;transform:none} }

.wrap {
  max-width:1400px; margin:0 auto; padding:20px;
  position:relative; z-index:1;
  animation: flicker 8s infinite;
}

/* ── TOPBAR ── */
.topbar {
  display:flex; align-items:center; justify-content:space-between;
  padding:12px 20px; margin-bottom:20px;
  background:var(--surf); border:1px solid var(--bdr2);
  position:relative; overflow:hidden;
}
.topbar::before {
  content:''; position:absolute; left:0; top:0; bottom:0; width:3px;
  background:var(--hi);
  box-shadow: var(--glow);
}
.logo-text {
  font-family:var(--vt); font-size:2.6rem; letter-spacing:6px;
  color:var(--hi); text-shadow: var(--glow), var(--glow2);
  line-height:1;
}
.logo-sub {
  font-family:var(--scan); font-size:0.58rem; color:var(--dim);
  letter-spacing:4px; text-transform:uppercase; margin-top:2px;
}
.topbar-right { display:flex; align-items:center; gap:20px; }
.live-badge {
  display:flex; align-items:center; gap:6px;
  font-family:var(--scan); font-size:0.7rem; color:var(--hi);
  padding:4px 12px; border:1px solid var(--bdr2);
  background:var(--off);
}
.pulse { width:7px; height:7px; border-radius:50%; background:var(--hi); animation:pulse 2s infinite; }
.tag-badge {
  font-family:var(--scan); font-size:0.68rem; color:var(--hi2);
  padding:4px 10px; border:1px solid var(--bdr);
  background:var(--off);
}

/* ── SECTION ── */
.section {
  background:var(--surf); border:1px solid var(--bdr);
  margin-bottom:16px; overflow:hidden;
  animation: scanin .3s ease;
}
.section-head {
  display:flex; align-items:center; justify-content:space-between;
  padding:8px 16px; border-bottom:1px solid var(--bdr);
  background:var(--surf2);
}
.section-title {
  font-family:var(--vt); font-size:1.1rem; color:var(--hi);
  letter-spacing:3px; text-shadow: var(--glow);
}
.section-title::before { content:'> '; color:var(--dim2); }
.section-body { padding:16px; }

/* ── STAT CARDS ── */
.cards { display:grid; grid-template-columns:repeat(auto-fill,minmax(160px,1fr)); gap:10px; margin-bottom:16px; }
.card {
  background:var(--surf); border:1px solid var(--bdr);
  padding:14px 16px; position:relative;
  transition: border-color .2s;
}
.card:hover { border-color:var(--bdr2); }
.card-val {
  font-family:var(--vt); font-size:1.9rem; letter-spacing:2px;
  color:var(--hi); text-shadow: var(--glow);
  line-height:1.1; margin-bottom:4px;
}
.card-label {
  font-size:0.6rem; color:var(--dim); text-transform:uppercase;
  letter-spacing:2px; font-family:var(--scan);
}
.card-sub { font-size:0.62rem; color:var(--dim2); margin-top:4px; }

/* ── RESOURCE BARS ── */
.res-grid { display:grid; grid-template-columns:1fr 1fr 1fr; gap:16px; }
@media(max-width:700px){ .res-grid { grid-template-columns:1fr; } }
.res-top { display:flex; justify-content:space-between; margin-bottom:6px; }
.res-name { font-size:0.6rem; text-transform:uppercase; letter-spacing:2px; color:var(--dim); font-family:var(--scan); }
.res-val { font-family:var(--scan); font-size:0.72rem; color:var(--hi2); }
.bar-track { height:5px; background:var(--off); border:1px solid var(--bdr); }
.bar-fill { height:100%; transition:width 1.2s cubic-bezier(.4,0,.2,1); }
.bar-cpu  { background:var(--hi2); box-shadow: var(--glow); }
.bar-ram  { background:var(--hi); box-shadow: var(--glow); }
.bar-temp { background:linear-gradient(90deg,var(--hi2),var(--amber),var(--red)); }

/* ── COIN GRID ── */
.coin-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(270px,1fr)); gap:12px; }
.coin-card {
  border:1px solid var(--bdr); padding:14px;
  background:var(--surf2); position:relative;
  transition: border-color .2s;
}
.coin-card:hover { border-color:var(--bdr2); }
.coin-card::before {
  content:''; position:absolute; left:0; top:0; bottom:0; width:2px;
  background:var(--dim2);
}
.coin-card.offline { opacity:.35; }
.coin-card.merge::before { background:var(--dim); }
.coin-head { display:flex; align-items:center; gap:10px; margin-bottom:12px; }
.coin-badge {
  width:38px; height:38px; display:flex;
  align-items:center; justify-content:center;
  font-family:var(--scan); font-size:0.58rem; font-weight:700;
  border:1px solid var(--bdr2); background:var(--off); color:var(--hi);
  flex-shrink:0; letter-spacing:1px;
}
.coin-name { font-family:var(--vt); font-size:1.2rem; color:var(--hi); letter-spacing:2px; }
.coin-algo { font-family:var(--scan); font-size:.58rem; color:var(--dim2); margin-top:1px; }
.daemon-dot { width:7px; height:7px; border-radius:50%; margin-left:auto; flex-shrink:0; }
.daemon-dot.on  { background:var(--hi); box-shadow: var(--glow); animation:pulse 2s infinite; }
.daemon-dot.off { background:var(--red); }
.merge-tag {
  font-family:var(--scan); font-size:.58rem; color:var(--dim);
  border:1px solid var(--bdr); padding:2px 8px;
  display:inline-block; margin-bottom:10px;
}
.coin-stats-grid { display:grid; grid-template-columns:1fr 1fr; gap:8px; }
.cs-l { font-family:var(--scan); font-size:.56rem; color:var(--dim2); text-transform:uppercase; letter-spacing:1px; margin-bottom:2px; }
.cs-v { font-family:var(--scan); font-size:.76rem; color:var(--hi2); }
.node-host { font-family:var(--scan); font-size:.56rem; color:var(--dim2); margin-top:4px; display:flex; align-items:center; gap:6px; }
.node-latency { font-size:.56rem; font-family:var(--scan); padding:1px 6px; border:1px solid var(--bdr); }
.latency-good { color:var(--hi);   border-color:var(--dim2); }
.latency-ok   { color:var(--amber); border-color:#664400; }
.latency-bad  { color:var(--red);  border-color:#660000; }
.latency-off  { color:var(--dim2); }

/* ── SYNC PROGRESS ── */
.sync-bar-wrap { margin-top:10px; margin-bottom:6px; }
.sync-label { display:flex; justify-content:space-between; font-family:var(--scan); font-size:.56rem; color:var(--dim2); margin-bottom:4px; }
.sync-label-left { color:var(--dim); }
.sync-pct { color:var(--hi2); }
.sync-track { height:4px; background:var(--off); border:1px solid var(--bdr); }
.sync-fill { height:100%; transition:width 1.2s cubic-bezier(.4,0,.2,1); background:var(--hi2); box-shadow:0 0 6px rgba(0,255,65,0.4); }
.sync-fill.ibd { background:var(--amber); box-shadow:0 0 6px rgba(255,170,0,0.4); }
.sync-fill.done { background:var(--hi); box-shadow:0 0 8px rgba(0,255,65,0.6); }
.sync-blocks { font-family:var(--scan); font-size:.54rem; color:var(--dim2); margin-top:3px; }

/* ── SYNC PROGRESS ── */
.sync-wrap { margin-top:10px; margin-bottom:4px; }
.sync-row { display:flex; justify-content:space-between; font-family:var(--scan); font-size:.54rem; margin-bottom:3px; }
.sync-status { color:var(--dim); letter-spacing:1px; }
.sync-pct { color:var(--hi2); }
.sync-track { height:4px; background:var(--off); border:1px solid var(--bdr); overflow:hidden; }
.sync-fill { height:100%; transition:width 1.5s cubic-bezier(.4,0,.2,1); background:var(--hi2); box-shadow:0 0 5px rgba(0,255,65,0.4); }
.sync-fill.syncing { background:var(--amber); box-shadow:0 0 5px rgba(255,170,0,0.4); }
.sync-fill.done { background:var(--hi); box-shadow:0 0 8px rgba(0,255,65,0.7); }
.sync-blocks { font-family:var(--scan); font-size:.52rem; color:var(--dim2); margin-top:3px; }

/* ── WORKERS ── */
.workers-table { width:100%; border-collapse:collapse; font-size:.72rem; }
.workers-table th {
  font-family:var(--scan); font-size:.56rem; color:var(--dim);
  text-transform:uppercase; letter-spacing:2px; text-align:left;
  padding:6px 12px; border-bottom:1px solid var(--bdr); font-weight:400;
}
.workers-table td { padding:8px 12px; border-bottom:1px solid var(--off); font-family:var(--scan); }
.workers-table tr:last-child td { border-bottom:none; }
.workers-table tr:hover td { background:rgba(0,255,65,0.03); }
.worker-name { color:var(--hi2); }
.worker-device { color:var(--dim); font-size:.64rem; }
.coin-pill {
  font-family:var(--scan); font-size:.58rem; padding:1px 7px;
  border:1px solid var(--bdr2); color:var(--hi2); background:var(--off);
}
.diff-val, .shares-val { font-family:var(--scan); color:var(--hi2); font-size:.7rem; }
.addr-val { font-family:var(--scan); color:var(--dim2); font-size:.62rem; }
.no-workers, .no-blocks {
  text-align:center; padding:28px; color:var(--dim2);
  font-family:var(--scan); font-size:.72rem;
}
.worker-status-on  { font-family:var(--scan); font-size:.6rem; color:var(--hi); letter-spacing:1px; }
.worker-status-off { font-family:var(--scan); font-size:.6rem; color:var(--dim2); letter-spacing:1px; }
.worker-row-offline td { opacity:.5; }
.shares-rej { font-family:var(--scan); color:var(--red); font-size:.7rem; }

/* ── BLOCK LOG ── */
.block-log { font-family:var(--scan); }
.block-entry {
  display:grid; grid-template-columns:auto 1fr auto;
  gap:14px; align-items:center;
  padding:10px 0; border-bottom:1px solid var(--off);
}
.block-entry:last-child { border-bottom:none; }
.block-trophy { font-size:1.1rem; }
.block-coin-height { font-size:.76rem; color:var(--hi); margin-bottom:3px; }
.block-hash { font-size:.58rem; color:var(--dim2); }
.block-meta { text-align:right; }
.block-reward { font-size:.76rem; color:var(--hi); font-weight:600; }
.block-time { font-size:.58rem; color:var(--dim2); margin-top:2px; }

/* ── STORAGE ── */
.disk-wrap { display:grid; grid-template-columns:repeat(auto-fill,minmax(300px,1fr)); gap:14px; }
.disk-card { background:var(--surf2); border:1px solid var(--bdr); padding:14px; }
.disk-title { font-family:var(--vt); font-size:1rem; color:var(--hi); letter-spacing:2px; margin-bottom:2px; }
.disk-mount { font-family:var(--scan); font-size:.56rem; color:var(--dim2); margin-bottom:10px; }
.disk-summary { display:flex; justify-content:space-between; font-family:var(--scan); font-size:.66rem; margin-bottom:6px; }
.disk-free { color:var(--hi2); }
.disk-used { color:var(--dim); }
.bar-ssd, .bar-sd { background:var(--hi2); box-shadow: 0 0 6px rgba(0,255,65,0.4); }
.chain-sizes { margin-top:10px; display:grid; grid-template-columns:repeat(auto-fill,minmax(90px,1fr)); gap:6px; }
.chain-size { border:1px solid var(--bdr); padding:5px 7px; background:var(--off); }
.chain-size-label { font-family:var(--scan); font-size:.54rem; color:var(--dim2); text-transform:uppercase; letter-spacing:1px; }
.chain-size-val { font-family:var(--scan); font-size:.7rem; color:var(--hi2); margin-top:2px; }

/* ── NOTIFICATIONS ── */
.notif-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(300px,1fr)); gap:10px; }
.notif-row {
  display:flex; align-items:flex-start; gap:12px;
  padding:10px 12px; border:1px solid var(--bdr); background:var(--surf2);
  cursor:pointer; transition: border-color .15s;
  user-select:none;
}
.notif-row:hover { border-color:var(--bdr2); }
.notif-row.active { border-color:var(--dim); }
.notif-check {
  font-family:var(--vt); font-size:1.2rem; color:var(--hi);
  flex-shrink:0; width:22px; text-align:center; line-height:1.1;
  text-shadow: var(--glow);
}
.notif-check.off { color:var(--bdr2); text-shadow:none; }
.notif-label { font-family:var(--scan); font-size:.7rem; color:var(--hi2); }
.notif-desc { font-family:var(--scan); font-size:.58rem; color:var(--dim2); margin-top:2px; }
.notif-sub { display:flex; align-items:center; gap:8px; margin-top:6px; }
.notif-input {
  background:var(--off); border:1px solid var(--bdr2); color:var(--hi);
  font-family:var(--scan); font-size:.68rem;
  padding:3px 7px; width:58px; text-align:center;
}
.notif-input:focus { outline:none; border-color:var(--hi); box-shadow: var(--glow); }
.notif-unit { font-family:var(--scan); font-size:.6rem; color:var(--dim2); }
.notif-save-status { font-family:var(--scan); font-size:.62rem; color:var(--dim2); margin-left:auto; }

/* ── LAYOUT ── */
.row2 { display:grid; grid-template-columns:1fr 1fr; gap:14px; margin-bottom:16px; }
@media(max-width:900px){ .row2 { grid-template-columns:1fr; } }

footer {
  text-align:center; padding:16px;
  font-family:var(--scan); font-size:.58rem; color:var(--dim2);
  border-top:1px solid var(--bdr); margin-top:8px; letter-spacing:2px;
}
</style>
</head>
<body>
<div class="wrap">

<div class="topbar">
  <div>
    <div class="logo-text">PIPOOL</div>
    <div class="logo-sub">Raspberry Pi 5 &bull; Solo Mining Terminal</div>
  </div>
  <div class="topbar-right">
    <div class="tag-badge" id="coinbaseTag">&#9889; /PiPool/</div>
    <div>
      <div class="live-badge"><span class="pulse"></span><span id="liveStatus">CONNECTING</span></div>
      <div style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);margin-top:4px;text-align:right" id="lastUpdate">--</div>
    </div>
  </div>
</div>

<div class="cards">
  <div class="card">
    <div class="card-val" id="totalKhs">--</div>
    <div class="card-label">Total Hashrate</div>
  </div>
  <div class="card">
    <div class="card-val" id="blocksFound">0</div>
    <div class="card-label">Blocks Found</div>
  </div>
  <div class="card">
    <div class="card-val" id="totalMiners">0</div>
    <div class="card-label">Miners Online</div>
  </div>
  <div class="card">
    <div class="card-val" id="cpuTemp">--</div>
    <div class="card-label">CPU Temp</div>
    <div class="card-sub" id="throttleStatus"></div>
  </div>
  <div class="card">
    <div class="card-val" style="font-size:1.3rem" id="uptime">--</div>
    <div class="card-label">Uptime</div>
  </div>
</div>

<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">SYSTEM RESOURCES</span>
    <span style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)">Pi 5 &bull; 8 GB</span>
  </div>
  <div class="section-body">
    <div class="res-grid">
      <div>
        <div class="res-top"><span class="res-name">CPU</span><span class="res-val" id="cpuPct">--%</span></div>
        <div class="bar-track"><div class="bar-fill bar-cpu" id="cpuBar" style="width:0%"></div></div>
      </div>
      <div>
        <div class="res-top"><span class="res-name">RAM</span><span class="res-val" id="ramVal">--</span></div>
        <div class="bar-track"><div class="bar-fill bar-ram" id="ramBar" style="width:0%"></div></div>
      </div>
      <div>
        <div class="res-top"><span class="res-name">TEMP</span><span class="res-val" id="tempVal">--</span></div>
        <div class="bar-track"><div class="bar-fill bar-temp" id="tempBar" style="width:0%"></div></div>
      </div>
    </div>
  </div>
</div>

<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">COINS</span>
    <span id="daemonCount" style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)"></span>
  </div>
  <div class="section-body">
    <div class="coin-grid" id="coinGrid"></div>
  </div>
</div>

<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">STORAGE</span>
    <span id="storageUpdated" style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)"></span>
  </div>
  <div class="section-body">
    <div class="disk-wrap" id="diskWrap">
      <div style="font-family:var(--scan);font-size:.7rem;color:var(--dim2)">No storage data</div>
    </div>
  </div>
</div>

<div class="row2">
  <div class="section">
    <div class="section-head">
      <span class="section-title">WORKERS</span>
      <span id="workerCount" style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)">0</span>
    </div>
    <div class="section-body" style="padding:0">
      <table class="workers-table" id="workersTable">
        <thead><tr>
          <th>Worker</th><th>Coin</th><th>Diff</th><th>Accepted</th><th>Rejected</th><th>Status</th><th>IP</th>
        </tr></thead>
        <tbody id="workersBody">
          <tr><td colspan="7" class="no-workers">No miners connected</td></tr>
        </tbody>
      </table>
    </div>
  </div>
  <div class="section">
    <div class="section-head">
      <span class="section-title">BLOCK LOG</span>
      <span style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)">This session</span>
    </div>
    <div class="section-body" id="blockLog">
      <div class="no-blocks">No blocks found this session.<br>Keep hashing.</div>
    </div>
  </div>
</div>

<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">NOTIFICATIONS</span>
    <span id="notifSaveStatus" style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)">DISCORD COMMS</span>
  </div>
  <div class="section-body">
    <div class="notif-grid" id="notifGrid">
      <div class="notif-row" id="nr-block_found" onclick="toggleNotif('block_found')">
        <div class="notif-check" id="nc-block_found">&#9632;</div>
        <div><div class="notif-label">BLOCK FOUND</div><div class="notif-desc">Alert when a block is successfully mined</div></div>
      </div>
      <div class="notif-row" id="nr-miner_connected" onclick="toggleNotif('miner_connected')">
        <div class="notif-check" id="nc-miner_connected">&#9632;</div>
        <div><div class="notif-label">MINER CONNECTED</div><div class="notif-desc">Alert when a new miner joins the pool</div></div>
      </div>
      <div class="notif-row" id="nr-miner_disconnect" onclick="toggleNotif('miner_disconnect')">
        <div class="notif-check" id="nc-miner_disconnect">&#9632;</div>
        <div><div class="notif-label">MINER DISCONNECTED</div><div class="notif-desc">Alert when a miner drops offline</div></div>
      </div>
      <div class="notif-row" id="nr-high_temp" onclick="toggleNotif('high_temp')">
        <div class="notif-check" id="nc-high_temp">&#9632;</div>
        <div><div class="notif-label">HIGH TEMP</div><div class="notif-desc">Alert when CPU exceeds thermal limit</div></div>
      </div>
      <div class="notif-row" id="nr-node_unreachable" onclick="toggleNotif('node_unreachable')">
        <div class="notif-check" id="nc-node_unreachable">&#9632;</div>
        <div><div class="notif-label">NODE UNREACHABLE</div><div class="notif-desc">Alert when Windows PC node goes offline</div></div>
      </div>
      <div class="notif-row" id="nr-hashrate_report" onclick="toggleNotif('hashrate_report')">
        <div class="notif-check" id="nc-hashrate_report">&#9632;</div>
        <div>
          <div class="notif-label">HASHRATE REPORT</div>
          <div class="notif-desc">Periodic hashrate summary</div>
          <div class="notif-sub" onclick="event.stopPropagation()">
            <span class="notif-unit">Every</span>
            <input class="notif-input" id="ni-hashrate_interval" type="number" min="5" max="1440" value="60" onchange="saveNotifs()">
            <span class="notif-unit">min</span>
          </div>
        </div>
      </div>
      <div class="notif-row" id="nr-hashrate_drop" onclick="toggleNotif('hashrate_drop')">
        <div class="notif-check" id="nc-hashrate_drop">&#9632;</div>
        <div>
          <div class="notif-label">HASHRATE DROP</div>
          <div class="notif-desc">Alert on sudden hashrate drop</div>
          <div class="notif-sub" onclick="event.stopPropagation()">
            <span class="notif-unit">Threshold</span>
            <input class="notif-input" id="ni-hashrate_drop_pct" type="number" min="5" max="100" value="20" onchange="saveNotifs()">
            <span class="notif-unit">%</span>
          </div>
        </div>
      </div>
    </div>
  </div>
</div>

<footer>PIPOOL &bull; RASPBERRY PI 5 SOLO MINING TERMINAL &bull; <span id="footerTime"></span></footer>
</div>

<script>
var notifState = {
  block_found: true,
  miner_connected: true,
  miner_disconnect: false,
  high_temp: true,
  hashrate_report: true,
  hashrate_interval_min: 60,
  hashrate_drop_pct: 20,
  node_unreachable: true
};

var ALGO = {LTC:'SCRYPT',DOGE:'SCRYPT / AUXPOW',BTC:'SHA-256D',BCH:'SHA-256D / AUXPOW',PEP:'SCRYPT-N'};

function fmtHash(khs) {
  if (!khs) return '0 KH/S';
  if (khs >= 1e9) return (khs/1e9).toFixed(2)+' TH/S';
  if (khs >= 1e6) return (khs/1e6).toFixed(2)+' GH/S';
  if (khs >= 1e3) return (khs/1e3).toFixed(2)+' MH/S';
  return khs.toFixed(2)+' KH/S';
}

function fmtDiff(d) {
  if (!d) return '0';
  if (d >= 1e6) return (d/1e6).toFixed(2)+'M';
  if (d >= 1e3) return (d/1e3).toFixed(2)+'K';
  return d.toFixed(4);
}

function renderNotifState() {
  var keys = ['block_found','miner_connected','miner_disconnect','high_temp','node_unreachable','hashrate_report'];
  keys.forEach(function(k) {
    var chk = document.getElementById('nc-'+k);
    var row = document.getElementById('nr-'+k);
    if (!chk || !row) return;
    var on = notifState[k] || false;
    chk.innerHTML = on ? '&#9632;' : '&#9633;';
    chk.className = on ? 'notif-check' : 'notif-check off';
    row.className = on ? 'notif-row active' : 'notif-row';
  });
  // hashrate drop uses its own key name in the display
  var dropChk = document.getElementById('nc-hashrate_drop');
  var dropRow = document.getElementById('nr-hashrate_drop');
  if (dropChk && dropRow) {
    var dropOn = (notifState.hashrate_drop_pct || 0) > 0;
    dropChk.innerHTML = dropOn ? '&#9632;' : '&#9633;';
    dropChk.className = dropOn ? 'notif-check' : 'notif-check off';
    dropRow.className = dropOn ? 'notif-row active' : 'notif-row';
  }
  var intEl = document.getElementById('ni-hashrate_interval');
  if (intEl) intEl.value = notifState.hashrate_interval_min || 60;
  var pctEl = document.getElementById('ni-hashrate_drop_pct');
  if (pctEl) pctEl.value = notifState.hashrate_drop_pct || 20;
}

function toggleNotif(key) {
  if (key === 'hashrate_drop') {
    notifState.hashrate_drop_pct = notifState.hashrate_drop_pct > 0 ? 0 : 20;
    var pctEl = document.getElementById('ni-hashrate_drop_pct');
    if (pctEl) pctEl.value = notifState.hashrate_drop_pct;
  } else {
    notifState[key] = !notifState[key];
  }
  renderNotifState();
  saveNotifs();
}

function saveNotifs() {
  var intEl = document.getElementById('ni-hashrate_interval');
  var pctEl = document.getElementById('ni-hashrate_drop_pct');
  if (intEl) notifState.hashrate_interval_min = parseInt(intEl.value) || 60;
  if (pctEl) notifState.hashrate_drop_pct = parseInt(pctEl.value) || 0;

  var status = document.getElementById('notifSaveStatus');
  fetch('/api/discord', {
    method:'POST',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify(notifState)
  }).then(function(r) {
    if (status) status.textContent = r.ok ? 'SAVED' : 'ERROR';
    setTimeout(function(){ if(status) status.textContent = 'DISCORD COMMS'; }, 2000);
  }).catch(function() {
    if (status) status.textContent = 'ERROR';
    setTimeout(function(){ if(status) status.textContent = 'DISCORD COMMS'; }, 2000);
  });
}

function applyNotifs(n) {
  if (!n) return;
  notifState = {
    block_found:          n.block_found          || false,
    miner_connected:      n.miner_connected       || false,
    miner_disconnect:     n.miner_disconnect      || false,
    high_temp:            n.high_temp             || false,
    hashrate_report:      n.hashrate_report       || false,
    hashrate_interval_min: n.hashrate_interval_min || 60,
    hashrate_drop_pct:    n.hashrate_drop_pct     || 0,
    node_unreachable:     n.node_unreachable       || false
  };
  renderNotifState();
}

function apply(s) {
  document.getElementById('liveStatus').textContent = 'ONLINE';
  document.getElementById('lastUpdate').textContent = new Date(s.timestamp).toLocaleTimeString();
  if (s.coinbase_tag) document.getElementById('coinbaseTag').textContent = '&#9889; ' + s.coinbase_tag;

  document.getElementById('totalKhs').textContent   = fmtHash(s.total_khs||0);
  document.getElementById('blocksFound').textContent = s.blocks_found||0;
  document.getElementById('uptime').textContent      = s.uptime||'--';
  document.getElementById('cpuTemp').textContent     = (s.cpu_temp_c||0).toFixed(1)+'C';
  document.getElementById('throttleStatus').textContent = s.throttling ? '! THROTTLING' : '';

  document.getElementById('totalMiners').textContent = (s.workers||[]).length;

  var cpu = s.cpu_usage_pct||0, ram = s.ram_used_gb||0, temp = s.cpu_temp_c||0;
  document.getElementById('cpuPct').textContent = cpu.toFixed(1)+'%';
  document.getElementById('ramVal').textContent = ram.toFixed(2)+' / 8.0 GB';
  document.getElementById('tempVal').textContent = temp.toFixed(1)+'C';
  document.getElementById('cpuBar').style.width = Math.min(cpu,100)+'%';
  document.getElementById('ramBar').style.width = Math.min((ram/8)*100,100)+'%';
  document.getElementById('tempBar').style.width = Math.min((temp/90)*100,100)+'%';

  var coins = s.coins||[];
  document.getElementById('daemonCount').textContent = coins.filter(function(c){return c.daemon_online;}).length+'/'+coins.length+' ONLINE';

  var grid = document.getElementById('coinGrid');
  grid.innerHTML = '';
  coins.forEach(function(c) {
    var offline = !c.enabled || !c.daemon_online ? 'offline' : '';
    var mergeClass = c.is_merge_aux ? 'merge' : '';
    var dot = c.daemon_online
      ? '<span class="daemon-dot on"></span>'
      : '<span class="daemon-dot off"></span>';
    var merge = c.is_merge_aux
      ? '<div class="merge-tag">MERGE VIA '+c.merge_parent+'</div>'
      : '';

    var latCls = 'latency-off', latTxt = 'OFFLINE';
    if (c.daemon_online && c.node_latency_ms >= 0) {
      latTxt = c.node_latency_ms+'MS';
      latCls = c.node_latency_ms < 50 ? 'latency-good' : c.node_latency_ms < 200 ? 'latency-ok' : 'latency-bad';
    }
    var isLocal = c.node_host === '127.0.0.1' || c.node_host === 'localhost';
    var hostLabel = isLocal ? 'LOCAL' : (c.node_host||'').toUpperCase();
    var nodeRow = hostLabel
      ? '<div class="node-host">NODE: '+hostLabel+'<span class="node-latency '+latCls+'">'+latTxt+'</span></div>'
      : '';

    // Sync progress bar
    var syncHtml = '';
    if (c.daemon_online && typeof c.sync_pct === 'number' && c.sync_pct >= 0) {
      var pct = Math.min(Math.max(c.sync_pct, 0), 100);
      var done = pct >= 99.95;
      var fillCls = done ? 'sync-fill done' : (c.ibd ? 'sync-fill syncing' : 'sync-fill');
      var pctStr = done ? 'SYNCED' : pct.toFixed(2)+'%';
      var statusStr = done ? 'READY' : (c.ibd ? 'IBD' : 'SYNCING');
      var blocksStr = '';
      if (c.headers && c.headers > (c.height||0)) {
        blocksStr = (c.height||0).toLocaleString()+' / '+c.headers.toLocaleString()+' blocks';
      } else if (c.height) {
        blocksStr = c.height.toLocaleString()+' blocks';
      }
      syncHtml = '<div class="sync-wrap">' +
        '<div class="sync-row"><span class="sync-status">'+statusStr+'</span><span class="sync-pct">'+pctStr+'</span></div>' +
        '<div class="sync-track"><div class="'+fillCls+'" style="width:'+pct+'%"></div></div>' +
        (blocksStr ? '<div class="sync-blocks">'+blocksStr+'</div>' : '') +
        '</div>';
    }

    grid.innerHTML +=
      '<div class="coin-card '+offline+' '+mergeClass+'">' +
        '<div class="coin-head">' +
          '<div class="coin-badge">'+c.symbol+'</div>' +
          '<div><div class="coin-name">'+c.symbol+'</div><div class="coin-algo">'+(ALGO[c.symbol]||c.symbol)+'</div></div>' +
          dot +
        '</div>' +
        nodeRow + merge + syncHtml +
        '<div class="coin-stats-grid">' +
          '<div><div class="cs-l">Hashrate</div><div class="cs-v">'+fmtHash(c.hashrate_khs)+'</div></div>' +
          '<div><div class="cs-l">Miners</div><div class="cs-v">'+c.miners+'</div></div>' +
          '<div><div class="cs-l">Blocks</div><div class="cs-v">'+c.blocks+'</div></div>' +
          '<div><div class="cs-l">Height</div><div class="cs-v">'+(c.height||'--')+'</div></div>' +
        '</div>' +
      '</div>';
  });

  var workers = s.workers||[];
  document.getElementById('workerCount').textContent = workers.length+' CONNECTED';
  var tbody = document.getElementById('workersBody');
  if (workers.length === 0) {
    tbody.innerHTML = '<tr><td colspan="5" class="no-workers">NO MINERS CONNECTED</td></tr>';
  } else {
    tbody.innerHTML = workers.map(function(w) {
      return '<tr>' +
        '<td><div class="worker-name">'+(w.name||'ANON')+'</div><div class="worker-device">'+(w.device||'UNKNOWN')+'</div></td>' +
        '<td><span class="coin-pill">'+w.coin+'</span></td>' +
        '<td><span class="diff-val">'+fmtDiff(w.difficulty||0)+'</span></td>' +
        '<td><span class="shares-val">'+((w.shares||0).toLocaleString())+'</span></td>' +
        '<td><span class="addr-val">'+((w.addr||'--').split(':')[0])+'</span></td>' +
        '</tr>';
    }).join('');
  }

  var blocks = s.block_log||[];
  var logEl = document.getElementById('blockLog');
  if (blocks.length === 0) {
    logEl.innerHTML = '<div class="no-blocks">NO BLOCKS FOUND THIS SESSION.<br>KEEP HASHING.</div>';
  } else {
    logEl.innerHTML = '<div class="block-log">'+blocks.map(function(b){
      return '<div class="block-entry">' +
        '<div class="block-trophy">***</div>' +
        '<div><div class="block-coin-height">'+b.coin+' // BLOCK #'+b.height+'</div><div class="block-hash">'+b.hash+'</div><div style="font-size:.58rem;color:var(--dim2);margin-top:2px">FOUND BY '+b.worker+'</div></div>' +
        '<div class="block-meta"><div class="block-reward">'+b.reward+'</div><div class="block-time">'+b.found_at+'</div></div>' +
        '</div>';
    }).join('')+'</div>';
  }

  var disks = s.disks||[];
  var dw = document.getElementById('diskWrap');
  if (disks.length === 0) {
    dw.innerHTML = '<div style="font-family:var(--scan);font-size:.7rem;color:var(--dim2)">NO STORAGE DATA</div>';
  } else {
    dw.innerHTML = '';
    disks.forEach(function(d) {
      var pct = Math.min(d.used_pct,100).toFixed(1);
      var chainHtml = '';
      if (d.chain_sizes && Object.keys(d.chain_sizes).length > 0) {
        chainHtml = '<div class="chain-sizes">';
        Object.keys(d.chain_sizes).sort().forEach(function(sym) {
          var sz = d.chain_sizes[sym];
          var szStr = sz >= 1 ? sz.toFixed(1)+' GB' : (sz*1024).toFixed(0)+' MB';
          chainHtml += '<div class="chain-size"><div class="chain-size-label">'+sym+'</div><div class="chain-size-val">'+szStr+'</div></div>';
        });
        chainHtml += '</div>';
      }
      dw.innerHTML += '<div class="disk-card">' +
        '<div class="disk-title">'+d.label.toUpperCase()+'</div>' +
        '<div class="disk-mount">'+d.mount+'</div>' +
        '<div class="disk-summary"><span class="disk-used">USED '+d.used_gb.toFixed(1)+'/'+d.total_gb.toFixed(1)+' GB ('+pct+'%)</span><span class="disk-free">FREE '+d.free_gb.toFixed(1)+' GB</span></div>' +
        '<div class="bar-track"><div class="bar-fill bar-ssd" style="width:'+pct+'%"></div></div>' +
        chainHtml + '</div>';
    });
    document.getElementById('storageUpdated').textContent = new Date().toLocaleTimeString();
  }

  if (s.notifs) applyNotifs(s.notifs);

  document.getElementById('footerTime').textContent = new Date().toLocaleString();
}

var evtSrc = new EventSource('/api/events');
evtSrc.onmessage = function(e) { try { apply(JSON.parse(e.data)); } catch(err){} };
evtSrc.onerror = function() { document.getElementById('liveStatus').textContent = 'RECONNECTING'; };

fetch('/api/stats').then(function(r){return r.json();}).then(apply).catch(function(){});
fetch('/api/discord').then(function(r){return r.json();}).then(applyNotifs).catch(function(){});
</script>
</body>
</html>`
