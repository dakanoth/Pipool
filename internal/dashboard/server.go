package dashboard

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ShareSample is a single submitted share difficulty for the telemetry chart
type ShareSample struct {
	Coin       string  `json:"coin"`
	Difficulty float64 `json:"diff"`
	TimeMS     int64   `json:"t"`
	Accepted   bool    `json:"ok"`
}

// HashrateSample is a periodic hashrate snapshot for the area chart
type HashrateSample struct {
	Coin   string  `json:"coin"`
	KHs    float64 `json:"khs"`
	TimeMS int64   `json:"t"`
}

// StatsSnapshot is the data pushed to dashboard clients every N seconds
type StatsSnapshot struct {
	Timestamp    time.Time      `json:"timestamp"`
	Uptime       string         `json:"uptime"`
	CPUTemp      float64        `json:"cpu_temp_c"`
	CPUUsage     float64        `json:"cpu_usage_pct"`
	RAMUsedGB    float64        `json:"ram_used_gb"`
	Throttling   bool           `json:"throttling"`
	TotalKHs     float64        `json:"total_khs"`
	Sha256KHs    float64        `json:"sha256_khs"`
	ScryptKHs    float64        `json:"scrypt_khs"`
	BlocksFound  uint64         `json:"blocks_found"`
	CoinbaseTag  string         `json:"coinbase_tag"`
	Coins        []CoinStats    `json:"coins"`
	Workers      []WorkerStat   `json:"workers"`
	BlockLog     []BlockEvent   `json:"block_log"`
	Disks        []DiskStat     `json:"disks"`
	Notifs       NotifSettings  `json:"notifs"`
	ShareHistory     []ShareSample    `json:"share_history"`
	HashrateHistory  []HashrateSample `json:"hashrate_history"`
	ChainDiags   []ChainDiag    `json:"chain_diags"`
	PoolName     string             `json:"pool_name"`
	Endpoints    []StratumEndpoint  `json:"endpoints"`
	Groups       []GroupStat        `json:"groups"`
	TotalCostPerDayUSD   float64    `json:"total_cost_per_day_usd"`
	TotalProfitPerDayUSD float64    `json:"total_profit_per_day_usd"`
	KwhRateUSD           float64    `json:"kwh_rate_usd"`
}

// StratumEndpoint holds connection info for a single stratum port
type StratumEndpoint struct {
	Symbol    string `json:"symbol"`    // e.g. "LTC"
	MergeDesc string `json:"merge_desc"` // e.g. "LTC+DOGE" when merge mining
	Algorithm string `json:"algorithm"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
}

// ChainDiag holds per-chain stratum health data for the debug panel
type ChainDiag struct {
	Symbol         string  `json:"symbol"`
	TotalShares    uint64  `json:"total_shares"`
	ValidShares    uint64  `json:"valid_shares"`
	StaleShares    uint64  `json:"stale_shares"`
	RejectedShares uint64  `json:"rejected_shares"`
	StalePct       float64 `json:"stale_pct"`
	RejectPct      float64 `json:"reject_pct"`
	CurrentJobID   string  `json:"current_job_id"`
	CurrentJobAge  int64   `json:"current_job_age_s"`
	WorkerCount    int     `json:"worker_count"`
	HasJob         bool    `json:"has_job"`
	// Issues is a list of human-readable problem strings detected this tick
	Issues         []string `json:"issues"`
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
	Difficulty       float64 `json:"difficulty"`
	PriceUSD         float64 `json:"price_usd"`          // 0 = unknown
	BlockReward      float64 `json:"block_reward"`        // in whole coins
	EarningsPerDayUSD float64 `json:"earnings_day_usd"`   // estimated daily earnings at current hashrate
	CostPerDayUSD     float64 `json:"cost_per_day_usd"`   // electrical cost of online miners on this coin
	IsMergeAux       bool    `json:"is_merge_aux"`
	MergeParent      string  `json:"merge_parent,omitempty"`
	SessionEffortPct float64 `json:"session_effort_pct"` // validWork/networkDiff*100; 0=unknown
	ExpectedBlockSec float64 `json:"expected_block_sec"` // estimated seconds to find a block at current hashrate; 0=unknown
	BalanceConfirmed float64 `json:"balance_confirmed"`  // spendable balance
	BalanceImmature  float64 `json:"balance_immature"`   // maturing coinbase
	DiffHistogram    []HistoBucket `json:"diff_histogram"` // share difficulty distribution
	DiffAdjust       *DiffAdjustment `json:"diff_adjust,omitempty"` // difficulty retarget info (BTC/LTC only)
}

// DiffAdjustment holds difficulty retarget info for coins with fixed epochs (BTC, LTC)
type DiffAdjustment struct {
	EpochSize       int     `json:"epoch_size"`        // 2016
	BlocksInEpoch   int64   `json:"blocks_in_epoch"`   // how many blocks done so far
	BlocksRemaining int64   `json:"blocks_remaining"`  // blocks until next retarget
	ProjectedPct    float64 `json:"projected_pct"`     // e.g. +5.3 means 5.3% harder
	EstimatedDate   string  `json:"estimated_date"`    // human-readable ETA
	TargetBlockSec  int     `json:"target_block_sec"`  // 600 BTC, 150 LTC
}

// GroupStat holds aggregate stats for a named worker group
type GroupStat struct {
	Name            string  `json:"name"`
	WorkerCount     int     `json:"worker_count"`
	OnlineCount     int     `json:"online_count"`
	HashrateKHs     float64 `json:"hashrate_khs"`
	SharesAccepted  uint64  `json:"shares_accepted"`
	CostPerDayUSD   float64 `json:"cost_per_day_usd"`
	ProfitPerDayUSD float64 `json:"profit_per_day_usd"`
}

type WorkerStat struct {
	Name            string  `json:"name"`
	Coin            string  `json:"coin"`
	Device          string  `json:"device"`
	Difficulty      float64 `json:"difficulty"`
	HashrateKHs     float64 `json:"hashrate_khs"`      // 5-minute rolling estimate
	SharesAccepted  uint64  `json:"shares_accepted"`
	SharesRejected  uint64  `json:"shares_rejected"`
	SharesStale     uint64  `json:"shares_stale"`
	BestShare       float64 `json:"best_share"`
	RemoteAddr      string  `json:"addr"`
	ConnectedAt     string  `json:"connected_at"`
	LastSeenAt      string  `json:"last_seen_at"`
	Online          bool    `json:"online"`
	ReconnectCount  int     `json:"reconnect_count"`   // 0 = first session
	SessionDuration string  `json:"session_duration"`  // empty if offline
	UserAgent       string  `json:"user_agent"`        // raw miner user-agent string
	WattsEstimate   float64 `json:"watts_estimate"`
	CostPerDayUSD   float64 `json:"cost_per_day_usd"`
	ProfitPerDayUSD float64 `json:"profit_per_day_usd"`
	ASICTempC       float64 `json:"asic_temp_c"`
	ASICFanRPM      int     `json:"asic_fan_rpm"`
	ASICFanPct      int     `json:"asic_fan_pct"`
	ASICPowerW      float64 `json:"asic_power_w"`
	ASICHashrateKHs float64 `json:"asic_hashrate_khs"`
	ASICError       string  `json:"asic_error,omitempty"`
}

// DiffEvent mirrors stratum.DiffEvent for JSON output (avoid import cycle by redefining).
type DiffEvent struct {
	Diff float64 `json:"diff"`
	AtMS int64   `json:"at_ms"`
}

// WorkerDetail is the full data set returned by /api/worker/{coin}/{name} for the modal.
type WorkerDetail struct {
	WorkerStat
	DiffHistory    []DiffEvent `json:"diff_history"`
	ShareSparkline []int       `json:"share_sparkline"`
}

type BlockEvent struct {
	Coin        string  `json:"coin"`
	Height      int64   `json:"height"`
	Hash        string  `json:"hash"`
	Reward      string  `json:"reward"`
	Worker      string  `json:"worker"`
	FoundAt     string  `json:"found_at"`
	FoundAtMS   int64   `json:"found_at_ms"` // epoch ms for chart positioning
	Luck        float64 `json:"luck"`        // luck %; -1 = N/A (aux chain)
	ExplorerURL string  `json:"explorer_url,omitempty"` // base URL + hash link
	Confirmations  int64 `json:"confirmations"`
	IsOrphaned     bool  `json:"is_orphaned"`
	MaturityTarget int64 `json:"maturity_target"`
}

// HistoBucket is one bin of the share difficulty histogram
type HistoBucket struct {
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Count int     `json:"count"`
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

// ConfigEditorData holds the pool configuration fields editable from the dashboard.
type ConfigEditorData struct {
	TempLimitC      int                `json:"temp_limit_c"`
	WorkerTimeoutS  int                `json:"worker_timeout_s"`
	DiscordWebhook  string             `json:"discord_webhook"`
	Wallets         map[string]string  `json:"wallets"`           // symbol → address
	VardiffMin      map[string]float64 `json:"vardiff_min"`        // symbol → min_diff
	VardiffMax      map[string]float64 `json:"vardiff_max"`        // symbol → max_diff
	AutoKickPct     int                `json:"auto_kick_pct"`      // 0 = disabled
	AutoKickMin     int                `json:"auto_kick_min_shares"`
	WorkerFixedDiff map[string]float64 `json:"worker_fixed_diff"`  // worker → diff (0 = remove)
	KwhRateUSD      float64            `json:"kwh_rate_usd"`

	// Per-coin node settings
	NodeHosts  map[string]string `json:"node_hosts"`  // coin → primary node host
	NodePorts  map[string]int    `json:"node_ports"`  // coin → primary node port
	NodeUsers  map[string]string `json:"node_users"`  // coin → rpc user
	NodePasswords map[string]string `json:"node_passwords"` // coin → rpc password

	// Per-coin backup node
	BackupEnabled   map[string]bool   `json:"backup_enabled"`
	BackupHosts     map[string]string `json:"backup_hosts"`
	BackupPorts     map[string]int    `json:"backup_ports"`
	BackupUsers     map[string]string `json:"backup_users"`
	BackupPasswords map[string]string `json:"backup_passwords"`

	// Per-coin upstream stratum fallback
	UpstreamEnabled map[string]bool   `json:"upstream_enabled"`
	UpstreamHosts   map[string]string `json:"upstream_hosts"`
	UpstreamPorts   map[string]int    `json:"upstream_ports"`
	UpstreamUsers   map[string]string `json:"upstream_users"`

	// Quai Network node
	QuaiNodeHost string `json:"quai_node_host"`
	QuaiNodePort int    `json:"quai_node_port"`
}

// StatsFn returns the current pool snapshot
type StatsFn func() StatsSnapshot

// Server is the optional web dashboard
type Server struct {
	port         int
	pushInterval time.Duration
	getStats        StatsFn
	getNotifs       func() NotifSettings
	setNotifs       func(NotifSettings)
	getCoinbaseTag  func() string
	setCoinbaseTag  func(string)
	getConfig       func() ConfigEditorData
	setConfig       func(ConfigEditorData)
	GetWorkerDetail func(coin, name string) *WorkerDetail
	TriggerRestart  func()

	mu      sync.Mutex
	clients map[chan string]struct{}
}

// New creates a new dashboard server
func New(port int, pushInterval time.Duration, stats StatsFn,
	getNotifs func() NotifSettings, setNotifs func(NotifSettings),
	getCoinbaseTag func() string, setCoinbaseTag func(string),
	getConfig func() ConfigEditorData, setConfig func(ConfigEditorData)) *Server {
	return &Server{
		port:           port,
		pushInterval:   pushInterval,
		getStats:       stats,
		getNotifs:      getNotifs,
		setNotifs:      setNotifs,
		getCoinbaseTag: getCoinbaseTag,
		setCoinbaseTag: setCoinbaseTag,
		getConfig:      getConfig,
		setConfig:      setConfig,
		clients:        make(map[chan string]struct{}),
	}
}

// Start begins serving the dashboard
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/discord", s.handleDiscord)
	mux.HandleFunc("/api/tag", s.handleTag)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/node/test", s.handleNodeTest)
	mux.HandleFunc("/api/restart", s.handleRestartReq)
	mux.HandleFunc("/api/worker/", s.handleWorkerDetail)

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

func (s *Server) handleTag(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]string{"tag": s.getCoinbaseTag()})
	case http.MethodPost:
		var body struct {
			Tag string `json:"tag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Sanitize: strip newlines, max 20 chars, must start/end with /
		tag := body.Tag
		if len(tag) > 20 {
			tag = tag[:20]
		}
		s.setCoinbaseTag(tag)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"tag": tag})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.getConfig())
	case http.MethodPost:
		var cd ConfigEditorData
		if err := json.NewDecoder(r.Body).Decode(&cd); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.setConfig(cd)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleNodeTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	type rpcResult struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Height  int64  `json:"height,omitempty"`
	}
	url := fmt.Sprintf("http://%s:%d/", req.Host, req.Port)
	body := `{"jsonrpc":"1.0","id":"test","method":"getblockcount","params":[]}`
	httpReq, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		json.NewEncoder(w).Encode(rpcResult{OK: false, Message: "bad request: " + err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.User != "" {
		httpReq.SetBasicAuth(req.User, req.Password)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		json.NewEncoder(w).Encode(rpcResult{OK: false, Message: "connection failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	var rpcResp struct {
		Result interface{} `json:"result"`
		Error  interface{} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		json.NewEncoder(w).Encode(rpcResult{OK: false, Message: "invalid response"})
		return
	}
	if rpcResp.Error != nil {
		json.NewEncoder(w).Encode(rpcResult{OK: false, Message: fmt.Sprintf("node error: %v", rpcResp.Error)})
		return
	}
	var height int64
	switch v := rpcResp.Result.(type) {
	case float64:
		height = int64(v)
	}
	json.NewEncoder(w).Encode(rpcResult{OK: true, Message: fmt.Sprintf("Connected — block height %d", height), Height: height})
}

func (s *Server) handleRestartReq(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.TriggerRestart != nil {
		go func() {
			time.Sleep(300 * time.Millisecond)
			s.TriggerRestart()
		}()
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "message": "PiPool restarting… back in a few seconds"})
	} else {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "message": "restart not available"})
	}
}

func (s *Server) handleWorkerDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// path: /api/worker/{coin}/{name}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/worker/"), "/", 2)
	if len(parts) != 2 || s.GetWorkerDetail == nil {
		http.Error(w, "not found", 404)
		return
	}
	detail := s.GetWorkerDetail(parts[0], parts[1])
	if detail == nil {
		http.Error(w, "worker not found", 404)
		return
	}
	json.NewEncoder(w).Encode(detail)
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
/* ── FALLOUT THEME (default) ── */
:root {
  --bg:    #000800; --surf:  #000f00; --surf2: #001500;
  --bdr:   #004400; --bdr2:  #007700;
  --hi:    #00ff41; --hi2:   #00cc33;
  --dim:   #008822; --dim2:  #005514; --off:   #002200;
  --red:   #ff4400; --amber: #ffaa00;
  --scan:  'Share Tech Mono', monospace; --vt: 'VT323', monospace;
  --glow:  0 0 8px rgba(0,255,65,0.6); --glow2: 0 0 20px rgba(0,255,65,0.3);
  --chart-line: #00ff41; --chart-net: #ff4400; --chart-rej: #ff6600;
}

/* ── MARATHON CLASSIC (original 1994 terminals: black + amber, Courier) ── */
[data-theme="marathon-classic"] {
  --bg:    #000000; --surf:  #080800; --surf2: #0d0d00;
  --bdr:   #443300; --bdr2:  #886600;
  --hi:    #ff8c00; --hi2:   #cc7000;
  --dim:   #664400; --dim2:  #442200; --off:   #1a1000;
  --red:   #ff2200; --amber: #ffcc00;
  --scan:  'Courier New', Courier, monospace; --vt: 'Courier New', Courier, monospace;
  --glow:  0 0 6px rgba(255,140,0,0.5); --glow2: 0 0 16px rgba(255,140,0,0.2);
  --chart-line: #ff8c00; --chart-net: #ff2200; --chart-rej: #ff4400;
}

/* ── MARATHON 2025 (Bungie retro-futurism: dark + neon lime + glitch) ── */
[data-theme="marathon-2025"] {
  --bg:    #060609; --surf:  #0c0c12; --surf2: #111118;
  --bdr:   #1a2a1a; --bdr2:  #2aff6a44;
  --hi:    #aaff00; --hi2:   #77dd00;
  --dim:   #446600; --dim2:  #223300; --off:   #0a120a;
  --red:   #ff1155; --amber: #ffdd00;
  --scan:  'Share Tech Mono', monospace; --vt: 'VT323', monospace;
  --glow:  0 0 10px rgba(170,255,0,0.7); --glow2: 0 0 24px rgba(170,255,0,0.25);
  --chart-line: #aaff00; --chart-net: #ff1155; --chart-rej: #ff6600;
}

