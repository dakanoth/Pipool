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
	Timestamp   time.Time   `json:"timestamp"`
	Uptime      string      `json:"uptime"`
	CPUTemp     float64     `json:"cpu_temp_c"`
	CPUUsage    float64     `json:"cpu_usage_pct"`
	RAMUsedGB   float64     `json:"ram_used_gb"`
	Throttling  bool        `json:"throttling"`
	TotalKHs    float64     `json:"total_khs"`
	BlocksFound uint64      `json:"blocks_found"`
	CoinbaseTag string      `json:"coinbase_tag"`
	Coins       []CoinStats `json:"coins"`
	Workers     []WorkerStat `json:"workers"`
	BlockLog    []BlockEvent `json:"block_log"`
}

type CoinStats struct {
	Symbol      string  `json:"symbol"`
	Enabled     bool    `json:"enabled"`
	DaemonOnline bool   `json:"daemon_online"`
	HashrateKHs float64 `json:"hashrate_khs"`
	Miners      int32   `json:"miners"`
	Blocks      uint64  `json:"blocks"`
	Height      int64   `json:"height"`
	Difficulty  float64 `json:"difficulty"`
	IsMergeAux  bool    `json:"is_merge_aux"`
	MergeParent string  `json:"merge_parent,omitempty"`
}

type WorkerStat struct {
	Name       string  `json:"name"`
	Coin       string  `json:"coin"`
	Device     string  `json:"device"`
	Difficulty float64 `json:"difficulty"`
	Shares     uint64  `json:"shares"`
	RemoteAddr string  `json:"addr"`
	ConnectedAt string `json:"connected_at"`
}

type BlockEvent struct {
	Coin      string `json:"coin"`
	Height    int64  `json:"height"`
	Hash      string `json:"hash"`
	Reward    string `json:"reward"`
	Worker    string `json:"worker"`
	FoundAt   string `json:"found_at"`
}

// StatsFn is a function that returns the current pool stats
type StatsFn func() StatsSnapshot

// Server is the optional web dashboard
type Server struct {
	port         int
	pushInterval time.Duration
	getStats     StatsFn

	mu      sync.Mutex
	clients map[chan string]struct{}
}

// New creates a new dashboard server
func New(port int, pushInterval time.Duration, stats StatsFn) *Server {
	return &Server{
		port:         port,
		pushInterval: pushInterval,
		getStats:     stats,
		clients:      make(map[chan string]struct{}),
	}
}

// Start begins serving the dashboard — no auth, open HTTP
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/events", s.handleSSE)

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[dashboard] http://0.0.0.0%s — open, no auth", addr)

	go s.broadcastLoop()

	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(s.getStats())
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
<title>PiPool</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@300;400;600;700&family=Bebas+Neue&family=Inter:wght@300;400;500;600&display=swap" rel="stylesheet">
<style>
:root {
  --bg: #080c10;
  --surface: #0d1318;
  --surface2: #111920;
  --border: #1c2d3a;
  --border2: #243545;
  --accent: #00e5ff;
  --accent2: #0099bb;
  --green: #00ff88;
  --red: #ff4466;
  --yellow: #ffcc00;
  --orange: #ff7700;
  --purple: #aa66ff;
  --text: #d0e8f4;
  --text2: #7a9bb0;
  --text3: #3d5a6a;
  --mono: 'JetBrains Mono', monospace;
  --display: 'Bebas Neue', sans-serif;
  --sans: 'Inter', sans-serif;
}
* { margin:0; padding:0; box-sizing:border-box; }
html { scrollbar-width: thin; scrollbar-color: var(--border2) var(--bg); }
body { background:var(--bg); color:var(--text); font-family:var(--sans); min-height:100vh; overflow-x:hidden; }

/* scanline overlay */
body::after {
  content:''; position:fixed; inset:0; pointer-events:none; z-index:999;
  background: repeating-linear-gradient(0deg, transparent, transparent 2px, rgba(0,0,0,0.04) 2px, rgba(0,0,0,0.04) 4px);
}

/* grid bg */
body::before {
  content:''; position:fixed; inset:0; pointer-events:none; z-index:0;
  background-image:
    linear-gradient(rgba(0,229,255,0.02) 1px, transparent 1px),
    linear-gradient(90deg, rgba(0,229,255,0.02) 1px, transparent 1px);
  background-size: 48px 48px;
}

