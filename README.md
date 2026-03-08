# ⛏️ PiPool

**A lightweight, original solo mining pool server built in Go for the Raspberry Pi 5.**

Supports merge mining for LTC+DOGE and BTC+BCH, Discord Sentinel notifications,
an optional web dashboard, and runs comfortably within 8GB RAM on Ubuntu Server.

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

| Feature | Detail |
|---|---|
| **Language** | Go — single binary, minimal RAM, native concurrency |
| **LTC + DOGE** | Merge mined via AuxPoW — one Scrypt sync, two rewards |
| **BTC + BCH** | Merge mined via AuxPoW — one SHA-256d sync, two rewards |
| **PEP** | Optional Scrypt-N, enable in config |
| **Quai Network** | SHA-256d and Scrypt ASIC support via built-in stratum |
| **Stratum V1** | Full vardiff, per-worker extranonce, clean job broadcast |
| **Discord** | Block found, miner events, high temp, hashrate reports, node down |
| **Dashboard** | Optional SSE-powered web UI — disabled by default to save RAM |
| **RAM Safety** | 512MB systemd MemoryMax cap on PiPool itself |
| **Config** | Single `pipool.json` — wallets, RPC creds, alerts all in one place |

---

## Stratum Ports

| Coin | Port | Notes |
|---|---|---|
| LTC | 3333 | Primary Scrypt — miners connect here for LTC+DOGE |
| DOGE | 3334 | AuxPoW merged via LTC — no separate miner needed |
| BTC | 3335 | Primary SHA-256d — miners connect here for BTC+BCH |
| BCH | 3336 | AuxPoW merged via BTC — no separate miner needed |
| PEP | 3337 | Optional Scrypt-N |
| QUAI (SHA-256) | 3340 | Quai Network SHA-256d ASICs |
| QUAI (Scrypt) | 3341 | Quai Network Scrypt ASICs |

**You only need to point your miner at ports 3333 (Scrypt) and/or 3335 (SHA-256d).**
DOGE and BCH are earned for free via merge mining.

---

## Quick Start

```bash
# 1. Install (Ubuntu Server, Pi 5)
git clone https://github.com/dakanoth/Pipool
cd Pipool
sudo bash scripts/install.sh

# 2. Or build manually
go build -o pipool ./cmd/pipool/

# 3. Generate default config
./pipool -init

# 4. Edit config
nano configs/pipool.json

# 5. Run
./pipool -config configs/pipool.json

# 6. Run with dashboard enabled
./pipool -config configs/pipool.json -dashboard

# 7. As a systemd service
sudo systemctl start pipool
sudo journalctl -u pipool -f
```

---

## RAM Budget (Pi 5, 8GB)

| Component | RAM Usage |
|---|---|
| PiPool itself | ~30–80 MB |
| litecoind (LTC) | ~400–600 MB |
| dogecoind (DOGE) | ~300–500 MB |
| bitcoind (BTC) | ~500–700 MB |
| bchd (BCH) | ~300–500 MB |
| Ubuntu Server overhead | ~400 MB |
| **Total (all 4 daemons)** | **~2–3 GB** |

> 💡 **Tip:** Use `dbcache=256` in coin daemon configs to cap their RAM usage.
> The install script sets these automatically.

---

## Config Reference (`pipool.json`)

```json
{
  "pool": {
    "max_connections": 64,
    "temp_limit_c": 75
  },
  "coins": {
    "DOGE": {
      "enabled": true,
      "merge_parent": "LTC"
    }
  },
  "quai": {
    "enabled": false,
    "node": {
      "host": "192.168.1.92",
      "ws_port": 8548
    },
    "sha256": {
      "enabled": true,
      "port": 3340,
      "min_diff": 65536,
      "max_diff": 67108864,
      "target_time": 15
    },
    "scrypt": {
      "enabled": true,
      "port": 3341,
      "min_diff": 64,
      "max_diff": 65536,
      "target_time": 15
    }
  },
  "discord": {
    "webhook_url": "https://discord.com/api/webhooks/...",
    "alerts": {
      "hashrate_interval_min": 60,
      "hashrate_drop_pct": 20
    }
  },
  "dashboard": {
    "enabled": false
  }
}
```

---

## Discord Alerts

| Event | Trigger |
|---|---|
| 🏆 Block Found | Every solved block |
| 🔌 Miner Connected | New miner connects (throttled: max 1/30s per coin) |
| 🌡️ High Temp | CPU exceeds `temp_limit_c` (throttled: max 1/10min) |
| 📊 Hashrate Report | Every `hashrate_interval_min` minutes |
| 📉 Hashrate Drop | When hashrate falls > `hashrate_drop_pct` % |
| 💀 Node Unreachable | Coin daemon goes offline (throttled: max 1/5min) |
| 🚀 Pool Started | On every startup |

---

## Architecture

```
cmd/pipool/main.go          — Entry point, wires everything together
internal/
  config/config.go          — Config structs + load/save
  rpc/client.go             — JSON-RPC client for coin daemons
  stratum/server.go         — Stratum V1 server (per primary coin)
  merge/auxpow.go           — AuxPoW merge mining coordinator
  mining/monitor.go         — CPU temp, RAM, uptime monitor
  discord/notifier.go       — Discord webhook notifications
  dashboard/server.go       — Optional HTTP dashboard + SSE push
  quai/rpc.go               — Quai Network WebSocket node client
  quai/server.go            — Quai Stratum V1 server (SHA-256d + Scrypt)
configs/pipool.json         — Your config (generated by -init or install.sh)
scripts/install.sh          — Full Ubuntu Server install script
```

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

### Bitcoin Cash
```bash
wget https://github.com/bitcoin-cash-node/bitcoin-cash-node/releases/download/v27.1.0/bitcoin-cash-node-27.1.0-aarch64-linux-gnu.tar.gz
tar xzf bitcoin-cash-node-*.tar.gz
sudo install -m 0755 bitcoin-cash-node-*/bin/* /usr/local/bin/
bitcoind -conf=/root/.bch/bitcoin.conf -daemon
```

---

## License

MIT License

Copyright (c) 2025 dakanoth

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

**THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.**