/* ── SLATE (modern dark — easy on the eyes, blue/cyan accents) ── */
[data-theme="slate"] {
  --bg:    #0f1117; --surf:  #1a1d27; --surf2: #22263a;
  --bdr:   #2a2f45; --bdr2:  #4a80ff;
  --hi:    #e0e6ff; --hi2:   #7ab4ff;
  --dim:   #8892aa; --dim2:  #4a5068; --off:   #0c0e16;
  --red:   #ff5566; --amber: #ffb347;
  --scan:  'Inter', 'Segoe UI', system-ui, sans-serif; --vt: 'Inter', 'Segoe UI', system-ui, sans-serif;
  --glow:  0 0 8px rgba(74,128,255,0.45); --glow2: 0 0 20px rgba(74,128,255,0.2);
  --chart-line: #7ab4ff; --chart-net: #ff5566; --chart-rej: #ffb347;
}
[data-theme="slate"] .logo-text { letter-spacing: 4px; font-size: 1.4rem; }
[data-theme="slate"] .section,
[data-theme="slate"] .coin-card,
[data-theme="slate"] .card { border-radius: 6px; }
[data-theme="slate"] .section-head { border-radius: 6px 6px 0 0; }
[data-theme="slate"] .section-body { border-radius: 0 0 6px 6px; }
* { margin:0; padding:0; box-sizing:border-box; }
html { scrollbar-width:thin; scrollbar-color:var(--bdr2) var(--bg); }

body {
  background:var(--bg); color:var(--hi2);
  font-family:var(--scan); min-height:100vh;
  overflow-x:hidden; transition: background .4s, color .4s;
}
[data-theme="marathon-2025"] .section,
[data-theme="marathon-2025"] .coin-card,
[data-theme="marathon-2025"] .card {
  border-style: solid;
  border-image: linear-gradient(135deg, var(--bdr2), transparent, var(--bdr2)) 1;
}
[data-theme="marathon-2025"] .logo-text {
  letter-spacing: 12px;
  text-shadow: 0 0 20px rgba(170,255,0,0.9), 0 0 60px rgba(170,255,0,0.3);
}
[data-theme="marathon-classic"] .section,
[data-theme="marathon-classic"] .coin-card {
  border-radius: 0;
  border-style: double;
}
[data-theme="marathon-classic"] .logo-sub {
  font-family: 'Courier New', Courier, monospace;
  letter-spacing: 6px;
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

/* ── SHARE HISTOGRAM ── */
.histo { display:flex; align-items:flex-end; gap:2px; height:32px; margin-top:6px; }
.histo-bar { flex:1; background:var(--hi2); opacity:.5; min-width:3px; transition:height .3s; }
.histo-bar:hover { opacity:1; cursor:default; }

/* ── WORKERS ── */
.workers-table { width:100%; border-collapse:collapse; font-size:.72rem; }
.workers-table th {
  font-family:var(--scan); font-size:.54rem; color:var(--dim);
  text-transform:uppercase; letter-spacing:1px; text-align:left;
  padding:4px 8px; border-bottom:1px solid var(--bdr); font-weight:400;
  white-space:nowrap;
}
.workers-table td { padding:4px 8px; border-bottom:1px solid var(--off); font-family:var(--scan); white-space:nowrap; }
.workers-table tr:last-child td { border-bottom:none; }
.workers-table tr:hover td { background:rgba(0,255,65,0.03); }
.workers-table td:first-child { white-space:normal; min-width:120px; max-width:200px; }
.worker-name { color:var(--hi2); }
.worker-device { color:var(--dim); font-size:.60rem; }
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

.profit-pos { color: var(--hi); }
.profit-neg { color: var(--red); }

/* ── EPOCH BAR ── */
.epoch-bar { background:var(--surf2); border:1px solid var(--bdr); height:4px; margin-top:4px; }
.epoch-fill { height:100%; background:var(--hi2); transition:width .5s; }

/* ── GROUPS ── */
.group-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(280px,1fr)); gap:10px; }
.group-card { background:var(--surf2); border:1px solid var(--bdr); padding:14px; }
.group-name { font-family:var(--scan); font-size:.78rem; color:var(--hi); letter-spacing:3px; margin-bottom:8px; }
.group-stat { display:flex; justify-content:space-between; margin-bottom:4px; }
.group-stat-l { font-family:var(--scan); font-size:.58rem; color:var(--dim2); }
.group-stat-v { font-family:var(--scan); font-size:.62rem; color:var(--hi2); }

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

/* ── DIAGNOSTICS ── */
.diag-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(260px,1fr)); gap:10px; }
.diag-card {
  border:1px solid var(--bdr); background:var(--surf2); padding:12px 14px;
  position:relative;
}
.diag-card.has-issues { border-color:#664400; }
.diag-card.has-critical { border-color:var(--red); animation: diagPulse 2s infinite; }
@keyframes diagPulse { 0%,100%{border-color:var(--red)} 50%{border-color:#660000} }
.diag-coin-head {
  display:flex; align-items:center; justify-content:space-between;
  margin-bottom:8px;
}
.diag-coin-sym { font-family:var(--scan); font-size:.78rem; color:var(--hi2); letter-spacing:2px; }
.diag-status-dot {
  width:8px; height:8px; border-radius:50%; background:var(--hi);
  box-shadow:0 0 6px var(--hi);
}
.diag-status-dot.warn { background:var(--amber); box-shadow:0 0 6px var(--amber); }
.diag-status-dot.crit { background:var(--red); box-shadow:0 0 6px var(--red); }
.diag-stat-row {
  display:grid; grid-template-columns:1fr 1fr 1fr; gap:4px; margin-bottom:8px;
}
.diag-stat { text-align:center; }
.diag-stat-val { font-family:var(--vt); font-size:.78rem; color:var(--hi); }
.diag-stat-val.warn { color:var(--amber); }
.diag-stat-val.crit { color:var(--red); }
.diag-stat-lbl { font-family:var(--scan); font-size:.5rem; color:var(--dim2); margin-top:1px; }
.diag-job-row {
  font-family:var(--scan); font-size:.56rem; color:var(--dim2);
  border-top:1px solid var(--bdr); padding-top:6px; margin-top:4px;
  display:flex; justify-content:space-between; gap:4px;
}
.diag-job-id { color:var(--dim); font-size:.54rem; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; max-width:120px; }
.diag-job-age { color:var(--dim2); }
.diag-job-age.warn { color:var(--amber); }
.diag-issues { margin-top:8px; }
.diag-issue {
  font-family:var(--scan); font-size:.56rem; color:var(--amber);
  padding:3px 6px; border-left:2px solid var(--amber); background:rgba(255,140,0,.06);
  margin-bottom:3px;
}
.diag-issue.crit {
  color:var(--red); border-left-color:var(--red); background:rgba(255,50,50,.06);
}
.diag-ok { font-family:var(--scan); font-size:.58rem; color:var(--dim2); text-align:center; padding:4px 0; }

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

/* ── CONFIG EDITOR ── */
.cfg-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(340px,1fr)); gap:14px; }
.cfg-group { background:var(--surf2); border:1px solid var(--bdr); padding:14px; }
.cfg-group-title { font-family:var(--scan); font-size:.6rem; color:var(--dim2); letter-spacing:2px; margin-bottom:10px; }
.cfg-row { display:flex; align-items:center; gap:10px; margin-bottom:8px; }
.cfg-row:last-child { margin-bottom:0; }
.cfg-label { font-family:var(--scan); font-size:.6rem; color:var(--hi2); flex:1; }
.cfg-input {
  background:var(--off); border:1px solid var(--bdr2); color:var(--hi);
  font-family:var(--scan); font-size:.68rem;
  padding:4px 8px; width:160px; text-align:left;
}
.cfg-input:focus { outline:none; border-color:var(--hi); box-shadow: var(--glow); }
.cfg-input.wide { width:100%; box-sizing:border-box; }
.cfg-save-btn {
  display:inline-block; margin-top:12px;
  background:none; border:1px solid var(--hi2); color:var(--hi); cursor:pointer;
  font-family:var(--scan); font-size:.68rem; letter-spacing:2px;
  padding:6px 20px; transition: border-color .15s, box-shadow .15s;
}
.cfg-save-btn:hover { border-color:var(--hi); box-shadow: var(--glow); }
.cfg-save-status { font-family:var(--scan); font-size:.58rem; color:var(--dim2); margin-left:12px; }
.cfg-fd-table { width:100%; border-collapse:collapse; margin-top:6px; }
.cfg-fd-table td { padding:3px 0; font-family:var(--scan); font-size:.6rem; vertical-align:middle; }
.cfg-fd-input { background:var(--off); border:1px solid var(--bdr2); color:var(--hi); font-family:var(--scan); font-size:.6rem; padding:2px 6px; width:90px; }
.cfg-fd-add { background:none; border:1px solid var(--bdr2); color:var(--dim); font-family:var(--scan); font-size:.58rem; padding:2px 10px; cursor:pointer; margin-top:6px; }
.cfg-fd-add:hover { border-color:var(--hi2); color:var(--hi2); }
.cfg-fd-del { background:none; border:none; color:var(--red); font-family:var(--vt); font-size:.9rem; cursor:pointer; padding:0 4px; line-height:1; }
.node-cfg-coin { border:1px solid var(--bdr); padding:8px 10px; margin-bottom:8px; background:var(--off); }
.node-cfg-coin-title { font-family:var(--scan); font-size:.65rem; color:var(--hi2); letter-spacing:2px; margin-bottom:6px; }
.node-cfg-row { display:flex; align-items:center; gap:6px; margin-bottom:5px; flex-wrap:wrap; }
.node-cfg-label { font-family:var(--scan); font-size:.56rem; color:var(--dim2); min-width:80px; letter-spacing:1px; }
.node-cfg-host { background:var(--off); border:1px solid var(--bdr2); color:var(--hi); font-family:var(--scan); font-size:.6rem; padding:2px 6px; width:160px; }
.node-cfg-port { background:var(--off); border:1px solid var(--bdr2); color:var(--hi); font-family:var(--scan); font-size:.6rem; padding:2px 6px; width:68px; }
.node-cfg-user { background:var(--off); border:1px solid var(--bdr); color:var(--hi); font-family:var(--scan); font-size:.6rem; padding:2px 6px; width:100px; }
.node-test-btn { background:none; border:1px solid var(--dim2); color:var(--dim); font-family:var(--scan); font-size:.56rem; padding:2px 8px; cursor:pointer; letter-spacing:1px; white-space:nowrap; }
.node-test-btn:hover { border-color:var(--hi2); color:var(--hi2); }
.node-test-ok { color:var(--hi2); font-family:var(--scan); font-size:.56rem; }
.node-test-fail { color:var(--red); font-family:var(--scan); font-size:.56rem; }
.node-enable-toggle { display:flex; align-items:center; gap:4px; cursor:pointer; font-family:var(--scan); font-size:.56rem; color:var(--dim); }
.node-enable-toggle input { accent-color:var(--hi2); cursor:pointer; }

/* ── LAYOUT ── */
.row2 { display:grid; grid-template-columns:1fr 1fr; gap:14px; margin-bottom:16px; }
/* ─── RESPONSIVE / MOBILE ─────────────────────────────────────────────── */
@media(max-width:900px){
  .row2 { grid-template-columns:1fr; }
}
@media(max-width:700px){
  .res-grid { grid-template-columns:1fr; }
}
@media(max-width:640px){
  /* Topbar */
  .topbar { padding:8px 12px; flex-wrap:wrap; gap:6px; }
  .logo-text { font-size:1.6rem; letter-spacing:3px; }
  .topbar-right { flex-wrap:wrap; gap:4px; }

  /* Stat cards — 2 per row on phones */
  .cards { grid-template-columns:1fr 1fr; gap:8px; margin-bottom:12px; }
  .card { padding:10px 12px; }
  .card-val { font-size:1.4rem; letter-spacing:1px; }
  .card-label { font-size:.54rem; }

  /* Coin grid — single column */
  .coin-grid { grid-template-columns:1fr !important; gap:8px; }
  /* Legacy class names */
  .coins-grid { grid-template-columns:1fr !important; }
  .stats-grid  { grid-template-columns:1fr 1fr !important; }

  /* Workers table — horizontal scroll */
  .section-body { overflow-x:auto; -webkit-overflow-scrolling:touch; }
  .workers-table { min-width:820px; }

  /* Section padding */
  .section { margin-bottom:10px; }
  .section-head { padding:8px 12px; }

  /* Chart — shorter on mobile */
  canvas#diffChart { height:160px !important; max-height:160px; }

  /* Modal — full width */
  #worker-modal > div { width:96vw !important; max-width:96vw !important; margin:8px auto; max-height:90vh; overflow-y:auto; }

  /* Block log entries — tighter */
  .block-entry { padding:10px 0; gap:8px; }
  .block-reward { font-size:.7rem; }

  /* Page padding */
  .wrap { padding:8px 4px; }
}
@media(max-width:400px){
  .cards { grid-template-columns:1fr; }
  .card-val { font-size:1.2rem; }
}

footer {
  text-align:center; padding:16px;
  font-family:var(--scan); font-size:.58rem; color:var(--dim2);
  border-top:1px solid var(--bdr); margin-top:8px; letter-spacing:2px;
}
.connect-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(300px,1fr)); gap:14px; }
.connect-card { background:var(--surf2); border:1px solid var(--bdr); padding:16px; }
.connect-card-head { display:flex; align-items:center; gap:10px; margin-bottom:14px; }
.connect-sym { font-family:var(--scan); font-size:.9rem; color:var(--hi); letter-spacing:3px; }
.connect-algo { font-family:var(--scan); font-size:.52rem; color:var(--dim); background:var(--off); border:1px solid var(--bdr); padding:2px 6px; letter-spacing:1px; }
.connect-field { margin-bottom:10px; }
.connect-label { font-family:var(--scan); font-size:.5rem; color:var(--dim2); letter-spacing:2px; margin-bottom:3px; }
.connect-val { display:flex; align-items:center; gap:6px; background:var(--off); border:1px solid var(--bdr); padding:6px 10px; }
.connect-val-text { font-family:var(--vt); font-size:.72rem; color:var(--hi2); flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.connect-copy { background:none; border:1px solid var(--bdr2); color:var(--dim); font-family:var(--scan); font-size:.48rem; padding:2px 6px; cursor:pointer; letter-spacing:1px; white-space:nowrap; flex-shrink:0; }
.connect-copy:hover { color:var(--hi); border-color:var(--hi); }
.connect-copy.copied { color:var(--hi); border-color:var(--hi); }
.connect-hint { font-family:var(--scan); font-size:.52rem; color:var(--dim2); margin-top:10px; padding-top:8px; border-top:1px solid var(--bdr); line-height:1.6; }
.connect-hint span { color:var(--hi2); }
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
    <div style="position:relative">
      <div class="tag-badge" id="coinbaseTag" onclick="openTagEditor()" title="Click to edit" style="cursor:pointer">&#9889; /PiPool/</div>
      <div id="tagEditor" style="display:none;position:absolute;right:0;top:110%;z-index:100;background:var(--surf);border:1px solid var(--bdr2);padding:10px;min-width:240px;box-shadow:0 4px 20px rgba(0,255,65,0.15)">
        <div style="font-family:var(--scan);font-size:.56rem;color:var(--dim);letter-spacing:2px;margin-bottom:6px">COINBASE TAG (MAX 20 CHARS)</div>
        <input id="tagInput" class="notif-input" style="width:100%;text-align:left;padding:4px 8px;font-size:.72rem" maxlength="20" placeholder="/PiPool/">
        <div style="display:flex;gap:6px;margin-top:8px">
          <button onclick="saveTag()" style="flex:1;background:var(--off);border:1px solid var(--bdr2);color:var(--hi);font-family:var(--scan);font-size:.64rem;padding:4px;cursor:pointer;letter-spacing:1px">SAVE</button>
          <button onclick="closeTagEditor()" style="flex:1;background:var(--off);border:1px solid var(--bdr);color:var(--dim);font-family:var(--scan);font-size:.64rem;padding:4px;cursor:pointer;letter-spacing:1px">CANCEL</button>
        </div>
        <div id="tagSaveStatus" style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);margin-top:6px;text-align:center"></div>
      </div>
    </div>
    <button id="themeBtn" onclick="cycleTheme()" style="
      background:var(--off);border:1px solid var(--bdr2);color:var(--hi2);
      font-family:var(--scan);font-size:.6rem;padding:5px 12px;cursor:pointer;
      letter-spacing:2px;text-transform:uppercase;transition:border-color .2s
    " title="Switch theme">FALLOUT</button>
    <div>
      <div class="live-badge"><span class="pulse"></span><span id="liveStatus">CONNECTING</span></div>
      <div style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);margin-top:4px;text-align:right" id="lastUpdate">--</div>
    </div>
  </div>
