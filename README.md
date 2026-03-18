# ⛏️ PiPool

**A lightweight, original solo mining pool server built in Go for the Raspberry Pi 5.**

Supports merge mining for LTC+DOGE and BTC+BCH, real-time web dashboard, Discord notifications,
Prometheus metrics, and runs comfortably within 8 GB RAM on Ubuntu Server.

---

## ⚠️ Disclaimer

**PiPool is provided strictly for personal, educational, and research purposes.**

By downloading, installing, or running this software you agree to the following:

- You are solely responsible for your own use of this software and any consequences thereof
- The author(s) of PiPool make no representations or warranties of any kind, express or implied, regarding the software's fitness for any particular purpose, accuracy, reliability, or legality in your jurisdiction
- Mining cryptocurrency may be subject to laws and regulations in your country or region — it is your responsibility to ensure compliance with all applicable laws before use
- The author(s) accept no liability for any financial loss, hardware damage, legal consequences, regulatory penalties, tax obligations, or any other damages arising from the use or misuse of this software
- This software is not financial advice. Mining cryptocurrency carries significant financial risk including but not limited to hardware costs, electricity costs, and the volatility of cryptocurrency markets
- The author(s) are not responsible for any third-party software, coin daemons, or external services used in conjunction with PiPool
- Use of this software to engage in any illegal activity is strictly prohibited

**This software is provided "as is", without warranty of any kind. Use at your own risk.**

---

## Features

| Category | Feature |
|---|---|
| **Language** | Go — single binary, minimal RAM, native concurrency |
| **Coins** | LTC, DOGE (merge), BTC, BCH (merge), PEP (Scrypt-N), DGB, DGBS, Quai Network ⚠️ *(EXPERIMENTAL — see warning below)* |
| **Merge Mining** | AuxPoW for LTC+DOGE and BTC+BCH — one sync, two rewards |
| **Stratum** | Full Stratum V1 — vardiff, per-worker extranonce, clean job broadcast |
| **ASIC Support** | 65+ device classes auto-detected from user-agent (Antminer, Whatsminer, Goldshell, BitAxe, Elphapex, Plexsource, …) |
| **ASIC Health** | HTTP polls each miner's API (Bitaxe/Hammer/Goldshell/Elphapex) for live temp, fan, power, hashrate — shown in dashboard and Discord alerts |
| **TLS** | Optional TLS stratum listener per coin (auto-generates self-signed cert) |
| **Vardiff** | Per-worker vardiff with fixed-diff override, `mining.suggest_difficulty` support |
| **ZMQ** | Instant block notifications via ZMQ — sub-second job broadcasts |
| **Node Watchdog** | Auto-restarts unresponsive coin daemons via `systemctl` |
| **Multi-node Failover** | Optional `backup_node` per coin — seamlessly switches RPC source when primary goes down, no miner interruption |
| **Proxy Fallback** | When local node is offline, transparently proxies miners to a configured upstream pool |
| **Block Log** | Persisted block log with confirmations, luck %, and maturity tracking |
| **State Persistence** | Worker difficulty and best share persist across restarts |
| **Discord** | Block found/matured, miner events, high temp, hashrate reports, node down, worker silent |
| **Dashboard** | SSE-powered web UI — live hashrate, workers, coin cards, block log, share chart |
| **Worker Detail Modal** | Click any worker row for vardiff timeline chart, 30-min share sparkline, full session metadata, and ASIC health |
| **Config Editor** | Edit wallets, vardiff, Discord webhook, auto-kick, fixed diffs, kWh rate — live from browser |
| **Worker Groups** | Tag workers into named groups; see per-group hashrate and profit |
| **Electricity Cost** | Per-worker and per-coin electrical cost based on device wattage and kWh rate — live wattage polled from miner API where available |
| **Stale Kick** | Automatically disconnects miners that ignore `clean_jobs=true` after 5 consecutive stale shares for the same job — forces reconnect and fresh work |
| **Diff Countdown** | BTC/LTC epoch progress bar and projected next difficulty change % |
| **Prometheus** | `/metrics` endpoint exposing hashrate, miners, temp, wallet balance, worker hashrate, and more |
| **pipoolctl** | Unix-socket CLI for live status, worker management, and config changes |
| **Security** | IP allowlist, high-reject-rate auto-kick, optional dashboard password |

---

