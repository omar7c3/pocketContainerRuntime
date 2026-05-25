#!/bin/bash

ENGINE="./pocketContainerRuntime"
ROOTFS="./rootfs"

# ── ANSI color & style codes ────────────────────────────────────────────────
RESET="\033[0m"
BOLD="\033[1m"
DIM="\033[2m"

BLACK="\033[30m"
RED="\033[31m"
GREEN="\033[32m"
YELLOW="\033[33m"
BLUE="\033[34m"
MAGENTA="\033[35m"
CYAN="\033[36m"
WHITE="\033[37m"

BG_BLACK="\033[40m"
BG_BLUE="\033[44m"
BG_CYAN="\033[46m"

# ── Helper: width ────────────────────────────────────────────────────────────
WIDTH=62

# ── Primitives ───────────────────────────────────────────────────────────────
hline() {
    printf "${DIM}%s${RESET}\n" "$(printf '─%.0s' $(seq 1 $WIDTH))"
}

# Section banner  e.g.  banner "[UTS]" "Hostname Isolation"  CYAN
banner() {
    local tag="$1" title="$2" color="$3"
    echo
    printf "${BOLD}${color}%-8s${RESET}  ${BOLD}%s${RESET}\n" "$tag" "$title"
    hline
}

# Key / value row
row() {
    local key="$1" val="$2" color="${3:-$WHITE}"
    printf "  ${DIM}%-30s${RESET}  ${color}%s${RESET}\n" "$key" "$val"
}

# PASS / FAIL / SKIP badge
badge_pass() { printf "${BOLD}${GREEN}  ✔  PASS${RESET}  %s\n" "$1"; }
badge_fail() { printf "${BOLD}${RED}  ✘  FAIL${RESET}  %s\n" "$1"; }
badge_skip() { printf "${BOLD}${YELLOW}  ⊘  SKIP${RESET}  %s\n" "$1"; }
badge_info() { printf "${BOLD}${CYAN}  ℹ  INFO${RESET}  %s\n" "$1"; }

# Context note
ctx() { printf "  ${DIM}%s${RESET}\n" "$1"; }

# ── Header ───────────────────────────────────────────────────────────────────
echo
printf "${BOLD}${BG_BLUE}${WHITE}  %-${WIDTH}s  ${RESET}\n" "pocketContainerRuntime — Namespace & Cgroup Test"
printf "${DIM}  %-${WIDTH}s${RESET}\n" "namespace-cgroup-test.sh"
echo

# ── 1. UTS ───────────────────────────────────────────────────────────────────
banner "[UTS]" "Hostname Isolation" "$CYAN"
ctx "With --uts: container gets its own hostname (changes stay inside)."
ctx "Without --uts: hostname changes bleed back to the host."
echo

HOST_BEFORE=$(hostname)
WITH_BEFORE=$($ENGINE run --uts $ROOTFS /bin/sh -c "hostname")
WITH_AFTER=$($ENGINE run --uts $ROOTFS /bin/sh -c "hostname uts-demo; hostname")
WITHOUT_BEFORE=$($ENGINE run $ROOTFS /bin/sh -c "hostname")
WITHOUT_AFTER=$($ENGINE run $ROOTFS /bin/sh -c "hostname uts-demo-$RANDOM; hostname")
HOST_AFTER=$(hostname)

row "Host before" "$HOST_BEFORE" "$WHITE"
row "Host after" "$HOST_AFTER" "$WHITE"
echo
row "With --uts  (before)" "$WITH_BEFORE"  "$GREEN"
row "With --uts  (after)"  "$WITH_AFTER"   "$GREEN"
echo
row "Without --uts (before)" "$WITHOUT_BEFORE" "$YELLOW"
row "Without --uts (after)"  "$WITHOUT_AFTER"  "$YELLOW"

echo
# --uts isolation: the hostname set inside the container must NOT match the host's current hostname
if [ "$WITH_AFTER" != "$HOST_AFTER" ]; then
    badge_pass "With --uts: hostname change stayed inside container"
else
    badge_fail "With --uts: hostname leaked to host"
fi
# without --uts: hostname set inside container IS expected to affect the host
if [ "$WITHOUT_AFTER" = "$HOST_AFTER" ]; then
    badge_pass "Without --uts: hostname change visible on host (expected)"
else
    badge_fail "Without --uts: hostname change not reflected on host"
fi

# ── 2. PID ───────────────────────────────────────────────────────────────────
banner "[PID]" "Process Isolation" "$MAGENTA"
ctx "With --pid: container init is PID 1 (full PID namespace)."
ctx "Without --pid: process keeps its host PID."
echo

