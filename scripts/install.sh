#!/usr/bin/env bash
# =============================================================================
#  PiPool — Install Script for Raspberry Pi 5 (Ubuntu Server)
#  Nodes run on Windows PC via pipool-nodes — Pi runs pool + dashboard only
#  Run as root: sudo bash scripts/install.sh
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

log()  { echo -e "${GREEN}[✔]${NC} $1"; }
warn() { echo -e "${YELLOW}[⚠]${NC} $1"; }
err()  { echo -e "${RED}[✖]${NC} $1"; exit 1; }
info() { echo -e "${CYAN}[ℹ]${NC} $1"; }
step() { echo -e "\n${BOLD}${CYAN}══ $1 ══${NC}"; }

[[ $EUID -ne 0 ]] && err "Run as root: sudo bash scripts/install.sh"

echo -e "${CYAN}${BOLD}"
cat <<'BANNER'
  ╔══════════════════════════════════════════════════════╗
  ║   ⛏️  PiPool — Raspberry Pi 5 Solo Mining Pool       ║
  ║   Nodes on Windows PC · Pool + Dashboard on Pi      ║
  ╚══════════════════════════════════════════════════════╝
BANNER
echo -e "${NC}"

PIPOOL_DIR="/opt/pipool"
GO_VERSION="1.21.6"
GOROOT="/usr/local/go"
GOPATH="/root/go"

# =============================================================================
# STEP 1 — COLLECT CONFIG
# =============================================================================
step "Configuration"
echo ""
echo -e "${CYAN}  ── Windows PC Node Connection ──────────────────────────────${NC}"
echo -e "  Run install.ps1 on your Windows PC first to get these values."
echo -e "  They are saved in C:\\PiPoolNodes\\pipool-nodes.json${NC}"
echo ""

read -p "  Windows PC IP address (e.g. 192.168.1.100): " PC_IP
[[ -z "$PC_IP" ]] && err "PC IP is required"

echo ""
echo -e "${CYAN}  ── RPC Passwords (from C:\\PiPoolNodes\\pipool-nodes.json) ──${NC}"
echo ""
read -p "  LTC  RPC password: " LTC_RPC_PASS
read -p "  DOGE RPC password: " DOGE_RPC_PASS
read -p "  BTC  RPC password: " BTC_RPC_PASS
read -p "  BCH  RPC password: " BCH_RPC_PASS
read -p "  DGB  RPC password (leave blank to skip DGB): " DGB_RPC_PASS
DGB_WALLET="YOUR_DGB_WALLET"
DGB_ENABLED="false"
if [[ -n "$DGB_RPC_PASS" ]]; then
  DGB_ENABLED="true"
  read -p "  DGB wallet address (D...): " DGB_WALLET
  [[ -z "$DGB_WALLET" ]] && err "DGB wallet is required when DGB is enabled"
fi

[[ -z "$LTC_RPC_PASS"  ]] && err "LTC RPC password is required"
[[ -z "$DOGE_RPC_PASS" ]] && err "DOGE RPC password is required"
[[ -z "$BTC_RPC_PASS"  ]] && err "BTC RPC password is required"
[[ -z "$BCH_RPC_PASS"  ]] && err "BCH RPC password is required"

echo ""
echo -e "${CYAN}  ── Wallet Addresses ─────────────────────────────────────────${NC}"
echo ""
read -p "  LTC  wallet (L, M, or ltc1...):         " LTC_WALLET
read -p "  DOGE wallet (D...):                     " DOGE_WALLET
read -p "  BTC  wallet (1, 3, or bc1...):          " BTC_WALLET
read -p "  BCH  wallet (q... or bitcoincash:...):  " BCH_WALLET

[[ -z "$LTC_WALLET"  ]] && err "LTC wallet is required"
[[ -z "$DOGE_WALLET" ]] && err "DOGE wallet is required"
[[ -z "$BTC_WALLET"  ]] && err "BTC wallet is required"
[[ -z "$BCH_WALLET"  ]] && err "BCH wallet is required"

echo ""
echo -e "${CYAN}  ── PEP (Pepecoin) ────────────────────────────────────────────${NC}"
read -p "  Enable PEP mining? [y/N]: " ENABLE_PEP
PEP_RPC_PASS=""
PEP_WALLET="YOUR_PEP_WALLET"
PEP_ENABLED="false"
if [[ "$ENABLE_PEP" =~ ^[Yy]$ ]]; then
  PEP_ENABLED="true"
  read -p "  PEP RPC password (from pipool-nodes.json): " PEP_RPC_PASS
  read -p "  PEP wallet address: " PEP_WALLET
  [[ -z "$PEP_RPC_PASS" ]] && err "PEP RPC password is required"
