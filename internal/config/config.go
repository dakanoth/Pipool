package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// PoolConfig is the root configuration loaded from pipool.json
type PoolConfig struct {
	Pool     PoolSettings            `json:"pool"`
	Coins    map[string]CoinConfig   `json:"coins"`
	Discord  DiscordConfig           `json:"discord"`
	Dashboard DashboardConfig        `json:"dashboard"`
	Logging  LoggingConfig           `json:"logging"`
	Quai     QuaiConfig              `json:"quai"`
	Guardian GuardianConfig          `json:"guardian"`
	Swap     SwapConfig              `json:"swap"`
}

// SwapConfig controls the auto-swap system that converts mined coins to a destination coin.
type SwapConfig struct {
	Enabled          bool              `json:"enabled"`
	Exchange         string            `json:"exchange"`           // "tradeogre"
	APIKey           string            `json:"api_key"`
	APISecret        string            `json:"api_secret"`
	CheckIntervalMin int               `json:"check_interval_min"` // default 15
	StateFile        string            `json:"state_file"`
	Rules            map[string]SwapRule   `json:"rules"`          // coin → swap rule
	DepositAddrs     map[string]string `json:"deposit_addrs"`      // coin → exchange deposit address
}

// SwapRule defines auto-swap behavior for a single source coin.
type SwapRule struct {
	Enabled     bool    `json:"enabled"`
	Destination string  `json:"destination"`      // target coin symbol
	MinBalance  float64 `json:"min_balance"`       // minimum balance before triggering
	KeepBalance float64 `json:"keep_balance"`      // keep this much in source wallet
	MaxSlippage float64 `json:"max_slippage_pct"`  // max acceptable slippage %
}

// GuardianConfig controls the autonomous monitoring system.
type GuardianConfig struct {
	Enabled          bool    `json:"enabled"`
	TickSec          int     `json:"tick_sec"`            // evaluation interval (default 30)
	LogLevel         int     `json:"log_level"`           // 0=errors, 1=actions, 2=verbose
	MaxStaleRatePct  float64 `json:"max_stale_rate_pct"`  // kick worker if stale% exceeds (default 40)
	MaxRejectRatePct float64 `json:"max_reject_rate_pct"` // kick worker if reject% exceeds (default 25)
	MinSharesForKick int     `json:"min_shares_for_kick"` // min shares before kick (default 50)
	SilentWorkerMin  int     `json:"silent_worker_min"`   // alert if no shares for N min (default 15)
	MaxJobAgeSec     int     `json:"max_job_age_sec"`     // alert if job older than this (default 300)
	TempWarnC        float64 `json:"temp_warn_c"`         // warning threshold (default 75)
	TempCritC        float64 `json:"temp_crit_c"`         // critical threshold (default 82)
	TempTrendWarn    float64 `json:"temp_trend_warn"`     // °C/min rise rate to warn (default 2.0)
	HashrateDropPct  float64 `json:"hashrate_drop_pct"`   // alert if hashrate drops >N% (default 50)
}