PID_WITH=$($ENGINE run --pid $ROOTFS /bin/sh -c "echo \$\$")
PID_WITHOUT=$($ENGINE run $ROOTFS /bin/sh -c "echo \$\$")

row "With --pid"    "$PID_WITH"    "$GREEN"
row "Without --pid" "$PID_WITHOUT" "$YELLOW"
echo

if [ "$PID_WITH" = "1" ]; then
    badge_pass "PID inside namespace is 1"
else
    badge_fail "Expected PID 1, got $PID_WITH"
fi

# ── 3. MOUNT ─────────────────────────────────────────────────────────────────
banner "[MNT]" "Mount-Tree Isolation" "$BLUE"
ctx "Tests mount namespace inode — NOT storage isolation."
ctx "With --mnt: inode differs from host. Without --mnt: inode matches."
echo

HOST_MNT=$(readlink /proc/self/ns/mnt)
CONTAINER_WITH_MNT=$($ENGINE run --mnt $ROOTFS /bin/sh -c "readlink /proc/self/ns/mnt" || echo "ERROR")
CONTAINER_WITHOUT_MNT=$($ENGINE run $ROOTFS /bin/sh -c "readlink /proc/self/ns/mnt" || echo "ERROR")

row "Host namespace inode"   "$HOST_MNT"             "$WHITE"
row "With --mnt inode"       "$CONTAINER_WITH_MNT"   "$GREEN"
row "Without --mnt inode"    "$CONTAINER_WITHOUT_MNT" "$YELLOW"
echo

if [ "$HOST_MNT" != "$CONTAINER_WITH_MNT" ] && [ "$CONTAINER_WITH_MNT" != "ERROR" ]; then
    badge_pass "With --mnt: namespace isolated ($CONTAINER_WITH_MNT)"
else
    badge_fail "With --mnt: not isolated or engine crashed"
fi

if [ "$HOST_MNT" = "$CONTAINER_WITHOUT_MNT" ]; then
    badge_pass "Without --mnt: namespace shared with host"
else
    badge_fail "Without --mnt: unexpected namespace leak ($CONTAINER_WITHOUT_MNT)"
fi

# ── 4. NETWORK ───────────────────────────────────────────────────────────────
banner "[NET]" "Network Isolation" "$CYAN"
ctx "With --net: container sees minimal interfaces (lo only)."
ctx "Without --net: container inherits all host interfaces."
echo

NET_WITH=$($ENGINE run --net $ROOTFS /bin/sh -c "ip link show | wc -l")
NET_WITHOUT=$($ENGINE run $ROOTFS /bin/sh -c "ip link show | wc -l")
# ip link show prints 2 lines per interface
IF_WITH=$(( NET_WITH / 2 ))
IF_WITHOUT=$(( NET_WITHOUT / 2 ))

row "With --net"    "$IF_WITH interface(s)"    "$GREEN"
row "Without --net" "$IF_WITHOUT interface(s)" "$YELLOW"
echo

if [ "$IF_WITH" -lt "$IF_WITHOUT" ]; then
    badge_pass "Fewer interfaces inside namespace ($IF_WITH vs $IF_WITHOUT)"
else
    badge_fail "Interface count unchanged — network isolation may have failed"
fi

# ── 5. IPC ───────────────────────────────────────────────────────────────────
banner "[IPC]" "IPC Isolation" "$MAGENTA"
ctx "With --ipc: container sees an empty IPC table."
ctx "Without --ipc: container inherits host IPC queues."
echo

IPC_WITH=$($ENGINE run --ipc $ROOTFS /bin/sh -c "ipcs -q | awk 'NR>2 && \$1 ~ /^0x/' | wc -l")
IPC_WITHOUT=$($ENGINE run $ROOTFS /bin/sh -c "ipcs -q | awk 'NR>2 && \$1 ~ /^0x/' | wc -l")

row "With --ipc"    "$IPC_WITH queue(s)"    "$GREEN"
row "Without --ipc" "$IPC_WITHOUT queue(s)" "$YELLOW"
echo

if [ "$IPC_WITH" = "0" ]; then
    badge_pass "With --ipc: empty IPC namespace"
else
    badge_fail "With --ipc: expected 0 queues, got $IPC_WITH"
fi

# ── 6. USER ──────────────────────────────────────────────────────────────────
banner "[USER]" "UID/GID Mapping Isolation" "$BLUE"
ctx "With --user: namespace inode changes AND uid_map is populated."
ctx "Without --user: user namespace inode matches host."
echo

HOST_USER_NS=$(readlink /proc/self/ns/user)
CONTAINER_WITH_USER=$($ENGINE run --user $ROOTFS /bin/sh -c "readlink /proc/self/ns/user" || echo "ERROR")
CONTAINER_WITH_MAP=$($ENGINE run --user $ROOTFS /bin/sh -c "cat /proc/self/uid_map" || echo "ERROR")
CONTAINER_WITHOUT_USER=$($ENGINE run $ROOTFS /bin/sh -c "readlink /proc/self/ns/user" || echo "ERROR")