fi

echo ""
echo -e "${CYAN}  ── LCC (Litecoin Cash) ───────────────────────────────────────${NC}"
read -p "  Enable LCC mining? [y/N]: " ENABLE_LCC
LCC_RPC_PASS=""
LCC_WALLET="YOUR_LCC_WALLET"
LCC_ENABLED="false"
if [[ "$ENABLE_LCC" =~ ^[Yy]$ ]]; then
  LCC_ENABLED="true"
  read -p "  LCC RPC password (from pipool-nodes.json): " LCC_RPC_PASS
  read -p "  LCC wallet address: " LCC_WALLET
  [[ -z "$LCC_RPC_PASS" ]] && err "LCC RPC password is required"
fi

echo ""
echo -e "${CYAN}  ── Pool Identity ─────────────────────────────────────────────${NC}"
echo ""
read -p "  Discord webhook URL (leave blank to skip): " DISCORD_WEBHOOK
echo ""
echo -e "${CYAN}  Coinbase tag — permanently embedded in any block you find."
echo -e "  Visible on mempool.space forever.  Max 20 chars. e.g. /PiPool/${NC}"
read -p "  Coinbase tag [/PiPool/]: " COINBASE_TAG_INPUT
COINBASE_TAG="${COINBASE_TAG_INPUT:-/PiPool/}"
COINBASE_TAG=$(echo "$COINBASE_TAG" | tr -d '\n' | cut -c1-20)
echo ""
echo -e "  Tag set to: ${YELLOW}${COINBASE_TAG}${NC}"

echo ""
echo -e "${CYAN}  ── Kiosk Display ─────────────────────────────────────────────${NC}"
echo -e "  Auto-launch dashboard on HDMI/touchscreen at boot.${NC}"
read -p "  Enable kiosk display? [Y/n]: " ENABLE_KIOSK

# =============================================================================
# STEP 2 — SYSTEM PACKAGES
# =============================================================================
step "Installing system packages"
apt-get update -qq
apt-get install -y -qq \
  build-essential git curl wget tar unzip \
  openssl jq htop screen ufw
log "Core packages installed"

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
# STEP 4 — BUILD PIPOOL
# =============================================================================
step "Building PiPool"
mkdir -p "$PIPOOL_DIR/configs" /var/log/pipool /run/pipool

# Remove stale duplicate .go files from previous installs
for stale in \
    "${SOURCE_DIR}/internal/dashboard/dashboard_server.go" \
    "${SOURCE_DIR}/internal/stratum/stratum_server.go" \
    "${SOURCE_DIR}/internal/ctl/ctl_server.go"; do
  if [[ -f "$stale" ]]; then
    warn "Removing stale duplicate: $stale"
    rm -f "$stale"
  fi
done

cd "$SOURCE_DIR"
go mod tidy 2>/dev/null || true
go build -ldflags="-s -w" -o "${PIPOOL_DIR}/pipool" ./cmd/pipool/
log "pipool binary built → ${PIPOOL_DIR}/pipool"
go build -ldflags="-s -w" -o "${PIPOOL_DIR}/pipoolctl" ./cmd/pipoolctl/
install -m 0755 "${PIPOOL_DIR}/pipoolctl" /usr/local/bin/pipoolctl
log "pipoolctl installed → /usr/local/bin/pipoolctl"