## Stratum Ports (defaults)

| Coin | Port | Algorithm | Notes |
|---|---|---|---|
| LTC | 3333 | Scrypt | Primary — miners connect here for LTC+DOGE |
| DOGE | 3334 | Scrypt / AuxPoW | Merged via LTC — no separate miner needed |
| BTC | 3335 | SHA-256d | Primary — miners connect here for BTC+BCH |
| BCH | 3336 | SHA-256d / AuxPoW | Merged via BTC — no separate miner needed |
| PEP | 3337 | Scrypt-N (N=2048) | Optional — enable in config |
| DGB | 3339 | SHA-256d | Optional DigiByte SHA-256d |
| DGBS | 3342 | Scrypt | Optional DigiByte Scrypt — uses 5s share target (fast blocks) |
| QUAI (SHA-256d) | 3340 | SHA-256d | ⚠️ **EXPERIMENTAL** — Quai Network (see warning below) |
| QUAI (Scrypt) | 3341 | Scrypt | ⚠️ **EXPERIMENTAL** — Quai Network (see warning below) |

**You only need to point your miner at 3333 (Scrypt) and/or 3335 (SHA-256d).**
DOGE and BCH are earned for free via merge mining.

---

## ⚠️ EXPERIMENTAL: Quai Network

> **WARNING: Quai Network support is EXPERIMENTAL and NOT PRODUCTION READY.**
>
> - Quai Network uses a non-standard mining protocol and block submission process that differs significantly from Bitcoin-derived coins
> - **Payouts are NOT guaranteed.** Block submission has not been exhaustively tested against a live Quai node under all conditions
> - The Quai stratum implementation may have undiscovered bugs that result in rejected blocks, lost work, or miner disconnects
> - **Do NOT use Quai mining with real money on the line until this warning is removed**
> - If you choose to test Quai support, do so on testnet or in a controlled environment and report any issues
>
> All other coins (LTC, DOGE, BTC, BCH, PEP, DGB, DGBS) are stable and production-ready.

---

## Quick Start

```bash
# 1. Clone and install (Ubuntu Server, Pi 5)
git clone https://github.com/dakanoth/Pipool
cd Pipool
sudo bash scripts/install.sh

# 2. Or build manually
go build -o pipool ./cmd/pipool/

# 3. Generate default config
./pipool -init

# 4. Edit config — set your wallets and RPC credentials
nano configs/pipool.json

# 5. Run
./pipool -config configs/pipool.json

# 6. As a systemd service
sudo systemctl start pipool
sudo journalctl -u pipool -f

# 7. Dashboard (always on by default, port 8080)
# Open http://<pi-ip>:8080 in your browser

# 8. Prometheus metrics (port 9100 by default)
./pipool -config configs/pipool.json -metrics-port 9100

# 9. pipoolctl (requires pipool to be running)
pipoolctl status
pipoolctl workers
pipoolctl kick wallet.miner1
pipoolctl fixdiff wallet.miner1 256
```

---

## Config Reference (`pipool.json`)

```json
{
  "pool": {
    "name": "PiPool",
    "host": "0.0.0.0",
    "coinbase_tag": "/PiPool/",
    "max_connections": 64,
    "worker_timeout": 120,
    "temp_limit_c": 75,
    "ip_allowlist": [],
    "state_file": "/opt/pipool/worker_state",
    "auto_kick_reject_pct": 0,
    "auto_kick_min_shares": 50,
    "last_share_alert_min": 0,
    "kwh_rate_usd": 0.12,
    "worker_fixed_diff": {
      "wallet.miner1": 256
    },
    "worker_fixed_watts": {
      "wallet.miner1": 3250
    },
    "worker_groups": {
      "garage rig": ["wallet.miner1", "wallet.miner2"],
      "office rig": ["wallet.miner3"]
    }
  },
  "coins": {
    "LTC": {
      "enabled": true,
      "algorithm": "scrypt",
      "wallet": "YOUR_LTC_WALLET",
      "block_reward": 6.25,
      "stratum": { "port": 3333 },
      "node": {
        "host": "127.0.0.1", "port": 9332,
        "user": "litecoind", "password": "changeme",
        "zmq_pub_hashblock": "tcp://127.0.0.1:28332",
        "systemd_service": "litecoind"
      },
      "backup_node": {
        "host": "192.168.1.50", "port": 9332,
        "user": "litecoind", "password": "changeme"
      },
      "upstream_pool": {
        "enabled": false,
        "host": "stratum.litecoinpool.org",
        "port": 3333,
        "user": "YOUR_LTC_WALLET",
        "password": "x"
      }
    },
    "DOGE": {
      "enabled": true,
      "algorithm": "scrypt",
      "merge_parent": "LTC",
      "wallet": "YOUR_DOGE_WALLET",
      "block_reward": 10000,
      "node": { "host": "127.0.0.1", "port": 22555 }
    }
  },
  "discord": {
    "enabled": true,
    "webhook_url": "https://discord.com/api/webhooks/YOUR/WEBHOOK",
    "alerts": {
      "block_found": true,
      "miner_connected": true,
      "miner_disconnect": false,
      "high_temp": true,
      "hashrate_report": true,
      "hashrate_interval_min": 60,
      "hashrate_drop_pct": 20,
      "node_unreachable": true
    }
  },
  "dashboard": {
    "enabled": true,
    "port": 8080,
    "push_interval_s": 15
  },
  "logging": {
    "level": "info",
    "to_file": true,
    "path": "/var/log/pipool/pipool.log"
  }
}
```

