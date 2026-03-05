#!/usr/bin/env bash
# =============================================================================
#  PiPool — Install Script for Raspberry Pi 5 (Ubuntu Server)
#  Installs Go, coin daemons, builds PiPool, sets up systemd services
#  Run as root: sudo bash scripts/install.sh
# =============================================================================

set -euo pipefail

# Capture where the script lives — used to find the Go source tree regardless
# of where the user cd'd before running
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"   # project root (contains go.mod)

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

log()  { echo -e "${GREEN}[✔]${NC} $1"; }
warn() { echo -e "${YELLOW}[⚠]${NC} $1"; }
err()  { echo -e "${RED}[✖]${NC} $1"; exit 1; }
info() { echo -e "${CYAN}[ℹ]${NC} $1"; }
step() { echo -e "\n${BOLD}${CYAN}══ $1 ══${NC}"; }

[[ $EUID -ne 0 ]] && err "Run as root: sudo bash scripts/install.sh"

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

LTC_VERSION="0.21.3"
DOGE_VERSION="1.14.7"
BTC_VERSION="26.0"
BCH_VERSION="27.1.0"

# =============================================================================
# STEP 1 — COLLECT CONFIG
# =============================================================================
step "Configuration"
echo ""

read -p "$(echo -e "${CYAN}") LTC wallet address (starts with L, M, or ltc1): $(echo -e "${NC}")" LTC_WALLET
read -p "$(echo -e "${CYAN}") DOGE wallet address (starts with D): $(echo -e "${NC}")" DOGE_WALLET
read -p "$(echo -e "${CYAN}") BTC wallet address (starts with 1, 3, or bc1): $(echo -e "${NC}")" BTC_WALLET
read -p "$(echo -e "${CYAN}") BCH wallet address (starts with q or bitcoincash:): $(echo -e "${NC}")" BCH_WALLET
echo ""
read -p "$(echo -e "${CYAN}") Discord webhook URL: $(echo -e "${NC}")" DISCORD_WEBHOOK
echo ""

# Coinbase tag
echo -e "${CYAN}  Coinbase tag — a short label permanently embedded in any block you find."
echo -e "  Visible on mempool.space and block explorers forever."
echo -e "  Convention: /YourName/  e.g. /PiPool/ or /Dakota/ or /SatoshisPi/"
echo -e "  Max 20 characters. Leave blank for default /PiPool/${NC}"
read -p "$(echo -e "${CYAN}") Coinbase tag [/PiPool/]: $(echo -e "${NC}")" COINBASE_TAG_INPUT
COINBASE_TAG="${COINBASE_TAG_INPUT:-/PiPool/}"
COINBASE_TAG=$(echo "$COINBASE_TAG" | tr -d '\n' | cut -c1-20)
echo ""
echo -e "  Tag set to: ${YELLOW}${COINBASE_TAG}${NC}"
echo ""

read -p "$(echo -e "${CYAN}") Enable PEP mining? [y/N]: $(echo -e "${NC}")" ENABLE_PEP
echo ""

# Blockchain storage
echo -e "${CYAN}  Blockchain storage — where to store chain data for each coin."
echo -e "  Default is each daemon's standard location inside /root/."
echo -e "  If you have a dedicated SSD mounted at e.g. /mnt/ssd, enter that."
echo -e "  Leave blank to use the default location for each coin.${NC}"
echo ""
read -p "$(echo -e "${CYAN}") LTC  datadir [/root/.litecoin]:   $(echo -e "${NC}")" LTC_DATADIR_INPUT
read -p "$(echo -e "${CYAN}") DOGE datadir [/root/.dogecoin]:   $(echo -e "${NC}")" DOGE_DATADIR_INPUT
read -p "$(echo -e "${CYAN}") BTC  datadir [/root/.bitcoin]:    $(echo -e "${NC}")" BTC_DATADIR_INPUT
read -p "$(echo -e "${CYAN}") BCH  datadir [/root/.bch]:        $(echo -e "${NC}")" BCH_DATADIR_INPUT
read -p "$(echo -e "${CYAN}") PEP  datadir [/root/.pepecoin]:   $(echo -e "${NC}")" PEP_DATADIR_INPUT
echo ""