type PoolSettings struct {
	Name           string   `json:"name"`
	Host           string   `json:"host"`            // bind address, e.g. "0.0.0.0"
	CoinbaseTag    string   `json:"coinbase_tag"`    // e.g. "/PiPool/" — embedded in every block found
	MaxConnections int      `json:"max_connections"` // cap RAM usage
	WorkerTimeout  int      `json:"worker_timeout"`  // seconds before idle disconnect
	TempLimitC     int      `json:"temp_limit_c"`    // CPU temp throttle threshold
	// IPAllowlist restricts stratum connections to specific IPs or CIDR ranges.
	// Empty = allow all (default). Example: ["192.168.1.0/24", "10.0.0.5"]
	IPAllowlist    []string `json:"ip_allowlist"`
	// StateFile is the path prefix for persisted worker state (LastDifficulty, BestShare).
	// Actual files are written as <StateFile>_<COIN>.json, e.g. worker_state_LTC.json.
	// Empty = no persistence.
	StateFile      string   `json:"state_file"`
	// WorkerFixedDiff pins specific workers to a fixed vardiff instead of auto-adjusting.
	// Key is the full worker name (e.g. "wallet.workerName"). Value is the target difficulty.
	// 0 or absent = normal vardiff.
	WorkerFixedDiff map[string]float64 `json:"worker_fixed_diff,omitempty"`
	// AutoKickRejectPct disconnects a worker if their invalid share rate exceeds this
	// percentage after AutoKickMinShares have been submitted. 0 = disabled.
	AutoKickRejectPct  int `json:"auto_kick_reject_pct"`
	// AutoKickMinShares is the minimum share count before auto-kick is evaluated.
	AutoKickMinShares  int `json:"auto_kick_min_shares"`
	// LastShareAlertMin fires a Discord alert if a connected worker goes this many minutes
	// without submitting a share. 0 = disabled.
	LastShareAlertMin  int `json:"last_share_alert_min"`
	// KwhRateUSD is the electricity cost in USD per kWh. 0 = disabled.
	KwhRateUSD         float64            `json:"kwh_rate_usd"`
	// WorkerFixedWatts overrides the wattage estimate for specific workers by name.
	WorkerFixedWatts   map[string]float64 `json:"worker_fixed_watts,omitempty"`
	// WorkerGroups groups workers by name for aggregate stats display. Group name → list of worker names.
	WorkerGroups       map[string][]string `json:"worker_groups,omitempty"`
}

// UpstreamConf configures a fallback upstream stratum pool for proxy mode.
// When the local node goes offline, PiPool transparently forwards miner
// connections to this upstream pool instead of serving no work.
type UpstreamConf struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
}

type CoinConfig struct {
	Enabled      bool          `json:"enabled"`
	Symbol       string        `json:"symbol"`
	Algorithm    string        `json:"algorithm"`    // "scrypt", "sha256d", "scryptn"
	MergeParent  string        `json:"merge_parent"` // e.g. "LTC" for DOGE; "" if primary
	DataDir      string        `json:"datadir"`      // blockchain storage path, e.g. /mnt/external/litecoin
	Stratum      StratumConf   `json:"stratum"`
	Node         NodeConf      `json:"node"`
	Wallet       string        `json:"wallet"`
	BlockReward  float64       `json:"block_reward"`
	TxFeeTarget  float64       `json:"tx_fee_target"`
	BackupNode   *NodeConf     `json:"backup_node,omitempty"`
	UpstreamPool UpstreamConf  `json:"upstream_pool"`
	// WorkerFixedDiff pins specific workers on this coin to a fixed difficulty,
	// overriding the global pool.worker_fixed_diff for this coin only.
	// Key is full worker name (e.g. "wallet.workerName"). 0 or absent = use global or vardiff.
	WorkerFixedDiff map[string]float64 `json:"worker_fixed_diff,omitempty"`
	// BlockExplorer is the base URL for viewing blocks on a public explorer.
	// The block hash is appended directly: BlockExplorer + hash
	// Example: "https://blockchair.com/litecoin/block/"
	BlockExplorer string `json:"block_explorer,omitempty"`
}

type StratumConf struct {
	Port    int         `json:"port"`
	Vardiff VardiffConf `json:"vardiff"`
	TLS     TLSConf     `json:"tls"`
	// StaleKickCount is how many consecutive stale submissions for the same aged-out
	// job before the worker is forcibly disconnected to obtain fresh work.
	// 0 = use default (5). For fast-block coins like DGBS (~15s blocks), set to 2
	// so the reconnect cycle fires after ~10s of stale work instead of ~25s.
	StaleKickCount int `json:"stale_kick_count,omitempty"`
	// RecentJobsSize controls how many recent jobs are kept for share validation.
	// Shares referencing a job not in this window are marked stale. Default is 4.
	// For fast-block coins (e.g. DGBS with ~15s blocks), increase this so that
	// slower miners (like DG1Home at high fixed diff) don't have their job evicted
	// before they can submit shares. Example: 8 keeps ~2 min of jobs at 15s blocks.
	RecentJobsSize int `json:"recent_jobs_size,omitempty"`
}

