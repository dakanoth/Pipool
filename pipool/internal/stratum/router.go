package stratum

import (
	"strings"
)

// DeviceClass represents a category of mining hardware with known hashrate characteristics
type DeviceClass struct {
	Name        string
	Algorithm   string // "scrypt", "sha256d", "scryptn"
	StartDiff   float64
	MaxDiff     float64
	Description string
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

	// ── BitAxe family (ESP32-based open source ASIC boards) ─────────────────
	// SHA-256d ~200–500 GH/s depending on chip
	{"bitaxe", DeviceClass{"BitAxe", "sha256d", 512, 4096, "BitAxe ESP32 ASIC ~200-500 GH/s"}},
	{"bitaxeultra", DeviceClass{"BitAxe Ultra", "sha256d", 1024, 8192, "BitAxe Ultra BM1366 ~500 GH/s"}},
	{"bitaxegamma", DeviceClass{"BitAxe Gamma", "sha256d", 2048, 16384, "BitAxe Gamma BM1370 ~1.2 TH/s"}},
	{"nerdaxe", DeviceClass{"NerdAxe", "sha256d", 256, 2048, "NerdAxe ~200 GH/s"}},
	{"nerdqaxe", DeviceClass{"NerdQAxe", "sha256d", 1024, 8192, "NerdQAxe quad ~800 GH/s"}},

	// ── Antminer S-series (SHA-256d) ─────────────────────────────────────────
	{"antminer s9", DeviceClass{"Antminer S9", "sha256d", 8192, 65536, "Bitmain S9 ~14 TH/s"}},
	{"antminer s17", DeviceClass{"Antminer S17", "sha256d", 32768, 131072, "Bitmain S17 ~56 TH/s"}},
	{"antminer s19", DeviceClass{"Antminer S19", "sha256d", 65536, 524288, "Bitmain S19 ~95-110 TH/s"}},
	{"antminer s19j", DeviceClass{"Antminer S19j Pro", "sha256d", 131072, 524288, "Bitmain S19j Pro ~104 TH/s"}},
	{"antminer s19k", DeviceClass{"Antminer S19k Pro", "sha256d", 131072, 524288, "Bitmain S19k Pro ~120 TH/s"}},
	{"antminer s19 pro+", DeviceClass{"Antminer S19 Pro+", "sha256d", 131072, 1048576, "Bitmain S19 Pro+ ~120 TH/s"}},
	{"antminer s19xp", DeviceClass{"Antminer S19 XP", "sha256d", 196608, 1048576, "Bitmain S19 XP ~140 TH/s"}},
	{"antminer s21", DeviceClass{"Antminer S21", "sha256d", 262144, 2097152, "Bitmain S21 ~200 TH/s"}},
	{"antminer s21+", DeviceClass{"Antminer S21+", "sha256d", 393216, 2097152, "Bitmain S21+ ~280 TH/s"}},
	{"antminer s21 pro", DeviceClass{"Antminer S21 Pro", "sha256d", 393216, 2097152, "Bitmain S21 Pro ~234 TH/s"}},
	{"antminer s21 xp", DeviceClass{"Antminer S21 XP", "sha256d", 524288, 4194304, "Bitmain S21 XP ~270 TH/s"}},

	// ── Antminer L-series (Scrypt) ────────────────────────────────────────────
	{"antminer l3+", DeviceClass{"Antminer L3+", "scrypt", 32, 512, "Bitmain L3+ ~504 MH/s"}},
	{"antminer l7", DeviceClass{"Antminer L7", "scrypt", 4096, 32768, "Bitmain L7 ~9.5 GH/s"}},
	{"antminer l9", DeviceClass{"Antminer L9", "scrypt", 8192, 65536, "Bitmain L9 ~16 GH/s"}},
	{"antminer l3", DeviceClass{"Antminer L3", "scrypt", 16, 256, "Bitmain L3 ~250 MH/s"}},

	// ── Whatsminer (SHA-256d) ─────────────────────────────────────────────────
	{"whatsminer m20", DeviceClass{"Whatsminer M20", "sha256d", 32768, 262144, "MicroBT M20 ~68 TH/s"}},
	{"whatsminer m30", DeviceClass{"Whatsminer M30", "sha256d", 65536, 524288, "MicroBT M30 ~86-112 TH/s"}},
	{"whatsminer m30s+", DeviceClass{"Whatsminer M30S+", "sha256d", 131072, 524288, "MicroBT M30S+ ~100-112 TH/s"}},
	{"whatsminer m30s++", DeviceClass{"Whatsminer M30S++", "sha256d", 131072, 524288, "MicroBT M30S++ ~112 TH/s"}},
	{"whatsminer m50", DeviceClass{"Whatsminer M50", "sha256d", 196608, 1048576, "MicroBT M50 ~126 TH/s"}},
	{"whatsminer m50s", DeviceClass{"Whatsminer M50S", "sha256d", 262144, 2097152, "MicroBT M50S ~150 TH/s"}},
	{"whatsminer m53", DeviceClass{"Whatsminer M53", "sha256d", 262144, 2097152, "MicroBT M53 ~226 TH/s"}},
	{"whatsminer m60", DeviceClass{"Whatsminer M60", "sha256d", 196608, 1048576, "MicroBT M60 ~186 TH/s"}},
	{"whatsminer m63", DeviceClass{"Whatsminer M63", "sha256d", 393216, 2097152, "MicroBT M63 ~390 TH/s"}},

	// ── Avalon (SHA-256d) ────────────────────────────────────────────────────
	{"avalon 1166", DeviceClass{"Avalon 1166", "sha256d", 65536, 524288, "Canaan Avalon 1166 ~68 TH/s"}},
	{"avalon 1246", DeviceClass{"Avalon 1246", "sha256d", 65536, 524288, "Canaan Avalon 1246 ~90 TH/s"}},
	{"avalon 1346", DeviceClass{"Avalon 1346", "sha256d", 131072, 1048576, "Canaan Avalon 1346 ~120 TH/s"}},
	{"avalon 1366", DeviceClass{"Avalon 1366", "sha256d", 131072, 1048576, "Canaan Avalon 1366 ~130 TH/s"}},
	{"avalon mini 3", DeviceClass{"Avalon Mini 3", "sha256d", 4096, 32768, "Canaan Avalon Mini 3 ~37.5 TH/s"}},
	{"avalon nano 3", DeviceClass{"Avalon Nano 3", "sha256d", 512, 4096, "Canaan Avalon Nano 3 ~4 TH/s"}},
	{"avalonminer", DeviceClass{"AvalonMiner", "sha256d", 32768, 262144, "Canaan AvalonMiner (generic)"}},

	// ── Goldshell (Scrypt + SHA-256d) ─────────────────────────────────────────
	{"goldshell lt5", DeviceClass{"Goldshell LT5", "scrypt", 256, 2048, "Goldshell LT5 ~2.45 GH/s"}},
	{"goldshell lt6", DeviceClass{"Goldshell LT6", "scrypt", 512, 4096, "Goldshell LT6 ~3.35 GH/s"}},
	{"goldshell mini-doge", DeviceClass{"Goldshell Mini-DOGE", "scrypt", 64, 512, "Goldshell Mini-DOGE ~185 MH/s"}},
	{"goldshell mini-doge pro", DeviceClass{"Goldshell Mini-DOGE Pro", "scrypt", 128, 1024, "Goldshell Mini-DOGE Pro ~233 MH/s"}},
	{"goldshell hs", DeviceClass{"Goldshell HS", "sha256d", 8192, 65536, "Goldshell HS-series"}},
	{"goldshell", DeviceClass{"Goldshell (generic)", "sha256d", 256, 2048, "Goldshell generic"}},

	// ── Jasminer (SHA-256d) ──────────────────────────────────────────────────
	{"jasminer x4", DeviceClass{"Jasminer X4", "sha256d", 32768, 262144, "Jasminer X4 ~80 TH/s"}},
	{"jasminer x16", DeviceClass{"Jasminer X16", "sha256d", 131072, 1048576, "Jasminer X16 ~130 TH/s"}},

	// ── Elphapex (Scrypt) ─────────────────────────────────────────────────────
	{"elphapex dg home 1", DeviceClass{"Elphapex DG Home 1", "scrypt", 512, 4096, "Elphapex DG Home 1 ~2 GH/s"}},
	{"elphapex dg1+", DeviceClass{"Elphapex DG1+", "scrypt", 2048, 16384, "Elphapex DG1+ ~11 GH/s"}},
	{"elphapex dg1", DeviceClass{"Elphapex DG1", "scrypt", 2048, 16384, "Elphapex DG1 ~11 GH/s"}},
	{"elphapex", DeviceClass{"Elphapex", "scrypt", 512, 4096, "Elphapex (generic)"}},

	// ── Hammer / iPollo / Innosilicon (Scrypt) ───────────────────────────────
	{"hammer d9", DeviceClass{"Hammer D9", "scrypt", 4096, 32768, "Hammer D9 Scrypt"}},
	{"ipollo v1", DeviceClass{"iPollo V1", "sha256d", 16384, 131072, "iPollo V1 ~130 TH/s"}},
	{"innosilicon a6+", DeviceClass{"Innosilicon A6+", "scrypt", 2048, 16384, "Innosilicon A6+ LTCMaster ~2.2 GH/s"}},
	{"innosilicon a4+", DeviceClass{"Innosilicon A4+", "scrypt", 512, 4096, "Innosilicon A4+ ~620 MH/s"}},
	{"innosilicon", DeviceClass{"Innosilicon", "scrypt", 256, 2048, "Innosilicon (generic)"}},

	// ── FutureBit (Scrypt) ───────────────────────────────────────────────────
	{"futurebit apollo", DeviceClass{"FutureBit Apollo", "scrypt", 128, 1024, "FutureBit Apollo ~100-150 MH/s"}},
	{"futurebit moonlander", DeviceClass{"FutureBit Moonlander", "scrypt", 4, 64, "FutureBit Moonlander 2 ~5 MH/s"}},

	// ── CPU miners (very low hashrate — scrypt or sha256d) ───────────────────
	{"cpuminer-opt", DeviceClass{"cpuminer-opt", "scrypt", 0.001, 1, "cpuminer-opt (CPU)"}},
	{"cpuminer", DeviceClass{"cpuminer", "scrypt", 0.001, 1, "cpuminer (CPU)"}},
	{"xmrig", DeviceClass{"XMRig", "sha256d", 0.001, 1, "XMRig (CPU)"}},
	{"bfgminer", DeviceClass{"BFGMiner", "sha256d", 1, 1024, "BFGMiner"}},
	{"cgminer", DeviceClass{"CGMiner", "sha256d", 1, 1024, "CGMiner"}},

	// ── ESP32 lottery miners ──────────────────────────────────────────────────
	{"esp32", DeviceClass{"ESP32 Miner", "sha256d", 0.001, 0.1, "ESP32 DIY lottery miner ~10-100 KH/s"}},
	{"public-pool", DeviceClass{"ESP32 (public-pool fw)", "sha256d", 0.001, 0.1, "ESP32 public-pool firmware"}},
	{"solo-miner", DeviceClass{"Solo Miner (generic)", "sha256d", 0.001, 1, "Generic solo miner firmware"}},

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
		return DeviceClass{"Unknown (Scrypt)", "scrypt", 0.001, 1024, "Unknown Scrypt miner"}
	case "sha256d":
		return DeviceClass{"Unknown (SHA-256d)", "sha256d", 1, 1048576, "Unknown SHA-256d miner"}
	case "scryptn":
		return DeviceClass{"Unknown (Scrypt-N)", "scryptn", 0.001, 512, "Unknown Scrypt-N miner"}
	default:
		return DeviceClass{"Unknown", "sha256d", 1, 1024, "Unknown miner"}
	}
}

// DeviceStats holds per-device routing stats for the dashboard / metrics
type DeviceStats struct {
	DeviceName string
	Count      int
	Algorithm  string
}
