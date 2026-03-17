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

// Alert severity colors
const (
	ColorAlert    = 0x00FF88 // ALERT — block found (green)
	ColorAdvisory = 0x00AAFF // ADVISORY — miner connect (blue)
	ColorCaution  = 0xFFCC00 // CAUTION — disconnect / hashrate drop (amber)
	ColorWarning  = 0xFF6600 // WARNING — high temp / node offline (orange)
	ColorCritical = 0xFF1133 // CRITICAL — severe / unrecoverable (red)
	ColorOnline   = 0x00FFAA // ONLINE — node restored (teal)
)

// Severity level prefixes
const (
	lvlAlert    = "**[ ALERT ]**"
	lvlAdvisory = "**[ ADVISORY ]**"
	lvlCaution  = "**[ CAUTION ]**"
	lvlWarning  = "**[ WARNING ]**"
	lvlCritical = "**[ CRITICAL ]**"
	lvlOnline   = "**[ ADVISORY ]**"
)

// Bot identity
const (
	botName   = "PIPOOL"
	botFooter = "PIPOOL · STRATUM MINING PLATFORM"
)

// Notifier sends Discord webhook notifications
type Notifier struct {
	cfg      *config.DiscordConfig
	httpCli  *http.Client
	lastSent map[string]time.Time
}

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
	Timestamp   string       `json:"timestamp,omitempty"`
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