type TLSConf struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`      // 0 = auto (plain port + 10)
	CertFile string `json:"cert_file"` // empty = auto-generate self-signed
	KeyFile  string `json:"key_file"`
}

type VardiffConf struct {
	MinDiff   float64 `json:"min_diff"`
	MaxDiff   float64 `json:"max_diff"`
	TargetMs  int     `json:"target_ms"`   // target share interval in ms
	RetargetS int     `json:"retarget_s"`  // how often to re-evaluate difficulty
}

type NodeConf struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	// ZMQ endpoint for instant block notifications (optional but recommended)
	ZmqPubHashBlock string `json:"zmq_pub_hashblock"`
	// SystemdService is the systemd unit name (e.g. "litecoind") for the watchdog
	// auto-restart feature. Empty = watchdog disabled for this node.
	SystemdService string `json:"systemd_service"`
}

type DiscordConfig struct {
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhook_url"`
	BotName    string `json:"bot_name"`
	AvatarURL  string `json:"avatar_url"`

	Alerts DiscordAlerts `json:"alerts"`
}

type DiscordAlerts struct {
	BlockFound      bool `json:"block_found"`
	MinerConnected  bool `json:"miner_connected"`
	MinerDisconnect bool `json:"miner_disconnect"`
	HighTemp        bool `json:"high_temp"`
	HashrateReport  bool `json:"hashrate_report"`
	// How often to send periodic hashrate summaries (minutes, 0 = off)
	HashrateIntervalMin int `json:"hashrate_interval_min"`
	// Minimum hashrate drop % before alerting (0 = off)
	HashrateDropPct int `json:"hashrate_drop_pct"`
	NodeUnreachable bool `json:"node_unreachable"`
	// StaleKickAlertCount fires a Discord alert when a worker is kicked for stale shares
	// this many times within a 1-hour rolling window. 0 = disabled.
	StaleKickAlertCount int `json:"stale_kick_alert_count,omitempty"`
	// WalletBalanceAlert: check wallet balances every N minutes and notify if above threshold.
	// 0 = disabled.
	WalletCheckMin int `json:"wallet_check_min,omitempty"`
	// Per-coin minimum balance to trigger an alert (e.g. {"BTC": 0.001, "LTC": 1.0}).
	// Coins not listed use a default threshold of 0 (always alert if any balance exists).
	WalletThresholds map[string]float64 `json:"wallet_thresholds,omitempty"`
}

type DashboardConfig struct {
	Enabled       bool `json:"enabled"`
	Port          int  `json:"port"`
	PushIntervalS int  `json:"push_interval_s"`
	// No auth — dashboard is LAN-only, open by default
}

type LoggingConfig struct {
	Level  string `json:"level"`  // "debug", "info", "warn", "error"
	ToFile bool   `json:"to_file"`
	Path   string `json:"path"`
}

