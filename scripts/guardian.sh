#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────────────
# PiPool Guardian — autonomous watchdog for stratum uptime
#
# Priorities (in order):
#   1. pipool stratum MUST be running and accepting connections
#   2. Protect RAM — kill digibyted before pipool gets OOM-killed
#   3. Restart pipool if it crashes
#   4. Restart digibyted if there's headroom
#
# Runs every 30s via systemd timer. Zero external dependencies.
# ──────────────────────────────────────────────────────────────────────────────

set -euo pipefail

LOG_TAG="[guardian]"
PIPOOL_SERVICE="pipool"
DIGIBYTED_SERVICE="digibyted"  # systemd user service (dakota)
DIGIBYTED_USER="dakota"

# RAM thresholds (in MB)
RAM_CRITICAL=400     # below this: kill digibyted immediately
RAM_WARNING=800      # below this: log warning, prepare to act
PIPOOL_MEM_MAX=500   # if pipool exceeds this MB, it's leaking — restart it

# Stratum ports to health-check (at least one must respond)
# These are the primary coins — if none respond, pipool is broken
STRATUM_PORTS=(3333 3335 3339 3342)

# State file for tracking repeated failures
STATE_DIR="/run/pipool"
FAIL_COUNT_FILE="$STATE_DIR/guardian_fails"

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') $LOG_TAG $*" | tee -a /var/log/pipool/guardian.log
}

# ── Helper: available RAM in MB (free + buffers/cache) ────────────────────────
available_ram_mb() {
    awk '/MemAvailable/ { printf "%.0f", $2/1024 }' /proc/meminfo
}

# ── Helper: RSS of a process in MB ────────────────────────────────────────────
process_rss_mb() {
    local name="$1"
    ps -C "$name" -o rss= 2>/dev/null | awk '{ sum += $1 } END { printf "%.0f", sum/1024 }' || echo 0
}

# ── Helper: is pipool systemd service active? ─────────────────────────────────
pipool_running() {
    systemctl is-active --quiet "$PIPOOL_SERVICE" 2>/dev/null
}

# ── Helper: is digibyted running? (user service) ─────────────────────────────
digibyted_running() {
    pgrep -x digibyted >/dev/null 2>&1
}

# ── Helper: can a miner TCP-connect to at least one stratum port? ─────────────
stratum_healthy() {
    for port in "${STRATUM_PORTS[@]}"; do
        if timeout 3 bash -c "echo | nc -w2 127.0.0.1 $port" >/dev/null 2>&1; then
            return 0
        fi
    done
    return 1
}

# ── Helper: track consecutive failures ────────────────────────────────────────
get_fail_count() {
    if [[ -f "$FAIL_COUNT_FILE" ]]; then
        cat "$FAIL_COUNT_FILE"
    else
        echo 0
    fi
}

set_fail_count() {
    mkdir -p "$STATE_DIR"
    echo "$1" > "$FAIL_COUNT_FILE"
}

# ── Helper: kill digibyted to protect pipool ──────────────────────────────────
kill_digibyted() {
    local reason="$1"
    log "KILLING digibyted — $reason"

    # Try graceful stop first (5s timeout)
    if sudo -u "$DIGIBYTED_USER" XDG_RUNTIME_DIR="/run/user/$(id -u $DIGIBYTED_USER)" \
       systemctl --user stop "$DIGIBYTED_SERVICE" 2>/dev/null; then
        log "digibyted stopped gracefully via systemd"
    else
        # Force kill
        pkill -9 -x digibyted 2>/dev/null && log "digibyted force-killed" || true
    fi

    # Disable so it doesn't auto-restart and eat RAM again
    sudo -u "$DIGIBYTED_USER" XDG_RUNTIME_DIR="/run/user/$(id -u $DIGIBYTED_USER)" \
       systemctl --user disable "$DIGIBYTED_SERVICE" 2>/dev/null || true
    log "digibyted disabled — manual re-enable required: systemctl --user enable --now digibyted"
}

# ══════════════════════════════════════════════════════════════════════════════
# MAIN WATCHDOG LOGIC
# ══════════════════════════════════════════════════════════════════════════════

avail_mb=$(available_ram_mb)
pipool_mb=$(process_rss_mb pipool)
dgb_mb=$(process_rss_mb digibyted)

# ── CHECK 1: RAM critical — protect pipool at all costs ───────────────────────
if (( avail_mb < RAM_CRITICAL )); then
    log "CRITICAL: available RAM ${avail_mb}MB < ${RAM_CRITICAL}MB threshold"

    if digibyted_running; then
        kill_digibyted "available RAM ${avail_mb}MB critically low (dgb using ${dgb_mb}MB)"
        sleep 2
        avail_mb=$(available_ram_mb)
        log "After killing digibyted: ${avail_mb}MB available"
    fi

    # If still critical after killing digibyted, restart pipool (possible leak)
    if (( $(available_ram_mb) < RAM_CRITICAL )); then
        log "Still critical after digibyted kill — restarting pipool (possible memory leak)"
        systemctl restart "$PIPOOL_SERVICE"
    fi
fi

# ── CHECK 2: pipool memory leak detection ─────────────────────────────────────
if (( pipool_mb > PIPOOL_MEM_MAX )); then
    log "WARNING: pipool RSS ${pipool_mb}MB exceeds ${PIPOOL_MEM_MAX}MB — restarting (likely memory leak)"
    systemctl restart "$PIPOOL_SERVICE"
    sleep 3
fi

# ── CHECK 3: is pipool service running? ───────────────────────────────────────
if ! pipool_running; then
    fails=$(get_fail_count)
    fails=$((fails + 1))
    set_fail_count "$fails"
    log "pipool DOWN (failure #$fails) — restarting"
    systemctl restart "$PIPOOL_SERVICE"
    sleep 5

    if pipool_running; then
        log "pipool restarted successfully"
        set_fail_count 0
    else
        log "pipool FAILED to restart (attempt #$fails)"
        # After 3 consecutive failures, kill digibyted to free resources and try again
        if (( fails >= 3 )) && digibyted_running; then
            kill_digibyted "pipool failed to start $fails times — freeing RAM"
            sleep 2
            systemctl restart "$PIPOOL_SERVICE"
            sleep 3
            if pipool_running; then
                log "pipool started after killing digibyted"
                set_fail_count 0
            fi
        fi
    fi
    exit 0
fi

# ── CHECK 4: stratum port health check ────────────────────────────────────────
if ! stratum_healthy; then
    fails=$(get_fail_count)
    fails=$((fails + 1))
    set_fail_count "$fails"
    log "STRATUM UNREACHABLE on all ports (failure #$fails) — pipool is running but not serving"

    if (( fails >= 2 )); then
        log "Restarting pipool — stratum unresponsive for 2+ checks"
        systemctl restart "$PIPOOL_SERVICE"
        sleep 5
        if stratum_healthy; then
            log "Stratum recovered after restart"
            set_fail_count 0
        else
            log "Stratum still dead after restart"
        fi
    fi
    exit 0
fi

# ── CHECK 5: RAM warning — log but don't act yet ─────────────────────────────
if (( avail_mb < RAM_WARNING )); then
    log "WARNING: available RAM ${avail_mb}MB (pipool=${pipool_mb}MB dgb=${dgb_mb}MB) — watching closely"
fi

# ── All clear — reset failure counter ─────────────────────────────────────────
set_fail_count 0
