package stratum

import (
	"strings"
)

// DeviceClass represents a category of mining hardware with known hashrate characteristics
type DeviceClass struct {
	Name          string
	Algorithm     string // "scrypt", "sha256d", "scryptn"
	StartDiff     float64
	MaxDiff       float64
	Description   string
	WattsEstimate float64 // estimated power consumption in watts (0 = unknown)
}

// deviceSignature maps substrings found in miner User-Agent / worker strings to a DeviceClass
type deviceSignature struct {
	pattern string // lowercase substring to match
	class   DeviceClass
}

// RouterTable is the full device signature table.
// Patterns are checked in order — first match wins.
// Hashrate estimates are conservative medians; vardiff will tune from there.
var RouterTable = []deviceSignature{

	// ── BitAxe / ESP-Miner family (ESP32-based open source ASIC boards) ─────
	// SHA-256d ~200–500 GH/s depending on chip
	{"bitaxeultra", DeviceClass{"BitAxe Ultra", "sha256d", 1024, 8192, "BitAxe Ultra BM1366 ~500 GH/s", 15}},
	{"bitaxegamma", DeviceClass{"BitAxe Gamma", "sha256d", 2048, 16384, "BitAxe Gamma BM1370 ~1.2 TH/s", 20}},
	{"bitaxe", DeviceClass{"BitAxe", "sha256d", 512, 4096, "BitAxe ESP32 ASIC ~200-500 GH/s", 15}},
	{"nerdoctaxe", DeviceClass{"NerdOCTAXE", "sha256d", 1024, 8192, "NerdOCTAXE 8-ASIC ESP32 board ~1-4 TH/s", 60}},
	{"octaxe", DeviceClass{"Octaxe", "sha256d", 1024, 8192, "Octaxe 8-ASIC ESP32 board ~1-4 TH/s", 60}},
	{"luckyminer", DeviceClass{"LuckyMiner", "sha256d", 512, 4096, "LuckyMiner BM1366 ~500 GH/s", 15}},
	{"nerdaxe", DeviceClass{"NerdAxe", "sha256d", 256, 2048, "NerdAxe ~200 GH/s", 10}},
	{"nerdqaxe", DeviceClass{"NerdQAxe", "sha256d", 1024, 8192, "NerdQAxe quad ~800 GH/s", 40}},
	{"esp-miner", DeviceClass{"ESP-Miner", "sha256d", 512, 4096, "ESP-Miner firmware (generic)", 15}},
	// ── Plexsource (Scrypt) ──────────────────────────────────────────────────
	{"volc/msbt", DeviceClass{"Plexsource PM120", "scrypt", 256, 4096, "Plexsource PM120 ~120 MH/s", 30}},
	{"plexsource", DeviceClass{"Plexsource", "scrypt", 256, 4096, "Plexsource Scrypt miner", 30}},

	// ── Antminer S-series (SHA-256d) ─────────────────────────────────────────
	{"antminer s9", DeviceClass{"Antminer S9", "sha256d", 8192, 65536, "Bitmain S9 ~14 TH/s", 1323}},
	{"antminer s17", DeviceClass{"Antminer S17", "sha256d", 32768, 131072, "Bitmain S17 ~56 TH/s", 2520}},
	{"antminer s19", DeviceClass{"Antminer S19", "sha256d", 65536, 524288, "Bitmain S19 ~95-110 TH/s", 3250}},
	{"antminer s19j", DeviceClass{"Antminer S19j Pro", "sha256d", 131072, 524288, "Bitmain S19j Pro ~104 TH/s", 3068}},
	{"antminer s19k", DeviceClass{"Antminer S19k Pro", "sha256d", 131072, 524288, "Bitmain S19k Pro ~120 TH/s", 2760}},
	{"antminer s19 pro+", DeviceClass{"Antminer S19 Pro+", "sha256d", 131072, 1048576, "Bitmain S19 Pro+ ~120 TH/s", 3400}},
	{"antminer s19xp", DeviceClass{"Antminer S19 XP", "sha256d", 196608, 1048576, "Bitmain S19 XP ~140 TH/s", 3010}},
	{"antminer s21", DeviceClass{"Antminer S21", "sha256d", 262144, 2097152, "Bitmain S21 ~200 TH/s", 3500}},
	{"antminer s21+", DeviceClass{"Antminer S21+", "sha256d", 393216, 2097152, "Bitmain S21+ ~280 TH/s", 4500}},
	{"antminer s21 pro", DeviceClass{"Antminer S21 Pro", "sha256d", 393216, 2097152, "Bitmain S21 Pro ~234 TH/s", 3551}},
	{"antminer s21 xp", DeviceClass{"Antminer S21 XP", "sha256d", 524288, 4194304, "Bitmain S21 XP ~270 TH/s", 3850}},

	// ── Antminer L-series (Scrypt) ────────────────────────────────────────────
	{"antminer l3+", DeviceClass{"Antminer L3+", "scrypt", 32, 512, "Bitmain L3+ ~504 MH/s", 800}},
	{"antminer l7", DeviceClass{"Antminer L7", "scrypt", 4096, 32768, "Bitmain L7 ~9.5 GH/s", 3425}},
	{"antminer l9", DeviceClass{"Antminer L9", "scrypt", 8192, 65536, "Bitmain L9 ~16 GH/s", 3360}},
	{"antminer l3", DeviceClass{"Antminer L3", "scrypt", 16, 256, "Bitmain L3 ~250 MH/s", 700}},

	// ── Whatsminer (SHA-256d) ─────────────────────────────────────────────────
	{"whatsminer m20", DeviceClass{"Whatsminer M20", "sha256d", 32768, 262144, "MicroBT M20 ~68 TH/s", 3360}},
	{"whatsminer m30", DeviceClass{"Whatsminer M30", "sha256d", 65536, 524288, "MicroBT M30 ~86-112 TH/s", 3268}},
	{"whatsminer m30s+", DeviceClass{"Whatsminer M30S+", "sha256d", 131072, 524288, "MicroBT M30S+ ~100-112 TH/s", 3400}},
	{"whatsminer m30s++", DeviceClass{"Whatsminer M30S++", "sha256d", 131072, 524288, "MicroBT M30S++ ~112 TH/s", 3472}},
	{"whatsminer m50", DeviceClass{"Whatsminer M50", "sha256d", 196608, 1048576, "MicroBT M50 ~126 TH/s", 3276}},
	{"whatsminer m50s", DeviceClass{"Whatsminer M50S", "sha256d", 262144, 2097152, "MicroBT M50S ~150 TH/s", 3600}},
	{"whatsminer m53", DeviceClass{"Whatsminer M53", "sha256d", 262144, 2097152, "MicroBT M53 ~226 TH/s", 6888}},
	{"whatsminer m60", DeviceClass{"Whatsminer M60", "sha256d", 196608, 1048576, "MicroBT M60 ~186 TH/s", 3344}},
	{"whatsminer m63", DeviceClass{"Whatsminer M63", "sha256d", 393216, 2097152, "MicroBT M63 ~390 TH/s", 6900}},

	// ── Avalon (SHA-256d) ────────────────────────────────────────────────────
	{"avalon 1166", DeviceClass{"Avalon 1166", "sha256d", 65536, 524288, "Canaan Avalon 1166 ~68 TH/s", 3400}},
	{"avalon 1246", DeviceClass{"Avalon 1246", "sha256d", 65536, 524288, "Canaan Avalon 1246 ~90 TH/s", 3420}},
	{"avalon 1346", DeviceClass{"Avalon 1346", "sha256d", 131072, 1048576, "Canaan Avalon 1346 ~120 TH/s", 3540}},
	{"avalon 1366", DeviceClass{"Avalon 1366", "sha256d", 131072, 1048576, "Canaan Avalon 1366 ~130 TH/s", 3530}},
	{"avalon mini 3", DeviceClass{"Avalon Mini 3", "sha256d", 4096, 32768, "Canaan Avalon Mini 3 ~37.5 TH/s", 1230}},
	{"avalon nano 3", DeviceClass{"Avalon Nano 3", "sha256d", 512, 4096, "Canaan Avalon Nano 3 ~4 TH/s", 140}},
	{"avalonminer", DeviceClass{"AvalonMiner", "sha256d", 32768, 262144, "Canaan AvalonMiner (generic)", 3000}},

	// ── Goldshell (Scrypt + SHA-256d) ─────────────────────────────────────────
	{"goldshell lt5", DeviceClass{"Goldshell LT5", "scrypt", 256, 2048, "Goldshell LT5 ~2.45 GH/s", 2080}},
	{"goldshell lt6", DeviceClass{"Goldshell LT6", "scrypt", 512, 4096, "Goldshell LT6 ~3.35 GH/s", 3200}},
	{"goldshell mini-doge", DeviceClass{"Goldshell Mini-DOGE", "scrypt", 64, 512, "Goldshell Mini-DOGE ~185 MH/s", 233}},
	{"goldshell mini-doge pro", DeviceClass{"Goldshell Mini-DOGE Pro", "scrypt", 128, 1024, "Goldshell Mini-DOGE Pro ~233 MH/s", 250}},
	{"goldshell hs", DeviceClass{"Goldshell HS", "sha256d", 8192, 65536, "Goldshell HS-series", 2000}},
	{"goldshell", DeviceClass{"Goldshell (generic)", "sha256d", 256, 2048, "Goldshell generic", 1500}},

	// ── Jasminer (SHA-256d) ──────────────────────────────────────────────────
	{"jasminer x4", DeviceClass{"Jasminer X4", "sha256d", 32768, 262144, "Jasminer X4 ~80 TH/s", 1040}},
	{"jasminer x16", DeviceClass{"Jasminer X16", "sha256d", 131072, 1048576, "Jasminer X16 ~130 TH/s", 1720}},

	// ── Elphapex (Scrypt) ─────────────────────────────────────────────────────
	// DG Home 1 ~2 GH/s → diff for 30s shares ≈ 64. StartDiff 64, MaxDiff 1024.
	{"elphapex dg home 1", DeviceClass{"Elphapex DG Home 1", "scrypt", 64, 1024, "Elphapex DG Home 1 ~2 GH/s", 1200}},
	// DG1 / DG1+ ~11 GH/s → diff for 30s shares ≈ 350. StartDiff 256, MaxDiff 4096.
	{"elphapex dg1+", DeviceClass{"Elphapex DG1+", "scrypt", 256, 4096, "Elphapex DG1+ ~11 GH/s", 3300}},
	{"elphapex dg1", DeviceClass{"Elphapex DG1", "scrypt", 256, 4096, "Elphapex DG1 ~11 GH/s", 3300}},
	{"elphapex", DeviceClass{"Elphapex", "scrypt", 64, 1024, "Elphapex (generic)", 1200}},

	// ── Hammer / iPollo / Innosilicon (Scrypt) ───────────────────────────────
	{"hammer d9", DeviceClass{"Hammer D9", "scrypt", 4096, 32768, "Hammer D9 Scrypt", 3068}},
	{"ipollo v1", DeviceClass{"iPollo V1", "sha256d", 16384, 131072, "iPollo V1 ~130 TH/s", 4320}},
	{"innosilicon a6+", DeviceClass{"Innosilicon A6+", "scrypt", 2048, 16384, "Innosilicon A6+ LTCMaster ~2.2 GH/s", 2100}},
	{"innosilicon a4+", DeviceClass{"Innosilicon A4+", "scrypt", 512, 4096, "Innosilicon A4+ ~620 MH/s", 750}},
	{"innosilicon", DeviceClass{"Innosilicon", "scrypt", 256, 2048, "Innosilicon (generic)", 1500}},

	// ── FutureBit (Scrypt) ───────────────────────────────────────────────────
	{"futurebit apollo", DeviceClass{"FutureBit Apollo", "scrypt", 128, 1024, "FutureBit Apollo ~100-150 MH/s", 250}},
	{"futurebit moonlander", DeviceClass{"FutureBit Moonlander", "scrypt", 4, 64, "FutureBit Moonlander 2 ~5 MH/s", 7}},

	// ── CPU miners (very low hashrate — scrypt or sha256d) ───────────────────
	{"cpuminer-opt", DeviceClass{"cpuminer-opt", "scrypt", 0.001, 1, "cpuminer-opt (CPU)", 65}},
	{"cpuminer", DeviceClass{"cpuminer", "scrypt", 0.001, 1, "cpuminer (CPU)", 65}},
	{"xmrig", DeviceClass{"XMRig", "sha256d", 0.001, 1, "XMRig (CPU)", 65}},
	{"bfgminer", DeviceClass{"BFGMiner", "sha256d", 1, 1024, "BFGMiner", 65}},
	{"cgminer", DeviceClass{"CGMiner", "sha256d", 1, 1024, "CGMiner", 65}},

	// ── ESP32 lottery miners ──────────────────────────────────────────────────
	{"esp32", DeviceClass{"ESP32 Miner", "sha256d", 0.001, 0.1, "ESP32 DIY lottery miner ~10-100 KH/s", 1}},
	{"public-pool", DeviceClass{"ESP32 (public-pool fw)", "sha256d", 0.001, 0.1, "ESP32 public-pool firmware", 1}},
	{"solo-miner", DeviceClass{"Solo Miner (generic)", "sha256d", 0.001, 1, "Generic solo miner firmware", 65}},

	// ── Generic / unknown fallback ────────────────────────────────────────────
	// Matched last — all other devices get conservative minimum
}

