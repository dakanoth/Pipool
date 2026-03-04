#!/usr/bin/env bash
# =============================================================================
#  PiPool — Install Script for Raspberry Pi 5 (Ubuntu Server)
#  Installs Go, builds PiPool, installs coin daemons, sets up systemd services
#  Run as root: sudo bash install.sh
# =============================================================================

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

log()  { echo -e "${GREEN}[✔]${NC} $1"; }
warn() { echo -e "${YELLOW}[⚠]${NC} $1"; }
err()  { echo -e "${RED}[✖]${NC} $1"; exit 1; }
info() { echo -e "${CYAN}[ℹ]${NC} $1"; }
step() { echo -e "\n${BOLD}${CYAN}══ $1 ══${NC}"; }

[[ $EUID -ne 0 ]] && err "Run as root: sudo bash install.sh"

echo -e "${CYAN}${BOLD}"
cat <<'EOF'
  ╔══════════════════════════════════════════════════════╗
  ║   ⛏️  PiPool — Raspberry Pi 5 Ubuntu Server          ║
  ║   LTC+DOGE (merge) · BTC+BCH (merge) · PEP opt-in  ║
  ╚══════════════════════════════════════════════════════╝
EOF
echo -e "${NC}"

PIPOOL_DIR="/opt/pipool"
GO_VERSION="1.21.6"
GOROOT="/usr/local/go"
GOPATH="/root/go"

# ─── Collect credentials ──────────────────────────────────────────────────────
step "Configuration"

read -p "$(echo -e "${CYAN}") LTC wallet address: $(echo -e "${NC}")" LTC_WALLET
read -p "$(echo -e "${CYAN}") DOGE wallet address: $(echo -e "${NC}")" DOGE_WALLET
read -p "$(echo -e "${CYAN}") BTC wallet address: $(echo -e "${NC}")" BTC_WALLET
read -p "$(echo -e "${CYAN}") BCH wallet address: $(echo -e "${NC}")" BCH_WALLET
read -p "$(echo -e "${CYAN}") Discord webhook URL: $(echo -e "${NC}")" DISCORD_WEBHOOK
read -p "$(echo -e "${CYAN}") Enable web dashboard? [y/N]: $(echo -e "${NC}")" ENABLE_DASH
read -p "$(echo -e "${CYAN}") Enable PEP mining? [y/N]: $(echo -e "${NC}")" ENABLE_PEP

[[ -z "$LTC_WALLET" ]] && err "LTC wallet is required"
[[ -z "$DOGE_WALLET" ]] && err "DOGE wallet is required"
[[ -z "$BTC_WALLET" ]] && err "BTC wallet is required"
[[ -z "$BCH_WALLET" ]] && err "BCH wallet is required"

# Generate random RPC passwords
LTC_RPC_PASS=$(openssl rand -hex 24)
DOGE_RPC_PASS=$(openssl rand -hex 24)
BTC_RPC_PASS=$(openssl rand -hex 24)
BCH_RPC_PASS=$(openssl rand -hex 24)
DASH_PASS=$(openssl rand -hex 12)

DASH_ENABLED="false"
[[ "$ENABLE_DASH" =~ ^[Yy]$ ]] && DASH_ENABLED="true"

PEP_ENABLED="false"
[[ "$ENABLE_PEP" =~ ^[Yy]$ ]] && PEP_ENABLED="true"

# ─── System packages ──────────────────────────────────────────────────────────
step "Installing system packages"
apt-get update -qq
apt-get install -y -qq \
  build-essential git curl wget tar \
  libssl-dev libdb-dev libdb++-dev \
  libboost-all-dev libminiupnpc-dev \
  libevent-dev libzmq3-dev \
  pkg-config autoconf automake libtool \
  jq htop screen ufw
log "System packages installed"

# ─── Install Go ───────────────────────────────────────────────────────────────
step "Installing Go ${GO_VERSION}"
if [[ -d "$GOROOT" ]]; then
    INSTALLED_GO=$(${GOROOT}/bin/go version 2>/dev/null | awk '{print $3}' | sed 's/go//')
    if [[ "$INSTALLED_GO" == "$GO_VERSION" ]]; then
        log "Go ${GO_VERSION} already installed"
    else
        warn "Removing old Go ${INSTALLED_GO}"
        rm -rf "$GOROOT"
    fi
fi

if [[ ! -d "$GOROOT" ]]; then
    GOARCH="arm64"  # Pi 5 is arm64
    GO_TAR="go${GO_VERSION}.linux-${GOARCH}.tar.gz"
    wget -q "https://go.dev/dl/${GO_TAR}" -O /tmp/${GO_TAR}
    tar -C /usr/local -xzf /tmp/${GO_TAR}
    rm /tmp/${GO_TAR}
    log "Go ${GO_VERSION} installed"
fi

