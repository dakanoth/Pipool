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
	Coins       []CoinStats `json:"coins"`
}

type CoinStats struct {
	Symbol      string  `json:"symbol"`
	Enabled     bool    `json:"enabled"`
	HashrateKHs float64 `json:"hashrate_khs"`
	Miners      int32   `json:"miners"`
	Blocks      uint64  `json:"blocks"`
	Height      int64   `json:"height"`
	Difficulty  float64 `json:"difficulty"`
	IsMergeAux  bool    `json:"is_merge_aux"`
	MergeParent string  `json:"merge_parent,omitempty"`
}

// StatsFn is a function that returns the current pool stats
type StatsFn func() StatsSnapshot

// Server is the optional web dashboard
type Server struct {
	port        int
	username    string
	password    string
	pushInterval time.Duration
	getStats    StatsFn

	// SSE clients
	mu      sync.Mutex
	clients map[chan string]struct{}
}

// New creates a new dashboard server
func New(port int, username, password string, pushInterval time.Duration, stats StatsFn) *Server {
	return &Server{
		port:         port,
		username:     username,
		password:     password,
		pushInterval: pushInterval,
		getStats:     stats,
		clients:      make(map[chan string]struct{}),
	}
}

// Start begins serving the dashboard
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.authMiddleware(s.handleIndex))
	mux.HandleFunc("/api/stats", s.authMiddleware(s.handleStats))
	mux.HandleFunc("/api/events", s.authMiddleware(s.handleSSE))

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[dashboard] listening on http://0.0.0.0%s (optional — disable in config to save RAM)", addr)

	// Push stats to all SSE clients on interval
	go s.broadcastLoop()

	return http.ListenAndServe(addr, mux)
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.username == "" {
			next(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != s.username || p != s.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="PiPool Dashboard"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(s.getStats())
}

// handleSSE streams stats updates as Server-Sent Events
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
				// client too slow, drop
			}
		}
		s.mu.Unlock()
	}
}

// handleIndex serves the embedded single-page dashboard HTML
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
}