---

## Discord Alerts

| Event | Trigger |
|---|---|
| **BLOCK FOUND** | Every solved block — includes height, reward, luck %, worker |
| **BLOCK MATURED** | Block reaches 100 confirmations (60 for DOGE) — reward is now spendable |
| **MINER CONNECTED** | New miner authorizes (throttled: 1 per 30s per coin) |
| **MINER DISCONNECTED** | Miner drops (3-minute debounce — skipped if miner reconnects) |
| **WORKER SILENT** | Connected worker goes N minutes without a share (`last_share_alert_min`) |
| **HIGH TEMP** | CPU exceeds `temp_limit_c` (throttled: 1 per 10 min) |
| **HASHRATE REPORT** | Periodic summary every `hashrate_interval_min` minutes |
| **HASHRATE DROP** | Hashrate falls more than `hashrate_drop_pct` % (throttled: 1 per 15 min) |
| **NODE UNREACHABLE** | Coin daemon goes offline (throttled: 1 per 5 min) |
| **WATCHDOG RESTART** | PiPool auto-restarts a crashed daemon via systemctl |
| **NODE RESTORED** | Daemon comes back online |
| **POOL STARTED** | On every startup |

---

## Dashboard

Open `http://<pi-ip>:8080` in any browser on your network.

| Section | What it shows |
|---|---|
| **Status bar** | Pool name, live/reconnecting, last update time |
| **Header metrics** | Scrypt hashrate, SHA-256d hashrate, blocks found, uptime |
| **System** | CPU %, RAM, core temp with live bars |
| **Coin cards** | Per-coin: hashrate, miners, difficulty, price, expected block time, session effort, elec cost/day, wallet balance (confirmed + maturing), difficulty retarget countdown (BTC/LTC), share histogram |
| **Workers table** | All seen workers (online + offline history): hashrate, difficulty, shares, best share, elec cost/day, device, user-agent tooltip, ASIC temp badge |
| **Worker detail modal** | Click any row — vardiff timeline chart, 30-min share-rate sparkline, ASIC health (temp/fan/power), full session metadata |
| **Groups** | Per-group aggregate hashrate, online count, profit/day (when `worker_groups` configured) |
| **Telemetry chart** | Per-coin toggle buttons — select any combination of coins simultaneously. Diff mode: multi-coin scatter plot with per-coin colors and net diff lines on a shared log scale. Hashrate mode: overlaid area/line chart per selected coin |
| **Block log** | All found blocks with height, reward, luck %, confirmations, orphan status |
| **Storage** | Disk usage per mount with per-chain data sizes |
| **Chain diagnostics** | Stale %, reject %, job age, issue detection per chain — includes QUAI/QUAIS |
| **Connection info** | Stratum address, port, and algorithm per coin |
| **Notifications** | Toggle Discord alerts live |
| **Pool settings** | Edit wallets, vardiff, webhook, auto-kick, fixed diffs, electricity rate, Quai node host/port — saved to disk |

---

## Electricity Cost Tracking