export PATH="${GOROOT}/bin:${GOPATH}/bin:${PATH}"
echo "export PATH=${GOROOT}/bin:${GOPATH}/bin:\$PATH" >> /etc/profile.d/pipool.sh

# ─── Build PiPool ─────────────────────────────────────────────────────────────
step "Building PiPool"
mkdir -p "$PIPOOL_DIR"
cp -r . "$PIPOOL_DIR/"
cd "$PIPOOL_DIR"

go mod tidy 2>/dev/null || true

# Build main pool server
go build -ldflags="-s -w" -o pipool ./cmd/pipool/
log "pipool binary built"

# Build pipoolctl CLI tool
go build -ldflags="-s -w" -o pipoolctl ./cmd/pipoolctl/
sudo install -m 0755 pipoolctl /usr/local/bin/pipoolctl
log "pipoolctl installed to /usr/local/bin/pipoolctl"

mkdir -p "${PIPOOL_DIR}/configs" /var/log/pipool /var/run/pipool

# ─── Write pipool.json config ─────────────────────────────────────────────────
step "Writing pipool.json"
cat > "${PIPOOL_DIR}/configs/pipool.json" <<EOFJSON
{
  "pool": {
    "name": "PiPool",
    "host": "0.0.0.0",
    "max_connections": 64,
    "worker_timeout": 120,
    "temp_limit_c": 75
  },
  "coins": {
    "LTC": {
      "enabled": true,
      "symbol": "LTC",
      "algorithm": "scrypt",
      "merge_parent": "",
      "stratum": {
        "port": 3333,
        "vardiff": { "min_diff": 0.001, "max_diff": 1024, "target_ms": 30000, "retarget_s": 60 }
      },
      "node": {
        "host": "127.0.0.1", "port": 9332,
        "user": "litecoind", "password": "${LTC_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://127.0.0.1:28332"
      },
      "wallet": "${LTC_WALLET}",
      "block_reward": 6.25
    },
    "DOGE": {
      "enabled": true,
      "symbol": "DOGE",
      "algorithm": "scrypt",
      "merge_parent": "LTC",
      "stratum": {
        "port": 3334,
        "vardiff": { "min_diff": 0.001, "max_diff": 1024, "target_ms": 30000, "retarget_s": 60 }
      },
      "node": {
        "host": "127.0.0.1", "port": 22555,
        "user": "dogecoind", "password": "${DOGE_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://127.0.0.1:28333"
      },
      "wallet": "${DOGE_WALLET}",
      "block_reward": 10000
    },
    "BTC": {
      "enabled": true,
      "symbol": "BTC",
      "algorithm": "sha256d",
      "merge_parent": "",
      "stratum": {
        "port": 3335,
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 }
      },
      "node": {
        "host": "127.0.0.1", "port": 8332,
        "user": "bitcoind", "password": "${BTC_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://127.0.0.1:28334"
      },
      "wallet": "${BTC_WALLET}",
      "block_reward": 3.125
    },
    "BCH": {
      "enabled": true,
      "symbol": "BCH",
      "algorithm": "sha256d",
      "merge_parent": "BTC",
      "stratum": {
        "port": 3336,
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 }
      },
      "node": {
        "host": "127.0.0.1", "port": 8336,
        "user": "bchd", "password": "${BCH_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://127.0.0.1:28335"
      },
      "wallet": "${BCH_WALLET}",
      "block_reward": 6.25
    },
    "PEP": {
      "enabled": ${PEP_ENABLED},
      "symbol": "PEP",
      "algorithm": "scryptn",
      "merge_parent": "",
      "stratum": {
        "port": 3337,
        "vardiff": { "min_diff": 0.001, "max_diff": 512, "target_ms": 30000, "retarget_s": 60 }
      },
      "node": {
        "host": "127.0.0.1", "port": 33873,
        "user": "pepd", "password": "changeme"
      },
      "wallet": "YOUR_PEP_WALLET",
      "block_reward": 50000
    }
  },
  "discord": {
    "enabled": true,
    "webhook_url": "${DISCORD_WEBHOOK}",
    "bot_name": "PiPool Bot",
    "avatar_url": "",
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
    "enabled": ${DASH_ENABLED},
    "port": 8080,
    "username": "admin",
    "password": "${DASH_PASS}",
    "push_interval_s": 5
  },
  "logging": {
    "level": "info",
    "to_file": true,
    "path": "/var/log/pipool/pipool.log"
  }
}
EOFJSON
chmod 600 "${PIPOOL_DIR}/configs/pipool.json"
log "Config written"

# ─── Coin daemon configs ──────────────────────────────────────────────────────
step "Writing coin daemon configs"

mkdir -p /root/.litecoin /root/.dogecoin /root/.bitcoin /root/.bch

cat > /root/.litecoin/litecoin.conf <<EOF
server=1
daemon=1
rpcuser=litecoind
rpcpassword=${LTC_RPC_PASS}
rpcallowip=127.0.0.1
rpcport=9332
zmqpubhashblock=tcp://127.0.0.1:28332
# Pruned node to save disk space on 1TB SSD
# prune=50000
# Limit dbcache to stay within 8GB RAM budget
dbcache=512
maxmempool=100
listen=1
EOF