.wrap { max-width: 1400px; margin:0 auto; padding: 20px; position:relative; z-index:1; }

/* ── TOPBAR ── */
.topbar {
  display:flex; align-items:center; justify-content:space-between;
  padding: 14px 20px; margin-bottom:20px;
  background:var(--surface); border:1px solid var(--border);
  border-radius:8px; position:relative; overflow:hidden;
}
.topbar::before {
  content:''; position:absolute; inset:0;
  background: linear-gradient(90deg, rgba(0,229,255,0.06), transparent 40%);
  pointer-events:none;
}
.logo { display:flex; align-items:baseline; gap:10px; }
.logo-text {
  font-family:var(--display); font-size:2rem; letter-spacing:3px;
  color:var(--accent); text-shadow:0 0 30px rgba(0,229,255,0.5);
  line-height:1;
}
.logo-sub { font-family:var(--mono); font-size:0.6rem; color:var(--text3); letter-spacing:4px; text-transform:uppercase; }
.topbar-right { display:flex; align-items:center; gap:16px; text-align:right; }
.live-badge {
  display:flex; align-items:center; gap:6px;
  font-family:var(--mono); font-size:0.7rem; color:var(--green);
  padding:4px 10px; border:1px solid rgba(0,255,136,0.2);
  border-radius:20px; background:rgba(0,255,136,0.05);
}
.pulse { width:6px; height:6px; border-radius:50%; background:var(--green); animation: pulse 2s infinite; }
@keyframes pulse { 0%,100%{opacity:1;box-shadow:0 0 0 0 rgba(0,255,136,0.4);} 50%{opacity:.6;box-shadow:0 0 0 4px rgba(0,255,136,0);} }
.tag-badge {
  font-family:var(--mono); font-size:0.68rem; color:var(--accent);
  padding:4px 10px; border:1px solid var(--border2);
  border-radius:4px; background:rgba(0,229,255,0.05);
}

/* ── STAT CARDS ── */
.cards { display:grid; grid-template-columns:repeat(auto-fill,minmax(170px,1fr)); gap:12px; margin-bottom:20px; }
.card {
  background:var(--surface); border:1px solid var(--border);
  border-radius:8px; padding:16px 18px; position:relative; overflow:hidden;
  transition: border-color .2s;
}
.card:hover { border-color:var(--border2); }
.card-accent { position:absolute; top:0; left:0; right:0; height:2px; }
.card-val {
  font-family:var(--display); font-size:1.7rem; letter-spacing:1px;
  line-height:1.1; margin-bottom:4px;
}
.card-label { font-size:0.62rem; color:var(--text3); text-transform:uppercase; letter-spacing:2px; font-family:var(--mono); }
.card-sub { font-family:var(--mono); font-size:0.65rem; color:var(--text2); margin-top:4px; }

/* ── SECTIONS ── */
.section { background:var(--surface); border:1px solid var(--border); border-radius:8px; margin-bottom:16px; overflow:hidden; }
.section-head {
  display:flex; align-items:center; justify-content:space-between;
  padding:12px 18px; border-bottom:1px solid var(--border);
  background:var(--surface2);
}
.section-title { font-family:var(--mono); font-size:0.68rem; color:var(--text2); text-transform:uppercase; letter-spacing:3px; }
.section-body { padding:18px; }