</div>

<div class="cards">
  <div class="card">
    <div class="card-val" id="sha256Khs">--</div>
    <div class="card-label">SHA-256D Hashrate</div>
    <div class="card-sub" style="font-family:var(--scan);font-size:.56rem;color:var(--dim2)">BTC · BCH</div>
  </div>
  <div class="card">
    <div class="card-val" id="scryptKhs">--</div>
    <div class="card-label">SCRYPT Hashrate</div>
    <div class="card-sub" style="font-family:var(--scan);font-size:.56rem;color:var(--dim2)">LTC · DOGE · DGB</div>
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
  <div class="card">
    <div class="card-val" id="totalWattsCard">--</div>
    <div class="card-label">Total Power</div>
    <div class="card-sub" id="totalCostCard" style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);margin-top:2px"></div>
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
    <span class="section-title">TELEMETRY</span>
    <div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap">
      <div id="coinFilterBtns" style="display:flex;gap:4px;flex-wrap:wrap"></div>
      <div style="display:flex;gap:4px">
        <button onclick="setChartMode('hashrate')" id="btnHashrate" style="background:var(--off);border:1px solid var(--bdr);color:var(--dim);font-family:var(--scan);font-size:.58rem;padding:3px 8px;cursor:pointer;letter-spacing:1px">HASHRATE</button>
        <button onclick="setChartMode('diff')" id="btnDiff" style="background:var(--off);border:1px solid var(--bdr2);color:var(--hi2);font-family:var(--scan);font-size:.58rem;padding:3px 8px;cursor:pointer;letter-spacing:1px">SHARE DIFF</button>
      </div>
    </div>
  </div>
  <div class="section-body" style="padding-bottom:8px">
    <!-- Stats row -->
    <div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:10px;margin-bottom:14px">
      <div style="background:var(--surf2);border:1px solid var(--bdr);padding:10px 14px">
        <div style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);letter-spacing:2px;margin-bottom:4px">BLOCK ODDS (NOW)</div>
        <div id="blockOddsNow" style="font-family:var(--vt);font-size:1.1rem;color:var(--hi);letter-spacing:1px">--</div>
      </div>
      <div style="background:var(--surf2);border:1px solid var(--bdr);padding:10px 14px">
        <div style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);letter-spacing:2px;margin-bottom:4px">EXPECTED TIME</div>
        <div id="blockExpected" style="font-family:var(--vt);font-size:1.1rem;color:var(--hi);letter-spacing:1px">--</div>
      </div>
      <div style="background:var(--surf2);border:1px solid var(--bdr);padding:10px 14px">
        <div style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);letter-spacing:2px;margin-bottom:4px">BEST SHARE (SESSION)</div>
        <div id="bestShareNow" style="font-family:var(--vt);font-size:1.1rem;color:var(--hi2);letter-spacing:1px">--</div>
      </div>
      <div style="background:var(--surf2);border:1px solid var(--bdr);padding:10px 14px">
        <div style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);letter-spacing:2px;margin-bottom:4px">NETWORK DIFFICULTY</div>
        <div id="netDiffDisplay" style="font-family:var(--vt);font-size:1.1rem;color:var(--dim);letter-spacing:1px">--</div>
      </div>
    </div>
    <!-- Aux chain odds — dynamically populated when merge children are active -->
    <div id="auxOddsWrap" style="margin-bottom:10px"></div>
    <!-- Legend -->
    <div style="display:flex;gap:16px;align-items:center;margin-bottom:8px;flex-wrap:wrap" id="chartLegend">
      <div style="display:flex;align-items:center;gap:6px"><div style="width:8px;height:8px;border-radius:50%;background:var(--chart-line)"></div><span style="font-family:var(--scan);font-size:.56rem;color:var(--dim)">ACCEPTED</span></div>
      <div style="display:flex;align-items:center;gap:6px"><div style="width:8px;height:8px;border-radius:50%;background:var(--chart-rej)"></div><span style="font-family:var(--scan);font-size:.56rem;color:var(--dim)">REJECTED</span></div>
      <div style="display:flex;align-items:center;gap:6px"><div style="width:8px;height:8px;border-radius:50%;background:#ffdd00"></div><span style="font-family:var(--scan);font-size:.56rem;color:var(--dim)">AUX BLOCK</span></div>
      <div style="display:flex;align-items:center;gap:6px"><div style="width:8px;height:8px;border-radius:50%;background:#ffffff"></div><span style="font-family:var(--scan);font-size:.56rem;color:var(--dim)">PARENT BLOCK</span></div>
      <div style="display:flex;align-items:center;gap:6px"><div style="width:20px;height:2px;background:var(--chart-net);border-top:2px dashed var(--chart-net)"></div><span style="font-family:var(--scan);font-size:.56rem;color:var(--dim)">PARENT NET DIFF</span></div>
      <div style="display:flex;align-items:center;gap:6px"><div style="width:20px;height:2px;border-top:2px dashed #ffdd00"></div><span style="font-family:var(--scan);font-size:.56rem;color:var(--dim)">AUX NET DIFF</span></div>
    </div>
    <canvas id="diffChart" style="width:100%;height:200px;display:block"></canvas>
    <div id="noShareData" style="font-family:var(--scan);font-size:.7rem;color:var(--dim2);text-align:center;padding:50px 0;display:none">
      NO DATA YET — CONNECT A MINER TO BEGIN
    </div>
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

<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">WORKERS</span>
    <span id="workerCount" style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)">0</span>
    <span style="margin-left:auto;display:flex;gap:6px">
      <button id="tabBtnActive" onclick="setWorkerTab('active')" style="background:var(--off);border:1px solid var(--bdr2);color:var(--hi2);font-family:var(--scan);font-size:.58rem;padding:3px 8px;cursor:pointer;letter-spacing:1px">ACTIVE</button>
      <button id="tabBtnKnown" onclick="setWorkerTab('known')" style="background:var(--off);border:1px solid var(--bdr);color:var(--dim);font-family:var(--scan);font-size:.58rem;padding:3px 8px;cursor:pointer;letter-spacing:1px">KNOWN WORKERS</button>
    </span>
  </div>
  <div class="section-body" style="padding:0;overflow-x:auto">
    <table class="workers-table" id="workersTable" style="min-width:900px">
      <thead><tr>
        <th>Worker</th><th>Coin</th><th>Diff</th><th>Hashrate</th><th>Accepted</th><th>Stale%</th><th>Inv%</th><th>Best</th><th>Sess</th><th>Watts</th><th>$/Day</th><th>Status</th>
      </tr></thead>
      <tbody id="workersBody">
        <tr><td colspan="12" class="no-workers">No miners connected</td></tr>
      </tbody>
    </table>
  </div>
</div>

<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">BLOCK LOG</span>
    <span style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)">This session</span>
  </div>
  <div class="section-body" id="blockLog">
    <div class="no-blocks">No blocks found this session.<br>Keep hashing.</div>
  </div>
</div>

<div class="section" id="sectionGroups" style="display:none;margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">GROUPS</span>
    <span id="groupCount" style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)"></span>
  </div>
  <div class="section-body">
    <div class="group-grid" id="groupGrid"></div>
  </div>
</div>

<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">CONNECT</span>
    <span style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)">STRATUM ENDPOINTS</span>
  </div>
  <div class="section-body">
    <div class="connect-grid" id="connectGrid">
      <div style="font-family:var(--scan);font-size:.6rem;color:var(--dim2)">Waiting for data...</div>
    </div>
  </div>
</div>

<div class="section" style="margin-bottom:16px">
  <div class="section-head">
    <span class="section-title">DIAGNOSTICS</span>
    <span id="diagStatus" style="font-family:var(--scan);font-size:.58rem;color:var(--dim2)">PER-CHAIN STRATUM HEALTH</span>
  </div>
  <div class="section-body">
    <div class="diag-grid" id="diagGrid">
      <div style="font-family:var(--scan);font-size:.6rem;color:var(--dim2)">Waiting for data...</div>
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

<div class="section" id="sectionConfig">
  <div class="section-head">
    <span class="section-title">POOL SETTINGS</span>
    <span id="cfgSaveStatus" class="cfg-save-status">CONFIG EDITOR</span>
  </div>
  <div class="section-body">
    <div class="cfg-grid" id="cfgGrid">

      <div class="cfg-group">
        <div class="cfg-group-title">WALLETS</div>
        <div id="cfgWallets"></div>
      </div>

      <div class="cfg-group">
        <div class="cfg-group-title">VARDIFF</div>
        <div id="cfgVardiff"></div>
      </div>

      <div class="cfg-group">
        <div class="cfg-group-title">SYSTEM</div>
        <div class="cfg-row">
          <div class="cfg-label">TEMP LIMIT (°C)</div>
          <input class="cfg-input" id="cfgTempLimit" type="number" min="50" max="90">
        </div>
        <div class="cfg-row">
          <div class="cfg-label">WORKER TIMEOUT (s)</div>
          <input class="cfg-input" id="cfgWorkerTimeout" type="number" min="30" max="600">
        </div>
        <div class="cfg-row">
          <div class="cfg-label">ELECTRICITY RATE ($/kWh)</div>
          <input class="cfg-input" id="cfgKwhRate" type="number" min="0" step="0.001" placeholder="0.12">
        </div>
        <div class="cfg-row">
          <div class="cfg-label">DISPLAY CURRENCY</div>
          <input class="cfg-input" id="cfgCurrencySymbol" style="width:70px" placeholder="USD" maxlength="8">
          <span style="color:var(--dim2);font-family:var(--scan);font-size:.58rem;white-space:nowrap">1 USD =</span>
          <input class="cfg-input" id="cfgCurrencyRate" type="number" style="width:90px" step="0.0001" placeholder="1.0">
        </div>
      </div>

      <div class="cfg-group">
        <div class="cfg-group-title">AUTO-KICK</div>
        <div class="cfg-row">
          <div class="cfg-label">REJECT THRESHOLD (%)</div>
          <input class="cfg-input" id="cfgAutoKickPct" type="number" min="0" max="100" placeholder="0=off">
        </div>
        <div class="cfg-row">
          <div class="cfg-label">MIN SHARES BEFORE KICK</div>
          <input class="cfg-input" id="cfgAutoKickMin" type="number" min="1" max="10000">
        </div>
      </div>

      <div class="cfg-group" style="grid-column:1/-1">
        <div class="cfg-group-title">DISCORD WEBHOOK</div>
        <div class="cfg-row">
          <input class="cfg-input wide" id="cfgWebhook" type="text" placeholder="https://discord.com/api/webhooks/...">
        </div>
      </div>

      <div class="cfg-group" style="grid-column:1/-1">
        <div class="cfg-group-title">FIXED WORKER DIFFICULTY</div>
        <table class="cfg-fd-table" id="cfgFdTable">
          <thead><tr>
            <td style="color:var(--dim2);font-size:.55rem;letter-spacing:1px">WORKER NAME</td>
            <td style="color:var(--dim2);font-size:.55rem;letter-spacing:1px">DIFFICULTY</td>
            <td></td>
          </tr></thead>
          <tbody id="cfgFdRows"></tbody>
        </table>
        <button class="cfg-fd-add" onclick="cfgFdAddRow('','')">+ ADD WORKER</button>
      </div>

      <div class="cfg-group" style="grid-column:1/-1">
        <div class="cfg-group-title">NODE SETTINGS</div>
        <div style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);margin-bottom:8px">
          Change which node PiPool connects to. After saving, click APPLY &amp; RESTART to reconnect.
        </div>
        <div id="cfgNodes"></div>
      </div>

    </div>
    <div style="margin-top:14px;display:flex;gap:10px;align-items:center;flex-wrap:wrap">
      <button class="cfg-save-btn" onclick="saveCfg()">SAVE SETTINGS</button>
      <button class="cfg-save-btn" id="cfgRestartBtn" onclick="applyAndRestart()" style="background:var(--off);border-color:var(--amber);color:var(--amber);display:none">APPLY &amp; RESTART</button>
      <span id="cfgRestartStatus" style="font-family:var(--scan);font-size:.6rem;color:var(--dim2)"></span>
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

var ALGO = {LTC:'SCRYPT',DOGE:'SCRYPT / AUXPOW',BTC:'SHA-256D',BCH:'SHA-256D / AUXPOW',PEP:'SCRYPT-N',DGB:'SHA-256D',DGBS:'SCRYPT'};

function fmtHash(khs) {
  if (!khs) return '0 KH/S';
  if (khs >= 1e9) return (khs/1e9).toFixed(2)+' TH/S';
  if (khs >= 1e6) return (khs/1e6).toFixed(2)+' GH/S';
  if (khs >= 1e3) return (khs/1e3).toFixed(2)+' MH/S';
  return khs.toFixed(2)+' KH/S';
}

function fmtDiff(d) {
  if (!d) return '0';
  if (d >= 1e9) return (d/1e9).toFixed(2)+'G';
  if (d >= 1e6) return (d/1e6).toFixed(2)+'M';
  if (d >= 1e3) return (d/1e3).toFixed(2)+'K';
  return d.toFixed(3);
}

