package asic

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Health is the result of polling a miner's HTTP API.
type Health struct {
	TempC        float64   `json:"temp_c"`
	FanRPM       int       `json:"fan_rpm"`
	FanPct       int       `json:"fan_pct"`
	PowerW       float64   `json:"power_w"`
	HashrateKHs  float64   `json:"hashrate_khs"`
	UptimeSec    int64     `json:"uptime_sec"`
	PoolURL      string    `json:"pool_url"`
	FetchedAt    time.Time `json:"fetched_at"`
	Error        string    `json:"error,omitempty"`
}

// classify maps a RouterTable device name to a miner API type.
func classify(deviceName string) string {
	n := strings.ToLower(deviceName)
	if strings.Contains(n, "goldshell") {
		return "goldshell"
	}
	if strings.Contains(n, "elphapex") || strings.Contains(n, "dg home") || strings.Contains(n, "dghome") {
		return "elphapex"
	}
	if strings.Contains(n, "bitaxe") || strings.Contains(n, "nerdaxe") || strings.Contains(n, "nerdqaxe") {
		return "bitaxe"
	}
	if strings.Contains(n, "hammer") {
		return "hammer"
	}
	return "generic"
}

// Poll fetches health data from the miner at ip using the appropriate API for deviceName.
// ip should be just the IP without port (e.g. "192.168.1.100").
func Poll(ip, deviceName string, timeout time.Duration) (*Health, error) {
	client := &http.Client{Timeout: timeout}
	switch classify(deviceName) {
	case "bitaxe", "generic":
		return pollBitaxeHammer(client, ip, true) // hashRate field is GH/s
	case "hammer":
		return pollBitaxeHammer(client, ip, false) // hashRate field is MH/s
	case "goldshell":
		return pollGoldshell(client, ip)
	case "elphapex":
		return pollElphapex(client, ip)
	default:
		return nil, fmt.Errorf("no API defined for device %q", deviceName)
	}
}

// ── Hammer / Bitaxe ──────────────────────────────────────────────────────────

type hammerInfoResp struct {
	HashRate       float64 `json:"hashRate"`
	Power          float64 `json:"power"`
	Temp           float64 `json:"temp"`
	Temp1          float64 `json:"temp1"`
	Fanspeed       float64 `json:"fanspeed"`
	Fanrpm         float64 `json:"fanrpm"`
	UptimeSeconds  int64   `json:"uptimeSeconds"`
	StratumURL     string  `json:"stratumURL"`
	ASICModel      string  `json:"ASICModel"`
	DeviceModel    string  `json:"DeviceModel"`
}

// pollBitaxeHammer polls /api/system/info. isGHs=true when the device reports
// hashRate in GH/s (Bitaxe); false when it reports in MH/s (Hammer D9).
func pollBitaxeHammer(client *http.Client, ip string, isGHs bool) (*Health, error) {
	resp, err := client.Get("http://" + ip + "/api/system/info")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var info hammerInfoResp
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	if info.HashRate == 0 && info.ASICModel == "" && info.DeviceModel == "" {
		return nil, fmt.Errorf("empty /api/system/info response from %s", ip)
	}
	h := &Health{
		FetchedAt:   time.Now(),
		PowerW:      info.Power,
		UptimeSec:   info.UptimeSeconds,
		PoolURL:     info.StratumURL,
		FanPct:      int(info.Fanspeed),
		FanRPM:      int(info.Fanrpm),
		// Bitaxe reports hashRate in GH/s; Hammer reports in MH/s
		HashrateKHs: func() float64 {
			if isGHs {
				return info.HashRate * 1_000_000 // GH/s → KH/s
			}
			return info.HashRate * 1_000 // MH/s → KH/s
		}(),
	}
	h.TempC = info.Temp
	if info.Temp1 > h.TempC {
		h.TempC = info.Temp1
	}
	return h, nil
}

// ── Elphapex ─────────────────────────────────────────────────────────────────
// API: GET /cgi-bin/stats.cgi
// temp_chip values are in millidegrees (53875 = 53.875 °C); temp_pcb in whole degrees.
// No power field — caller uses static WattsEstimate from RouterTable.

type elphapexStatsResp struct {
	STATS []struct {
		Elapsed  int64   `json:"elapsed"`
		Rate15m  float64 `json:"rate_15m"`
		RateAvg  float64 `json:"rate_avg"`
		RateUnit string  `json:"rate_unit"`
		Fan      []json.RawMessage `json:"fan"`
		Chain    []struct {
			TempChip []json.RawMessage `json:"temp_chip"` // millidegrees as strings or numbers
			TempPCB  []float64         `json:"temp_pcb"`  // degrees C
		} `json:"chain"`
	} `json:"STATS"`
}