// DefaultConfig returns a sensible Pi 5 default configuration
func DefaultConfig() *PoolConfig {
	return &PoolConfig{
		Pool: PoolSettings{
			Name:              "PiPool",
			Host:              "0.0.0.0",
			CoinbaseTag:       "/PiPool/",
			MaxConnections:    64,
			WorkerTimeout:     120,
			TempLimitC:        75,
			IPAllowlist:       []string{},
			StateFile:         "/opt/pipool/worker_state",
			AutoKickRejectPct: 0, // disabled by default
			AutoKickMinShares: 50,
			LastShareAlertMin: 0, // disabled by default
			KwhRateUSD:        0, // disabled by default
		},
		Coins: map[string]CoinConfig{
			"LTC": {
				Enabled:   true,
				Symbol:    "LTC",
				Algorithm: "scrypt",
				Stratum: StratumConf{
					Port: 3333,
					Vardiff: VardiffConf{
						MinDiff: 0.001, MaxDiff: 1024,
						TargetMs: 30000, RetargetS: 60,
					},
				},
				Node: NodeConf{
					Host: "127.0.0.1", Port: 9332,
					User: "litecoind", Password: "changeme",
					ZmqPubHashBlock: "tcp://127.0.0.1:28332",
				},
				Wallet:      "YOUR_LTC_WALLET",
				BlockReward: 6.25,
				UpstreamPool: UpstreamConf{
					Enabled:  false,
					Host:     "stratum.litecoinpool.org",
					Port:     3333,
					User:     "YOUR_LTC_WALLET",
					Password: "x",
				},
			},
			"DOGE": {
				Enabled:     true,
				Symbol:      "DOGE",
				Algorithm:   "scrypt",
				MergeParent: "LTC", // AuxPoW merged with LTC
				Stratum: StratumConf{
					Port: 3334,
					Vardiff: VardiffConf{
						MinDiff: 0.001, MaxDiff: 1024,
						TargetMs: 30000, RetargetS: 60,
					},
				},
				Node: NodeConf{
					Host: "127.0.0.1", Port: 22555,
					User: "dogecoind", Password: "changeme",
					ZmqPubHashBlock: "tcp://127.0.0.1:28333",
				},
				Wallet:      "YOUR_DOGE_WALLET",
				BlockReward: 10000,
			},
			"BTC": {
				Enabled:   true,
				Symbol:    "BTC",
				Algorithm: "sha256d",
				Stratum: StratumConf{
					Port: 3335,
					Vardiff: VardiffConf{
						MinDiff: 1, MaxDiff: 1048576,
						TargetMs: 30000, RetargetS: 60,
					},
				},
				Node: NodeConf{
					Host: "127.0.0.1", Port: 8332,
					User: "bitcoind", Password: "changeme",
					ZmqPubHashBlock: "tcp://127.0.0.1:28334",
				},
				Wallet:      "YOUR_BTC_WALLET",
				BlockReward: 3.125,
			},
			"BCH": {
				Enabled:     true,
				Symbol:      "BCH",
				Algorithm:   "sha256d",
				MergeParent: "BTC",
				Stratum: StratumConf{
					Port: 3336,
					Vardiff: VardiffConf{
						MinDiff: 1, MaxDiff: 1048576,
						TargetMs: 30000, RetargetS: 60,
					},
				},
				Node: NodeConf{
					Host: "127.0.0.1", Port: 8336,
					User: "bchd", Password: "changeme",
					ZmqPubHashBlock: "tcp://127.0.0.1:28335",
				},
				Wallet:      "YOUR_BCH_WALLET",
				BlockReward: 6.25,
			},
			"PEP": {
				Enabled:   false, // opt-in
				Symbol:    "PEP",
				Algorithm: "scryptn",
				Stratum: StratumConf{
					Port: 3337,
					Vardiff: VardiffConf{
						MinDiff: 0.001, MaxDiff: 512,
						TargetMs: 30000, RetargetS: 60,
					},
				},
				Node: NodeConf{
					Host: "127.0.0.1", Port: 33873,
					User: "pepd", Password: "changeme",
				},
				Wallet:      "YOUR_PEP_WALLET",
				BlockReward: 50000,
			},
			"DGB": {
				Enabled:   false, // opt-in — SHA-256d primary chain; ~30 GB blockchain, safe for Pi
				Symbol:    "DGB",
				Algorithm: "sha256d",
				Stratum: StratumConf{
					Port: 3339,
					Vardiff: VardiffConf{
						MinDiff: 1, MaxDiff: 1048576,
						TargetMs: 30000, RetargetS: 60,
					},
				},
				Node: NodeConf{
					Host: "127.0.0.1", Port: 14022,
					User: "digibyted", Password: "changeme",
					ZmqPubHashBlock: "tcp://127.0.0.1:28337",
				},
				Wallet:      "YOUR_DGB_WALLET",
				BlockReward: 665,
			},
			"DGBS": {
				Enabled:   false, // opt-in — DGB Scrypt; shares digibyted node with DGB
				Symbol:    "DGBS",
				Algorithm: "scrypt",
				Stratum: StratumConf{
					Port: 3342,
					Vardiff: VardiffConf{
						// DGB Scrypt blocks every ~15s — use 5s share target to minimize stales
						MinDiff: 64, MaxDiff: 524288,
						TargetMs: 5000, RetargetS: 30,
					},
				},
				Node: NodeConf{
					Host: "127.0.0.1", Port: 14022,
					User: "digibyted", Password: "changeme",
					ZmqPubHashBlock: "tcp://127.0.0.1:28337",
				},
				Wallet:      "YOUR_DGB_WALLET",
				BlockReward: 665,
			},
		},
		Discord: DiscordConfig{
			Enabled:    true,
			WebhookURL: "https://discord.com/api/webhooks/YOUR/WEBHOOK",
			BotName:    "PiPool Bot",
			AvatarURL:  "",
			Alerts: DiscordAlerts{
				BlockFound:          true,
				MinerConnected:      true,
				MinerDisconnect:     false, // noisy — off by default
				HighTemp:            true,
				HashrateReport:      true,
				HashrateIntervalMin: 60,
				HashrateDropPct:     20,
				NodeUnreachable:     true,
			},
		},
		Dashboard: DashboardConfig{
			Enabled:       true,  // on by default — open HTTP on :8080
			Port:          8080,
			PushIntervalS: 15,
		},
		Logging: LoggingConfig{
			Level:  "info",
			ToFile: true,
			Path:   "/var/log/pipool/pipool.log",
		},
	}
}