# Apply defaults
LTC_DATADIR="${LTC_DATADIR_INPUT:-/root/.litecoin}"
DOGE_DATADIR="${DOGE_DATADIR_INPUT:-/root/.dogecoin}"
BTC_DATADIR="${BTC_DATADIR_INPUT:-/root/.bitcoin}"
BCH_DATADIR="${BCH_DATADIR_INPUT:-/root/.bch}"
PEP_DATADIR="${PEP_DATADIR_INPUT:-/root/.pepecoin}"

echo -e "  Storage locations:"
echo -e "  ${YELLOW}LTC${NC}  → ${LTC_DATADIR}"
echo -e "  ${YELLOW}DOGE${NC} → ${DOGE_DATADIR}"
echo -e "  ${YELLOW}BTC${NC}  → ${BTC_DATADIR}"
echo -e "  ${YELLOW}BCH${NC}  → ${BCH_DATADIR}"
[[ "$ENABLE_PEP" =~ ^[Yy]$ ]] && echo -e "  ${YELLOW}PEP${NC}  → ${PEP_DATADIR}"
echo ""

# PEP wallet if enabled
PEP_WALLET="YOUR_PEP_WALLET"
if [[ "$ENABLE_PEP" =~ ^[Yy]$ ]]; then
  read -p "$(echo -e "${CYAN}") PEP wallet address: $(echo -e "${NC}")" PEP_WALLET
fi

# Which daemons to install
echo -e "${CYAN}  Which coin daemons should be installed now?"
echo -e "  Selecting N skips the install — you can add them manually later.${NC}"
echo ""
read -p "$(echo -e "${CYAN}") Install Litecoin daemon?      [Y/n]: $(echo -e "${NC}")" INSTALL_LTC
read -p "$(echo -e "${CYAN}") Install Dogecoin daemon?      [Y/n]: $(echo -e "${NC}")" INSTALL_DOGE
read -p "$(echo -e "${CYAN}") Install Bitcoin daemon?       [Y/n]: $(echo -e "${NC}")" INSTALL_BTC
read -p "$(echo -e "${CYAN}") Install Bitcoin Cash daemon?  [Y/n]: $(echo -e "${NC}")" INSTALL_BCH
if [[ "$ENABLE_PEP" =~ ^[Yy]$ ]]; then
  read -p "$(echo -e "${CYAN}") Install Pepecoin daemon?      [Y/n]: $(echo -e "${NC}")" INSTALL_PEP_D
fi

# Validate wallets
[[ -z "$LTC_WALLET" ]]  && err "LTC wallet is required"
[[ -z "$DOGE_WALLET" ]] && err "DOGE wallet is required"
[[ -z "$BTC_WALLET" ]]  && err "BTC wallet is required"
[[ -z "$BCH_WALLET" ]]  && err "BCH wallet is required"

# Generate random RPC passwords
LTC_RPC_PASS=$(openssl rand -hex 24)
DOGE_RPC_PASS=$(openssl rand -hex 24)
BTC_RPC_PASS=$(openssl rand -hex 24)
BCH_RPC_PASS=$(openssl rand -hex 24)
PEP_RPC_PASS=$(openssl rand -hex 24)
PEP_ENABLED="false";  [[ "$ENABLE_PEP"  =~ ^[Yy]$ ]] && PEP_ENABLED="true"

DO_LTC=true;  [[ "${INSTALL_LTC:-y}"   =~ ^[Nn]$ ]] && DO_LTC=false
DO_DOGE=true; [[ "${INSTALL_DOGE:-y}"  =~ ^[Nn]$ ]] && DO_DOGE=false
DO_BTC=true;  [[ "${INSTALL_BTC:-y}"   =~ ^[Nn]$ ]] && DO_BTC=false
DO_BCH=true;  [[ "${INSTALL_BCH:-y}"   =~ ^[Nn]$ ]] && DO_BCH=false
DO_PEP=false
if [[ "$PEP_ENABLED" == "true" ]] && [[ ! "${INSTALL_PEP_D:-y}" =~ ^[Nn]$ ]]; then
  DO_PEP=true
fi

# =============================================================================
# STEP 2 — SYSTEM PACKAGES
# =============================================================================
step "Installing system packages"
apt-get update -qq
apt-get install -y -qq \
  build-essential git curl wget tar unzip \
  libssl-dev libdb-dev libdb++-dev \
  libboost-all-dev libminiupnpc-dev \
  libevent-dev libzmq3-dev \
  pkg-config autoconf automake libtool \
  jq htop screen ufw
log "System packages installed"