func pollElphapex(client *http.Client, ip string) (*Health, error) {
	resp, err := client.Get("http://" + ip + "/cgi-bin/stats.cgi")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var info elphapexStatsResp
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	if len(info.STATS) == 0 {
		return nil, fmt.Errorf("empty stats from elphapex %s", ip)
	}
	st := info.STATS[0]
	h := &Health{FetchedAt: time.Now(), UptimeSec: st.Elapsed}

	// Hashrate: prefer 15m average, fall back to overall average
	mhs := st.Rate15m
	if mhs == 0 {
		mhs = st.RateAvg
	}
	h.HashrateKHs = mhs * 1_000 // MH/s → KH/s

	// Temperature: max of all chip temps (millidegrees) and PCB temps (degrees)
	maxTemp := 0.0
	for _, chain := range st.Chain {
		for _, raw := range chain.TempChip {
			// temp_chip values come as either a quoted string ("53875") or number
			var fv float64
			if json.Unmarshal(raw, &fv) == nil && fv > 0 {
				if c := fv / 1000.0; c > maxTemp {
					maxTemp = c
				}
				continue
			}
			var sv string
			if json.Unmarshal(raw, &sv) == nil && sv != "" {
				var f float64
				if _, err := fmt.Sscanf(sv, "%f", &f); err == nil && f > 0 {
					if c := f / 1000.0; c > maxTemp {
						maxTemp = c
					}
				}
			}
		}
		for _, t := range chain.TempPCB {
			if t > maxTemp {
				maxTemp = t
			}
		}
	}
	h.TempC = maxTemp

	// Fan: values come as strings ("3300") or numbers
	maxFan := 0
	for _, raw := range st.Fan {
		var fv float64
		if json.Unmarshal(raw, &fv) == nil {
			if int(fv) > maxFan {
				maxFan = int(fv)
			}
			continue
		}
		var sv string
		if json.Unmarshal(raw, &sv) == nil {
			var f float64
			if _, err := fmt.Sscanf(sv, "%f", &f); err == nil {
				if int(f) > maxFan {
					maxFan = int(f)
				}
			}
		}
	}
	h.FanRPM = maxFan
	return h, nil
}

// ── Goldshell ─────────────────────────────────────────────────────────────────

type goldshellDevResp struct {
	Minfos []struct {
		Ginfo struct {
			AvHashrate float64 `json:"avHashrate"`
			Temp0      float64 `json:"temp0"`
			Temp1      float64 `json:"temp1"`
		} `json:"ginfo"`
	} `json:"minfos"`
	DEVS []struct {
		MHSav float64 `json:"MHS av"`
		GHSav float64 `json:"GHS av"`
		MHS5s float64 `json:"MHS 5s"`
		GHS5s float64 `json:"GHS 5s"`
	} `json:"DEVS"`
}

type goldshellStatusResp struct {
	Power float64 `json:"power"`
}

func pollGoldshell(client *http.Client, ip string) (*Health, error) {
	resp, err := client.Get("http://" + ip + "/mcb/cgminer")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var info goldshellDevResp
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	h := &Health{FetchedAt: time.Now()}
	maxTemp := 0.0
	if len(info.Minfos) > 0 {
		for _, m := range info.Minfos {
			h.HashrateKHs += m.Ginfo.AvHashrate * 1000 // MH/s → KH/s
			for _, t := range []float64{m.Ginfo.Temp0, m.Ginfo.Temp1} {
				if t > maxTemp {
					maxTemp = t
				}
			}
		}
	} else {
		for _, dev := range info.DEVS {
			switch {
			case dev.MHSav > 0:
				h.HashrateKHs += dev.MHSav * 1000
			case dev.GHSav > 0:
				h.HashrateKHs += dev.GHSav * 1_000_000
			case dev.MHS5s > 0:
				h.HashrateKHs += dev.MHS5s * 1000
			case dev.GHS5s > 0:
				h.HashrateKHs += dev.GHS5s * 1_000_000
			}
		}
	}
	h.TempC = maxTemp
	// Get power from /mcb/status
	sr, err := client.Get("http://" + ip + "/mcb/status")
	if err == nil {
		defer sr.Body.Close()
		sb, _ := io.ReadAll(sr.Body)
		var status goldshellStatusResp
		if json.Unmarshal(sb, &status) == nil {
			h.PowerW = status.Power
		}
	}
	return h, nil
}
