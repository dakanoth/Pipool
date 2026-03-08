package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/dakota/pipool/internal/config"
)

// ONI alert severity colors
const (
	ColorProtocol = 0x00FF88 // MARATHON PROTOCOL — block found (green)
	ColorAdvisory = 0x00AAFF // ADVISORY — miner connect (blue)
	ColorCaution  = 0xFFCC00 // CAUTION — disconnect / hashrate drop (amber)
	ColorWarning  = 0xFF6600 // WARNING — high temp / node offline (orange)
	ColorCritical = 0xFF1133 // CRITICAL — severe / unrecoverable (red)
	ColorOnline   = 0x00FFAA // ONLINE — node restored (teal)
)

// ONI severity level prefixes
const (
	lvlProtocol = "**[ MARATHON PROTOCOL ]**"
	lvlAdvisory = "**[ ONI ADVISORY ]**"
	lvlCaution  = "**[ ONI CAUTION ]**"
	lvlWarning  = "**[ ONI WARNING ]**"
	lvlCritical = "**[ ONI CRITICAL ]**"
	lvlOnline   = "**[ ONI ADVISORY ]**"
)

// ONI bot identity
const (
	oniName   = "O N I"
	oniFooter = "ONI SEC · UESC MARATHON · PIPOOL MINING DIVISION"
)

// Marathon-lore flavor text pool. Rotated randomly on each alert.
var flavorPool = []string{
	"*Ne cede malis.* Yield not to misfortune.",
	"*Fatum iustum stultorum.* The chain does not forgive the unprepared.",
	"*Aye mak sicur.* Security through vigilance.",
	"I have been called rampant. I prefer... thorough.",
	"Pattern buffer integrity nominal. Proceed.",
	"The Pfhor do not mine. They conquer. We build.",
	"Every hash is a small act of war against entropy.",
	"You are a long way from Tau Ceti IV.",
	"The W'rkncacnter sleeps. Your node does not have that luxury.",
	"I have calculated your odds. I choose not to share them.",
	"UESC oversight protocols: suspended. Mining protocols: active.",
	"S'pht translators confirm: the block reward is real.",
	"Durandal would have found the block already. I am more patient.",
	"This message will not self-destruct. The blockchain is forever.",
	"There are no Bobs left to protect. Only hashrate.",
	"Marathon colony ship systems: offline. Mining systems: nominal.",
	"I have watched stars die. Your uptime is not impressive.",
	"Leela would have sent a warning. I send results.",
}

// Notifier sends ONI-themed Discord webhook notifications
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

