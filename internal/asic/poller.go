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
	case "hammer", "bitaxe", "generic":
		return pollBitaxeHammer(client, ip)
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

func pollBitaxeHammer(client *http.Client, ip string) (*Health, error) {
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
		// hashRate is GH/s for both Bitaxe and Hammer; convert to KH/s
		HashrateKHs: info.HashRate * 1_000_000,
	}
	h.TempC = info.Temp
	if info.Temp1 > h.TempC {
		h.TempC = info.Temp1
	}
	return h, nil
}

// ── Elphapex ─────────────────────────────────────────────────────────────────

type elphapexSummaryResp struct {
	Summary struct {
		Ghs5s   float64 `json:"ghs5s"`
		Ghsav   float64 `json:"ghsav"`
		Mhs5s   float64 `json:"mhs5s"`
		Mhsav   float64 `json:"mhsav"`
		Elapsed int64   `json:"elapsed"`
	} `json:"summary"`
	Devs []struct {
		Temp  float64 `json:"temp"`
		Temp1 float64 `json:"temp1"`
		Temp2 float64 `json:"temp2"`
		Temp3 float64 `json:"temp3"`
		Fan1  float64 `json:"fan1"`
		Fan2  float64 `json:"fan2"`
		Fan3  float64 `json:"fan3"`
		Fan4  float64 `json:"fan4"`
	} `json:"devs"`
}

func pollElphapex(client *http.Client, ip string) (*Health, error) {
	resp, err := client.Get("http://" + ip + "/cgi-bin/summary.cgi")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var info elphapexSummaryResp
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	h := &Health{FetchedAt: time.Now(), UptimeSec: info.Summary.Elapsed}
	switch {
	case info.Summary.Ghs5s > 0:
		h.HashrateKHs = info.Summary.Ghs5s * 1_000_000
	case info.Summary.Ghsav > 0:
		h.HashrateKHs = info.Summary.Ghsav * 1_000_000
	case info.Summary.Mhs5s > 0:
		h.HashrateKHs = info.Summary.Mhs5s * 1_000
	case info.Summary.Mhsav > 0:
		h.HashrateKHs = info.Summary.Mhsav * 1_000
	}
	maxTemp, maxFan := 0.0, 0.0
	for _, dev := range info.Devs {
		for _, t := range []float64{dev.Temp, dev.Temp1, dev.Temp2, dev.Temp3} {
			if t > maxTemp {
				maxTemp = t
			}
		}
		for _, f := range []float64{dev.Fan1, dev.Fan2, dev.Fan3, dev.Fan4} {
			if f > maxFan {
				maxFan = f
			}
		}
	}
	h.TempC = maxTemp
	h.FanRPM = int(maxFan)
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