func (n *Notifier) BlockFound(coin, blockHash string, height int64, reward, luck float64, workerName string) {
	if !n.cfg.Enabled || !n.cfg.Alerts.BlockFound {
		return
	}

	luckStr := "N/A"
	if luck >= 0 {
		luckStr = fmt.Sprintf("%.1f%%", luck)
	}

	go n.send(webhookPayload{
		Username:  botName,
		AvatarURL: n.cfg.AvatarURL,
		Embeds: []embed{{
			Title: fmt.Sprintf("BLOCK FOUND — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nBlock **#%d** on the **%s** network has been solved.",
				lvlAlert, height, coin,
			),
			Color: ColorAlert,
			Fields: []embedField{
				{Name: "CHAIN", Value: coin, Inline: true},
				{Name: "WORKER", Value: workerName, Inline: true},
				{Name: "BLOCK HEIGHT", Value: fmt.Sprintf("%d", height), Inline: true},
				{Name: "REWARD", Value: fmt.Sprintf("%.4f %s", reward, coin), Inline: true},
				{Name: "LUCK", Value: luckStr, Inline: true},
				{Name: "BLOCK HASH", Value: fmt.Sprintf("`%s`", truncateHash(blockHash)), Inline: false},
			},
			Footer:    &embedFooter{Text: botFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

func (n *Notifier) BlockMatured(coin, blockHash string, height int64, reward string) {
	if !n.cfg.Enabled || !n.cfg.Alerts.BlockFound {
		return
	}
	go n.send(webhookPayload{
		Username:  botName,
		AvatarURL: n.cfg.AvatarURL,
		Embeds: []embed{{
			Title: fmt.Sprintf("BLOCK MATURED — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nBlock **#%d** on **%s** has reached maturity. Reward is now **spendable**.",
				lvlAlert, height, coin,
			),
			Color: ColorAlert,
			Fields: []embedField{
				{Name: "CHAIN", Value: coin, Inline: true},
				{Name: "BLOCK HEIGHT", Value: fmt.Sprintf("%d", height), Inline: true},
				{Name: "REWARD", Value: reward, Inline: true},
				{Name: "BLOCK HASH", Value: fmt.Sprintf("`%s`", truncateHash(blockHash)), Inline: false},
			},
			Footer:    &embedFooter{Text: botFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

func (n *Notifier) MinerConnected(coin, workerName, remoteAddr string, totalMiners int32) {
	if !n.cfg.Enabled || !n.cfg.Alerts.MinerConnected {
		return
	}
	key := "connect:" + coin
	if time.Since(n.lastSent[key]) < 30*time.Second {
		return
	}
	n.lastSent[key] = time.Now()

	go n.send(webhookPayload{
		Username: botName,
		Embeds: []embed{{
			Title: fmt.Sprintf("MINER CONNECTED — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nNew miner connected on **%s**. Active miners: **%d**.",
				lvlAdvisory, coin, totalMiners,
			),
			Color: ColorAdvisory,
			Fields: []embedField{
				{Name: "WORKER", Value: workerName, Inline: true},
				{Name: "ADDRESS", Value: remoteAddr, Inline: true},
				{Name: "ACTIVE MINERS", Value: fmt.Sprintf("%d", totalMiners), Inline: true},
			},
			Footer:    &embedFooter{Text: botFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

func (n *Notifier) MinerDisconnected(coin, workerName string, totalMiners int32) {
	if !n.cfg.Enabled || !n.cfg.Alerts.MinerDisconnect {
		return
	}
	go n.send(webhookPayload{
		Username: botName,
		Embeds: []embed{{
			Title: fmt.Sprintf("MINER DISCONNECTED — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nMiner **%s** disconnected from **%s**. Remaining miners: **%d**.",
				lvlCaution, workerName, coin, totalMiners,
			),
			Color: ColorCaution,
			Fields: []embedField{
				{Name: "WORKER", Value: workerName, Inline: true},
				{Name: "CHAIN", Value: coin, Inline: true},
				{Name: "REMAINING MINERS", Value: fmt.Sprintf("%d", totalMiners), Inline: true},
			},
			Footer:    &embedFooter{Text: botFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

func (n *Notifier) HighTemp(tempC float64, limitC int) {
	if !n.cfg.Enabled || !n.cfg.Alerts.HighTemp {
		return
	}
	key := "temp"
	if time.Since(n.lastSent[key]) < 10*time.Minute {
		return
	}
	n.lastSent[key] = time.Now()

	severity := lvlWarning
	color := ColorWarning
	urgency := "Thermal throttling imminent."
	if tempC >= float64(limitC) {
		severity = lvlCritical
		color = ColorCritical
		urgency = "THROTTLING ACTIVE. Mining efficiency compromised."
	}

	go n.send(webhookPayload{
		Username: botName,
		Embeds: []embed{{
			Title: "HIGH TEMPERATURE DETECTED",
			Description: fmt.Sprintf(
				"%s\n\nCore temperature has exceeded safe operating parameters. "+
					"**%.1f°C** recorded against a threshold of **%d°C**. %s",
				severity, tempC, limitC, urgency,
			),
			Color: color,
			Fields: []embedField{
				{Name: "CURRENT TEMP", Value: fmt.Sprintf("%.1f°C", tempC), Inline: true},
				{Name: "THRESHOLD", Value: fmt.Sprintf("%d°C", limitC), Inline: true},
				{Name: "DELTA", Value: fmt.Sprintf("+%.1f°C", tempC-float64(limitC)), Inline: true},
			},
			Footer:    &embedFooter{Text: botFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

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
		{Name: "TOTAL HASHRATE", Value: formatHashrate(data.TotalKHs), Inline: true},
		{Name: "UPTIME", Value: formatDuration(data.Uptime), Inline: true},
		{Name: "BLOCKS FOUND", Value: fmt.Sprintf("%d", data.BlocksFound), Inline: true},
		{Name: "CPU LOAD", Value: fmt.Sprintf("%.1f%%", data.CPU), Inline: true},
		{Name: "CORE TEMP", Value: fmt.Sprintf("%.1f°C", data.TempC), Inline: true},
		{Name: "RAM USED", Value: fmt.Sprintf("%.2f GB", data.RAMUsedGB), Inline: true},
	}

	for _, coin := range data.ActiveCoins {
		fields = append(fields, embedField{
			Name:   fmt.Sprintf("[ %s ]", coin.Symbol),
			Value:  fmt.Sprintf("%s · %d miners · %d blocks", formatHashrate(coin.HashrateKHs), coin.Miners, coin.Blocks),
			Inline: false,
		})
	}

	go n.send(webhookPayload{
		Username: botName,
		Embeds: []embed{{
			Title: "STATUS REPORT",
			Description: fmt.Sprintf(
				"%s\n\nScheduled telemetry report. All systems nominal unless indicated.",
				lvlAdvisory,
			),
			Color:     ColorAdvisory,
			Fields:    fields,
			Footer:    &embedFooter{Text: botFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

func (n *Notifier) NodeBackOnline(coin string) {
	if !n.cfg.Enabled || !n.cfg.Alerts.NodeUnreachable {
		return
	}
	go n.send(webhookPayload{
		Username: botName,
		Embeds: []embed{{
			Title: fmt.Sprintf("NODE RESTORED — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\n**%s** daemon has reconnected. Stratum operations resumed.",
				lvlOnline, coin,
			),
			Color:     ColorOnline,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: botFooter},
		}},
	})
}

func (n *Notifier) DaemonRestarted(coin, service string) {
	if !n.cfg.Enabled || !n.cfg.Alerts.NodeUnreachable {
		return
	}
	go n.send(webhookPayload{
		Username: botName,
		Embeds: []embed{{
			Title: fmt.Sprintf("WATCHDOG RESTART — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\n**%s** daemon was unreachable. Watchdog issued `systemctl restart %s`.",
				lvlWarning, coin, service,
			),
			Color: ColorWarning,
			Fields: []embedField{
				{Name: "CHAIN", Value: coin, Inline: true},
				{Name: "SERVICE", Value: service, Inline: true},
			},
			Footer:    &embedFooter{Text: botFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

func (n *Notifier) NodeUnreachable(coin string, err error) {
	if !n.cfg.Enabled || !n.cfg.Alerts.NodeUnreachable {
		return
	}
	key := "node:" + coin
	if time.Since(n.lastSent[key]) < 5*time.Minute {
		return
	}
	n.lastSent[key] = time.Now()

	go n.send(webhookPayload{
		Username: botName,
		Embeds: []embed{{
			Title: fmt.Sprintf("NODE UNREACHABLE — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nRPC contact with the **%s** daemon has been lost. "+
					"Mining on this chain is suspended pending node recovery.\n```%v```",
				lvlWarning, coin, err,
			),
			Color:     ColorWarning,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: botFooter},
		}},
	})
}

func (n *Notifier) WorkerStale(coin, workerName string, silentMin int) {
	if !n.cfg.Enabled {
		return
	}
	key := "stale:" + coin + ":" + workerName
	if time.Since(n.lastSent[key]) < 30*time.Minute {
		return
	}
	n.lastSent[key] = time.Now()

	go n.send(webhookPayload{
		Username: botName,
		Embeds: []embed{{
			Title: fmt.Sprintf("WORKER SILENT — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nMiner **%s** has not submitted a share in **%d minutes** on **%s**. "+
					"It may be stalled, overheated, or disconnected without a TCP RST.",
				lvlCaution, workerName, silentMin, coin,
			),
			Color: ColorCaution,
			Fields: []embedField{
				{Name: "WORKER", Value: workerName, Inline: true},
				{Name: "CHAIN", Value: coin, Inline: true},
				{Name: "SILENT FOR", Value: fmt.Sprintf("%d min", silentMin), Inline: true},
			},
			Footer:    &embedFooter{Text: botFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

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
		Username: botName,
		Embeds: []embed{{
			Title: fmt.Sprintf("HASHRATE DROP — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nHashrate on **%s** has dropped by **%d%%**. Check miner status.",
				lvlCaution, coin, dropPct,
			),
			Color: ColorCaution,
			Fields: []embedField{
				{Name: "PREVIOUS", Value: formatHashrate(previousKHs), Inline: true},
				{Name: "CURRENT", Value: formatHashrate(currentKHs), Inline: true},
				{Name: "DROP", Value: fmt.Sprintf("%d%%", dropPct), Inline: true},
			},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: botFooter},
		}},
	})
}

func (n *Notifier) PoolStarted(coins []string) {
	if !n.cfg.Enabled {
		return
	}
	coinList := ""
	for _, c := range coins {
		coinList += fmt.Sprintf("• **%s**\n", c)
	}
	go n.send(webhookPayload{
		Username: botName,
		Embeds: []embed{{
			Title: "PIPOOL ONLINE",
			Description: fmt.Sprintf(
				"%s\n\nMining platform is online. All stratum servers initialized.\n\n"+
					"**ACTIVE CHAINS:**\n%s",
				lvlAdvisory, coinList,
			),
			Color:     ColorAdvisory,
			Footer:    &embedFooter{Text: botFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

func (n *Notifier) StaleKick(coin, workerName string, kickCount int) {
	if !n.cfg.Enabled || n.cfg.Alerts.StaleKickAlertCount <= 0 {
		return
	}
	if kickCount < n.cfg.Alerts.StaleKickAlertCount {
		return
	}
	go n.send(webhookPayload{
		Username:  n.cfg.BotName,
		AvatarURL: n.cfg.AvatarURL,
		Embeds: []embed{{
			Title:       "Worker Stale-Kick Loop",
			Description: fmt.Sprintf("**%s** on **%s** has been kicked **%d times** in the last hour for submitting stale shares (firmware ignoring clean_jobs).", workerName, coin, kickCount),
			Color:       0xFF6600,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

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
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		log.Printf("[discord] unexpected status: %d", resp.StatusCode)
	}
}

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
