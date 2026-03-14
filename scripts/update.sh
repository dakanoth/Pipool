#!/usr/bin/env bash
# Quick update: rebuild binary, push to /opt/pipool, optionally update config
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
PIPOOL_DIR="/opt/pipool"
GREEN='\033[0;32m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

[[ $EUID -ne 0 ]] && { echo "Run as root: sudo bash scripts/update.sh"; exit 1; }

echo -e "${CYAN}${BOLD}Building PiPool...${NC}"
cd "$SOURCE_DIR"
export PATH="/usr/local/go/bin:$PATH"
go build -ldflags="-s -w" -o "${PIPOOL_DIR}/pipool" ./cmd/pipool/
go build -ldflags="-s -w" -o "${PIPOOL_DIR}/pipoolctl" ./cmd/pipoolctl/
install -m 0755 "${PIPOOL_DIR}/pipoolctl" /usr/local/bin/pipoolctl
echo -e "${GREEN}Binary updated.${NC}"

read -p "Also update /opt/pipool/configs/pipool.json from dev config? [y/N]: " UPDATE_CFG
if [[ "$UPDATE_CFG" =~ ^[Yy]$ ]]; then
  cp "${SOURCE_DIR}/configs/pipool.json" "${PIPOOL_DIR}/configs/pipool.json"
  chmod 600 "${PIPOOL_DIR}/configs/pipool.json"
  echo -e "${GREEN}Config updated.${NC}"
fi

echo -e "${CYAN}Restarting pipool service...${NC}"
systemctl restart pipool
sleep 2
systemctl status pipool --no-pager | head -10
echo -e "${GREEN}Done!${NC}"