# =============================================================================
# STEP 3 — GO
# =============================================================================
step "Installing Go ${GO_VERSION}"
if [[ -d "$GOROOT" ]]; then
    INSTALLED_GO=$(${GOROOT}/bin/go version 2>/dev/null | awk '{print $3}' | sed 's/go//')
    if [[ "$INSTALLED_GO" == "$GO_VERSION" ]]; then
        log "Go ${GO_VERSION} already installed"
    else
        warn "Removing old Go ${INSTALLED_GO}"; rm -rf "$GOROOT"
    fi
fi
if [[ ! -d "$GOROOT" ]]; then
    GO_TAR="go${GO_VERSION}.linux-arm64.tar.gz"
    info "Downloading Go ${GO_VERSION} for arm64..."
    wget -q --show-progress "https://go.dev/dl/${GO_TAR}" -O /tmp/${GO_TAR}
    tar -C /usr/local -xzf /tmp/${GO_TAR}
    rm /tmp/${GO_TAR}
    log "Go ${GO_VERSION} installed"
fi
export PATH="${GOROOT}/bin:${GOPATH}/bin:${PATH}"
echo "export PATH=${GOROOT}/bin:${GOPATH}/bin:\$PATH" >> /etc/profile.d/pipool.sh

# =============================================================================
# STEP 4 — COIN DAEMONS
# =============================================================================