row "Host user namespace inode"  "$HOST_USER_NS"          "$WHITE"
row "With --user inode"          "$CONTAINER_WITH_USER"   "$GREEN"
row "Without --user inode"       "$CONTAINER_WITHOUT_USER" "$YELLOW"
echo

if [ "$HOST_USER_NS" != "$CONTAINER_WITH_USER" ] && [ "$CONTAINER_WITH_USER" != "ERROR" ]; then
    if [ -n "$CONTAINER_WITH_MAP" ] && [ "$CONTAINER_WITH_MAP" != "ERROR" ]; then
        badge_pass "With --user: isolated namespace + uid_map present"
    else
        badge_fail "With --user: namespace changed but uid_map missing"
    fi
else
    badge_fail "With --user: not isolated or engine crashed"
fi

if [ "$HOST_USER_NS" = "$CONTAINER_WITHOUT_USER" ]; then
    badge_pass "Without --user: shared host namespace (expected)"
else
    badge_fail "Without --user: unexpected namespace change ($CONTAINER_WITHOUT_USER)"
fi

# ── 7. RESOURCE LIMITS ───────────────────────────────────────────────────────
banner "[RESOURCE]" "Memory / CPU Limit Enforcement" "$YELLOW"
ctx "Real Linux: cgroup v2 limits enforced in-kernel."
ctx "WSL2: no cgroup support — RLIMITs used as fallback."
echo

KERNEL=$(uname -r | tr '[:upper:]' '[:lower:]')
if echo "$KERNEL" | grep -q "microsoft"; then
    IS_WSL2=1
else
    IS_WSL2=0
fi

if [ "$IS_WSL2" -eq 1 ]; then
    badge_info "WSL2 detected — inspecting RLIMITs inside container"
    echo

    RLIMITS=$($ENGINE run $ROOTFS /bin/sh -c \
        'cat /proc/self/limits | grep -E "Max cpu time|Max address space|Max processes|Max open files"' 2>/dev/null)

    if [ -z "$RLIMITS" ]; then
        badge_fail "RLIMIT read failed (container did not launch)"
    else
        badge_pass "RLIMIT read OK"
        echo
        printf "  ${DIM}%-26s %10s  %10s  %s${RESET}\n" "Limit" "Soft" "Hard" "Unit"
        printf "  ${DIM}%s${RESET}\n" "$(printf '─%.0s' $(seq 1 56))"
        while IFS= read -r line; do
            label=$(echo "$line" | cut -c1-25 | sed 's/[[:space:]]*$//')
            soft=$(echo "$line"  | cut -c26-45 | tr -d ' ')
            hard=$(echo "$line"  | cut -c46-65 | tr -d ' ')
            unit=$(echo "$line"  | cut -c66-   | tr -d ' ')
            printf "  ${WHITE}%-26s${RESET} ${GREEN}%10s${RESET}  ${GREEN}%10s${RESET}  ${DIM}%s${RESET}\n" \
                "$label" "$soft" "$hard" "$unit"
        done <<< "$RLIMITS"
        echo
        badge_skip "Cgroup v2 tests not supported on WSL2 — skipped"
    fi
fi

if [ "$IS_WSL2" -eq 0 ]; then
    badge_info "Linux detected — checking cgroup v2 memory.max"
    echo

    CGROUP_PATH=$($ENGINE run $ROOTFS /bin/sh -c \
        'CG=$(grep "^0::" /proc/self/cgroup | cut -d: -f3); echo "$CG"')

    if [ -z "$CGROUP_PATH" ]; then
        badge_fail "Container cgroup path empty — cgroup not mounted?"
    else
        row "Container cgroup path" "$CGROUP_PATH" "$CYAN"
        echo

        if [ -f "/sys/fs/cgroup/$CGROUP_PATH/memory.max" ]; then
            MEM_LIMIT=$(cat "/sys/fs/cgroup/$CGROUP_PATH/memory.max")
            if [ "$MEM_LIMIT" = "104857600" ]; then
                badge_pass "memory.max = $MEM_LIMIT (100 MiB)"
            else
                badge_fail "memory.max = $MEM_LIMIT (expected 104857600)"
            fi
        else
            badge_fail "memory.max file not found at /sys/fs/cgroup/$CGROUP_PATH/"
        fi
    fi
fi

# ── Footer ───────────────────────────────────────────────────────────────────
echo
hline
printf "${BOLD}  %-20s${RESET}\n" "Test run complete"
hline
echo