Set `kwh_rate_usd` in config (e.g. `0.12` for 12¢/kWh). PiPool looks up wattage for each connected ASIC from its built-in device table (65+ models), live-polled from the miner's HTTP API where available. You can override per-worker with `worker_fixed_watts`.

The dashboard then shows:
- **Workers table** — WATTS and $/Day (electrical cost) per worker
- **Coin cards** — Elec/Day: summed electrical cost of all online miners on that coin

> Some miners report an unusual user-agent (e.g. Elphapex DG Home reports `cpuminer/2.5.1`). Use `worker_fixed_watts` to pin the correct wattage for those workers.

---

## ASIC Health Polling

PiPool polls each miner's HTTP API every 30 seconds for live hardware stats using the same endpoints as the miner's own web UI:

| Miner Type | Endpoint | Data |
|---|---|---|
| Bitaxe / ESP-Miner / Plexsource | `GET /api/system/info` | temp, fan RPM/%, power W, hashrate, uptime |
| Elphapex DG Home / DG1 | `GET /cgi-bin/stats.cgi` | temp (chip + PCB), fan RPM, hashrate |
| Goldshell | `GET /mcb/cgminer` + `/mcb/status` | temp, hashrate, power W |
| Generic | `GET /api/system/info` (fallback) | temp, fan, hashrate |

The miner type is inferred from the device class auto-detected during stratum authorization — no extra config needed. Health data is shown in the worker table (temp badge) and in the worker detail modal. A Discord alert fires if the ASIC temperature reaches `temp_limit_c`.

---

## Stale Share Handling

PiPool tracks the job ID of every stale submission per worker. If a worker submits **5 consecutive stale shares for the same old job**, the pool disconnects it. This targets firmware that ignores `mining.notify` with `clean_jobs=true` and instead rolls the `ntime` field on an expired job indefinitely — a known behaviour in some ASIC and open-source miner firmware. On reconnect the miner receives a fresh job and resumes useful work.

One-off stale shares (new block arrived mid-search) are normal and do not trigger a kick. The streak resets to zero on any accepted share.

For coins with fast block times (DGB Scrypt: ~15s), PiPool uses a shorter vardiff target (5 seconds) so shares are submitted frequently enough that most are still valid when they arrive.

---

## Multi-node Failover

Add a `backup_node` to any coin's config to enable automatic failover:

```json
"LTC": {
  "node": { "host": "127.0.0.1", "port": 9332, "user": "litecoind", "password": "changeme" },
  "backup_node": { "host": "192.168.1.50", "port": 9332, "user": "litecoind", "password": "changeme" }
}
```

When the primary node fails an RPC call, PiPool instantly switches to the backup and continues mining without interrupting connected miners. If the backup also fails, the existing upstream proxy fallback activates. When the primary recovers, PiPool switches back automatically.

---

## Prometheus Metrics

Start with `-metrics-port 9100` (or any port). Exposes at `http://<pi-ip>:9100/metrics`:

| Metric | Type | Labels |
|---|---|---|
| `pipool_cpu_temp_celsius` | gauge | — |
| `pipool_cpu_usage_percent` | gauge | — |
| `pipool_ram_used_gigabytes` | gauge | — |
| `pipool_uptime_seconds` | counter | — |
| `pipool_connected_miners` | gauge | `coin` |
| `pipool_hashrate_khs` | gauge | `coin` |
| `pipool_blocks_found_total` | counter | `coin` |
| `pipool_valid_shares_total` | counter | `coin` |
| `pipool_total_shares_total` | counter | `coin` |
| `pipool_rejected_shares_total` | counter | `coin` |
| `pipool_stale_shares_total` | counter | `coin` |
| `pipool_network_difficulty` | gauge | `coin` |
| `pipool_block_height` | gauge | `coin` |
| `pipool_wallet_balance` | gauge | `coin` |
| `pipool_wallet_immature` | gauge | `coin` |
| `pipool_worker_hashrate_khs` | gauge | `worker` |
| `pipool_device_count` | gauge | `device` |

---

## pipoolctl

```bash
pipoolctl status               # system health snapshot
pipoolctl workers              # list all connected workers
pipoolctl coins                # per-coin hashrate and block count
pipoolctl kick wallet.miner1   # disconnect a specific worker
pipoolctl fixdiff wallet.miner1 256   # pin worker to difficulty (0 = remove)
pipoolctl coin enable DGB      # enable/disable a coin
pipoolctl vardiff LTC 0.1 1024 # update vardiff bounds
pipoolctl reload               # hot-reload discord/dashboard config from disk
pipoolctl loglevel debug        # change log verbosity live
pipoolctl discord test          # send a test Discord notification
pipoolctl stop                 # graceful stop (systemd restarts automatically)
pipoolctl version              # print version
```