cat > /root/.dogecoin/dogecoin.conf <<EOF
server=1
daemon=1
rpcuser=dogecoind
rpcpassword=${DOGE_RPC_PASS}
rpcallowip=127.0.0.1
rpcport=22555
zmqpubhashblock=tcp://127.0.0.1:28333
dbcache=256
maxmempool=50
listen=1
EOF

cat > /root/.bitcoin/bitcoin.conf <<EOF
server=1
daemon=1
rpcuser=bitcoind
rpcpassword=${BTC_RPC_PASS}
rpcallowip=127.0.0.1
rpcport=8332
zmqpubhashblock=tcp://127.0.0.1:28334
dbcache=512
maxmempool=100
# prune=50000
listen=1
EOF

cat > /root/.bch/bitcoin.conf <<EOF
server=1
daemon=1
rpcuser=bchd
rpcpassword=${BCH_RPC_PASS}
rpcallowip=127.0.0.1
rpcport=8336
zmqpubhashblock=tcp://127.0.0.1:28335
dbcache=256
maxmempool=50
EOF

log "Coin daemon configs written"

# ─── Systemd service for PiPool ───────────────────────────────────────────────
step "Installing systemd service"
cat > /etc/systemd/system/pipool.service <<EOF
[Unit]
Description=PiPool — Raspberry Pi 5 Solo Mining Pool
After=network.target litecoind.service dogecoind.service bitcoind.service

[Service]
Type=simple
User=root
WorkingDirectory=${PIPOOL_DIR}
ExecStart=${PIPOOL_DIR}/pipool -config ${PIPOOL_DIR}/configs/pipool.json
Restart=always
RestartSec=10
StandardOutput=append:/var/log/pipool/pipool.log
StandardError=append:/var/log/pipool/pipool.log

# Memory limit — safety net to protect the Pi's 8GB RAM
MemoryMax=512M
MemoryHigh=400M

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable pipool
log "pipool.service installed and enabled"

# ─── UFW firewall rules ───────────────────────────────────────────────────────
step "Configuring firewall"
ufw allow 22/tcp     comment "SSH"
ufw allow 3333/tcp   comment "PiPool LTC Stratum"
ufw allow 3343/tcp   comment "PiPool LTC Stratum TLS"
ufw allow 3334/tcp   comment "PiPool DOGE Stratum (merge)"
ufw allow 3335/tcp   comment "PiPool BTC Stratum"
ufw allow 3345/tcp   comment "PiPool BTC Stratum TLS"
ufw allow 3336/tcp   comment "PiPool BCH Stratum (merge)"
ufw allow 9100/tcp   comment "PiPool Prometheus metrics"
if [[ "$DASH_ENABLED" == "true" ]]; then
    ufw allow 8080/tcp comment "PiPool Dashboard"
fi
ufw --force enable
log "Firewall configured"

# ─── Summary ──────────────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}${GREEN}╔══════════════════════════════════════════════════════╗"
echo -e "║  ✅  PiPool Installation Complete!                   ║"
echo -e "╚══════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "${BOLD}Next steps:${NC}"
echo ""
echo "  1. Install & sync coin daemons (litecoind, dogecoind, bitcoind, bchd)"
echo "     Daemon configs are written to ~/.litecoin/, ~/.dogecoin/, ~/.bitcoin/, ~/.bch/"
echo ""
echo "  2. Wait for daemons to fully sync before starting PiPool"
echo "     Check sync: bitcoin-cli getblockchaininfo | grep 'verificationprogress'"
echo ""
echo "  3. Start PiPool:"
echo "     sudo systemctl start pipool"
echo "     sudo journalctl -u pipool -f"
echo ""
echo "  4. Point your miner at this Pi's IP:"
echo -e "     ${CYAN}LTC/DOGE:${NC}  stratum+tcp://$(hostname -I | awk '{print $1}'):3333"
echo -e "     ${CYAN}BTC/BCH:${NC}   stratum+tcp://$(hostname -I | awk '{print $1}'):3335"
[[ "$DASH_ENABLED" == "true" ]] && echo -e "     ${CYAN}Dashboard:${NC} http://$(hostname -I | awk '{print $1}'):8080  (admin / ${DASH_PASS})"
echo ""
echo -e "  Config: ${YELLOW}${PIPOOL_DIR}/configs/pipool.json${NC}"
echo -e "  Logs:   ${YELLOW}/var/log/pipool/pipool.log${NC}"
echo ""
echo -e "${YELLOW}⚠  RAM TIP:${NC} Run at most 2 coin daemons at once to stay within 8GB."
echo "   LTC+DOGE require only ONE Scrypt sync (DOGE is merged). BTC+BCH require two."
echo ""