# =============================================================================
# STEP 5 — WRITE pipool.json
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
      "datadir": "",
      "stratum": {
        "port": 3333,
        "vardiff": { "min_diff": 0.001, "max_diff": 1024, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3343, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "${PC_IP}", "port": 9332,
        "user": "litecoind", "password": "${LTC_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://${PC_IP}:28332"
      },
      "wallet": "${LTC_WALLET}",
      "block_reward": 6.25
    },
    "DOGE": {
      "enabled": true,
      "symbol": "DOGE",
      "algorithm": "scrypt",
      "merge_parent": "LTC",
      "datadir": "",
      "stratum": {
        "port": 3334,
        "vardiff": { "min_diff": 0.001, "max_diff": 1024, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3344, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "${PC_IP}", "port": 22555,
        "user": "dogecoind", "password": "${DOGE_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://${PC_IP}:28333"
      },
      "wallet": "${DOGE_WALLET}",
      "block_reward": 10000
    },
    "BTC": {
      "enabled": true,
      "symbol": "BTC",
      "algorithm": "sha256d",
      "merge_parent": "",
      "datadir": "",
      "stratum": {
        "port": 3335,
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3345, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "${PC_IP}", "port": 8332,
        "user": "bitcoind", "password": "${BTC_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://${PC_IP}:28334"
      },
      "wallet": "${BTC_WALLET}",
      "block_reward": 3.125
    },
    "BCH": {
      "enabled": true,
      "symbol": "BCH",
      "algorithm": "sha256d",
      "merge_parent": "BTC",
      "datadir": "",
      "stratum": {
        "port": 3336,
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3346, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "${PC_IP}", "port": 8336,
        "user": "bchd", "password": "${BCH_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://${PC_IP}:28335"
      },
      "wallet": "${BCH_WALLET}",
      "block_reward": 6.25
    },
    "DGB": {
      "enabled": ${DGB_ENABLED},
      "symbol": "DGB",
      "algorithm": "sha256d",
      "merge_parent": "",
      "datadir": "",
      "stratum": {
        "port": 3339,
        "vardiff": { "min_diff": 65536, "max_diff": 8388608, "target_ms": 30000, "retarget_s": 90 },
        "tls": { "enabled": false, "port": 3349, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "${PC_IP}", "port": 14022,
        "user": "digibyted", "password": "${DGB_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://${PC_IP}:28338"
      },
      "wallet": "${DGB_WALLET}",
      "block_reward": 665
    },
    "PEP": {
      "enabled": ${PEP_ENABLED},
      "symbol": "PEP",
      "algorithm": "scryptn",
      "merge_parent": "",
      "datadir": "",
      "stratum": {
        "port": 3337,
        "vardiff": { "min_diff": 0.001, "max_diff": 512, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3347, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "${PC_IP}", "port": 33873,
        "user": "pepd", "password": "${PEP_RPC_PASS}"
      },
      "wallet": "${PEP_WALLET}",
    },
    "LCC": {
      "enabled": ${LCC_ENABLED},
      "symbol": "LCC",
      "algorithm": "sha256d",
      "merge_parent": "BTC",
      "datadir": "",
      "stratum": {
        "port": 3338,
        "vardiff": { "min_diff": 1, "max_diff": 1048576, "target_ms": 30000, "retarget_s": 60 },
        "tls": { "enabled": false, "port": 3348, "cert_file": "", "key_file": "" }
      },
      "node": {
        "host": "${PC_IP}", "port": 62457,
        "user": "lccd", "password": "${LCC_RPC_PASS}",
        "zmq_pub_hashblock": "tcp://${PC_IP}:28336"
      },
      "wallet": "${LCC_WALLET}",
      "block_reward": 250
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
log "Config written → ${PIPOOL_DIR}/configs/pipool.json"

# =============================================================================
# STEP 6 — PIPOOL SYSTEMD SERVICE
# =============================================================================
step "Installing PiPool systemd service"

cat > /etc/systemd/system/pipool.service <<EOF
[Unit]
Description=PiPool — Raspberry Pi 5 Solo Mining Pool
After=network-online.target local-fs.target
Wants=network-online.target

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
RuntimeDirectory=pipool
RuntimeDirectoryMode=0755
MemoryMax=512M
MemoryHigh=400M

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable pipool
log "pipool.service installed and enabled"

# ─── Ensure network-online.target waits for real network ─────────────────────
step "Ensuring network-online.target is active"
if systemctl list-unit-files | grep -q 'NetworkManager-wait-online.service'; then
    systemctl enable NetworkManager-wait-online.service 2>/dev/null && \
        log "NetworkManager-wait-online.service enabled" || \
        warn "Already active or could not be enabled"
elif systemctl list-unit-files | grep -q 'systemd-networkd-wait-online.service'; then
    systemctl enable systemd-networkd-wait-online.service 2>/dev/null && \
        log "systemd-networkd-wait-online.service enabled" || \
        warn "Already active or could not be enabled"
else
    warn "No network-wait service found — boot ordering may be approximate"
fi

# =============================================================================
# STEP 7 — KIOSK DISPLAY
# =============================================================================
if [[ "$ENABLE_KIOSK" =~ ^[Nn]$ ]]; then
    info "Kiosk display skipped"
else
    step "Setting up kiosk display"

    apt-get install -y -qq xorg chromium-browser unclutter x11-xserver-utils openbox
    log "X11 + Chromium installed"

    cat > "${PIPOOL_DIR}/kiosk.sh" <<'KIOSK'
#!/bin/bash
# Disable screen blanking
xset s off
xset s noblank
xset -dpms

# Hide cursor after 1s idle
unclutter -idle 1 -root &

# Wait for PiPool dashboard to be ready
until curl -sf http://localhost:8080 > /dev/null 2>&1; do
    sleep 2
done

# Launch Chromium in kiosk mode
chromium-browser \
  --kiosk \
  --no-sandbox \
  --disable-infobars \
  --disable-translate \
  --disable-features=TranslateUI \
  --noerrdialogs \
  --disable-session-crashed-bubble \
  --check-for-update-interval=31536000 \
  http://localhost:8080
KIOSK
    chmod +x "${PIPOOL_DIR}/kiosk.sh"
    log "kiosk.sh written"

    cat > /etc/systemd/system/pipool-kiosk.service <<EOF
[Unit]
Description=PiPool Dashboard Kiosk
After=pipool.service graphical.target
Wants=pipool.service

[Service]
Type=simple
User=root
Environment=DISPLAY=:0
Environment=XAUTHORITY=/root/.Xauthority
ExecStartPre=/bin/sleep 5
ExecStart=/usr/bin/startx ${PIPOOL_DIR}/kiosk.sh
Restart=on-failure
RestartSec=15

[Install]
WantedBy=graphical.target
EOF

    systemctl daemon-reload
    systemctl enable pipool-kiosk
    log "pipool-kiosk.service installed and enabled"

    systemctl set-default graphical.target
    log "Boot target set to graphical.target"
fi

# =============================================================================
# STEP 8 — FIREWALL
# =============================================================================
step "Configuring firewall"
ufw allow 22/tcp   comment "SSH"
ufw allow 3333/tcp comment "PiPool LTC Stratum"
ufw allow 3334/tcp comment "PiPool DOGE Stratum"
ufw allow 3335/tcp comment "PiPool BTC Stratum"
ufw allow 3336/tcp comment "PiPool BCH Stratum"
ufw allow 3337/tcp comment "PiPool PEP Stratum"
ufw allow 3338/tcp comment "PiPool LCC Stratum"
ufw allow 3339/tcp comment "PiPool DGB Stratum"
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
echo -e "  Permanently embedded in every block you find. 🏆"
echo ""
echo -e "${BOLD}Node connection:${NC}"
echo -e "  Windows PC:  ${YELLOW}${PC_IP}${NC}"
echo -e "  LTC RPC:     ${PC_IP}:9332"
echo -e "  DOGE RPC:    ${PC_IP}:22555"
echo -e "  BTC RPC:     ${PC_IP}:8332"
echo -e "  BCH RPC:     ${PC_IP}:8336"
if [[ "$DGB_ENABLED" == "true" ]]; then
echo -e "  DGB RPC:     ${PC_IP}:14022"
fi
if [[ "$PEP_ENABLED" == "true" ]]; then
echo -e "  PEP RPC:     ${PC_IP}:33873"
fi
if [[ "$LCC_ENABLED" == "true" ]]; then
echo -e "  LCC RPC:     ${PC_IP}:62457"
fi
echo ""
echo -e "${BOLD}Next steps:${NC}"
echo ""
echo "  1. On your Windows PC — make sure nodes are running:"
echo "     .\\start-nodes.ps1"
echo "     .\\status-nodes.ps1"
echo ""
echo "  2. Start PiPool:"
echo "     sudo systemctl start pipool"
echo "     sudo journalctl -u pipool -f"
echo ""
echo "  3. Point your miner at this Pi:"
echo -e "     ${CYAN}LTC/DOGE:${NC}   stratum+tcp://${PI_IP}:3333"
echo -e "     ${CYAN}BTC/BCH:${NC}    stratum+tcp://${PI_IP}:3335"
if [[ "$DGB_ENABLED" == "true" ]]; then
echo -e "     ${CYAN}DGB:${NC}        stratum+tcp://${PI_IP}:3339"
fi
if [[ "$PEP_ENABLED" == "true" ]]; then
echo -e "     ${CYAN}PEP:${NC}        stratum+tcp://${PI_IP}:3337"
fi
if [[ "$LCC_ENABLED" == "true" ]]; then
echo -e "     ${CYAN}LCC:${NC}        stratum+tcp://${PI_IP}:3338"
fi
echo -e "     ${CYAN}Dashboard:${NC}  http://${PI_IP}:8080"
echo ""
echo "  4. Control the pool:"
echo "     pipoolctl status"
echo "     pipoolctl workers"
echo "     pipoolctl discord test"
echo ""
echo -e "  Config:  ${YELLOW}${PIPOOL_DIR}/configs/pipool.json${NC}"
echo -e "  Logs:    ${YELLOW}/var/log/pipool/pipool.log${NC}"
if [[ ! "$ENABLE_KIOSK" =~ ^[Nn]$ ]]; then
echo ""
echo -e "  ${CYAN}Kiosk display:${NC} Dashboard will appear on screen after:"
echo "     sudo reboot"
fi
echo ""