---

## Architecture

```
cmd/pipool/main.go              — Entry point, wires everything together
internal/
  config/config.go              — Config structs, load/save, defaults
  rpc/client.go                 — JSON-RPC client for coin daemons
  stratum/server.go             — Stratum V1 server (per primary coin)
  stratum/proxy.go              — Upstream proxy fallback (when node offline)
  stratum/router.go             — ASIC fingerprinting — 65 device classes
  stratum/tls.go                — Optional TLS listener
  asic/poller.go                — ASIC HTTP health polling (temp, fan, power)
  merge/auxpow.go               — AuxPoW merge mining coordinator
  mining/monitor.go             — CPU temp, RAM, uptime monitor
  discord/notifier.go           — Discord webhook notifications
  dashboard/server.go           — HTTP dashboard + SSE push + config editor + worker detail modal
  metrics/prometheus.go         — Prometheus text-format metrics registry
  ctl/server.go                 — pipoolctl Unix socket server
  quai/rpc.go                   — Quai Network HTTP JSON-RPC node client
  quai/server.go                — Quai Stratum V1 server
configs/pipool.json             — Your config (generated by -init or install.sh)
scripts/install.sh              — Full Ubuntu Server install script
```

---

## RAM Budget (Pi 5, 8 GB)

> **Recommendation:** Run coin daemons on a separate machine (desktop PC, NAS, etc.) and point PiPool at them over the network. The Pi 5 has only 8 GB of RAM — coin daemons are memory-hungry and will compete with PiPool, the dashboard, and the OS for resources. A single daemon like `digibyted` can consume 6+ GB on its own. PiPool itself is lightweight (~30–80 MB) and is perfectly happy talking to remote nodes via RPC. Use the Pi for what it's good at — running the pool — and let beefier hardware handle the blockchain.

| Component | RAM |
|---|---|
| PiPool itself | ~30–80 MB |
| litecoind | ~400–600 MB |
| dogecoind | ~300–500 MB |
| bitcoind | ~500–700 MB |
| bitcoin-cash-node | ~300–500 MB |
| digibyted | ~4–7 GB (!) |
| Ubuntu Server overhead | ~400 MB |

> Use `dbcache=256` in coin daemon configs to cap their LevelDB cache. This does **not** limit total memory usage — the UTXO set, block index, and mempool are held separately in RAM and can dwarf the dbcache setting, especially on high-block-count chains like DigiByte (23M+ blocks).

---

## Coin Daemon Installation

### Litecoin
```bash
wget https://download.litecoin.org/litecoin-0.21.3/linux/litecoin-0.21.3-aarch64-linux-gnu.tar.gz
tar xzf litecoin-0.21.3-aarch64-linux-gnu.tar.gz
sudo install -m 0755 litecoin-0.21.3/bin/* /usr/local/bin/
litecoind -daemon
```

### Dogecoin
```bash
git clone https://github.com/dogecoin/dogecoin
cd dogecoin && ./autogen.sh && ./configure --without-gui --with-incompatible-bdb
make -j4 && sudo make install
dogecoind -daemon
```

### Bitcoin Core
```bash
wget https://bitcoincore.org/bin/bitcoin-core-26.0/bitcoin-26.0-aarch64-linux-gnu.tar.gz
tar xzf bitcoin-26.0-aarch64-linux-gnu.tar.gz
sudo install -m 0755 bitcoin-26.0/bin/* /usr/local/bin/
bitcoind -daemon
```

### Bitcoin Cash Node
```bash
wget https://github.com/bitcoin-cash-node/bitcoin-cash-node/releases/download/v27.1.0/bitcoin-cash-node-27.1.0-aarch64-linux-gnu.tar.gz
tar xzf bitcoin-cash-node-*.tar.gz
sudo install -m 0755 bitcoin-cash-node-*/bin/* /usr/local/bin/
bitcoind -conf=/root/.bch/bitcoin.conf -daemon
```

---

## License

MIT License — Copyright (c) 2025 dakanoth

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

**THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.**
