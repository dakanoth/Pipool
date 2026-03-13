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
}

type PoolSettings struct {
	Name           string `json:"name"`
	Host           string `json:"host"`            // bind address, e.g. "0.0.0.0"
	CoinbaseTag    string `json:"coinbase_tag"`    // e.g. "/PiPool/" — embedded in every block found
	MaxConnections int    `json:"max_connections"` // cap RAM usage
	WorkerTimeout  int    `json:"worker_timeout"`  // seconds before idle disconnect
	TempLimitC     int    `json:"temp_limit_c"`    // CPU temp throttle threshold
}

type CoinConfig struct {
	Enabled     bool        `json:"enabled"`
	Symbol      string      `json:"symbol"`
	Algorithm   string      `json:"algorithm"`    // "scrypt", "sha256d", "scryptn"
	MergeParent string      `json:"merge_parent"` // e.g. "LTC" for DOGE; "" if primary
	DataDir     string      `json:"datadir"`      // blockchain storage path, e.g. /mnt/external/litecoin
	Stratum     StratumConf `json:"stratum"`
	Node        NodeConf    `json:"node"`
	Wallet      string      `json:"wallet"`
	BlockReward float64     `json:"block_reward"`
	TxFeeTarget float64     `json:"tx_fee_target"`
}

type StratumConf struct {
	Port    int         `json:"port"`
	Vardiff VardiffConf `json:"vardiff"`
	TLS     TLSConf     `json:"tls"`
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
			Name:           "PiPool",
			Host:           "0.0.0.0",
			CoinbaseTag:    "/PiPool/",
			MaxConnections: 64,
			WorkerTimeout:  120,
			TempLimitC:     75,
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
			"LCC": {
				Enabled:     false, // opt-in — SHA-256d merge mined with BTC
				Symbol:      "LCC",
				Algorithm:   "sha256d",
				MergeParent: "BTC",
				Stratum: StratumConf{
					Port: 3338,
					Vardiff: VardiffConf{
						MinDiff: 1, MaxDiff: 1048576,
						TargetMs: 30000, RetargetS: 60,
					},
				},
				Node: NodeConf{
					Host: "127.0.0.1", Port: 62457,
					User: "lccd", Password: "changeme",
					ZmqPubHashBlock: "tcp://127.0.0.1:28336",
				},
				Wallet:      "YOUR_LCC_WALLET",
				BlockReward: 250,
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
			PushIntervalS: 5,
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
