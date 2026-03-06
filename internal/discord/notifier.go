package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/dakota/pipool/internal/config"
)

// Colors for Discord embeds (decimal)
const (
	ColorGreen  = 0x00FF88  // block found
	ColorRed    = 0xFF3355  // crash / error
	ColorYellow = 0xFFD700  // warning (high temp, low hashrate)
	ColorBlue   = 0x00D4FF  // info (miner connect)
	ColorGray   = 0x4A6A85  // miner disconnect
	ColorOrange = 0xFF6B00  // hashrate report
)

// Notifier sends Discord webhook notifications
type Notifier struct {
	cfg      *config.DiscordConfig // pointer so dashboard alert-toggle changes are seen live
	httpCli  *http.Client
	lastSent map[string]time.Time
}

// NewNotifier creates a new Discord notifier.
// Pass &cfg.Discord so live edits (e.g. from the dashboard) are reflected immediately.
func NewNotifier(cfg *config.DiscordConfig) *Notifier {
	return &Notifier{
		cfg:      cfg,
		lastSent: make(map[string]time.Time),
		httpCli:  &http.Client{Timeout: 10 * time.Second},
	}
}

// ─── Webhook payload structs ──────────────────────────────────────────────────

type webhookPayload struct {
	Username  string  `json:"username,omitempty"`
	AvatarURL string  `json:"avatar_url,omitempty"`
	Embeds    []embed `json:"embeds"`
}

type embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color,omitempty"`
	Fields      []embedField `json:"fields,omitempty"`
	Footer      *embedFooter `json:"footer,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"` // ISO 8601
}

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type embedFooter struct {
	Text string `json:"text"`
}

// ─── Public notification methods ──────────────────────────────────────────────