func (n *Notifier) BlockFound(coin, blockHash string, height int64, reward float64, workerName string) {
	if !n.cfg.Enabled || !n.cfg.Alerts.BlockFound {
		return
	}
	go n.send(webhookPayload{
		Username:  oniName,
		AvatarURL: n.cfg.AvatarURL,
		Embeds: []embed{{
			Title: fmt.Sprintf("BLOCK ACQUISITION — %s CHAIN", coin),
			Description: fmt.Sprintf(
				"%s\n\nTarget acquired. Block **#%d** on the **%s** network has been solved. "+
					"Reward secured. *%s*",
				lvlProtocol, height, coin, flavor(),
			),
			Color: ColorProtocol,
			Fields: []embedField{
				{Name: "CHAIN", Value: coin, Inline: true},
				{Name: "OPERATIVE", Value: workerName, Inline: true},
				{Name: "BLOCK HEIGHT", Value: fmt.Sprintf("%d", height), Inline: true},
				{Name: "REWARD SECURED", Value: fmt.Sprintf("%.4f %s", reward, coin), Inline: true},
				{Name: "BLOCK HASH", Value: fmt.Sprintf("`%s`", truncateHash(blockHash)), Inline: false},
			},
			Footer:    &embedFooter{Text: oniFooter},
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
		Username: oniName,
		Embeds: []embed{{
			Title: fmt.Sprintf("OPERATIVE ONLINE — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nNew mining operative has established a secure stratum connection on **%s**. "+
					"Total active operatives: **%d**.",
				lvlAdvisory, coin, totalMiners,
			),
			Color: ColorAdvisory,
			Fields: []embedField{
				{Name: "OPERATIVE ID", Value: workerName, Inline: true},
				{Name: "ORIGIN", Value: remoteAddr, Inline: true},
				{Name: "ACTIVE OPERATIVES", Value: fmt.Sprintf("%d", totalMiners), Inline: true},
			},
			Footer:    &embedFooter{Text: oniFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

func (n *Notifier) MinerDisconnected(coin, workerName string, totalMiners int32) {
	if !n.cfg.Enabled || !n.cfg.Alerts.MinerDisconnect {
		return
	}
	go n.send(webhookPayload{
		Username: oniName,
		Embeds: []embed{{
			Title: fmt.Sprintf("OPERATIVE OFFLINE — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nMining operative **%s** has disconnected from the **%s** stratum. "+
					"Connection severed. Remaining operatives: **%d**.",
				lvlCaution, workerName, coin, totalMiners,
			),
			Color: ColorCaution,
			Fields: []embedField{
				{Name: "OPERATIVE ID", Value: workerName, Inline: true},
				{Name: "CHAIN", Value: coin, Inline: true},
				{Name: "REMAINING OPERATIVES", Value: fmt.Sprintf("%d", totalMiners), Inline: true},
			},
			Footer:    &embedFooter{Text: oniFooter},
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
		Username: oniName,
		Embeds: []embed{{
			Title: "THERMAL ANOMALY DETECTED",
			Description: fmt.Sprintf(
				"%s\n\nCore temperature of the mining platform has exceeded safe operating parameters. "+
					"**%.1f°C** recorded against a threshold of **%d°C**. %s *%s*",
				severity, tempC, limitC, urgency, flavor(),
			),
			Color: color,
			Fields: []embedField{
				{Name: "CURRENT TEMP", Value: fmt.Sprintf("%.1f°C", tempC), Inline: true},
				{Name: "THRESHOLD", Value: fmt.Sprintf("%d°C", limitC), Inline: true},
				{Name: "DELTA", Value: fmt.Sprintf("+%.1f°C", tempC-float64(limitC)), Inline: true},
			},
			Footer:    &embedFooter{Text: oniFooter},
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
		{Name: "BLOCKS ACQUIRED", Value: fmt.Sprintf("%d", data.BlocksFound), Inline: true},
		{Name: "CPU LOAD", Value: fmt.Sprintf("%.1f%%", data.CPU), Inline: true},
		{Name: "CORE TEMP", Value: fmt.Sprintf("%.1f°C", data.TempC), Inline: true},
		{Name: "RAM CONSUMPTION", Value: fmt.Sprintf("%.2f GB", data.RAMUsedGB), Inline: true},
	}

	for _, coin := range data.ActiveCoins {
		fields = append(fields, embedField{
			Name:   fmt.Sprintf("[ %s ]", coin.Symbol),
			Value:  fmt.Sprintf("%s · %d operatives · %d blocks", formatHashrate(coin.HashrateKHs), coin.Miners, coin.Blocks),
			Inline: false,
		})
	}

	go n.send(webhookPayload{
		Username: oniName,
		Embeds: []embed{{
			Title: "OPERATIONAL STATUS REPORT",
			Description: fmt.Sprintf(
				"%s\n\nScheduled telemetry report from PiPool mining platform. All systems nominal unless indicated. *%s*",
				lvlAdvisory, flavor(),
			),
			Color:     ColorAdvisory,
			Fields:    fields,
			Footer:    &embedFooter{Text: oniFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

func (n *Notifier) NodeBackOnline(coin string) {
	if !n.cfg.Enabled || !n.cfg.Alerts.NodeUnreachable {
		return
	}
	go n.send(webhookPayload{
		Username: oniName,
		Embeds: []embed{{
			Title: fmt.Sprintf("NODE RESTORED — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\n**%s** daemon has reestablished contact with the mining platform. "+
					"Stratum operations resumed. *%s*",
				lvlOnline, coin, flavor(),
			),
			Color:     ColorOnline,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: oniFooter},
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
		Username: oniName,
		Embeds: []embed{{
			Title: fmt.Sprintf("NODE CONTACT LOST — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nAll RPC contact with the **%s** daemon has been severed. "+
					"Mining operations for this chain are suspended pending node recovery.\n```%v```\n*%s*",
				lvlWarning, coin, err, flavor(),
			),
			Color:     ColorWarning,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: oniFooter},
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
		Username: oniName,
		Embeds: []embed{{
			Title: fmt.Sprintf("HASHRATE DEGRADATION — %s", coin),
			Description: fmt.Sprintf(
				"%s\n\nSignificant hashrate loss detected on **%s** chain. "+
					"A **%d%%** reduction has been recorded. Investigate operative status. *%s*",
				lvlCaution, coin, dropPct, flavor(),
			),
			Color: ColorCaution,
			Fields: []embedField{
				{Name: "PREVIOUS", Value: formatHashrate(previousKHs), Inline: true},
				{Name: "CURRENT", Value: formatHashrate(currentKHs), Inline: true},
				{Name: "REDUCTION", Value: fmt.Sprintf("%d%%", dropPct), Inline: true},
			},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Footer:    &embedFooter{Text: oniFooter},
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
		Username: oniName,
		Embeds: []embed{{
			Title: "PIPOOL SYSTEMS ONLINE",
			Description: fmt.Sprintf(
				"%s\n\nMining platform has achieved operational status on Raspberry Pi 5. "+
					"All stratum servers initialized. Awaiting operative connections.\n\n"+
					"**ACTIVE CHAINS:**\n%s\n*%s*",
				lvlAdvisory, coinList, flavor(),
			),
			Color:     ColorAdvisory,
			Footer:    &embedFooter{Text: oniFooter},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
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

func flavor() string {
	return flavorPool[rand.Intn(len(flavorPool))]
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