// Load reads and parses a config file
func Load(path string) (*PoolConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg PoolConfig
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Save writes the config to disk as pretty JSON
func Save(cfg *PoolConfig, path string) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}

// EnabledCoins returns coins that are enabled, primary ones first
func (c *PoolConfig) EnabledCoins() []CoinConfig {
	var primaries, aux []CoinConfig
	for _, coin := range c.Coins {
		if !coin.Enabled {
			continue
		}
		if coin.MergeParent == "" {
			primaries = append(primaries, coin)
		} else {
			aux = append(aux, coin)
		}
	}
	return append(primaries, aux...)
}

// MergeChildren returns all AuxPoW children of a given parent symbol
func (c *PoolConfig) MergeChildren(parentSymbol string) []CoinConfig {
	var out []CoinConfig
	for _, coin := range c.Coins {
		if coin.Enabled && coin.MergeParent == parentSymbol {
			out = append(out, coin)
		}
	}
	return out
}

// WorkerTimeoutDuration converts config seconds to time.Duration
func (p *PoolSettings) WorkerTimeoutDuration() time.Duration {
	return time.Duration(p.WorkerTimeout) * time.Second
}

// ─── Quai Network config ──────────────────────────────────────────────────────

// QuaiConfig holds settings for Quai Network solo/pool mining integration.
type QuaiConfig struct {
	Enabled bool            `json:"enabled"`
	Node    QuaiNodeConf    `json:"node"`
	SHA256  QuaiStratumConf `json:"sha256"`  // for SHA-256d ASICs
	Scrypt  QuaiStratumConf `json:"scrypt"`  // for Scrypt ASICs
}

// QuaiNodeConf points to the go-quai zone node HTTP JSON-RPC endpoint.
type QuaiNodeConf struct {
	Host     string `json:"host"`      // e.g. "192.168.1.92"
	HTTPPort int    `json:"http_port"` // zone HTTP port, e.g. 9200 (zone-0-0)
}

// QuaiStratumConf configures one stratum listener (SHA-256 or Scrypt).
type QuaiStratumConf struct {
	Enabled    bool    `json:"enabled"`
	Port       int     `json:"port"`        // e.g. 3340 (SHA-256), 3341 (Scrypt)
	Wallet     string  `json:"wallet"`      // Quai wallet address (0x...) for block rewards
	MinDiff    float64 `json:"min_diff"`    // vardiff floor
	MaxDiff    float64 `json:"max_diff"`    // vardiff ceiling
	TargetTime float64 `json:"target_time"` // seconds per share (default 15)
}