// BlockFound sends a 🏆 block found alert
func (n *Notifier) BlockFound(coin, blockHash string, height int64, reward float64, workerName string) {
	if !n.cfg.Enabled || !n.cfg.Alerts.BlockFound {
		return
	}
	go n.send(webhookPayload{
		Username:  n.cfg.BotName,
		AvatarURL: n.cfg.AvatarURL,
		Embeds: []embed{{
			Title:       fmt.Sprintf("🏆 Block Found — %s!", coin),
			Description: fmt.Sprintf("**PiPool** has solved a block on the **%s** chain!", coin),
			Color:       ColorGreen,
			Fields: []embedField{
				{Name: "🪙 Coin", Value: coin, Inline: true},
				{Name: "⛏️ Found By", Value: workerName, Inline: true},
				{Name: "📦 Height", Value: fmt.Sprintf("%d", height), Inline: true},
				{Name: "💰 Reward", Value: fmt.Sprintf("%.4f %s", reward, coin), Inline: true},
				{Name: "🔗 Block Hash", Value: fmt.Sprintf("`%s`", truncateHash(blockHash)), Inline: false},
			},
			Footer:    &embedFooter{Text: "PiPool · Raspberry Pi 5 Solo Miner"},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

// MinerConnected sends a miner connect notification
func (n *Notifier) MinerConnected(coin, workerName, remoteAddr string, totalMiners int32) {
	if !n.cfg.Enabled || !n.cfg.Alerts.MinerConnected {
		return
	}
	// Throttle: max one connect alert per 30 seconds per coin to avoid spam on reconnects
	key := "connect:" + coin
	if time.Since(n.lastSent[key]) < 30*time.Second {
		return
	}
	n.lastSent[key] = time.Now()

	go n.send(webhookPayload{
		Username: n.cfg.BotName,
		Embeds: []embed{{
			Title: fmt.Sprintf("🔌 Miner Connected — %s", coin),
			Color: ColorBlue,
			Fields: []embedField{
				{Name: "👷 Worker", Value: workerName, Inline: true},
				{Name: "🌐 Address", Value: remoteAddr, Inline: true},
				{Name: "📊 Total Miners", Value: fmt.Sprintf("%d", totalMiners), Inline: true},
			},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: "PiPool · Raspberry Pi 5"},
		}},
	})
}

// MinerDisconnected sends a miner disconnect notification
func (n *Notifier) MinerDisconnected(coin, workerName string, totalMiners int32) {
	if !n.cfg.Enabled || !n.cfg.Alerts.MinerDisconnect {
		return
	}
	go n.send(webhookPayload{
		Username: n.cfg.BotName,
		Embeds: []embed{{
			Title: fmt.Sprintf("📴 Miner Disconnected — %s", coin),
			Color: ColorGray,
			Fields: []embedField{
				{Name: "👷 Worker", Value: workerName, Inline: true},
				{Name: "📊 Total Miners", Value: fmt.Sprintf("%d", totalMiners), Inline: true},
			},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: "PiPool · Raspberry Pi 5"},
		}},
	})
}

// HighTemp sends a temperature warning
func (n *Notifier) HighTemp(tempC float64, limitC int) {
	if !n.cfg.Enabled || !n.cfg.Alerts.HighTemp {
		return
	}
	// Throttle: max one temp alert per 10 minutes
	key := "temp"
	if time.Since(n.lastSent[key]) < 10*time.Minute {
		return
	}
	n.lastSent[key] = time.Now()

	go n.send(webhookPayload{
		Username: n.cfg.BotName,
		Embeds: []embed{{
			Title:       "🌡️ High Temperature Warning",
			Description: fmt.Sprintf("CPU temperature has reached **%.1f°C** (limit: %d°C). Mining may throttle.", tempC, limitC),
			Color:       ColorYellow,
			Fields: []embedField{
				{Name: "🌡️ Current Temp", Value: fmt.Sprintf("%.1f°C", tempC), Inline: true},
				{Name: "⚠️ Limit", Value: fmt.Sprintf("%d°C", limitC), Inline: true},
			},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: "PiPool · Raspberry Pi 5"},
		}},
	})
}

// HashrateReport sends a periodic hashrate summary
type HashrateReportData struct {
	TotalKHs    float64
	ActiveCoins []CoinStat
	Uptime      time.Duration
	BlocksFound uint64
	CPU         float64
	TempC       float64
	RAMUsedGB   float64
}

type CoinStat struct {
	Symbol      string
	HashrateKHs float64
	Miners      int32
	Blocks      uint64
}

func (n *Notifier) HashrateReport(data HashrateReportData) {
	if !n.cfg.Enabled || !n.cfg.Alerts.HashrateReport {
		return
	}

	fields := []embedField{
		{Name: "⚡ Total Hashrate", Value: formatHashrate(data.TotalKHs), Inline: true},
		{Name: "⏱️ Uptime", Value: formatDuration(data.Uptime), Inline: true},
		{Name: "🏆 Blocks Found", Value: fmt.Sprintf("%d", data.BlocksFound), Inline: true},
		{Name: "🖥️ CPU Usage", Value: fmt.Sprintf("%.1f%%", data.CPU), Inline: true},
		{Name: "🌡️ Temp", Value: fmt.Sprintf("%.1f°C", data.TempC), Inline: true},
		{Name: "💾 RAM Used", Value: fmt.Sprintf("%.2f GB", data.RAMUsedGB), Inline: true},
	}

	for _, coin := range data.ActiveCoins {
		fields = append(fields, embedField{
			Name:   fmt.Sprintf("🪙 %s", coin.Symbol),
			Value:  fmt.Sprintf("%s · %d miners · %d blocks", formatHashrate(coin.HashrateKHs), coin.Miners, coin.Blocks),
			Inline: false,
		})
	}

	go n.send(webhookPayload{
		Username: n.cfg.BotName,
		Embeds: []embed{{
			Title:     "📊 Mining Status Report",
			Color:     ColorOrange,
			Fields:    fields,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: "PiPool · Raspberry Pi 5 Solo Miner"},
		}},
	})
}

// NodeBackOnline sends an alert when a coin daemon comes back after being unreachable
func (n *Notifier) NodeBackOnline(coin string) {
	if !n.cfg.Enabled || !n.cfg.Alerts.NodeUnreachable {
		return
	}
	go n.send(webhookPayload{
		Username: n.cfg.BotName,
		Embeds: []embed{{
			Title:       fmt.Sprintf("✅ Node Back Online — %s", coin),
			Description: fmt.Sprintf("The **%s** daemon is reachable again. Mining resumed.", coin),
			Color:       ColorGreen,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Footer:      &embedFooter{Text: "PiPool · Raspberry Pi 5"},
		}},
	})
}

// NodeUnreachable sends an alert when a coin daemon cannot be reached
func (n *Notifier) NodeUnreachable(coin string, err error) {
	if !n.cfg.Enabled || !n.cfg.Alerts.NodeUnreachable {
		return
	}
	// Throttle: max one alert per 5 minutes per coin
	key := "node:" + coin
	if time.Since(n.lastSent[key]) < 5*time.Minute {
		return
	}
	n.lastSent[key] = time.Now()

	go n.send(webhookPayload{
		Username: n.cfg.BotName,
		Embeds: []embed{{
			Title:       fmt.Sprintf("💀 Node Unreachable — %s", coin),
			Description: fmt.Sprintf("Cannot reach the **%s** daemon.\n```%v```\nMining for this coin is paused until the node is back.", coin, err),
			Color:       ColorRed,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Footer:      &embedFooter{Text: "PiPool · Raspberry Pi 5"},
		}},
	})
}

// HashrateDropped sends an alert when hashrate drops significantly
func (n *Notifier) HashrateDropped(coin string, currentKHs, previousKHs float64, dropPct int) {
	if !n.cfg.Enabled || n.cfg.Alerts.HashrateDropPct <= 0 {
		return
	}
	key := "drop:" + coin
	if time.Since(n.lastSent[key]) < 15*time.Minute {
		return
	}
	n.lastSent[key] = time.Now()

	go n.send(webhookPayload{
		Username: n.cfg.BotName,
		Embeds: []embed{{
			Title:       fmt.Sprintf("📉 Hashrate Drop — %s", coin),
			Description: fmt.Sprintf("Hashrate dropped by **%d%%** on **%s**.", dropPct, coin),
			Color:       ColorYellow,
			Fields: []embedField{
				{Name: "Before", Value: formatHashrate(previousKHs), Inline: true},
				{Name: "Now", Value: formatHashrate(currentKHs), Inline: true},
			},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: "PiPool · Raspberry Pi 5"},
		}},
	})
}