// RouteWorker classifies a miner based on its subscribe user-agent or worker name
// and returns the DeviceClass to use for initial difficulty assignment.
// Falls back to a safe default if no signature matches.
func RouteWorker(userAgent, workerName, algorithm string) DeviceClass {
	// Combine both fields for matching — some miners put model in worker name
	haystack := strings.ToLower(userAgent + " " + workerName)

	for _, sig := range RouterTable {
		if strings.Contains(haystack, sig.pattern) {
			// Only use if algorithm matches (or algorithm is unset/generic)
			if sig.class.Algorithm == algorithm || sig.class.Algorithm == "" {
				return sig.class
			}
		}
	}

	// Unknown device — return safe conservative default for this algorithm
	return defaultDevice(algorithm)
}

func defaultDevice(algorithm string) DeviceClass {
	switch algorithm {
	case "scrypt":
		return DeviceClass{"Unknown (Scrypt)", "scrypt", 0.001, 1024, "Unknown Scrypt miner", 100}
	case "sha256d":
		return DeviceClass{"Unknown (SHA-256d)", "sha256d", 1, 1048576, "Unknown SHA-256d miner", 100}
	case "scryptn":
		return DeviceClass{"Unknown (Scrypt-N)", "scryptn", 0.001, 512, "Unknown Scrypt-N miner", 65}
	default:
		return DeviceClass{"Unknown", "sha256d", 1, 1024, "Unknown miner", 65}
	}
}

// DeviceStats holds per-device routing stats for the dashboard / metrics
type DeviceStats struct {
	DeviceName string
	Count      int
	Algorithm  string
}