function fmtDuration(sec) {
  if (!sec || sec <= 0) return '--';
  if (sec < 60) return Math.round(sec)+'s';
  if (sec < 3600) return Math.floor(sec/60)+'m '+Math.round(sec%60)+'s';
  if (sec < 86400) return Math.floor(sec/3600)+'h '+Math.floor((sec%3600)/60)+'m';
  return (sec/86400).toFixed(1)+' days';
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

var lastState = {};

function apply(s) {
  lastState = s;
  document.getElementById('liveStatus').textContent = 'ONLINE';
  document.getElementById('lastUpdate').textContent = new Date(s.timestamp).toLocaleTimeString();
  if (s.coinbase_tag) {
    document.getElementById('coinbaseTag').innerHTML = '&#9889; ' + s.coinbase_tag;
    var inp = document.getElementById('tagInput');
    if (inp && document.activeElement !== inp) inp.value = s.coinbase_tag;
  }

  document.getElementById('sha256Khs').textContent  = fmtHash(s.sha256_khs||0);
  document.getElementById('scryptKhs').textContent  = fmtHash(s.scrypt_khs||0);
  document.getElementById('blocksFound').textContent = s.blocks_found||0;
  document.getElementById('uptime').textContent      = s.uptime||'--';
  document.getElementById('cpuTemp').textContent     = (s.cpu_temp_c||0).toFixed(1)+'C';
  document.getElementById('throttleStatus').textContent = s.throttling ? '! THROTTLING' : '';

  document.getElementById('totalMiners').textContent = (s.workers||[]).filter(function(w){return w.online;}).length;

  // Total power / cost card
  var totalWatts = 0;
  (s.workers||[]).forEach(function(w) { if (w.online && w.watts_estimate > 0) totalWatts += w.watts_estimate; });
  var wattsCardEl = document.getElementById('totalWattsCard');
  if (wattsCardEl) wattsCardEl.textContent = totalWatts > 0 ? totalWatts.toFixed(0)+'W' : '--';
  var costCardEl = document.getElementById('totalCostCard');
  if (costCardEl) {
    var kwhChk = s.kwh_rate_usd || 0;
    if (kwhChk > 0 && s.total_cost_per_day_usd > 0) {
      costCardEl.textContent = fmtCurrency(s.total_cost_per_day_usd)+'/day';
    } else if (kwhChk > 0) {
      costCardEl.textContent = '$0.00/day';
    } else {
      costCardEl.textContent = 'set kWh rate in config';
    }
  }

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
          '<div><div class="cs-l">Price</div><div class="cs-v">'+(c.price_usd > 0 ? '$'+c.price_usd.toLocaleString(undefined,{minimumFractionDigits:2,maximumFractionDigits:4}) : '--')+'</div></div>' +
          (function(){
            var rewardUsd = (c.block_reward > 0 && c.price_usd > 0) ? ' ($'+( c.block_reward * c.price_usd ).toLocaleString(undefined,{minimumFractionDigits:2,maximumFractionDigits:2})+')' : '';
            return '<div><div class="cs-l">Reward</div><div class="cs-v">'+(c.block_reward > 0 ? c.block_reward+' '+c.symbol+rewardUsd : '--')+'</div></div>';
          })() +
          (c.cost_per_day_usd > 0 ? '<div><div class="cs-l">Elec/Day</div><div class="cs-v" style="color:var(--red)">'+fmtCurrency(c.cost_per_day_usd)+'</div></div>' : '') +
          (function(){
            if (!c.session_effort_pct || c.session_effort_pct <= 0) return '';
            var eff = c.session_effort_pct;
            var effColor = eff < 80 ? 'var(--hi2)' : (eff < 150 ? '#ffcc00' : '#ff6600');
            return '<div><div class="cs-l">Session Effort</div><div class="cs-v" style="color:'+effColor+'">'+eff.toFixed(1)+'%</div></div>';
          })() +
          (function(){
            if (!c.expected_block_sec || c.expected_block_sec <= 0) return '';
            return '<div><div class="cs-l">Exp. Block</div><div class="cs-v" style="color:var(--dim)">'+fmtDuration(c.expected_block_sec)+'</div></div>';
          })() +
        '</div>' +
        (function(){
          var balLine = '';
          if ((c.balance_confirmed||0) > 0 || (c.balance_immature||0) > 0) {
            balLine = '<div class="cs-row" style="margin-top:6px;display:flex;gap:6px;align-items:center"><div class="cs-l">WALLET</div><div class="cs-v">'+(c.balance_confirmed||0).toFixed(4)+' confirmed';
            if ((c.balance_immature||0) > 0) {
              balLine += ' \u00b7 <span style="color:var(--amber)">'+(c.balance_immature||0).toFixed(4)+' maturing</span>';
            }
            balLine += '</div></div>';
          }
          return balLine;
        })() +
        (function(){
          var histoHtml = '';
          if (c.diff_histogram && c.diff_histogram.length) {
            var maxCount = Math.max.apply(null, c.diff_histogram.map(function(b){return b.count;}));
            if (maxCount > 0) {
              histoHtml = '<div class="cs-row" style="margin-top:6px;display:flex;gap:6px;align-items:flex-start"><div class="cs-l">SHARES</div><div class="cs-v" style="flex:1"><div class="histo" title="Share difficulty distribution">';
              c.diff_histogram.forEach(function(b) {
                var h = maxCount > 0 ? Math.round(b.count/maxCount*100) : 0;
                var label = fmtDiffShort(b.min)+'-'+fmtDiffShort(b.max)+': '+b.count;
                histoHtml += '<div class="histo-bar" style="height:'+Math.max(h,b.count>0?4:0)+'%" title="'+label+'"></div>';
              });
              histoHtml += '</div></div></div>';
            }
          }
          return histoHtml;
        })() +
        (function(){
          var adjHtml = '';
          if (c.diff_adjust) {
            var da = c.diff_adjust;
            var pct = da.projected_pct;
            var pctStr = (pct >= 0 ? '+' : '') + pct.toFixed(1) + '%';
            var pctColor = pct > 2 ? 'var(--red)' : pct < -2 ? 'var(--hi)' : 'var(--dim)';
            var progress = da.epoch_size > 0 ? Math.round(da.blocks_in_epoch / da.epoch_size * 100) : 0;
            adjHtml = '<div class="cs-row" style="display:flex;gap:6px;margin-top:4px"><div class="cs-l">RETARGET</div><div class="cs-v">'+
              da.blocks_remaining+' blocks · <span style="color:'+pctColor+'">'+pctStr+'</span>'+
              (da.estimated_date ? ' · '+da.estimated_date : '')+
              '</div></div>'+
              '<div class="epoch-bar"><div class="epoch-fill" style="width:'+progress+'%"></div></div>';
          }
          return adjHtml;
        })() +
      '</div>';
  });

  renderWorkersTab(s);

  var blocks = s.block_log||[];
  var logEl = document.getElementById('blockLog');
  if (blocks.length === 0) {
    logEl.innerHTML = '<div class="no-blocks">NO BLOCKS FOUND THIS SESSION.<br>KEEP HASHING.</div>';
  } else {
    logEl.innerHTML = '<div class="block-log">'+blocks.map(function(b){
      var luckStr = '';
      if (b.luck !== undefined && b.luck >= 0) {
        var luckColor = b.luck >= 100 ? 'var(--hi)' : (b.luck >= 50 ? 'var(--hi2)' : '#ff6600');
        luckStr = ' · <span style="color:'+luckColor+'">LUCK '+b.luck.toFixed(1)+'%</span>';
      }
      var hashDisplay = b.explorer_url
        ? '<a href="'+b.explorer_url+b.hash+'" target="_blank" rel="noopener" style="color:inherit;text-decoration:underline dotted;word-break:break-all">'+b.hash.slice(0,16)+'…</a>'
        : b.hash.slice(0,16)+'…';
      var confBar = '';
      if (b.is_orphaned) {
        confBar = '<div style="margin-top:4px;font-family:var(--scan);font-size:.58rem;color:#c0392b;letter-spacing:1px">&#9888; ORPHANED</div>';
      } else if (b.maturity_target > 0) {
        var pct = Math.min(100, Math.round((b.confirmations / b.maturity_target) * 100));
        var mature = b.confirmations >= b.maturity_target;
        var barColor = mature ? 'var(--hi)' : 'var(--hi2)';
        var label = mature ? 'MATURED' : (b.confirmations + ' / ' + b.maturity_target + ' CONF');
        confBar = '<div style="margin-top:5px">' +
          '<div style="display:flex;justify-content:space-between;font-family:var(--scan);font-size:.54rem;color:var(--dim2);margin-bottom:2px">' +
            '<span>' + label + '</span>' +
            '<span>' + pct + '%</span>' +
          '</div>' +
          '<div style="height:3px;background:var(--off);border-radius:2px">' +
            '<div style="height:100%;width:'+pct+'%;background:'+barColor+';border-radius:2px;transition:width 1s"></div>' +
          '</div>' +
        '</div>';
      }
      return '<div class="block-entry">' +
        '<div class="block-trophy">***</div>' +
        '<div><div class="block-coin-height">'+b.coin+' // BLOCK #'+b.height+'</div><div class="block-hash">'+hashDisplay+'</div><div style="font-size:.58rem;color:var(--dim2);margin-top:2px">FOUND BY '+b.worker+luckStr+'</div>'+confBar+'</div>' +
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

  // Update telemetry chart data
  if (s.share_history && s.share_history.length > 0) shareHistoryGlobal = s.share_history;
  if (s.hashrate_history && s.hashrate_history.length > 0) hashrateHistoryGlobal = s.hashrate_history;
  if (s.block_log) blockLogGlobal = s.block_log;
  // Derive best share from workers
  if (s.workers) {
    s.workers.forEach(function(w){ if ((w.best_share||0) > bestShareSession) bestShareSession = w.best_share; });
  }
  if (s.coins) { lastCoins = s.coins; populateCoinFilter(s.coins); updateTelemetryStats(s.coins); }
  renderChart();

  if (s.notifs) applyNotifs(s.notifs);
  if (s.chain_diags) renderDiag(s.chain_diags);
  renderConnect(s);

  // Worker groups
  var groups = s.groups || [];
  var gg = document.getElementById('groupGrid');
  var gc = document.getElementById('groupCount');
  var sec = document.getElementById('sectionGroups');
  if (gg && groups.length > 0) {
    if (sec) sec.style.display = '';
    if (gc) gc.textContent = groups.length + ' GROUPS';
    gg.innerHTML = '';
    groups.forEach(function(g) {
      var profitStr = '';
      var kwhRate = s.kwh_rate_usd || 0;
      if (kwhRate > 0) {
        var p = g.profit_per_day_usd;
        var pClass = p >= 0 ? 'profit-pos' : 'profit-neg';
        profitStr = '<div class="group-stat"><div class="group-stat-l">PROFIT/DAY</div>'+
          '<div class="group-stat-v '+pClass+'">'+(p>=0?'+':'')+
          '$'+Math.abs(p).toFixed(2)+'</div></div>';
      }
      gg.innerHTML += '<div class="group-card">'+
        '<div class="group-name">'+g.name+'</div>'+
        '<div class="group-stat"><div class="group-stat-l">MINERS</div>'+
        '<div class="group-stat-v">'+g.online_count+' / '+g.worker_count+' ONLINE</div></div>'+
        '<div class="group-stat"><div class="group-stat-l">HASHRATE</div>'+
        '<div class="group-stat-v">'+fmtHash(g.hashrate_khs)+'</div></div>'+
        '<div class="group-stat"><div class="group-stat-l">SHARES</div>'+
        '<div class="group-stat-v">'+g.shares_accepted+'</div></div>'+
        profitStr+
        '</div>';
    });
  } else if (sec) {
    sec.style.display = 'none';
  }

  document.getElementById('footerTime').textContent = new Date().toLocaleString();
}

function renderDiag(diags) {
  var grid = document.getElementById('diagGrid');
  if (!diags || !diags.length) return;

  var totalIssues = 0;
  diags.forEach(function(d){ totalIssues += (d.issues||[]).length; });
  var statusEl = document.getElementById('diagStatus');
  if (totalIssues === 0) {
    statusEl.textContent = 'ALL CHAINS OK';
    statusEl.style.color = 'var(--hi)';
  } else {
    statusEl.textContent = totalIssues + ' ISSUE' + (totalIssues>1?'S':'') + ' DETECTED';
    statusEl.style.color = 'var(--red)';
  }

  grid.innerHTML = diags.map(function(d) {
    var total = d.total_shares || 0;
    var valid = d.valid_shares || 0;
    var stale = d.stale_shares || 0;
    var rejected = d.rejected_shares || 0;
    var stalePct = d.stale_pct || 0;
    var rejectPct = d.reject_pct || 0;
    var issues = d.issues || [];
    var hasCrit = issues.some(function(i){ return i.indexOf('No active job') >= 0 || i.indexOf('High stale') >= 0 || i.indexOf('High reject') >= 0; });
    var hasWarn = issues.length > 0 && !hasCrit;
    var cardCls = hasCrit ? 'diag-card has-critical' : (hasWarn ? 'diag-card has-issues' : 'diag-card');
    var dotCls = hasCrit ? 'diag-status-dot crit' : (hasWarn ? 'diag-status-dot warn' : 'diag-status-dot');
    var staleCls = stalePct >= 10 ? 'diag-stat-val crit' : (stalePct >= 3 ? 'diag-stat-val warn' : 'diag-stat-val');
    var rejectCls = rejectPct >= 5 ? 'diag-stat-val crit' : (rejectPct >= 1 ? 'diag-stat-val warn' : 'diag-stat-val');
    var jobAge = d.current_job_age_s || 0;
    var hasJobAgeIssue = issues.some(function(i){ return i.indexOf('Job is') >= 0; });
    var jobAgeCls = hasJobAgeIssue ? 'diag-job-age warn' : 'diag-job-age';
    var jobAgeStr = d.has_job ? (jobAge < 60 ? jobAge+'s' : Math.floor(jobAge/60)+'m'+jobAge%60+'s') : 'NO JOB';
    var issuesHtml = issues.length === 0
      ? '<div class="diag-ok">&#10003; NO ISSUES</div>'
      : issues.map(function(iss){
          var isCrit = iss.indexOf('No active job') >= 0 || iss.indexOf('High stale') >= 0 || iss.indexOf('High reject') >= 0 || iss.indexOf('Initial Block') >= 0;
          return '<div class="diag-issue'+(isCrit?' crit':'')+'">&#9654; '+iss+'</div>';
        }).join('');

    return '<div class="'+cardCls+'">' +
      '<div class="diag-coin-head">' +
        '<span class="diag-coin-sym">'+d.symbol+'</span>' +
        '<div class="'+dotCls+'"></div>' +
      '</div>' +
      '<div class="diag-stat-row">' +
        '<div class="diag-stat"><div class="diag-stat-val">'+valid+'</div><div class="diag-stat-lbl">VALID</div></div>' +
        '<div class="diag-stat"><div class="'+staleCls+'">'+stalePct.toFixed(1)+'%</div><div class="diag-stat-lbl">STALE</div></div>' +
        '<div class="diag-stat"><div class="'+rejectCls+'">'+rejectPct.toFixed(1)+'%</div><div class="diag-stat-lbl">REJECT</div></div>' +
      '</div>' +
      '<div class="diag-job-row">' +
        '<span>JOB: <span class="diag-job-id">'+(d.current_job_id||'--')+'</span></span>' +
        '<span class="'+jobAgeCls+'">AGE: '+jobAgeStr+'</span>' +
        '<span>'+d.worker_count+' MINER'+(d.worker_count!==1?'S':'')+'</span>' +
      '</div>' +
      '<div class="diag-issues">'+issuesHtml+'</div>' +
    '</div>';
  }).join('');
}

function copyToClipboard(text, btnEl) {
  navigator.clipboard.writeText(text).then(function() {
    var orig = btnEl.textContent;
    btnEl.textContent = 'COPIED';
    btnEl.classList.add('copied');
    setTimeout(function(){ btnEl.textContent = orig; btnEl.classList.remove('copied'); }, 1500);
  }).catch(function() {
    // Fallback for non-HTTPS
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.select();
    document.execCommand('copy');
    document.body.removeChild(ta);
    var orig = btnEl.textContent;
    btnEl.textContent = 'COPIED';
    btnEl.classList.add('copied');
    setTimeout(function(){ btnEl.textContent = orig; btnEl.classList.remove('copied'); }, 1500);
  });
}

function renderConnect(s) {
  var grid = document.getElementById('connectGrid');
  var endpoints = s.endpoints || [];
  var poolName = s.pool_name || 'PiPool';
  if (endpoints.length === 0) {
    grid.innerHTML = '<div style="font-family:var(--scan);font-size:.6rem;color:var(--dim2)">No endpoints configured.</div>';
    return;
  }
  grid.innerHTML = endpoints.map(function(ep) {
    var url = 'stratum+tcp://' + ep.host + ':' + ep.port;
    var workerFmt = 'YOUR_ADDRESS.WORKER_NAME';
    var algoLabel = ep.algorithm === 'scrypt' ? 'SCRYPT' : ep.algorithm === 'sha256d' ? 'SHA-256D' : ep.algorithm.toUpperCase();
    var mergeNote = ep.merge_desc && ep.merge_desc !== ep.symbol
      ? '<div style="font-family:var(--scan);font-size:.52rem;color:var(--hi2);margin-top:6px">&#9889; Merge mining: <span style="color:var(--hi)">'+ep.merge_desc+'</span> — single connection mines both coins</div>'
      : '';
    return '<div class="connect-card">' +
      '<div class="connect-card-head">' +
        '<span class="connect-sym">'+ep.merge_desc+'</span>' +
        '<span class="connect-algo">'+algoLabel+'</span>' +
      '</div>' +
      '<div class="connect-field">' +
        '<div class="connect-label">STRATUM URL</div>' +
        '<div class="connect-val">' +
          '<span class="connect-val-text">'+url+'</span>' +
          '<button class="connect-copy" onclick="copyToClipboard(\''+url+'\',this)">COPY</button>' +
        '</div>' +
      '</div>' +
      '<div class="connect-field">' +
        '<div class="connect-label">WORKER / USERNAME</div>' +
        '<div class="connect-val">' +
          '<span class="connect-val-text">'+workerFmt+'</span>' +
          '<button class="connect-copy" onclick="copyToClipboard(\''+workerFmt+'\',this)">COPY</button>' +
        '</div>' +
      '</div>' +
      '<div class="connect-field">' +
        '<div class="connect-label">PASSWORD</div>' +
        '<div class="connect-val"><span class="connect-val-text">x</span></div>' +
      '</div>' +
      '<div class="connect-hint">' +
        'Replace <span>YOUR_ADDRESS</span> with your wallet address and <span>WORKER_NAME</span> with any label (e.g. <span>rig1</span>).' +
        mergeNote +
      '</div>' +
    '</div>';
  }).join('');
}

function openTagEditor() {
  var ed = document.getElementById('tagEditor');
  var inp = document.getElementById('tagInput');
  ed.style.display = 'block';
  inp.focus();
  inp.select();
}

function closeTagEditor() {
  document.getElementById('tagEditor').style.display = 'none';
  document.getElementById('tagSaveStatus').textContent = '';
}

function saveTag() {
  var tag = document.getElementById('tagInput').value.trim();
  var status = document.getElementById('tagSaveStatus');
  if (!tag) { status.textContent = 'TAG CANNOT BE EMPTY'; return; }
  fetch('/api/tag', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({tag: tag})
  }).then(function(r) { return r.json(); }).then(function(d) {
    document.getElementById('coinbaseTag').innerHTML = '&#9889; ' + d.tag;
    status.textContent = 'SAVED';
    setTimeout(closeTagEditor, 800);
  }).catch(function() {
    status.textContent = 'ERROR';
  });
}

// Close tag editor if clicking outside
document.addEventListener('click', function(e) {
  var ed = document.getElementById('tagEditor');
  var badge = document.getElementById('coinbaseTag');
  if (ed && ed.style.display !== 'none' && !ed.contains(e.target) && e.target !== badge) {
    closeTagEditor();
  }
});

var THEMES = ['fallout','marathon-classic','marathon-2025','slate'];
var THEME_LABELS = {fallout:'FALLOUT','marathon-classic':'M-CLASSIC','marathon-2025':'M-2025',slate:'SLATE'};
var currentTheme = 'fallout';

function applyTheme(t) {
  currentTheme = t;
  document.documentElement.setAttribute('data-theme', t === 'fallout' ? '' : t);
  var btn = document.getElementById('themeBtn');
  if (btn) btn.textContent = THEME_LABELS[t] || t.toUpperCase();
  try { localStorage.setItem('pipool_theme', t); } catch(e) {}
}

function cycleTheme() {
  var idx = THEMES.indexOf(currentTheme);
  applyTheme(THEMES[(idx + 1) % THEMES.length]);
}

(function() {
  try {
    var saved = localStorage.getItem('pipool_theme');
    if (saved && THEMES.indexOf(saved) >= 0) applyTheme(saved);
  } catch(e) {}
  try {
    var c = JSON.parse(localStorage.getItem('pipool_currency') || 'null');
    if (c && c.rate > 0) { currencyRate = c.rate; currencySymbol = c.symbol || 'USD'; }
  } catch(e) {}
  // Sync tab button visual state
  var bA = document.getElementById('tabBtnActive');
  var bK = document.getElementById('tabBtnKnown');
  if (bA) { bA.style.borderColor = 'var(--bdr2)'; bA.style.color = 'var(--hi2)'; }
  if (bK) { bK.style.borderColor = 'var(--bdr)';  bK.style.color = 'var(--dim)'; }
})();

// ─── Telemetry Chart ───────────────────────────────────────────────────────
var shareHistoryGlobal = [];
var hashrateHistoryGlobal = [];
var blockLogGlobal = [];
var coinNetDiff = {};
var mergeChildrenMap = {}; // parent symbol -> [aux symbols]
var chartMode = 'diff'; // 'hashrate' | 'diff'
var selectedCoins = {}; // sym -> bool (toggled on/off)
var lastCoins = [];
var COIN_COLORS = {
  'BTC':'#ff8c00','BCH':'#00cc88','LTC':'#7ab4ff','DOGE':'#ccbb00',
  'DGB':'#0099ff','DGBS':'#6655ff','PEP':'#ff44aa',
  'QUAI':'#ff6644','QUAIS':'#ffaa66'
};
var workerTab = 'active'; // 'active' | 'known'
var bestShareSession = 0;
var currencyRate = 1.0;
var currencySymbol = 'USD';
function fmtCurrency(usd) {
  var v = usd * currencyRate;
  var sym = currencySymbol === 'USD' ? '$' : currencySymbol + ' ';
  if (Math.abs(v) < 0.001) return sym + v.toExponential(2);
  if (Math.abs(v) < 10) return sym + v.toFixed(3);
  return sym + v.toFixed(2);
}

function setWorkerTab(t) {
  workerTab = t;
  var bA = document.getElementById('tabBtnActive');
  var bK = document.getElementById('tabBtnKnown');
  if (bA) { bA.style.borderColor = t==='active' ? 'var(--bdr2)' : 'var(--bdr)'; bA.style.color = t==='active' ? 'var(--hi2)' : 'var(--dim)'; }
  if (bK) { bK.style.borderColor = t==='known'  ? 'var(--bdr2)' : 'var(--bdr)'; bK.style.color = t==='known'  ? 'var(--hi2)' : 'var(--dim)'; }
  renderWorkersTab(lastState);
}

function renderWorkersTab(s) {
  var allWorkers = s.workers||[];
  var online = allWorkers.filter(function(w){return w.online;}).length;
  document.getElementById('workerCount').textContent = online+' ONLINE / '+allWorkers.length+' SEEN';

  var workers = workerTab === 'active'
    ? allWorkers.filter(function(w){return w.online;})
    : allWorkers.filter(function(w){return !w.online;});

  var tbody = document.getElementById('workersBody');
  if (workers.length === 0) {
    tbody.innerHTML = workerTab === 'active'
      ? '<tr><td colspan="12" class="no-workers">NO MINERS CONNECTED</td></tr>'
      : '<tr><td colspan="12" class="no-workers">NO KNOWN OFFLINE WORKERS</td></tr>';
    return;
  }
  // Build map of primary coin → merge-mined children
  var mergeChildren = {};
  (s.coins||[]).forEach(function(c) {
    if (c.is_merge_aux && c.merge_parent) {
      if (!mergeChildren[c.merge_parent]) mergeChildren[c.merge_parent] = [];
      mergeChildren[c.merge_parent].push(c.symbol);
    }
  });
  tbody.innerHTML = '';
  workers.forEach(function(w) {
    var total = (w.shares_accepted||0)+(w.shares_rejected||0)+(w.shares_stale||0);
    var staleRate = total>0 ? (w.shares_stale||0)/total : 0;
    var stalePct = (staleRate*100).toFixed(2)+'%';
    var invPct   = total>0 ? ((w.shares_rejected||0)/total*100).toFixed(2)+'%' : '0.00%';
    var staleHigh = parseFloat(stalePct) >= 3;
    var invHigh   = parseFloat(invPct)   >= 1;
    var staleBadge = '';
    if (staleRate >= 0.25) {
      staleBadge = '<span style="background:#c0392b;color:#fff;border-radius:3px;padding:1px 4px;font-size:10px;margin-left:3px">'+Math.round(staleRate*100)+'%</span>';
    } else if (staleRate >= 0.10) {
      staleBadge = '<span style="background:#f39c12;color:#fff;border-radius:3px;padding:1px 4px;font-size:10px;margin-left:3px">'+Math.round(staleRate*100)+'%</span>';
    }
    var statusCls = w.online ? 'worker-status-on' : 'worker-status-off';
    var statusTxt = w.online ? 'ONLINE' : (w.last_seen_at||'OFFLINE');
    var diffHtml = w.online ? '<span class="shares-val">'+fmtDiffShort(w.difficulty||0)+'</span>' : '<span class="worker-device">--</span>';
    var khs = w.hashrate_khs||0;
    var hrHtml = w.online && khs>0 ? '<span class="shares-val">'+fmtHash(khs)+'</span>' : '<span class="worker-device">--</span>';
    var bestHtml = (w.best_share&&w.best_share>0) ? '<span class="shares-val">'+fmtDiffShort(w.best_share)+'</span>' : '<span class="worker-device">--</span>';
    var nameParts = (w.name||'ANON').split('.');
    var workerShort = nameParts.length > 1 ? nameParts.slice(1).join('.') : nameParts[0];
    var addrAbbrev  = nameParts.length > 1 ? nameParts[0].substring(0,10)+'…' : '';
    var ip = (w.addr||'').replace(/:\d+$/, '');
    var tempBadge = (w.asic_temp_c && w.asic_temp_c > 0) ? ' <span style="color:'+(w.asic_temp_c>=80?'#f44':w.asic_temp_c>=70?'#fa0':'#4af')+';font-size:.58rem">'+w.asic_temp_c.toFixed(0)+'°</span>' : '';
    var deviceDisplay = w.user_agent
      ? '<span title="'+w.user_agent+'" style="cursor:help">'+(w.device||'')+'</span>'
      : (w.device||'');
    var subLine = [deviceDisplay+tempBadge, ip].filter(Boolean).join(' ');
    var coin = w.coin || '?';
    var children = mergeChildren[coin];
    var coinLabel = children && children.length ? coin+'+'+children.join('+') : coin;
    var sessCount = (w.reconnect_count||0) + 1;
    var sessLabel = sessCount+'×';
    var sessDur = (w.online && w.session_duration) ? ' '+w.session_duration : '';
    var sessHtml = '<span class="shares-val" title="'+sessCount+' session'+(sessCount>1?'s':'')+'" style="font-size:.65rem">'+sessLabel+sessDur+'</span>';
    var wattsStr = (w.watts_estimate && w.watts_estimate > 0) ? w.watts_estimate.toFixed(0)+'W' : '--';
    var kwhRate = s.kwh_rate_usd || 0;
    var costStr = '--';
    if (kwhRate > 0 && w.watts_estimate > 0) {
      costStr = fmtCurrency(w.cost_per_day_usd || 0);
    }
    var tr = document.createElement('tr');
    if (!w.online) tr.className = 'worker-row-offline';
    tr.style.cursor = 'pointer';
    tr.onclick = (function(ww){ return function(){ openWorkerModal(ww.coin, ww.name); }; })(w);
    tr.innerHTML =
      '<td><div class="worker-name">'+workerShort+'</div><div class="worker-device">'+subLine+'</div></td>' +
      '<td><span class="coin-pill">'+coinLabel+'</span></td>' +
      '<td>'+diffHtml+'</td>' +
      '<td>'+hrHtml+'</td>' +
      '<td><span class="shares-val">'+((w.shares_accepted||0).toLocaleString())+'</span></td>' +
      '<td><span class="'+(staleHigh?'shares-rej':'shares-val')+'">'+stalePct+'</span></td>' +
      '<td><span class="'+(invHigh?'shares-rej':'shares-val')+'">'+invPct+'</span>'+staleBadge+'</td>' +
      '<td>'+bestHtml+'</td>' +
      '<td>'+sessHtml+'</td>' +
      '<td><span class="shares-val">'+wattsStr+'</span></td>' +
      '<td><span class="shares-val">'+costStr+'</span></td>' +
      '<td><span class="'+statusCls+'">'+statusTxt+'</span></td>';
    tbody.appendChild(tr);
  });
}

function setChartMode(m) {
  chartMode = m;
  var bH = document.getElementById('btnHashrate');
  var bD = document.getElementById('btnDiff');
  if (bH) { bH.style.color = m==='hashrate' ? 'var(--hi2)' : 'var(--dim)'; bH.style.borderColor = m==='hashrate' ? 'var(--bdr2)' : 'var(--bdr)'; }
  if (bD) { bD.style.color = m==='diff' ? 'var(--hi2)' : 'var(--dim)'; bD.style.borderColor = m==='diff' ? 'var(--bdr2)' : 'var(--bdr)'; }
  renderChart();
}

function populateCoinFilter(coins) {
  var wrap = document.getElementById('coinFilterBtns');
  if (!wrap) return;

  // Rebuild merge children map and net diff cache
  mergeChildrenMap = {};
  coins.forEach(function(c) {
    if (c.difficulty && c.daemon_online) coinNetDiff[c.symbol] = c.difficulty;
    if (c.is_merge_aux && c.merge_parent) {
      if (!mergeChildrenMap[c.merge_parent]) mergeChildrenMap[c.merge_parent] = [];
      if (mergeChildrenMap[c.merge_parent].indexOf(c.symbol) < 0)
        mergeChildrenMap[c.merge_parent].push(c.symbol);
    }
  });

  var existing = {};
  wrap.querySelectorAll('[data-coin]').forEach(function(b){ existing[b.getAttribute('data-coin')] = true; });

  coins.forEach(function(c) {
    if (c.is_merge_aux) return;
    if (existing[c.symbol]) return;
    existing[c.symbol] = true;
    // Auto-select first coin on first load; subsequent coins default off
    if (selectedCoins[c.symbol] === undefined) {
      var hasAny = Object.keys(selectedCoins).some(function(k){ return selectedCoins[k]; });
      selectedCoins[c.symbol] = !hasAny;
    }
    var btn = document.createElement('button');
    btn.setAttribute('data-coin', c.symbol);
    btn.textContent = c.symbol;
    btn.onclick = function(){ toggleCoinFilter(c.symbol); };
    applyBtnStyle(btn, c.symbol);
    wrap.appendChild(btn);
  });
}

function applyBtnStyle(btn, sym) {
  var on = !!selectedCoins[sym];
  var color = on ? (COIN_COLORS[sym] || 'var(--hi2)') : 'var(--dim)';
  btn.style.cssText = 'background:var(--off);border:1px solid '+(on?'var(--bdr2)':'var(--bdr)')+';color:'+color+';font-family:var(--scan);font-size:.55rem;padding:2px 7px;cursor:pointer;letter-spacing:1px';
}

function toggleCoinFilter(sym) {
  selectedCoins[sym] = !selectedCoins[sym];
  var btn = document.querySelector('[data-coin="'+sym+'"]');
  if (btn) applyBtnStyle(btn, sym);
  renderChart();
  updateTelemetryStats(lastCoins);
}

function updateTelemetryStats(coins) {
  // Use the first selected primary (non-aux) coin for stats
  var activeList = Object.keys(selectedCoins).filter(function(s){ return selectedCoins[s]; });
  var filter = activeList.length === 1 ? activeList[0] : (activeList[0] || '');

  var netDiff = 0, totalKHs = 0;
  coins.forEach(function(c) {
    if (!c.daemon_online || c.symbol !== filter) return;
    netDiff = c.difficulty || 0;
    totalKHs = c.hashrate_khs || 0;
  });

  var netDiffEl = document.getElementById('netDiffDisplay');
  if (netDiffEl) netDiffEl.textContent = netDiff > 0 ? fmtDiffShort(netDiff) : '--';

  if (netDiff > 0 && totalKHs > 0) {
    var hashPerSec = totalKHs * 1000;
    var expectedSec = (netDiff * 4294967296) / hashPerSec;
    document.getElementById('blockExpected').textContent = fmtSeconds(expectedSec);
    var pctPerHour = (1 - Math.exp(-3600 / expectedSec)) * 100;
    var oddsPerHr = 100 / pctPerHour;
    var oddsStr = oddsPerHr < 100
      ? '1 in ' + oddsPerHr.toFixed(1) + ' /hr'
      : oddsPerHr < 10000
        ? '1 in ' + Math.round(oddsPerHr).toLocaleString() + ' /hr'
        : oddsPerHr < 1e6
          ? '1 in ' + (oddsPerHr/1000).toFixed(1) + 'K /hr'
          : '1 in ' + (oddsPerHr/1e6).toFixed(2) + 'M /hr';
    var POWERBALL = 292201338;
    var ratio = POWERBALL / oddsPerHr;
    var compStr;
    if (ratio >= 1) {
      var rx = ratio >= 1e6 ? (ratio/1e6).toFixed(1)+'M×' : ratio >= 1e3 ? Math.round(ratio/1000)+'K×' : Math.round(ratio)+'×';
      compStr = rx + ' better than Powerball jackpot (1 in 292M)';
    } else {
      var rx2 = 1/ratio >= 1e6 ? (1/ratio/1e6).toFixed(1)+'M×' : 1/ratio >= 1e3 ? Math.round(1/ratio/1000)+'K×' : Math.round(1/ratio)+'×';
      compStr = rx2 + ' worse than Powerball jackpot (1 in 292M)';
    }
    document.getElementById('blockOddsNow').innerHTML =
      oddsStr + '<div style="font-family:var(--scan);font-size:.5rem;color:var(--dim2);margin-top:3px;letter-spacing:.5px">'+compStr+'</div>';
  } else {
    document.getElementById('blockOddsNow').textContent = '--';
    document.getElementById('blockExpected').textContent = '--';
  }

  // Aux chain expected time / odds rows
  var auxWrap = document.getElementById('auxOddsWrap');
  if (auxWrap) {
    var auxChildren = mergeChildrenMap[filter] || [];
    var hashPerSec2 = totalKHs * 1000;
    var rows = auxChildren.map(function(ac) {
      var auxNetD = coinNetDiff[ac] || 0;
      if (auxNetD <= 0 || hashPerSec2 <= 0) return '';
      var expSec = (auxNetD * 4294967296) / hashPerSec2;
      var pct = (1 - Math.exp(-3600 / expSec)) * 100;
      var auxOdds = 100 / pct;
      var pctStr = auxOdds < 100 ? '1 in '+auxOdds.toFixed(1)+' /hr'
        : auxOdds < 10000 ? '1 in '+Math.round(auxOdds).toLocaleString()+' /hr'
        : auxOdds < 1e6 ? '1 in '+(auxOdds/1000).toFixed(1)+'K /hr'
        : '1 in '+(auxOdds/1e6).toFixed(2)+'M /hr';
      var col = (window.AUX_COLORS && AUX_COLORS[ac]) ? AUX_COLORS[ac] : '#aaaaaa';
      return '<div style="display:flex;gap:10px;align-items:center;padding:6px 12px;background:var(--surf2);border:1px solid var(--bdr);margin-bottom:4px;flex-wrap:wrap">' +
        '<span style="font-family:var(--scan);font-size:.56rem;color:'+col+';letter-spacing:2px;min-width:36px">'+ac+'</span>' +
        '<span style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);letter-spacing:2px">EXPECTED</span>' +
        '<span style="font-family:var(--vt);font-size:.9rem;color:var(--hi);letter-spacing:1px;margin-right:8px">'+fmtSeconds(expSec)+'</span>' +
        '<span style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);letter-spacing:2px">ODDS</span>' +
        '<span style="font-family:var(--vt);font-size:.9rem;color:var(--hi);letter-spacing:1px;margin-right:8px">'+pctStr+'</span>' +
        '<span style="font-family:var(--scan);font-size:.56rem;color:var(--dim2);letter-spacing:2px">NET DIFF</span>' +
        '<span style="font-family:var(--vt);font-size:.9rem;color:var(--dim);letter-spacing:1px">'+fmtDiffShort(auxNetD)+'</span>' +
        '</div>';
    }).join('');
    auxWrap.innerHTML = rows;
  }

  var bsEl = document.getElementById('bestShareNow');
  if (bsEl && bestShareSession > 0) bsEl.textContent = fmtDiffShort(bestShareSession);
}

function fmtSeconds(s) {
  if (s < 60)    return s.toFixed(0) + 's';
  if (s < 3600)  return (s/60).toFixed(1) + ' min';
  if (s < 86400) return (s/3600).toFixed(1) + ' hrs';
  return (s/86400).toFixed(1) + ' days';
}

function renderChart() {
  var canvas = document.getElementById('diffChart');
  var noData = document.getElementById('noShareData');
  if (!canvas) return;

  // Active coins = those toggled on
  var activeCoins = Object.keys(selectedCoins).filter(function(s){ return selectedCoins[s]; });

  var cssSt = getComputedStyle(document.documentElement);
  var cLine = cssSt.getPropertyValue('--chart-line').trim() || '#00ff41';
  var cNet  = cssSt.getPropertyValue('--chart-net').trim()  || '#ff4400';
  var cRej  = cssSt.getPropertyValue('--chart-rej').trim()  || '#ff6600';
  var cBdr  = cssSt.getPropertyValue('--bdr').trim()        || '#004400';
  var cDim2 = cssSt.getPropertyValue('--dim2').trim()       || '#005514';
  var cSurf = cssSt.getPropertyValue('--surf').trim()       || '#000f00';

  if (chartMode === 'hashrate') {
    // Group hashrate history by coin, filtered to selected coins
    var coinGroups = {};
    hashrateHistoryGlobal.forEach(function(s) {
      if (activeCoins.length > 0 && activeCoins.indexOf(s.coin) < 0) return;
      if (!coinGroups[s.coin]) coinGroups[s.coin] = [];
      coinGroups[s.coin].push(s);
    });
    var visCoins = Object.keys(coinGroups).filter(function(c){ return coinGroups[c].length >= 2; });
    if (visCoins.length === 0) {
      canvas.style.display = 'none'; noData.style.display = 'block'; return;
    }
    canvas.style.display = 'block'; noData.style.display = 'none';

    var rect = canvas.getBoundingClientRect();
    var dpr = window.devicePixelRatio || 1;
    canvas.width = rect.width*dpr; canvas.height = rect.height*dpr;
    var ctx = canvas.getContext('2d');
    ctx.scale(dpr, dpr);
    var W = rect.width, H = rect.height;
    var PAD = {t:14, r:12, b:28, l:68};
    var cW = W-PAD.l-PAD.r, cH = H-PAD.t-PAD.b;

    var allSamples = [].concat.apply([], visCoins.map(function(c){ return coinGroups[c]; }));
    var tMin = Math.min.apply(null, allSamples.map(function(s){return s.t;}));
    var tMax = Math.max.apply(null, allSamples.map(function(s){return s.t;}));
    var tSpan = tMax - tMin || 1;
    function xPos(t) { return PAD.l + (t - tMin) / tSpan * cW; }

    var FALLBACK_COLORS = [cLine,'#ff8c00','#00aaff','#ff4466','#aa44ff','#00cc88','#ccbb00'];
    var multiCoin = visCoins.length > 1;

    var coinMax = {};
    visCoins.forEach(function(c) {
      coinMax[c] = Math.max.apply(null, coinGroups[c].map(function(s){return s.khs;})) * 1.15 || 1;
    });

    var useLog = false;
    if (!multiCoin) {
      var singleMax = coinMax[visCoins[0]];
      var posKHs = coinGroups[visCoins[0]].filter(function(s){return s.khs>0;}).map(function(s){return s.khs;});
      var singleMin = posKHs.length ? Math.min.apply(null, posKHs) : singleMax;
      useLog = singleMax / Math.max(singleMin, 1e-9) > 100;
    }
    var logMax = useLog ? Math.log10(coinMax[visCoins[0]]) : 0;
    var logMin = useLog ? Math.log10(Math.max(coinMax[visCoins[0]] * 1e-6, 0.001)) : 0;

    function yPos(khs, coin) {
      if (multiCoin) return PAD.t + cH * (1 - Math.min(khs / coinMax[coin], 1));
      if (useLog) {
        var v = Math.max(khs, Math.pow(10, logMin));
        return PAD.t + cH * (1 - (Math.log10(v) - logMin) / (logMax - logMin));
      }
      return PAD.t + cH * (1 - khs / coinMax[visCoins[0]]);
    }

    ctx.fillStyle = cSurf; ctx.fillRect(0,0,W,H);

    ctx.strokeStyle = cBdr; ctx.lineWidth = 0.5;
    if (multiCoin) {
      for (var gi=0; gi<=4; gi++) {
        var gy = PAD.t + (gi/4)*cH;
        ctx.beginPath(); ctx.moveTo(PAD.l,gy); ctx.lineTo(W-PAD.r,gy); ctx.stroke();
        ctx.fillStyle = cDim2; ctx.font='9px monospace'; ctx.textAlign='right';
        ctx.fillText((100-gi*25)+'%', PAD.l-4, gy+3);
      }
    } else if (useLog) {
      var p0 = Math.floor(logMin), p1 = Math.ceil(logMax);
      for (var p=p0; p<=p1; p++) {
        [1,2,5].forEach(function(m) {
          var v = m * Math.pow(10, p);
          if (v < Math.pow(10,logMin) || v > Math.pow(10,logMax)) return;
          var gy2 = yPos(v, visCoins[0]);
          ctx.beginPath(); ctx.moveTo(PAD.l,gy2); ctx.lineTo(W-PAD.r,gy2); ctx.stroke();
          ctx.fillStyle = cDim2; ctx.font='9px monospace'; ctx.textAlign='right';
          ctx.fillText(fmtKHs(v), PAD.l-4, gy2+3);
        });
      }
    } else {
      var linMax = coinMax[visCoins[0]];
      for (var gi2=0; gi2<=4; gi2++) {
        var gy3 = PAD.t + (gi2/4)*cH;
        ctx.beginPath(); ctx.moveTo(PAD.l,gy3); ctx.lineTo(W-PAD.r,gy3); ctx.stroke();
        ctx.fillStyle = cDim2; ctx.font='9px monospace'; ctx.textAlign='right';
        ctx.fillText(fmtKHs(linMax*(1-gi2/4)), PAD.l-4, gy3+3);
      }
    }

    if (!multiCoin) {
      var netDiffH = coinNetDiff[visCoins[0]] || 0;
      if (netDiffH > 0) {
        var ny = PAD.t + cH*0.15;
        ctx.strokeStyle = cNet; ctx.lineWidth=1.5; ctx.setLineDash([6,4]);
        ctx.beginPath(); ctx.moveTo(PAD.l,ny); ctx.lineTo(W-PAD.r,ny); ctx.stroke();
        ctx.setLineDash([]); ctx.fillStyle=cNet; ctx.font='9px monospace'; ctx.textAlign='left';
        ctx.fillText('NET DIFF ~'+fmtDiffShort(netDiffH), PAD.l+4, ny-3);
      }
    }

    visCoins.forEach(function(coin, ci) {
      var csamps = coinGroups[coin];
      var color = COIN_COLORS[coin] || FALLBACK_COLORS[ci % FALLBACK_COLORS.length];
      var pts = csamps.map(function(s){ return {x:xPos(s.t), y:yPos(s.khs, coin)}; });

      if (!multiCoin) {
        ctx.beginPath();
        ctx.moveTo(pts[0].x, PAD.t+cH);
        pts.forEach(function(p){ ctx.lineTo(p.x,p.y); });
        ctx.lineTo(pts[pts.length-1].x, PAD.t+cH);
        ctx.closePath();
        ctx.fillStyle = 'rgba(0,200,60,0.10)';
        ctx.fill();
      }

      ctx.beginPath();
      ctx.strokeStyle = color; ctx.lineWidth = 2;
      ctx.shadowColor = color; ctx.shadowBlur = 4;
      pts.forEach(function(p,i){ i===0?ctx.moveTo(p.x,p.y):ctx.lineTo(p.x,p.y); });
      ctx.stroke(); ctx.shadowBlur = 0;

      if (multiCoin) {
        var lp = pts[pts.length-1];
        var lastKHs = csamps[csamps.length-1].khs;
        ctx.fillStyle = color; ctx.font = 'bold 8px monospace'; ctx.textAlign = 'left';
        var lx = Math.min(lp.x+4, W-80), ly = Math.max(PAD.t+8, Math.min(lp.y, PAD.t+cH-4));
        ctx.fillText(coin+': '+fmtKHs(lastKHs), lx, ly);
      }
    });

    ctx.fillStyle=cDim2; ctx.font='9px monospace'; ctx.textAlign='center';
    [0,0.25,0.5,0.75,1].forEach(function(f){
      var t=tMin+tSpan*f, d=new Date(t);
      ctx.fillText(d.getHours().toString().padStart(2,'0')+':'+d.getMinutes().toString().padStart(2,'0'), PAD.l+f*cW, H-6);
    });

  } else {
    // DIFF MODE — scatter plot; supports multiple coins simultaneously
    var multiDiff = activeCoins.length > 1;

    // Collect and group share data
    var coinShares = {};
    activeCoins.forEach(function(sym) {
      var arr = shareHistoryGlobal.filter(function(s){ return s.coin === sym; });
      arr.sort(function(a,b){ return a.t-b.t; });
      if (arr.length > 0) coinShares[sym] = arr;
    });
    var visSyms = Object.keys(coinShares);

    if (visSyms.length === 0) {
      canvas.style.display='none'; noData.style.display='block'; return;
    }
    canvas.style.display='block'; noData.style.display='none';

    var rect2 = canvas.getBoundingClientRect();
    var dpr2 = window.devicePixelRatio||1;
    canvas.width=rect2.width*dpr2; canvas.height=rect2.height*dpr2;
    var ctx2=canvas.getContext('2d');
    ctx2.scale(dpr2,dpr2);
    var W2=rect2.width, H2=rect2.height;
    var PAD2={t:16,r:12,b:28,l:72};
    var cW2=W2-PAD2.l-PAD2.r, cH2=H2-PAD2.t-PAD2.b;

    // Unified log scale across all visible share data + all net diffs
    var posDiffs = [];
    visSyms.forEach(function(sym) {
      coinShares[sym].forEach(function(s){ if(s.diff>0) posDiffs.push(s.diff); });
      if (coinNetDiff[sym] > 0) posDiffs.push(coinNetDiff[sym]);
      // include merge children net diffs for single-coin view
      if (!multiDiff) {
        (mergeChildrenMap[sym]||[]).forEach(function(ac){ if(coinNetDiff[ac]>0) posDiffs.push(coinNetDiff[ac]); });
      }
    });
    if (posDiffs.length === 0) { canvas.style.display='none'; noData.style.display='block'; return; }

    var rawMin = Math.min.apply(null, posDiffs);
    var rawMax = Math.max.apply(null, posDiffs);
    var logMin2 = Math.log10(Math.max(rawMin * 0.5, 1));
    var logMax2 = Math.log10(rawMax * 3.0);
    var logRange2 = logMax2 - logMin2 || 1;
    function yDiff(d) {
      var v = Math.max(d, Math.pow(10, logMin2));
      return PAD2.t + cH2 * (1 - (Math.log10(v) - logMin2) / logRange2);
    }

    // Unified time axis
    var allTimes = [];
    visSyms.forEach(function(sym){ coinShares[sym].forEach(function(s){ allTimes.push(s.t); }); });
    var tMin2 = Math.min.apply(null, allTimes);
    var tMax2 = Math.max.apply(null, allTimes);
    var tSpan2 = tMax2-tMin2 || 1;
    function xDiff(t) { return PAD2.l + (t-tMin2)/tSpan2*cW2; }

    // Background
    ctx2.fillStyle=cSurf; ctx2.fillRect(0,0,W2,H2);

    // Grid lines
    var pLow=Math.floor(logMin2), pHigh=Math.ceil(logMax2);
    for (var p=pLow; p<=pHigh; p++) {
      [1,2,5].forEach(function(m) {
        var v=m*Math.pow(10,p), lv=Math.log10(v);
        if (lv < logMin2 || lv > logMax2) return;
        var gy=PAD2.t+cH2*(1-(lv-logMin2)/logRange2);
        ctx2.strokeStyle = m===1 ? cBdr : 'rgba(0,68,0,0.25)';
        ctx2.lineWidth = m===1 ? 0.7 : 0.3;
        ctx2.beginPath(); ctx2.moveTo(PAD2.l,gy); ctx2.lineTo(W2-PAD2.r,gy); ctx2.stroke();
        if (m===1) {
          ctx2.fillStyle=cDim2; ctx2.font='9px monospace'; ctx2.textAlign='right';
          ctx2.fillText(fmtDiffShort(v), PAD2.l-4, gy+3);
        }
      });
    }

    // Net diff lines — one per visible coin (and aux children in single-coin mode)
    if (!multiDiff && visSyms.length === 1) {
      var sym1 = visSyms[0];
      var auxChildren = mergeChildrenMap[sym1] || [];
      // Aux chain net diffs
      auxChildren.forEach(function(ac, ai) {
        var acD = coinNetDiff[ac] || 0;
        if (acD <= 0) return;
        var lv = Math.log10(acD);
        if (lv < logMin2 || lv > logMax2) return;
        var ay = yDiff(acD);
        var acColor = COIN_COLORS[ac] || '#aaaaaa';
        ctx2.strokeStyle = acColor; ctx2.lineWidth = 1; ctx2.setLineDash([4,4]);
        ctx2.beginPath(); ctx2.moveTo(PAD2.l, ay); ctx2.lineTo(W2-PAD2.r, ay); ctx2.stroke();
        ctx2.setLineDash([]);
        var labelX = PAD2.l + 4 + ai * 80;
        ctx2.fillStyle = acColor; ctx2.font = '8px monospace'; ctx2.textAlign = 'left';
        ctx2.fillText(ac+' '+fmtDiffShort(acD), labelX, ay-3);
      });
      // Parent net diff
      var parentNetD = coinNetDiff[sym1] || 0;
      if (parentNetD > 0) {
        var lv2 = Math.log10(parentNetD);
        if (lv2 >= logMin2 && lv2 <= logMax2) {
          var ny2=yDiff(parentNetD);
          ctx2.strokeStyle=cNet; ctx2.lineWidth=1.5; ctx2.setLineDash([6,4]);
          ctx2.beginPath(); ctx2.moveTo(PAD2.l,ny2); ctx2.lineTo(W2-PAD2.r,ny2); ctx2.stroke();
          ctx2.setLineDash([]);
          ctx2.fillStyle=cNet; ctx2.font='bold 9px monospace'; ctx2.textAlign='left';
          ctx2.fillText(sym1+' NET  '+fmtDiffShort(parentNetD), PAD2.l+4, ny2-4);
        }
      }
    } else {
      // Multi-coin: draw net diff line for each visible coin in its color
      visSyms.forEach(function(sym, si) {
        var nd = coinNetDiff[sym] || 0;
        if (nd <= 0) return;
        var lv = Math.log10(nd);
        if (lv < logMin2 || lv > logMax2) return;
        var ny3 = yDiff(nd);
        var nColor = COIN_COLORS[sym] || cNet;
        ctx2.strokeStyle=nColor; ctx2.lineWidth=1.2; ctx2.setLineDash([6,4]);
        ctx2.beginPath(); ctx2.moveTo(PAD2.l,ny3); ctx2.lineTo(W2-PAD2.r,ny3); ctx2.stroke();
        ctx2.setLineDash([]);
        ctx2.fillStyle=nColor; ctx2.font='8px monospace'; ctx2.textAlign='left';
        ctx2.fillText(sym+' '+fmtDiffShort(nd), PAD2.l + 4 + si*90, ny3-3);
      });
    }

    // Draw each coin's shares
    var FALLBACK2 = [cLine,'#ff8c00','#00aaff','#ff4466','#aa44ff','#00cc88','#ccbb00'];
    visSyms.forEach(function(sym, si) {
      var dsamples = coinShares[sym];
      var coinColor = COIN_COLORS[sym] || FALLBACK2[si % FALLBACK2.length];
      var parentNetD2 = coinNetDiff[sym] || 0;
      var auxCh = (!multiDiff && visSyms.length === 1) ? (mergeChildrenMap[sym]||[]) : [];

      // Faint connecting line
      ctx2.strokeStyle = coinColor;
      ctx2.globalAlpha = 0.2;
      ctx2.lineWidth=1; ctx2.setLineDash([]);
      ctx2.beginPath();
      dsamples.forEach(function(s,i){
        var x=xDiff(s.t), y=yDiff(s.diff);
        i===0 ? ctx2.moveTo(x,y) : ctx2.lineTo(x,y);
      });
      ctx2.stroke();
      ctx2.globalAlpha = 1;

      // Dots
      dsamples.forEach(function(s){
        var x=xDiff(s.t), y=yDiff(s.diff);
        var dotColor, dotR=3, dotGlow=5;
        if (!s.ok) {
          dotColor = cRej;
        } else if (parentNetD2 > 0 && s.diff >= parentNetD2) {
          dotColor = '#ffffff'; dotR=6; dotGlow=16;
        } else {
          var isAuxHit = auxCh.some(function(ac){ return coinNetDiff[ac] > 0 && s.diff >= coinNetDiff[ac]; });
          if (isAuxHit) {
            dotColor = '#ffdd00'; dotR=5; dotGlow=12;
            auxCh.forEach(function(ac){
              if (coinNetDiff[ac] > 0 && s.diff >= coinNetDiff[ac])
                dotColor = COIN_COLORS[ac] || '#ffdd00';
            });
          } else {
            dotColor = multiDiff ? coinColor : cLine;
          }
        }
        ctx2.beginPath(); ctx2.arc(x,y,dotR,0,Math.PI*2);
        ctx2.fillStyle=dotColor;
        ctx2.shadowColor=dotColor; ctx2.shadowBlur=dotGlow;
        ctx2.fill(); ctx2.shadowBlur=0;
      });
    });

    // Block found vertical markers
    var allVisCoins = visSyms.slice();
    if (!multiDiff && visSyms.length === 1) {
      (mergeChildrenMap[visSyms[0]]||[]).forEach(function(ac){ allVisCoins.push(ac); });
    }
    blockLogGlobal.forEach(function(b) {
      if (allVisCoins.indexOf(b.coin) < 0) return;
      if (!b.found_at_ms) return;
      var bx = xDiff(b.found_at_ms);
      if (bx < PAD2.l || bx > W2-PAD2.r) return;
      var bColor = (COIN_COLORS[b.coin] || '#ffdd00');
      if (!multiDiff && b.coin === visSyms[0]) bColor = '#ffffff';
      ctx2.strokeStyle = bColor; ctx2.lineWidth = 1.5; ctx2.setLineDash([3,3]);
      ctx2.globalAlpha = 0.7;
      ctx2.beginPath(); ctx2.moveTo(bx, PAD2.t); ctx2.lineTo(bx, H2-PAD2.b); ctx2.stroke();
      ctx2.setLineDash([]); ctx2.globalAlpha = 1;
      ctx2.fillStyle = bColor; ctx2.font = 'bold 8px monospace'; ctx2.textAlign = 'center';
      ctx2.shadowColor = bColor; ctx2.shadowBlur = 8;
      ctx2.fillText('▲', bx, H2-PAD2.b-2);
      ctx2.fillText(b.coin, bx, PAD2.t+8);
      ctx2.shadowBlur = 0;
    });

    // Time labels
    ctx2.fillStyle=cDim2; ctx2.font='9px monospace'; ctx2.textAlign='center';
    [0,0.25,0.5,0.75,1].forEach(function(f){
      var t=tMin2+tSpan2*f, d=new Date(t);
      ctx2.fillText(d.getHours().toString().padStart(2,'0')+':'+d.getMinutes().toString().padStart(2,'0'), PAD2.l+f*cW2, H2-6);
    });
  }
}

function fmtKHs(khs) {
  if (!khs||khs===0) return '0';
  if (khs>=1e9) return (khs/1e9).toFixed(1)+' TH/s';
  if (khs>=1e6) return (khs/1e6).toFixed(1)+' GH/s';
  if (khs>=1e3) return (khs/1e3).toFixed(1)+' MH/s';
  return khs.toFixed(0)+' KH/s';
}

function fmtDiffShort(d) {
  if (!d||d===0) return '0';
  if (d>=1e12) return (d/1e12).toFixed(1)+'T';
  if (d>=1e9)  return (d/1e9).toFixed(1)+'G';
  if (d>=1e6)  return (d/1e6).toFixed(1)+'M';
  if (d>=1e3)  return (d/1e3).toFixed(1)+'K';
  return d.toFixed(2);
}

var evtSrc = new EventSource('/api/events');
evtSrc.onmessage = function(e) { try { apply(JSON.parse(e.data)); } catch(err){} };
evtSrc.onerror = function() { document.getElementById('liveStatus').textContent = 'RECONNECTING'; };

fetch('/api/stats').then(function(r){return r.json();}).then(apply).catch(function(){});
fetch('/api/discord').then(function(r){return r.json();}).then(applyNotifs).catch(function(){});
fetch('/api/config').then(function(r){return r.json();}).then(applyCfg).catch(function(){});

// ── Config editor ──────────────────────────────────────────────────────────

var cfgState = {};

function applyCfg(c) {
  if (!c) return;
  cfgState = c;
  var el;
  el = document.getElementById('cfgTempLimit');      if(el) el.value = c.temp_limit_c || 75;
  el = document.getElementById('cfgWorkerTimeout');  if(el) el.value = c.worker_timeout_s || 120;
  el = document.getElementById('cfgWebhook');        if(el) el.value = c.discord_webhook || '';
  el = document.getElementById('cfgAutoKickPct');    if(el) el.value = c.auto_kick_pct || 0;
  el = document.getElementById('cfgAutoKickMin');    if(el) el.value = c.auto_kick_min_shares || 50;
  el = document.getElementById('cfgKwhRate');        if(el) el.value = c.kwh_rate_usd || '';
  el = document.getElementById('cfgCurrencySymbol'); if(el) el.value = currencySymbol !== 'USD' ? currencySymbol : '';
  el = document.getElementById('cfgCurrencyRate');   if(el) el.value = currencyRate !== 1.0 ? currencyRate : '';

  // Wallets
  var wDiv = document.getElementById('cfgWallets');
  if (wDiv) {
    wDiv.innerHTML = '';
    var wallets = c.wallets || {};
    Object.keys(wallets).sort().forEach(function(sym) {
      var row = document.createElement('div');
      row.className = 'cfg-row';
      row.innerHTML = '<div class="cfg-label">'+sym+'</div>'+
        '<input class="cfg-input wide" id="cfgWallet_'+sym+'" type="text" value="'+wallets[sym]+'">';
      wDiv.appendChild(row);
    });
  }

  // Vardiff
  var vDiv = document.getElementById('cfgVardiff');
  if (vDiv) {
    vDiv.innerHTML = '';
    var vmins = c.vardiff_min || {}, vmaxs = c.vardiff_max || {};
    var coins = Array.from(new Set(Object.keys(vmins).concat(Object.keys(vmaxs)))).sort();
    coins.forEach(function(sym) {
      var row = document.createElement('div');
      row.className = 'cfg-row';
      row.style.marginBottom = '4px';
      row.innerHTML = '<div class="cfg-label" style="min-width:50px">'+sym+' MIN</div>'+
        '<input class="cfg-input" id="cfgVmin_'+sym+'" type="number" step="any" value="'+(vmins[sym]||0)+'" style="width:90px">'+
        '<div class="cfg-label" style="min-width:10px;text-align:center">MAX</div>'+
        '<input class="cfg-input" id="cfgVmax_'+sym+'" type="number" step="any" value="'+(vmaxs[sym]||0)+'" style="width:90px">';
      vDiv.appendChild(row);
    });
  }

  // Fixed diff rows
  var tbody = document.getElementById('cfgFdRows');
  if (tbody) {
    tbody.innerHTML = '';
    var fd = c.worker_fixed_diff || {};
    Object.keys(fd).sort().forEach(function(w) { cfgFdAddRow(w, fd[w]); });
  }

  // Node settings
  var nodesDiv = document.getElementById('cfgNodes');
  if (nodesDiv) {
    nodesDiv.innerHTML = '';
    var nodeHosts = c.node_hosts || {};
    var nodePorts = c.node_ports || {};
    var nodeUsers = c.node_users || {};
    var nodePws = c.node_passwords || {};
    var bkEnabled = c.backup_enabled || {};
    var bkHosts = c.backup_hosts || {};
    var bkPorts = c.backup_ports || {};
    var bkUsers = c.backup_users || {};
    var bkPws = c.backup_passwords || {};
    var upEnabled = c.upstream_enabled || {};
    var upHosts = c.upstream_hosts || {};
    var upPorts = c.upstream_ports || {};
    var upUsers = c.upstream_users || {};
    var coinList = Object.keys(nodeHosts).sort();
    coinList.forEach(function(sym) {
      var div = document.createElement('div');
      div.className = 'node-cfg-coin';
      var bkOn = bkEnabled[sym] ? 'checked' : '';
      var upOn = upEnabled[sym] ? 'checked' : '';
      div.innerHTML =
        '<div class="node-cfg-coin-title">'+sym+'</div>'+
        // Primary node
        '<div class="node-cfg-row">'+
          '<div class="node-cfg-label">PRIMARY NODE</div>'+
          '<input class="node-cfg-host" id="nHost_'+sym+'" placeholder="host/IP" value="'+(nodeHosts[sym]||'')+'">'+
          '<span style="font-family:var(--scan);font-size:.56rem;color:var(--dim2)">:</span>'+
          '<input class="node-cfg-port" id="nPort_'+sym+'" type="number" placeholder="port" value="'+(nodePorts[sym]||0)+'">'+
          '<input class="node-cfg-user" id="nUser_'+sym+'" placeholder="rpc user" value="'+(nodeUsers[sym]||'')+'">'+
          '<input class="node-cfg-user" id="nPass_'+sym+'" type="password" placeholder="rpc pass" value="'+(nodePws[sym]||'')+'" autocomplete="new-password">'+
          '<button class="node-test-btn" onclick="testNode(\''+sym+'\',\'primary\')">TEST</button>'+
          '<span class="node-test-ok" id="nTest_'+sym+'_primary"></span>'+
        '</div>'+
        // Backup node
        '<div class="node-cfg-row">'+
          '<div class="node-cfg-label">BACKUP NODE</div>'+
          '<label class="node-enable-toggle"><input type="checkbox" id="bkEnabled_'+sym+'" '+bkOn+'> ENABLED</label>'+
          '<input class="node-cfg-host" id="bkHost_'+sym+'" placeholder="host/IP" value="'+(bkHosts[sym]||'')+'">'+
          '<span style="font-family:var(--scan);font-size:.56rem;color:var(--dim2)">:</span>'+
          '<input class="node-cfg-port" id="bkPort_'+sym+'" type="number" placeholder="port" value="'+(bkPorts[sym]||0)+'">'+
          '<input class="node-cfg-user" id="bkUser_'+sym+'" placeholder="rpc user" value="'+(bkUsers[sym]||'')+'">'+
          '<input class="node-cfg-user" id="bkPass_'+sym+'" type="password" placeholder="rpc pass" value="'+(bkPws[sym]||'')+'" autocomplete="new-password">'+
          '<button class="node-test-btn" onclick="testNode(\''+sym+'\',\'backup\')">TEST</button>'+
          '<span class="node-test-ok" id="nTest_'+sym+'_backup"></span>'+
        '</div>'+
        // Upstream pool
        '<div class="node-cfg-row">'+
          '<div class="node-cfg-label">UPSTREAM POOL</div>'+
          '<label class="node-enable-toggle"><input type="checkbox" id="upEnabled_'+sym+'" '+upOn+'> ENABLED</label>'+
          '<input class="node-cfg-host" id="upHost_'+sym+'" placeholder="pool host" value="'+(upHosts[sym]||'')+'">'+
          '<span style="font-family:var(--scan);font-size:.56rem;color:var(--dim2)">:</span>'+
          '<input class="node-cfg-port" id="upPort_'+sym+'" type="number" placeholder="port" value="'+(upPorts[sym]||0)+'">'+
          '<input class="node-cfg-user" id="upUser_'+sym+'" placeholder="username" value="'+(upUsers[sym]||'')+'">'+
        '</div>';
      nodesDiv.appendChild(div);
    });

    // Quai node (shared for QUAI + QUAIS)
    if (c.quai_node_host !== undefined || c.quai_node_port !== undefined) {
      var quaiDiv = document.createElement('div');
      quaiDiv.className = 'node-cfg-coin';
      quaiDiv.innerHTML =
        '<div class="node-cfg-coin-title">QUAI NETWORK (QUAI + QUAIS)</div>'+
        '<div class="node-cfg-row">'+
          '<div class="node-cfg-label">NODE HOST</div>'+
          '<input class="node-cfg-host" id="cfgQuaiHost" placeholder="e.g. 192.168.1.100" value="'+(c.quai_node_host||'')+'">'+
          '<span style="font-family:var(--scan);font-size:.56rem;color:var(--dim2)">:</span>'+
          '<input class="node-cfg-port" id="cfgQuaiPort" type="number" placeholder="9001" value="'+(c.quai_node_port||9001)+'">'+
          '<span style="font-family:var(--scan);font-size:.58rem;color:var(--dim2);padding-left:6px">HTTP RPC PORT</span>'+
        '</div>';
      nodesDiv.appendChild(quaiDiv);
    }
  }
}

function cfgFdAddRow(worker, diff) {
  var tbody = document.getElementById('cfgFdRows');
  if (!tbody) return;
  var tr = document.createElement('tr');
  tr.innerHTML =
    '<td><input class="cfg-fd-input" style="width:160px" type="text" placeholder="wallet.worker" value="'+worker+'"></td>'+
    '<td style="padding-left:8px"><input class="cfg-fd-input" type="number" step="any" placeholder="e.g. 256" value="'+diff+'"></td>'+
    '<td style="padding-left:6px"><button class="cfg-fd-del" onclick="this.closest(\'tr\').remove()" title="Remove">&#215;</button></td>';
  tbody.appendChild(tr);
}

function saveCfg() {
  var status = document.getElementById('cfgSaveStatus');

  // Collect wallets
  var wallets = {};
  if (cfgState.wallets) {
    Object.keys(cfgState.wallets).forEach(function(sym) {
      var el = document.getElementById('cfgWallet_'+sym);
      if (el) wallets[sym] = el.value.trim();
    });
  }

  // Collect vardiff
  var vmins = {}, vmaxs = {};
  if (cfgState.vardiff_min) {
    Object.keys(cfgState.vardiff_min).forEach(function(sym) {
      var elMin = document.getElementById('cfgVmin_'+sym);
      var elMax = document.getElementById('cfgVmax_'+sym);
      if (elMin) vmins[sym] = parseFloat(elMin.value) || 0;
      if (elMax) vmaxs[sym] = parseFloat(elMax.value) || 0;
    });
  }

  // Collect fixed diffs
  var fd = {};
  var rows = document.querySelectorAll('#cfgFdRows tr');
  rows.forEach(function(tr) {
    var inputs = tr.querySelectorAll('input');
    var name = inputs[0] ? inputs[0].value.trim() : '';
    var d = inputs[1] ? parseFloat(inputs[1].value) : 0;
    if (name && d > 0) fd[name] = d;
  });

  // Collect node settings
  var nodeHosts = {}, nodePorts = {}, nodeUsers = {}, nodePws = {};
  var bkEnabled = {}, bkHosts = {}, bkPorts = {}, bkUsers = {}, bkPws = {};
  var upEnabled = {}, upHosts = {}, upPorts = {}, upUsers = {};
  var nodeCoins = Object.keys(cfgState.node_hosts || {});
  nodeCoins.forEach(function(sym) {
    var h = document.getElementById('nHost_'+sym);    if (h) nodeHosts[sym] = h.value.trim();
    var p = document.getElementById('nPort_'+sym);    if (p) nodePorts[sym] = parseInt(p.value)||0;
    var u = document.getElementById('nUser_'+sym);    if (u) nodeUsers[sym] = u.value.trim();
    var pw= document.getElementById('nPass_'+sym);    if (pw) nodePws[sym] = pw.value;
    var be= document.getElementById('bkEnabled_'+sym); if (be) bkEnabled[sym] = be.checked;
    var bh= document.getElementById('bkHost_'+sym);    if (bh) bkHosts[sym] = bh.value.trim();
    var bp= document.getElementById('bkPort_'+sym);    if (bp) bkPorts[sym] = parseInt(bp.value)||0;
    var bu= document.getElementById('bkUser_'+sym);    if (bu) bkUsers[sym] = bu.value.trim();
    var bpw=document.getElementById('bkPass_'+sym);    if (bpw) bkPws[sym] = bpw.value;
    var ue= document.getElementById('upEnabled_'+sym); if (ue) upEnabled[sym] = ue.checked;
    var uh= document.getElementById('upHost_'+sym);    if (uh) upHosts[sym] = uh.value.trim();
    var up= document.getElementById('upPort_'+sym);    if (up) upPorts[sym] = parseInt(up.value)||0;
    var uu= document.getElementById('upUser_'+sym);    if (uu) upUsers[sym] = uu.value.trim();
  });

  // Collect Quai node settings
  var quaiNodeHost = '';
  var quaiNodePort = 9001;
  var qhEl = document.getElementById('cfgQuaiHost');
  var qpEl = document.getElementById('cfgQuaiPort');
  if (qhEl) quaiNodeHost = qhEl.value.trim();
  if (qpEl) quaiNodePort = parseInt(qpEl.value) || 9001;

  var payload = {
    temp_limit_c:       parseInt(document.getElementById('cfgTempLimit').value)||75,
    worker_timeout_s:   parseInt(document.getElementById('cfgWorkerTimeout').value)||120,
    discord_webhook:    document.getElementById('cfgWebhook').value.trim(),
    auto_kick_pct:      parseInt(document.getElementById('cfgAutoKickPct').value)||0,
    auto_kick_min_shares: parseInt(document.getElementById('cfgAutoKickMin').value)||50,
    wallets:            wallets,
    vardiff_min:        vmins,
    vardiff_max:        vmaxs,
    worker_fixed_diff:  fd,
    kwh_rate_usd:       parseFloat(document.getElementById('cfgKwhRate').value) || 0,
    node_hosts: nodeHosts, node_ports: nodePorts, node_users: nodeUsers, node_passwords: nodePws,
    backup_enabled: bkEnabled, backup_hosts: bkHosts, backup_ports: bkPorts, backup_users: bkUsers, backup_passwords: bkPws,
    upstream_enabled: upEnabled, upstream_hosts: upHosts, upstream_ports: upPorts, upstream_users: upUsers,
    quai_node_host: quaiNodeHost,
    quai_node_port: quaiNodePort
  };

  // Currency preference is client-side only — save to localStorage
  var symEl = document.getElementById('cfgCurrencySymbol');
  var rateEl = document.getElementById('cfgCurrencyRate');
  if (symEl && rateEl) {
    var sym = symEl.value.trim().toUpperCase() || 'USD';
    var rate = parseFloat(rateEl.value) || 1.0;
    currencySymbol = sym;
    currencyRate = rate;
    try { localStorage.setItem('pipool_currency', JSON.stringify({symbol:sym, rate:rate})); } catch(e) {}
  }

  fetch('/api/config', {
    method:'POST',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify(payload)
  }).then(function(r) {
    if (status) status.textContent = r.ok ? 'SAVED' : 'ERROR';
    setTimeout(function(){ if(status) status.textContent = 'CONFIG EDITOR'; }, 3000);
    if (r.ok) {
      fetch('/api/config').then(function(r2){return r2.json();}).then(applyCfg);
      var rb = document.getElementById('cfgRestartBtn');
      if (rb) rb.style.display = 'inline-block';
    }
  }).catch(function() {
    if (status) status.textContent = 'ERROR';
    setTimeout(function(){ if(status) status.textContent = 'CONFIG EDITOR'; }, 3000);
  });
}

function testNode(sym, which) {
  var prefix = which === 'backup' ? 'bk' : 'n';
  var hostEl = document.getElementById(prefix+'Host_'+sym);
  var portEl = document.getElementById(prefix+'Port_'+sym);
  var userEl = document.getElementById(prefix+'User_'+sym);
  var passEl = document.getElementById(prefix+'Pass_'+sym);
  var statusEl = document.getElementById('nTest_'+sym+'_'+which);
  if (!hostEl || !portEl || !statusEl) return;
  var host = hostEl.value.trim();
  var port = parseInt(portEl.value)||0;
  if (!host || !port) { statusEl.textContent = 'enter host:port first'; statusEl.className = 'node-test-fail'; return; }
  statusEl.textContent = 'testing…'; statusEl.className = 'node-test-ok';
  fetch('/api/node/test', {
    method:'POST',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify({host:host, port:port, user:userEl?userEl.value.trim():'', password:passEl?passEl.value:''})
  }).then(function(r){return r.json();}).then(function(d) {
    statusEl.textContent = d.ok ? '✓ '+d.message : '✗ '+d.message;
    statusEl.className = d.ok ? 'node-test-ok' : 'node-test-fail';
  }).catch(function(e) {
    statusEl.textContent = '✗ request failed';
    statusEl.className = 'node-test-fail';
  });
}

function applyAndRestart() {
  var btn = document.getElementById('cfgRestartBtn');
  var sts = document.getElementById('cfgRestartStatus');
  if (btn) btn.disabled = true;
  if (sts) sts.textContent = 'Restarting…';
  fetch('/api/restart', {method:'POST'})
    .then(function(r){return r.json();})
    .then(function(d) {
      if (sts) sts.textContent = d.message || 'Restarting…';
      // Poll for reconnect
      setTimeout(function pollBack() {
        fetch('/api/stats').then(function(){
          if (sts) sts.textContent = 'Back online!';
          if (btn) { btn.disabled = false; btn.style.display = 'none'; }
          setTimeout(function(){ if(sts) sts.textContent = ''; }, 3000);
        }).catch(function(){ setTimeout(pollBack, 1500); });
      }, 2000);
    }).catch(function() {
      if (sts) sts.textContent = 'Restart failed';
      if (btn) btn.disabled = false;
    });
}
// ── Worker detail modal ──────────────────────────────────────────────────
function openWorkerModal(coin, name) {
  document.getElementById('worker-modal').style.display = 'block';
  document.getElementById('wm-title').textContent = name + ' (' + coin + ')';
  document.getElementById('wm-meta').innerHTML = '<span style="color:#666">Loading...</span>';
  fetch('/api/worker/' + encodeURIComponent(coin) + '/' + encodeURIComponent(name))
    .then(function(r){ return r.json(); })
    .then(function(d) { renderWorkerModal(d); })
    .catch(function(e) {
      document.getElementById('wm-meta').innerHTML = '<span style="color:#f66">Error loading worker data</span>';
    });
}
function closeWorkerModal() {
  document.getElementById('worker-modal').style.display = 'none';
}
document.getElementById('worker-modal').addEventListener('click', function(e) {
  if (e.target === this) closeWorkerModal();
});
function renderWorkerModal(d) {
  var meta = document.getElementById('wm-meta');
  var rows = [
    ['Status', d.online ? '<span style="color:#4f4">ONLINE</span>' : '<span style="color:#f64">OFFLINE</span>'],
    ['Address', d.addr || '—'],
    ['Device', d.device || '—'],
    ['User-Agent', (d.user_agent||'—').substring(0,40)],
    ['Connected', d.connected_at || '—'],
    ['Reconnects', d.reconnect_count],
    ['Difficulty', fmtDiff(d.difficulty)],
    ['Hashrate', fmtKHs(d.hashrate_khs)],
    ['Shares', d.shares_accepted + ' acc / ' + d.shares_rejected + ' rej / ' + d.shares_stale + ' stale'],
    ['Best Share', fmtDiff(d.best_share)],
    ['Watts', d.watts_estimate > 0 ? d.watts_estimate.toFixed(0) + ' W' : '—'],
  ];
  meta.innerHTML = rows.map(function(r){ return '<div><span style="color:#666">'+r[0]+'</span><br><span style="color:#dde">'+r[1]+'</span></div>'; }).join('');
  // Step-line diff chart (new canvas-based)
  renderWorkerChart(d);
  // Legacy line chart (same data, smaller canvas below)
  drawLineChart('wm-diff-chart', (d.diff_history||[]).map(function(e){ return {x:e.at_ms, y:e.diff}; }), '#7090ff');
  // Share sparkline
  drawBarChart('wm-share-chart', d.share_sparkline||[], '#5fa');
  // ASIC
  var asicDiv = document.getElementById('wm-asic');
  if (d.asic_temp_c > 0 || d.asic_power_w > 0) {
    asicDiv.style.display = 'block';
    var vals = [];
    if (d.asic_temp_c > 0) vals.push('<span>🌡 ' + d.asic_temp_c.toFixed(1) + '°C</span>');
    if (d.asic_fan_rpm > 0) vals.push('<span>💨 ' + d.asic_fan_rpm + ' RPM</span>');
    if (d.asic_fan_pct > 0) vals.push('<span>Fan ' + d.asic_fan_pct + '%</span>');
    if (d.asic_power_w > 0) vals.push('<span>⚡ ' + d.asic_power_w.toFixed(0) + ' W</span>');
    if (d.asic_hashrate_khs > 0) vals.push('<span>⛏ ' + fmtKHs(d.asic_hashrate_khs) + '</span>');
    document.getElementById('wm-asic-vals').innerHTML = vals.join('');
  } else {
    asicDiv.style.display = 'none';
  }
}
function drawLineChart(canvasId, points, color) {
  var canvas = document.getElementById(canvasId);
  if (!canvas || !canvas.getContext) return;
  var ctx = canvas.getContext('2d');
  canvas.width = canvas.offsetWidth || 640;
  var W = canvas.width, H = canvas.height;
  ctx.clearRect(0, 0, W, H);
  if (!points || points.length < 2) {
    ctx.fillStyle = '#444';
    ctx.font = '12px monospace';
    ctx.fillText('No data', W/2-25, H/2+4);
    return;
  }
  var xs = points.map(function(p){return p.x;}), ys = points.map(function(p){return p.y;});
  var minX = Math.min.apply(null,xs), maxX = Math.max.apply(null,xs);
  var minY = Math.min.apply(null,ys), maxY = Math.max.apply(null,ys);
  if (maxX===minX) maxX=minX+1;
  if (maxY===minY) maxY=minY*2||1;
  var pad = 4;
  function px(x){ return pad + (x-minX)/(maxX-minX)*(W-2*pad); }
  function py(y){ return H-pad - (y-minY)/(maxY-minY)*(H-2*pad); }
  ctx.beginPath();
  ctx.moveTo(px(xs[0]), py(ys[0]));
  for (var i=1;i<points.length;i++) ctx.lineTo(px(xs[i]),py(ys[i]));
  ctx.strokeStyle=color; ctx.lineWidth=1.5; ctx.stroke();
  // Dots
  ctx.fillStyle=color;
  for (var i=0;i<points.length;i++){
    ctx.beginPath(); ctx.arc(px(xs[i]),py(ys[i]),2,0,Math.PI*2); ctx.fill();
  }
  // Y-axis label
  ctx.fillStyle='#666'; ctx.font='10px monospace';
  ctx.fillText(fmtDiff(maxY), 2, 10);
  ctx.fillText(fmtDiff(minY), 2, H-2);
}
function drawBarChart(canvasId, values, color) {
  var canvas = document.getElementById(canvasId);
  if (!canvas || !canvas.getContext) return;
  var ctx = canvas.getContext('2d');
  canvas.width = canvas.offsetWidth || 640;
  var W = canvas.width, H = canvas.height;
  ctx.clearRect(0, 0, W, H);
  if (!values || values.length === 0) return;
  var maxV = Math.max.apply(null, values) || 1;
  var bw = W / values.length;
  for (var i=0;i<values.length;i++){
    var bh = (values[i]/maxV) * (H-4);
    ctx.fillStyle = color;
    ctx.fillRect(i*bw+1, H-bh, bw-2, bh);
  }
}
function renderWorkerChart(detail) {
  var canvas = document.getElementById('workerChart');
  if (!canvas || !detail.diff_history || detail.diff_history.length < 2) return;
  var ctx = canvas.getContext('2d');
  var W = canvas.width = canvas.offsetWidth;
  var H = canvas.height = 160;
  ctx.clearRect(0, 0, W, H);
  var hist = detail.diff_history;
  var now = Date.now();
  var minT = hist[0].at_ms, maxT = now;
  if (maxT <= minT) maxT = minT + 1;
  var diffs = hist.map(function(h){ return h.diff; });
  var minD = Math.min.apply(null, diffs), maxD = Math.max.apply(null, diffs);
  var logMin = Math.log10(Math.max(1, minD * 0.5));
  var logMax = Math.log10(maxD * 2);
  if (logMax <= logMin) logMax = logMin + 1;
  function xPos(t){ return (t - minT) / (maxT - minT) * (W - 40) + 20; }
  function yPos(d){ return H - 20 - (Math.log10(Math.max(1,d)) - logMin) / (logMax - logMin) * (H - 40); }
  ctx.strokeStyle = '#3498db';
  ctx.lineWidth = 2;
  ctx.beginPath();
  for (var i=0; i<hist.length; i++) {
    var x = xPos(hist[i].at_ms), y = yPos(hist[i].diff);
    if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    if (i < hist.length - 1) ctx.lineTo(xPos(hist[i+1].at_ms), y);
  }
  if (hist.length > 0) ctx.lineTo(xPos(now), yPos(hist[hist.length-1].diff));
  ctx.stroke();
  ctx.fillStyle = '#3498db';
  for (var i=0; i<hist.length; i++) {
    ctx.beginPath();
    ctx.arc(xPos(hist[i].at_ms), yPos(hist[i].diff), 3, 0, 2*Math.PI);
    ctx.fill();
  }
  ctx.fillStyle = '#999';
  ctx.font = '10px monospace';
  ctx.textAlign = 'right';
  for (var p=Math.ceil(logMin); p<=Math.floor(logMax); p++) {
    var d = Math.pow(10, p);
    var y = yPos(d);
    if (y > 10 && y < H - 10) {
      ctx.fillText(d >= 1e6 ? (d/1e6).toFixed(0)+'M' : d >= 1e3 ? (d/1e3).toFixed(0)+'K' : d, 18, y+3);
      ctx.strokeStyle = '#333';
      ctx.setLineDash([2,4]);
      ctx.beginPath(); ctx.moveTo(20, y); ctx.lineTo(W, y); ctx.stroke();
      ctx.setLineDash([]);
    }
  }
  ctx.textAlign = 'left';
}

</script>
<!-- Worker detail modal -->
<div id="worker-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,.7);z-index:1000;overflow-y:auto;">
 <div style="background:#1a1a2e;border:1px solid #4a4a6a;border-radius:8px;max-width:700px;margin:40px auto;padding:24px;position:relative;">
  <button onclick="closeWorkerModal()" style="position:absolute;top:12px;right:16px;background:none;border:none;color:#aaa;font-size:20px;cursor:pointer;">✕</button>
  <h2 id="wm-title" style="color:#e0e0ff;margin:0 0 16px;font-size:1.1rem;"></h2>
  <div id="wm-meta" style="display:grid;grid-template-columns:1fr 1fr;gap:8px 24px;margin-bottom:16px;font-size:.85rem;color:#aaa;"></div>
  <div style="margin-bottom:16px;">
   <div style="color:#aaa;font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;margin-bottom:6px;">Difficulty History</div>
   <canvas id="workerChart" height="160" style="width:100%;background:#111122;border-radius:4px;display:block;"></canvas>
   <canvas id="wm-diff-chart" height="80" style="width:100%;background:#111122;border-radius:4px;margin-top:4px;"></canvas>
  </div>
  <div style="margin-bottom:16px;">
   <div style="color:#aaa;font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;margin-bottom:6px;">Shares / Minute (last 30 min)</div>
   <canvas id="wm-share-chart" height="60" style="width:100%;background:#111122;border-radius:4px;"></canvas>
  </div>
  <div id="wm-asic" style="display:none;margin-bottom:8px;">
   <div style="color:#aaa;font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;margin-bottom:6px;">ASIC Health</div>
   <div id="wm-asic-vals" style="display:flex;gap:16px;flex-wrap:wrap;font-size:.85rem;"></div>
  </div>
 </div>
</div>
</body>
</html>`