/* ── RESOURCE BARS ── */
.res-grid { display:grid; grid-template-columns:1fr 1fr 1fr; gap:16px; }
@media(max-width:700px){ .res-grid { grid-template-columns:1fr; } }
.res-item {}
.res-top { display:flex; justify-content:space-between; margin-bottom:7px; }
.res-name { font-size:0.65rem; text-transform:uppercase; letter-spacing:2px; color:var(--text3); font-family:var(--mono); }
.res-val { font-family:var(--mono); font-size:0.75rem; color:var(--text); }
.bar-track { height:4px; background:rgba(255,255,255,0.04); border-radius:2px; overflow:hidden; }
.bar-fill { height:100%; border-radius:2px; transition:width 1.2s cubic-bezier(.4,0,.2,1); }
.bar-cpu  { background: linear-gradient(90deg, #0066ff, var(--accent)); }
.bar-ram  { background: linear-gradient(90deg, #7700cc, var(--purple)); }
.bar-temp { background: linear-gradient(90deg, var(--green), var(--yellow), var(--orange)); }

/* ── COIN GRID ── */
.coin-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(280px,1fr)); gap:14px; }
.coin-card {
  border:1px solid var(--border); border-radius:8px; padding:16px;
  background:var(--surface2); position:relative; overflow:hidden;
  transition: border-color .2s, transform .15s;
}
.coin-card:hover { border-color:var(--border2); transform:translateY(-1px); }
.coin-card.offline { opacity:.45; }
.coin-card.merge { border-left: 2px solid var(--accent2); }
.coin-head { display:flex; align-items:center; gap:12px; margin-bottom:14px; }
.coin-badge {
  width:40px; height:40px; border-radius:8px; display:flex;
  align-items:center; justify-content:center;
  font-family:var(--mono); font-size:0.65rem; font-weight:700; flex-shrink:0;
}
.ltc  { background:linear-gradient(135deg,#c8c8c8,#888); color:#111; }
.doge { background:linear-gradient(135deg,#e8c84a,#b8940a); color:#111; }
.btc  { background:linear-gradient(135deg,#ffaa44,#f7931a); color:#111; }
.bch  { background:linear-gradient(135deg,#8fd64a,#5aa010); color:#111; }
.pep  { background:linear-gradient(135deg,#66cc66,#338833); color:#fff; }
.coin-name-wrap {}
.coin-name { font-weight:600; font-size:.95rem; }
.coin-algo { font-family:var(--mono); font-size:.6rem; color:var(--text3); margin-top:2px; }
.daemon-dot {
  width:7px; height:7px; border-radius:50%; margin-left:auto; flex-shrink:0;
  transition:background .3s;
}
.daemon-dot.on  { background:var(--green); box-shadow:0 0 6px rgba(0,255,136,0.5); }
.daemon-dot.off { background:var(--red); }
.merge-tag {
  font-family:var(--mono); font-size:.58rem; color:var(--accent2);
  background:rgba(0,153,187,0.1); border:1px solid rgba(0,153,187,0.2);
  padding:2px 8px; border-radius:10px; display:inline-block; margin-bottom:10px;
}
.coin-stats-grid { display:grid; grid-template-columns:1fr 1fr; gap:8px; }
.cs { }
.cs-l { font-family:var(--mono); font-size:.58rem; color:var(--text3); text-transform:uppercase; letter-spacing:1px; margin-bottom:2px; }
.cs-v { font-family:var(--mono); font-size:.78rem; font-weight:600; color:var(--text); }

/* ── WORKERS TABLE ── */
.workers-table { width:100%; border-collapse:collapse; font-size:.75rem; }
.workers-table th {
  font-family:var(--mono); font-size:.58rem; color:var(--text3);
  text-transform:uppercase; letter-spacing:2px; text-align:left;
  padding:8px 12px; border-bottom:1px solid var(--border); font-weight:400;
}
.workers-table td { padding:9px 12px; border-bottom:1px solid rgba(28,45,58,0.5); }
.workers-table tr:last-child td { border-bottom:none; }
.workers-table tr:hover td { background:rgba(255,255,255,0.015); }
.worker-name { font-family:var(--mono); font-weight:600; color:var(--text); }
.worker-device { color:var(--text2); font-family:var(--mono); font-size:.68rem; }
.coin-pill {
  font-family:var(--mono); font-size:.6rem; padding:2px 8px;
  border-radius:10px; display:inline-block;
}
.pill-ltc  { background:rgba(200,200,200,.1); color:#c8c8c8; border:1px solid rgba(200,200,200,.2); }
.pill-doge { background:rgba(232,200,74,.1); color:#e8c84a; border:1px solid rgba(232,200,74,.2); }
.pill-btc  { background:rgba(247,147,26,.1); color:#f7931a; border:1px solid rgba(247,147,26,.2); }
.pill-bch  { background:rgba(143,214,74,.1); color:#8fd64a; border:1px solid rgba(143,214,74,.2); }
.pill-pep  { background:rgba(102,204,102,.1); color:#66cc66; border:1px solid rgba(102,204,102,.2); }
.diff-val { font-family:var(--mono); color:var(--accent); font-size:.72rem; }
.shares-val { font-family:var(--mono); color:var(--text2); font-size:.72rem; }
.addr-val { font-family:var(--mono); color:var(--text3); font-size:.65rem; }
.no-workers { text-align:center; padding:32px; color:var(--text3); font-family:var(--mono); font-size:.75rem; }

/* ── BLOCK LOG ── */
.block-log { font-family:var(--mono); }
.block-entry {
  display:grid; grid-template-columns:auto 1fr auto;
  gap:16px; align-items:center;
  padding:12px 0; border-bottom:1px solid rgba(28,45,58,0.5);
}
.block-entry:last-child { border-bottom:none; }
.block-trophy { font-size:1.2rem; }
.block-info {}
.block-coin-height { font-size:.78rem; font-weight:600; margin-bottom:3px; }
.block-hash { font-size:.6rem; color:var(--text3); }
.block-meta { text-align:right; }
.block-reward { font-size:.78rem; color:var(--green); font-weight:600; }
.block-time { font-size:.6rem; color:var(--text3); margin-top:2px; }
.no-blocks { text-align:center; padding:32px; color:var(--text3); font-size:.75rem; }

/* ── TWO-COL LAYOUT ── */
.row2 { display:grid; grid-template-columns:1fr 1fr; gap:16px; margin-bottom:16px; }
@media(max-width:900px){ .row2 { grid-template-columns:1fr; } }

footer {
  text-align:center; padding:20px;
  font-family:var(--mono); font-size:.6rem; color:var(--text3);
  border-top:1px solid var(--border); margin-top:8px; letter-spacing:2px;
}
</style>
</head>
<body>
<div class="wrap">

<!-- TOPBAR -->
<div class="topbar">
  <div class="logo">
    <div>
      <div class="logo-text">PIPOOL</div>
      <div class="logo-sub">Raspberry Pi 5 · Solo Mining</div>
    </div>
  </div>
  <div class="topbar-right">
    <div class="tag-badge" id="coinbaseTag">⛏ /PiPool/</div>
    <div>
      <div class="live-badge"><span class="pulse"></span><span id="liveStatus">CONNECTING</span></div>
      <div style="font-family:var(--mono);font-size:.58rem;color:var(--text3);margin-top:5px;text-align:right" id="lastUpdate">--</div>
    </div>
  </div>
</div>

<!-- STAT CARDS -->
<div class="cards">
  <div class="card">
    <div class="card-accent" style="background:linear-gradient(90deg,var(--accent),transparent)"></div>
    <div class="card-val" id="totalKhs" style="color:var(--accent)">--</div>
    <div class="card-label">Total Hashrate</div>
  </div>
  <div class="card">
    <div class="card-accent" style="background:linear-gradient(90deg,var(--green),transparent)"></div>
    <div class="card-val" id="blocksFound" style="color:var(--green)">0</div>
    <div class="card-label">Blocks Found</div>
  </div>
  <div class="card">
    <div class="card-accent" style="background:linear-gradient(90deg,var(--purple),transparent)"></div>
    <div class="card-val" id="totalMiners" style="color:var(--purple)">0</div>
    <div class="card-label">Connected Miners</div>
  </div>
  <div class="card">
    <div class="card-accent" style="background:linear-gradient(90deg,var(--yellow),transparent)"></div>
    <div class="card-val" id="cpuTemp" style="color:var(--yellow)">--</div>
    <div class="card-label">CPU Temp</div>
    <div class="card-sub" id="throttleStatus"></div>
  </div>
  <div class="card">
    <div class="card-accent" style="background:linear-gradient(90deg,var(--text2),transparent)"></div>
    <div class="card-val" id="uptime" style="color:var(--text);font-size:1.2rem">--</div>
    <div class="card-label">Uptime</div>
  </div>
</div>

<!-- SYSTEM RESOURCES -->
<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">System Resources</span>
    <span style="font-family:var(--mono);font-size:.6rem;color:var(--text3)">Pi 5 · 8GB RAM</span>
  </div>
  <div class="section-body">
    <div class="res-grid">
      <div class="res-item">
        <div class="res-top"><span class="res-name">CPU</span><span class="res-val" id="cpuPct">--%</span></div>
        <div class="bar-track"><div class="bar-fill bar-cpu" id="cpuBar" style="width:0%"></div></div>
      </div>
      <div class="res-item">
        <div class="res-top"><span class="res-name">RAM</span><span class="res-val" id="ramVal">-- / 8.0 GB</span></div>
        <div class="bar-track"><div class="bar-fill bar-ram" id="ramBar" style="width:0%"></div></div>
      </div>
      <div class="res-item">
        <div class="res-top"><span class="res-name">Temp</span><span class="res-val" id="tempVal">--°C</span></div>
        <div class="bar-track"><div class="bar-fill bar-temp" id="tempBar" style="width:0%"></div></div>
      </div>
    </div>
  </div>
</div>

<!-- COIN CARDS -->
<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">Coins</span>
    <span id="daemonCount" style="font-family:var(--mono);font-size:.6rem;color:var(--text3)"></span>
  </div>
  <div class="section-body">
    <div class="coin-grid" id="coinGrid"></div>
  </div>
</div>

<!-- WORKERS + BLOCK LOG side by side -->
<div class="row2">
  <div class="section">
    <div class="section-head">
      <span class="section-title">Connected Workers</span>
      <span id="workerCount" style="font-family:var(--mono);font-size:.6rem;color:var(--text3)">0</span>
    </div>
    <div class="section-body" style="padding:0">
      <table class="workers-table" id="workersTable">
        <thead>
          <tr>
            <th>Worker</th>
            <th>Coin</th>
            <th>Diff</th>
            <th>Shares</th>
            <th>IP</th>
          </tr>
        </thead>
        <tbody id="workersBody">
          <tr><td colspan="5" class="no-workers">No miners connected</td></tr>
        </tbody>
      </table>
    </div>
  </div>

  <div class="section">
    <div class="section-head">
      <span class="section-title">Block Log</span>
      <span style="font-family:var(--mono);font-size:.6rem;color:var(--text3)">This session</span>
    </div>
    <div class="section-body" id="blockLog">
      <div class="no-blocks">No blocks found yet this session.<br>Keep hashing — it only takes one. 🎰</div>
    </div>
  </div>
</div>

<footer>PIPOOL · RASPBERRY PI 5 SOLO MINING POOL · <span id="footerTime"></span></footer>
</div>

<script>
const ALGO = {LTC:'Scrypt',DOGE:'Scrypt · AuxPoW',BTC:'SHA-256d',BCH:'SHA-256d · AuxPoW',PEP:'Scrypt-N'};
const COIN_CLASS = {LTC:'ltc',DOGE:'doge',BTC:'btc',BCH:'bch',PEP:'pep'};

function fmtHash(khs) {
  if (!khs) return '0 KH/s';
  if (khs >= 1e9) return (khs/1e9).toFixed(2)+' TH/s';
  if (khs >= 1e6) return (khs/1e6).toFixed(2)+' GH/s';
  if (khs >= 1e3) return (khs/1e3).toFixed(2)+' MH/s';
  return khs.toFixed(2)+' KH/s';
}

function fmtDiff(d) {
  if (d >= 1e6) return (d/1e6).toFixed(2)+'M';
  if (d >= 1e3) return (d/1e3).toFixed(2)+'K';
  return d.toFixed(4);
}

function apply(s) {
  // topbar
  document.getElementById('liveStatus').textContent = 'LIVE';
  document.getElementById('lastUpdate').textContent = new Date(s.timestamp).toLocaleTimeString();
  if (s.coinbase_tag) {
    document.getElementById('coinbaseTag').textContent = '⛏ ' + s.coinbase_tag;
  }

  // cards
  document.getElementById('totalKhs').textContent = fmtHash(s.total_khs||0);
  document.getElementById('blocksFound').textContent = s.blocks_found||0;
  document.getElementById('uptime').textContent = s.uptime||'--';
  document.getElementById('cpuTemp').textContent = (s.cpu_temp_c||0).toFixed(1)+'°C';
  document.getElementById('throttleStatus').textContent = s.throttling ? '⚠ THROTTLING' : '';

  const totalMiners = (s.workers||[]).length;
  document.getElementById('totalMiners').textContent = totalMiners;

  // system bars
  const cpu = s.cpu_usage_pct||0, ram = s.ram_used_gb||0, temp = s.cpu_temp_c||0;
  document.getElementById('cpuPct').textContent = cpu.toFixed(1)+'%';
  document.getElementById('ramVal').textContent = ram.toFixed(2)+' / 8.0 GB';
  document.getElementById('tempVal').textContent = temp.toFixed(1)+'°C';
  document.getElementById('cpuBar').style.width = Math.min(cpu,100)+'%';
  document.getElementById('ramBar').style.width = Math.min((ram/8)*100,100)+'%';
  document.getElementById('tempBar').style.width = Math.min((temp/90)*100,100)+'%';

  // coins
  const coins = s.coins||[];
  const onlineCount = coins.filter(c=>c.daemon_online).length;
  document.getElementById('daemonCount').textContent = onlineCount+'/'+coins.length+' daemons online';

  const grid = document.getElementById('coinGrid');
  grid.innerHTML = '';
  coins.forEach(c => {
    const cls = COIN_CLASS[c.symbol]||'ltc';
    const offline = !c.enabled || !c.daemon_online ? 'offline' : '';
    const mergeClass = c.is_merge_aux ? 'merge' : '';
    const daemonDot = c.daemon_online
      ? '<span class="daemon-dot on" title="Daemon online"></span>'
      : '<span class="daemon-dot off" title="Daemon offline"></span>';
    const mergePill = c.is_merge_aux
      ? '<div class="merge-tag">⛓ Merge-mined via '+c.merge_parent+'</div>'
      : '';
    grid.innerHTML += `
      <div class="coin-card ${offline} ${mergeClass}">
        <div class="coin-head">
          <div class="coin-badge ${cls}">${c.symbol}</div>
          <div class="coin-name-wrap">
            <div class="coin-name">${c.symbol}</div>
            <div class="coin-algo">${ALGO[c.symbol]||c.symbol}</div>
          </div>
          ${daemonDot}
        </div>
        ${mergePill}
        <div class="coin-stats-grid">
          <div class="cs"><div class="cs-l">Hashrate</div><div class="cs-v">${fmtHash(c.hashrate_khs)}</div></div>
          <div class="cs"><div class="cs-l">Miners</div><div class="cs-v">${c.miners}</div></div>
          <div class="cs"><div class="cs-l">Blocks Found</div><div class="cs-v">${c.blocks}</div></div>
          <div class="cs"><div class="cs-l">Height</div><div class="cs-v">${c.height||'--'}</div></div>
        </div>
      </div>`;
  });

  // workers table
  const workers = s.workers||[];
  document.getElementById('workerCount').textContent = workers.length + ' connected';
  const tbody = document.getElementById('workersBody');
  if (workers.length === 0) {
    tbody.innerHTML = '<tr><td colspan="5" class="no-workers">No miners connected</td></tr>';
  } else {
    tbody.innerHTML = workers.map(w => {
      const coinCls = 'pill-'+(COIN_CLASS[w.coin]||'ltc');
      const addrShort = w.addr ? w.addr.split(':')[0] : '--';
      return `<tr>
        <td>
          <div class="worker-name">${w.name||'anonymous'}</div>
          <div class="worker-device">${w.device||'Unknown device'}</div>
        </td>
        <td><span class="coin-pill ${coinCls}">${w.coin}</span></td>
        <td><span class="diff-val">${fmtDiff(w.difficulty||0)}</span></td>
        <td><span class="shares-val">${(w.shares||0).toLocaleString()}</span></td>
        <td><span class="addr-val">${addrShort}</span></td>
      </tr>`;
    }).join('');
  }

  // block log
  const blocks = s.block_log||[];
  const logEl = document.getElementById('blockLog');
  if (blocks.length === 0) {
    logEl.innerHTML = '<div class="no-blocks">No blocks found yet this session.<br>Keep hashing — it only takes one. 🎰</div>';
  } else {
    logEl.innerHTML = '<div class="block-log">' + blocks.map(b => `
      <div class="block-entry">
        <div class="block-trophy">🏆</div>
        <div class="block-info">
          <div class="block-coin-height">${b.coin} · Block #${b.height}</div>
          <div class="block-hash">${b.hash}</div>
          <div style="font-size:.62rem;color:var(--text2);margin-top:3px">by ${b.worker}</div>
        </div>
        <div class="block-meta">
          <div class="block-reward">${b.reward}</div>
          <div class="block-time">${b.found_at}</div>
        </div>
      </div>`).join('') + '</div>';
  }

  document.getElementById('footerTime').textContent = new Date().toLocaleString();
}

const evtSrc = new EventSource('/api/events');
evtSrc.onmessage = e => { try { apply(JSON.parse(e.data)); } catch(err){} };
evtSrc.onerror = () => { document.getElementById('liveStatus').textContent = 'RECONNECTING...'; };
fetch('/api/stats').then(r=>r.json()).then(apply).catch(()=>{});
</script>
</body>
</html>`