install_litecoin() {
    step "Installing Litecoin ${LTC_VERSION}"
    if command -v litecoind &>/dev/null; then
        log "litecoind already installed"; return
    fi
    LTC_TAR="litecoin-${LTC_VERSION}-aarch64-linux-gnu.tar.gz"
    info "Downloading Litecoin ${LTC_VERSION}..."
    wget -q --show-progress \
      "https://download.litecoin.org/litecoin-${LTC_VERSION}/linux/${LTC_TAR}" \
      -O /tmp/${LTC_TAR}
    tar xzf /tmp/${LTC_TAR} -C /tmp/
    install -m 0755 /tmp/litecoin-${LTC_VERSION}/bin/litecoind /usr/local/bin/
    install -m 0755 /tmp/litecoin-${LTC_VERSION}/bin/litecoin-cli /usr/local/bin/
    rm -rf /tmp/litecoin-${LTC_VERSION} /tmp/${LTC_TAR}
    log "litecoind installed"

    cat > /etc/systemd/system/litecoind.service <<EOF
[Unit]
Description=Litecoin daemon
After=network-online.target local-fs.target
Wants=network-online.target
[Service]
Type=forking
User=root
ExecStart=/usr/local/bin/litecoind -daemon -datadir=${LTC_DATADIR} -conf=${LTC_DATADIR}/litecoin.conf
ExecStop=/usr/local/bin/litecoin-cli -datadir=${LTC_DATADIR} stop
Restart=on-failure
RestartSec=30
TimeoutStartSec=300
TimeoutStopSec=300
[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable litecoind
    log "litecoind.service enabled (not started — wait for sync)"
}

install_dogecoin() {
    step "Installing Dogecoin ${DOGE_VERSION}"
    if command -v dogecoind &>/dev/null; then
        log "dogecoind already installed"; return
    fi
    DOGE_TAR="dogecoin-${DOGE_VERSION}-aarch64-linux-gnu.tar.gz"
    info "Downloading Dogecoin ${DOGE_VERSION}..."
    wget -q --show-progress \
      "https://github.com/dogecoin/dogecoin/releases/download/v${DOGE_VERSION}/${DOGE_TAR}" \
      -O /tmp/${DOGE_TAR}
    tar xzf /tmp/${DOGE_TAR} -C /tmp/
    install -m 0755 /tmp/dogecoin-${DOGE_VERSION}/bin/dogecoind /usr/local/bin/
    install -m 0755 /tmp/dogecoin-${DOGE_VERSION}/bin/dogecoin-cli /usr/local/bin/
    rm -rf /tmp/dogecoin-${DOGE_VERSION} /tmp/${DOGE_TAR}
    log "dogecoind installed"

    cat > /etc/systemd/system/dogecoind.service <<EOF
[Unit]
Description=Dogecoin daemon
After=network-online.target local-fs.target
Wants=network-online.target
[Service]
Type=forking
User=root
ExecStart=/usr/local/bin/dogecoind -daemon -datadir=${DOGE_DATADIR} -conf=${DOGE_DATADIR}/dogecoin.conf
ExecStop=/usr/local/bin/dogecoin-cli -datadir=${DOGE_DATADIR} stop
Restart=on-failure
RestartSec=30
TimeoutStartSec=300
TimeoutStopSec=300
[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable dogecoind
    log "dogecoind.service enabled"
}

install_bitcoin() {
    step "Installing Bitcoin Core ${BTC_VERSION}"
    if command -v bitcoind &>/dev/null; then
        log "bitcoind already installed"; return
    fi
    BTC_TAR="bitcoin-${BTC_VERSION}-aarch64-linux-gnu.tar.gz"
    info "Downloading Bitcoin Core ${BTC_VERSION}..."
    wget -q --show-progress \
      "https://bitcoincore.org/bin/bitcoin-core-${BTC_VERSION}/${BTC_TAR}" \
      -O /tmp/${BTC_TAR}
    tar xzf /tmp/${BTC_TAR} -C /tmp/
    install -m 0755 /tmp/bitcoin-${BTC_VERSION}/bin/bitcoind /usr/local/bin/bitcoind
    install -m 0755 /tmp/bitcoin-${BTC_VERSION}/bin/bitcoin-cli /usr/local/bin/bitcoin-cli
    rm -rf /tmp/bitcoin-${BTC_VERSION} /tmp/${BTC_TAR}
    log "bitcoind installed"

    cat > /etc/systemd/system/bitcoind.service <<EOF
[Unit]
Description=Bitcoin daemon
After=network-online.target local-fs.target
Wants=network-online.target
[Service]
Type=forking
User=root
ExecStart=/usr/local/bin/bitcoind -daemon -datadir=${BTC_DATADIR} -conf=${BTC_DATADIR}/bitcoin.conf
ExecStop=/usr/local/bin/bitcoin-cli -datadir=${BTC_DATADIR} stop
Restart=on-failure
RestartSec=30
TimeoutStartSec=300
TimeoutStopSec=300
[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable bitcoind
    log "bitcoind.service enabled"
}

install_bch() {
    step "Installing Bitcoin Cash Node ${BCH_VERSION}"
    if command -v bitcoind-bch &>/dev/null; then
        log "bitcoind-bch already installed"; return
    fi
    BCH_TAR="bitcoin-cash-node-${BCH_VERSION}-aarch64-linux-gnu.tar.gz"
    info "Downloading Bitcoin Cash Node ${BCH_VERSION}..."
    wget -q --show-progress \
      "https://github.com/bitcoin-cash-node/bitcoin-cash-node/releases/download/v${BCH_VERSION}/${BCH_TAR}" \
      -O /tmp/${BCH_TAR}
    tar xzf /tmp/${BCH_TAR} -C /tmp/
    # Install as bitcoind-bch to avoid naming conflict with Bitcoin Core
    install -m 0755 /tmp/bitcoin-cash-node-${BCH_VERSION}/bin/bitcoind /usr/local/bin/bitcoind-bch
    install -m 0755 /tmp/bitcoin-cash-node-${BCH_VERSION}/bin/bitcoin-cli /usr/local/bin/bitcoin-cli-bch
    rm -rf /tmp/bitcoin-cash-node-${BCH_VERSION} /tmp/${BCH_TAR}
    log "bitcoind-bch installed"

    cat > /etc/systemd/system/bchd.service <<EOF
[Unit]
Description=Bitcoin Cash Node daemon
After=network-online.target local-fs.target
Wants=network-online.target
[Service]
Type=forking
User=root
ExecStart=/usr/local/bin/bitcoind-bch -daemon -datadir=${BCH_DATADIR} -conf=${BCH_DATADIR}/bitcoin.conf
ExecStop=/usr/local/bin/bitcoin-cli-bch -datadir=${BCH_DATADIR} stop
Restart=on-failure
RestartSec=30
TimeoutStartSec=300
TimeoutStopSec=300
[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable bchd
    log "bchd.service enabled"
}

install_pepecoin() {
    step "Installing Pepecoin (building from source — ~20 min)"
    if command -v pepecoind &>/dev/null; then
        log "pepecoind already installed"; return
    fi
    apt-get install -y -qq libdb5.3-dev libdb5.3++-dev
    git clone --depth=1 https://github.com/pepecoinppc/pepecoin /tmp/pepecoin-src
    cd /tmp/pepecoin-src
    ./autogen.sh
    ./configure --without-gui --disable-tests --disable-bench \
      --with-incompatible-bdb CXXFLAGS="-O2" CFLAGS="-O2"
    make -j4
    install -m 0755 src/pepecoind /usr/local/bin/
    install -m 0755 src/pepecoin-cli /usr/local/bin/
    cd /
    rm -rf /tmp/pepecoin-src
    log "pepecoind installed"

    cat > /etc/systemd/system/pepecoind.service <<EOF
[Unit]
Description=Pepecoin daemon
After=network-online.target local-fs.target
Wants=network-online.target
[Service]
Type=forking
User=root
ExecStart=/usr/local/bin/pepecoind -daemon -datadir=${PEP_DATADIR} -conf=${PEP_DATADIR}/pepecoin.conf
ExecStop=/usr/local/bin/pepecoin-cli -datadir=${PEP_DATADIR} stop
Restart=on-failure
RestartSec=30
TimeoutStartSec=300
TimeoutStopSec=300
[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable pepecoind
    log "pepecoind.service enabled"
}

if $DO_LTC;  then install_litecoin; fi
if $DO_DOGE; then install_dogecoin; fi
if $DO_BTC;  then install_bitcoin;  fi
if $DO_BCH;  then install_bch;      fi
if $DO_PEP;  then install_pepecoin; fi

# =============================================================================
# STEP 5 — COIN DAEMON CONFIGS
# =============================================================================
step "Writing coin daemon configs"
mkdir -p /root/.litecoin /root/.dogecoin /root/.bitcoin /root/.bch /root/.pepecoin


# Create all datadirs with correct permissions
mkdir -p "$LTC_DATADIR" "$DOGE_DATADIR" "$BTC_DATADIR" "$BCH_DATADIR" "$PEP_DATADIR"
chmod 700 "$LTC_DATADIR" "$DOGE_DATADIR" "$BTC_DATADIR" "$BCH_DATADIR" "$PEP_DATADIR"
log "Blockchain data directories created"

# Write configs — datadir only added when it differs from the daemon's default
# (all daemons read their conf from the datadir, so we point -conf at the right place)
LTC_CONF="${LTC_DATADIR}/litecoin.conf"
DOGE_CONF="${DOGE_DATADIR}/dogecoin.conf"
BTC_CONF="${BTC_DATADIR}/bitcoin.conf"
BCH_CONF="${BCH_DATADIR}/bitcoin.conf"
PEP_CONF="${PEP_DATADIR}/pepecoin.conf"

cat > "$LTC_CONF" <<EOF
server=1
daemon=1
datadir=${LTC_DATADIR}
rpcuser=litecoind
rpcpassword=${LTC_RPC_PASS}
rpcallowip=127.0.0.1
rpcport=9332
zmqpubhashblock=tcp://127.0.0.1:28332
dbcache=512
maxmempool=100
listen=1
EOF

cat > "$DOGE_CONF" <<EOF
server=1
daemon=1
datadir=${DOGE_DATADIR}
rpcuser=dogecoind
rpcpassword=${DOGE_RPC_PASS}
rpcallowip=127.0.0.1
rpcport=22555
zmqpubhashblock=tcp://127.0.0.1:28333
dbcache=256
maxmempool=50
listen=1
EOF

cat > "$BTC_CONF" <<EOF
server=1
daemon=1
datadir=${BTC_DATADIR}
rpcuser=bitcoind
rpcpassword=${BTC_RPC_PASS}
rpcallowip=127.0.0.1
rpcport=8332
zmqpubhashblock=tcp://127.0.0.1:28334
dbcache=512
maxmempool=100
listen=1
EOF

cat > "$BCH_CONF" <<EOF
server=1
daemon=1
datadir=${BCH_DATADIR}
rpcuser=bchd
rpcpassword=${BCH_RPC_PASS}
rpcallowip=127.0.0.1
rpcport=8336
zmqpubhashblock=tcp://127.0.0.1:28335
dbcache=256
maxmempool=50
EOF

cat > "$PEP_CONF" <<EOF
server=1
daemon=1
datadir=${PEP_DATADIR}
rpcuser=pepd
rpcpassword=${PEP_RPC_PASS}
rpcallowip=127.0.0.1
rpcport=33873
dbcache=128
maxmempool=50
listen=1
EOF

log "Coin daemon configs written"

# =============================================================================
# STEP 6 — BUILD PIPOOL
# =============================================================================
step "Building PiPool"
mkdir -p "$PIPOOL_DIR/configs" /var/log/pipool /run/pipool

# Remove any stale duplicate .go files that may have been copied in from
# previous install attempts with mismatched filenames. The canonical filename
# in every package is server.go — any _server.go variant is a leftover copy.
for stale in \
    "${SOURCE_DIR}/internal/dashboard/dashboard_server.go" \
    "${SOURCE_DIR}/internal/stratum/stratum_server.go" \
    "${SOURCE_DIR}/internal/ctl/ctl_server.go"; do
  if [[ -f "$stale" ]]; then
    warn "Removing stale duplicate: $stale"
    rm -f "$stale"
  fi
done

# Build from the source tree — no need to copy the whole repo
cd "$SOURCE_DIR"
go mod tidy 2>/dev/null || true
go build -ldflags="-s -w" -o "${PIPOOL_DIR}/pipool" ./cmd/pipool/
log "pipool binary built → ${PIPOOL_DIR}/pipool"
go build -ldflags="-s -w" -o "${PIPOOL_DIR}/pipoolctl" ./cmd/pipoolctl/
install -m 0755 "${PIPOOL_DIR}/pipoolctl" /usr/local/bin/pipoolctl
log "pipoolctl installed → /usr/local/bin/pipoolctl"

# =============================================================================
# STEP 7 — WRITE pipool.json  (coinbase_tag included)
# =============================================================================
step "Writing pipool.json"
cat > "${PIPOOL_DIR}/configs/pipool.json" <<EOFJSON
{
  "pool": {
    "name": "PiPool",
    "host": "0.0.0.0",
    "coinbase_tag": "${COINBASE_TAG}",
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
        "vardiff": { "min_diff": 0.001, "max_diff": 1024, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": true, "port": 3343, "cert_file": "", "key_file": "" }
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
        "vardiff": { "min_diff": 0.001, "max_diff": 1024, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3344, "cert_file": "", "key_file": "" }
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
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": true, "port": 3345, "cert_file": "", "key_file": "" }
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
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3346, "cert_file": "", "key_file": "" }
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
        "vardiff": { "min_diff": 0.001, "max_diff": 512, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3347, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "127.0.0.1", "port": 33873,
        "user": "pepd", "password": "${PEP_RPC_PASS}"
      },
      "wallet": "${PEP_WALLET}",
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
    "enabled": true,
    "port": 8080,
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
log "Config written — coinbase tag: ${COINBASE_TAG}"

# =============================================================================
# STEP 8 — SYSTEMD SERVICE
# =============================================================================
step "Installing PiPool systemd service"

# Build a Wants= line for whichever coin daemons were installed
DAEMON_WANTS=""
if $DO_LTC;  then DAEMON_WANTS="$DAEMON_WANTS litecoind.service";  fi
if $DO_DOGE; then DAEMON_WANTS="$DAEMON_WANTS dogecoind.service";  fi
if $DO_BTC;  then DAEMON_WANTS="$DAEMON_WANTS bitcoind.service";   fi
if $DO_BCH;  then DAEMON_WANTS="$DAEMON_WANTS bchd.service";       fi
if $DO_PEP;  then DAEMON_WANTS="$DAEMON_WANTS pepecoind.service";  fi
DAEMON_WANTS="${DAEMON_WANTS# }" # trim leading space

cat > /etc/systemd/system/pipool.service <<EOF
[Unit]
Description=PiPool — Raspberry Pi 5 Solo Mining Pool
# Wait for real network (IP assigned) and all filesystems mounted
After=network-online.target local-fs.target${DAEMON_WANTS:+ $DAEMON_WANTS}
Wants=network-online.target${DAEMON_WANTS:+
Wants=$DAEMON_WANTS}

[Service]
Type=simple
User=root
WorkingDirectory=${PIPOOL_DIR}
ExecStart=${PIPOOL_DIR}/pipool -config ${PIPOOL_DIR}/configs/pipool.json
Restart=always
RestartSec=10
TimeoutStartSec=60
StandardOutput=append:/var/log/pipool/pipool.log
StandardError=append:/var/log/pipool/pipool.log

# Recreates /run/pipool on every boot (it lives on tmpfs — gets wiped on reboot)
RuntimeDirectory=pipool
RuntimeDirectoryMode=0755

# RAM guard
MemoryMax=512M
MemoryHigh=400M

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable pipool
log "pipool.service installed and enabled"

# ─── Ensure network-online.target actually waits for network ─────────────────
step "Ensuring network-online.target is active"
if systemctl list-unit-files | grep -q 'NetworkManager-wait-online.service'; then
    systemctl enable NetworkManager-wait-online.service 2>/dev/null && \
        log "NetworkManager-wait-online.service enabled" || \
        warn "NetworkManager-wait-online already active or could not be enabled"
elif systemctl list-unit-files | grep -q 'systemd-networkd-wait-online.service'; then
    systemctl enable systemd-networkd-wait-online.service 2>/dev/null && \
        log "systemd-networkd-wait-online.service enabled" || \
        warn "systemd-networkd-wait-online already active or could not be enabled"
else
    warn "No network-wait service found — boot ordering may be approximate"
fi

# =============================================================================
# STEP 9 — FIREWALL
# =============================================================================
step "Configuring firewall"
ufw allow 22/tcp   comment "SSH"
ufw allow 3333/tcp comment "PiPool LTC Stratum"
ufw allow 3343/tcp comment "PiPool LTC Stratum TLS"
ufw allow 3334/tcp comment "PiPool DOGE Stratum"
ufw allow 3335/tcp comment "PiPool BTC Stratum"
ufw allow 3345/tcp comment "PiPool BTC Stratum TLS"
ufw allow 3336/tcp comment "PiPool BCH Stratum"
ufw allow 8080/tcp comment "PiPool Dashboard"
ufw allow 9100/tcp comment "PiPool Prometheus metrics"
ufw --force enable
log "Firewall configured"

# =============================================================================
# SUMMARY
# =============================================================================
PI_IP=$(hostname -I | awk '{print $1}')

echo ""
echo -e "${BOLD}${GREEN}╔══════════════════════════════════════════════════════╗"
echo -e "║  ✅  PiPool Installation Complete!                   ║"
echo -e "╚══════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "  ${BOLD}Coinbase tag:${NC} ${YELLOW}${COINBASE_TAG}${NC}"
echo -e "  This tag will appear permanently in any block you mine,"
echo -e "  visible on mempool.space and block explorers forever. 🏆"
echo ""
echo -e "${BOLD}Installed daemons:${NC}"
if $DO_LTC;  then echo "  ✔ litecoind    → ${LTC_DATADIR}"; fi
if $DO_DOGE; then echo "  ✔ dogecoind    → ${DOGE_DATADIR}"; fi
if $DO_BTC;  then echo "  ✔ bitcoind     → ${BTC_DATADIR}"; fi
if $DO_BCH;  then echo "  ✔ bitcoind-bch → ${BCH_DATADIR}"; fi
if $DO_PEP;  then echo "  ✔ pepecoind    → ${PEP_DATADIR}"; fi
echo ""
echo -e "${BOLD}Next steps:${NC}"
echo ""
echo "  1. Start your coin daemons and let them fully sync"
echo "     Check progress: litecoin-cli getblockchaininfo | grep verificationprogress"
echo "     (1.000000 = fully synced)"
echo ""
echo "  2. Once synced, start PiPool:"
echo "     sudo systemctl start pipool"
echo "     sudo journalctl -u pipool -f"
echo ""
echo "  3. Point your miner at this Pi:"
echo -e "     ${CYAN}LTC/DOGE plain:${NC}  stratum+tcp://${PI_IP}:3333"
echo -e "     ${CYAN}LTC/DOGE TLS:${NC}    stratum+ssl://${PI_IP}:3343"
echo -e "     ${CYAN}BTC/BCH  plain:${NC}  stratum+tcp://${PI_IP}:3335"
echo -e "     ${CYAN}BTC/BCH  TLS:${NC}    stratum+ssl://${PI_IP}:3345"
echo -e "     ${CYAN}Dashboard:${NC}       http://${PI_IP}:8080"
echo ""
echo "  4. Control the pool:"
echo "     pipoolctl status"
echo "     pipoolctl workers"
echo "     pipoolctl discord test"
echo ""
echo -e "  Config:  ${YELLOW}${PIPOOL_DIR}/configs/pipool.json${NC}"
echo -e "  Logs:    ${YELLOW}/var/log/pipool/pipool.log${NC}"
echo ""
echo -e "${YELLOW}⚠  RAM TIP:${NC} LTC+DOGE ~700MB · BTC+BCH ~800MB · All 4 fit in 8GB."
echo ""