// dashboardHTML is the single-file dashboard UI (embedded at compile time)
const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>PiPool Dashboard</title>
<style>
@import url('https://fonts.googleapis.com/css2?family=Share+Tech+Mono&family=Orbitron:wght@700;900&family=Exo+2:wght@300;400;600&display=swap');
:root{--bg:#050a0f;--panel:#0a1520;--border:#1a3a5c;--accent:#00d4ff;--green:#00ff88;--red:#ff3355;--yellow:#ffd700;--orange:#ff6b00;--text:#c8e6f5;--dim:#4a6a85;}
*{margin:0;padding:0;box-sizing:border-box;}
body{background:var(--bg);color:var(--text);font-family:'Exo 2',sans-serif;min-height:100vh;padding:20px;}
body::before{content:'';position:fixed;inset:0;background-image:linear-gradient(rgba(0,212,255,0.025) 1px,transparent 1px),linear-gradient(90deg,rgba(0,212,255,0.025) 1px,transparent 1px);background-size:40px 40px;pointer-events:none;}
.header{display:flex;align-items:center;justify-content:space-between;margin-bottom:24px;padding:16px 24px;background:var(--panel);border:1px solid var(--border);border-radius:12px;}
h1{font-family:'Orbitron',monospace;color:var(--accent);font-size:1.3rem;letter-spacing:2px;text-shadow:0 0 20px rgba(0,212,255,0.4);}
.sub{font-size:0.7rem;color:var(--dim);letter-spacing:3px;font-family:'Share Tech Mono',monospace;margin-top:3px;}
.dot{width:8px;height:8px;border-radius:50%;background:var(--green);animation:blink 1.5s infinite;display:inline-block;margin-right:6px;}
@keyframes blink{0%,100%{opacity:1;}50%{opacity:.2;}}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(160px,1fr));gap:12px;margin-bottom:20px;}
.card{background:var(--panel);border:1px solid var(--border);border-radius:10px;padding:16px;position:relative;overflow:hidden;}
.card::before{content:'';position:absolute;top:0;left:0;right:0;height:2px;background:linear-gradient(90deg,transparent,var(--accent),transparent);opacity:.4;}
.card-val{font-family:'Orbitron',monospace;font-size:1.2rem;font-weight:700;color:var(--text);margin-bottom:4px;}
.card-label{font-size:0.65rem;color:var(--dim);text-transform:uppercase;letter-spacing:1px;font-family:'Share Tech Mono',monospace;}
.bar-wrap{margin-bottom:12px;}
.bar-label{display:flex;justify-content:space-between;font-size:0.72rem;margin-bottom:5px;font-family:'Share Tech Mono',monospace;}
.bar-name{color:var(--dim)}.bar-val{color:var(--text)}
.bar-track{height:6px;background:rgba(255,255,255,.05);border-radius:3px;overflow:hidden;}
.bar-fill{height:100%;border-radius:3px;transition:width 1s ease;}
.bar-cpu{background:linear-gradient(90deg,#0080ff,var(--accent));}
.bar-ram{background:linear-gradient(90deg,#8833ff,#dd66ff);}
.bar-temp{background:linear-gradient(90deg,var(--green),var(--yellow));}
.coins{display:grid;grid-template-columns:repeat(auto-fill,minmax(260px,1fr));gap:14px;}
.coin{background:var(--panel);border:1px solid var(--border);border-radius:10px;padding:16px;}
.coin-header{display:flex;align-items:center;gap:10px;margin-bottom:12px;}
.coin-icon{width:36px;height:36px;border-radius:50%;display:flex;align-items:center;justify-content:center;font-size:.7rem;font-weight:900;font-family:'Orbitron',monospace;flex-shrink:0;}
.ltc{background:radial-gradient(circle,#d0cece,#888);color:#111;}
.doge{background:radial-gradient(circle,#e8c84a,#b8940a);color:#111;}
.btc{background:radial-gradient(circle,#ffaa44,#f7931a);color:#111;}
.bch{background:radial-gradient(circle,#a8dd6a,#6aa830);color:#111;}
.pep{background:radial-gradient(circle,#66cc66,#339933);color:#fff;}
.coin-name{font-weight:700;}
.coin-algo{font-size:.68rem;color:var(--dim);font-family:'Share Tech Mono',monospace;}
.merge-pill{display:inline-block;font-size:.6rem;padding:2px 8px;border-radius:10px;font-family:'Share Tech Mono',monospace;background:rgba(0,212,255,.1);border:1px solid rgba(0,212,255,.3);color:var(--accent);margin-top:8px;}
.coin-stats{display:grid;grid-template-columns:1fr 1fr;gap:8px;font-size:.78rem;font-family:'Share Tech Mono',monospace;}
.cs-label{color:var(--dim);font-size:.62rem;text-transform:uppercase;letter-spacing:1px;}
.cs-val{color:var(--text);font-weight:600;}
.disabled{opacity:.4;}
.panel{background:var(--panel);border:1px solid var(--border);border-radius:10px;padding:20px;margin-bottom:16px;}
.pt{font-family:'Orbitron',monospace;font-size:.7rem;letter-spacing:2px;text-transform:uppercase;color:var(--dim);margin-bottom:14px;}
footer{text-align:center;color:var(--dim);font-size:.7rem;font-family:'Share Tech Mono',monospace;margin-top:24px;}
@media(max-width:600px){.grid{grid-template-columns:1fr 1fr;}.coins{grid-template-columns:1fr;}}
</style>
</head>
<body>
<div class="header">
  <div>
    <h1>⛏️ PIPOOL</h1>
    <div class="sub">RASPBERRY PI 5 · SOLO MINING DASHBOARD</div>
  </div>
  <div style="text-align:right">
    <div style="font-size:.8rem;color:var(--green);font-family:'Share Tech Mono',monospace"><span class="dot"></span><span id="status">CONNECTING...</span></div>
    <div style="font-size:.7rem;color:var(--dim);margin-top:4px;font-family:'Share Tech Mono',monospace" id="lastUpdate">--</div>
  </div>
</div>

<!-- Summary cards -->
<div class="grid" id="summaryCards">
  <div class="card"><div class="card-val" id="totalKhs">--</div><div class="card-label">Total Hashrate</div></div>
  <div class="card"><div class="card-val" id="blocksFound">--</div><div class="card-label">Blocks Found</div></div>
  <div class="card"><div class="card-val" id="uptime">--</div><div class="card-label">Uptime</div></div>
  <div class="card"><div class="card-val" id="cpuTemp" style="color:var(--yellow)">--</div><div class="card-label">CPU Temp</div></div>
</div>

<!-- System resources -->
<div class="panel">
  <div class="pt">🖥️ System Resources</div>
  <div class="bar-wrap"><div class="bar-label"><span class="bar-name">CPU</span><span class="bar-val" id="cpuPct">--</span></div><div class="bar-track"><div class="bar-fill bar-cpu" id="cpuBar" style="width:0"></div></div></div>
  <div class="bar-wrap"><div class="bar-label"><span class="bar-name">RAM</span><span class="bar-val" id="ramVal">--</span></div><div class="bar-track"><div class="bar-fill bar-ram" id="ramBar" style="width:0"></div></div></div>
  <div class="bar-wrap"><div class="bar-label"><span class="bar-name">TEMP</span><span class="bar-val" id="tempVal">--</span></div><div class="bar-track"><div class="bar-fill bar-temp" id="tempBar" style="width:0"></div></div></div>
</div>

<!-- Coin cards -->
<div class="pt" style="padding:0 4px;margin-bottom:12px;">🪙 Active Coins</div>
<div class="coins" id="coinCards"></div>

<footer>PiPool · Raspberry Pi 5 Solo Mining Pool · <span id="footerTime"></span></footer>

<script>
const ALGO = {LTC:'Scrypt',DOGE:'Scrypt (AuxPoW)',BTC:'SHA-256d',BCH:'SHA-256d (AuxPoW)',PEP:'Scrypt-N'};
const COIN_CLASS = {LTC:'ltc',DOGE:'doge',BTC:'btc',BCH:'bch',PEP:'pep'};

function fmtHash(khs){
  if(khs>=1e6)return(khs/1e6).toFixed(2)+' GH/s';
  if(khs>=1e3)return(khs/1e3).toFixed(2)+' MH/s';
  return khs.toFixed(2)+' KH/s';
}

function applyStats(s){
  document.getElementById('status').textContent='LIVE';
  document.getElementById('lastUpdate').textContent='Updated: '+new Date(s.timestamp).toLocaleTimeString();
  document.getElementById('totalKhs').textContent=fmtHash(s.total_khs||0);
  document.getElementById('blocksFound').textContent=s.blocks_found||0;
  document.getElementById('uptime').textContent=s.uptime||'--';
  document.getElementById('cpuTemp').textContent=(s.cpu_temp_c||0).toFixed(1)+'°C';

  const cpu=s.cpu_usage_pct||0;
  const ram=(s.ram_used_gb||0);
  const temp=s.cpu_temp_c||0;
  document.getElementById('cpuPct').textContent=cpu.toFixed(1)+'%';
  document.getElementById('ramVal').textContent=ram.toFixed(2)+' / 8.0 GB';
  document.getElementById('tempVal').textContent=temp.toFixed(1)+'°C';
  document.getElementById('cpuBar').style.width=Math.min(cpu,100)+'%';
  document.getElementById('ramBar').style.width=Math.min((ram/8)*100,100)+'%';
  document.getElementById('tempBar').style.width=Math.min((temp/90)*100,100)+'%';

  const container=document.getElementById('coinCards');
  container.innerHTML='';
  (s.coins||[]).forEach(c=>{
    const cl=COIN_CLASS[c.symbol]||'ltc';
    const disabled=!c.enabled?'disabled':'';
    const merge=c.is_merge_aux?'<span class="merge-pill">⛓ Merge → '+c.merge_parent+'</span>':'';
    container.innerHTML+=`<div class="coin ${disabled}">
      <div class="coin-header">
        <div class="coin-icon ${cl}">${c.symbol}</div>
        <div><div class="coin-name">${c.symbol}</div><div class="coin-algo">${ALGO[c.symbol]||c.symbol}</div></div>
      </div>
      ${merge}
      <div class="coin-stats" style="margin-top:10px">
        <div><div class="cs-label">Hashrate</div><div class="cs-val">${fmtHash(c.hashrate_khs||0)}</div></div>
        <div><div class="cs-label">Miners</div><div class="cs-val">${c.miners}</div></div>
        <div><div class="cs-label">Blocks</div><div class="cs-val">${c.blocks}</div></div>
        <div><div class="cs-label">Height</div><div class="cs-val">${c.height||'--'}</div></div>
      </div>
    </div>`;
  });

  document.getElementById('footerTime').textContent=new Date().toLocaleString();
}

// Connect to SSE for live updates
const evtSrc=new EventSource('/api/events');
evtSrc.onmessage=e=>{try{applyStats(JSON.parse(e.data));}catch(err){}};
evtSrc.onerror=()=>{document.getElementById('status').textContent='RECONNECTING...';};

// Also do an immediate fetch
fetch('/api/stats').then(r=>r.json()).then(applyStats).catch(()=>{});
</script>
</body>
</html>`