// PoolStarted sends a notification when the pool boots up
func (n *Notifier) PoolStarted(coins []string) {
	if !n.cfg.Enabled {
		return
	}
	coinList := ""
	for _, c := range coins {
		coinList += fmt.Sprintf("• %s\n", c)
	}
	go n.send(webhookPayload{
		Username: n.cfg.BotName,
		Embeds: []embed{{
			Title:       "🚀 PiPool Started",
			Description: fmt.Sprintf("Solo mining pool is online on Raspberry Pi 5.\n\n**Active coins:**\n%s", coinList),
			Color:       ColorGreen,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Footer:      &embedFooter{Text: "PiPool · Raspberry Pi 5 Solo Miner"},
		}},
	})
}

// ─── Internal send ────────────────────────────────────────────────────────────

func (n *Notifier) send(payload webhookPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[discord] marshal error: %v", err)
		return
	}

	req, err := http.NewRequest("POST", n.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[discord] build request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.httpCli.Do(req)
	if err != nil {
		log.Printf("[discord] send error: %v", err)
		return
	}
	defer resp.Body.Close()

	// 204 No Content = success for Discord webhooks
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		log.Printf("[discord] unexpected status: %d", resp.StatusCode)
	}
}

// ─── Formatters ───────────────────────────────────────────────────────────────

func formatHashrate(khs float64) string {
	switch {
	case khs >= 1_000_000:
		return fmt.Sprintf("%.2f GH/s", khs/1_000_000)
	case khs >= 1_000:
		return fmt.Sprintf("%.2f MH/s", khs/1_000)
	default:
		return fmt.Sprintf("%.2f KH/s", khs)
	}
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

func truncateHash(h string) string {
	if len(h) <= 16 {
		return h
	}
	return h[:8] + "..." + h[len(h)-8:]
}
