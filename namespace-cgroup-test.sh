#!/bin/bash

ENGINE="./pocketContainerRuntime"
ROOTFS="./rootfs"
RAND="test-$RANDOM"

echo
echo "==================== FULL NAMESPACE TEST ===================="
echo

line() { printf "%-32s %s\n" "$1" "$2"; }

############################################################
# 1. UTS NAMESPACE
############################################################
echo "[UTS] Hostname isolation"
echo "Context:"
echo " - With --uts: container has its own hostname."
echo " - Without --uts: hostname changes affect host."
echo

HOST_BEFORE=$(hostname)

WITH_BEFORE=$($ENGINE run --uts $ROOTFS /bin/sh -c "hostname")
WITH_AFTER=$($ENGINE run --uts $ROOTFS /bin/sh -c "hostname uts-demo; hostname")

WITHOUT_BEFORE=$($ENGINE run $ROOTFS /bin/sh -c "hostname")
WITHOUT_AFTER=$($ENGINE run $ROOTFS /bin/sh -c "hostname uts-demo-$RANDOM; hostname")

HOST_AFTER=$(hostname)

line "Host before:" "$HOST_BEFORE"
line "Host after:" "$HOST_AFTER"
echo

line "With --uts (before):" "$WITH_BEFORE"
line "With --uts (after):" "$WITH_AFTER"
echo

line "Without --uts (before):" "$WITHOUT_BEFORE"
line "Without --uts (after):" "$WITHOUT_AFTER"
echo

############################################################
# 2. PID NAMESPACE
############################################################
echo "[PID] Process isolation"
echo "Context:"
echo " - With --pid: container init becomes PID 1."
echo " - Without --pid: process has normal host PID."
echo

WITH=$($ENGINE run --pid $ROOTFS /bin/sh -c "echo \$\$")
WITHOUT=$($ENGINE run $ROOTFS /bin/sh -c "echo \$\$")

line "With --pid:" "$WITH"
line "Without --pid:" "$WITHOUT"
echo

############################################################
# 3. MOUNT NAMESPACE (INODE TEST)
############################################################
echo "[MNT] Mount-tree isolation (NOT storage isolation)"
echo "Context:"
echo " - With --mnt: Inode should DIFFERENT from the host."
echo " - Without --mnt: Inode should MATCH the host."
echo

# 1. Capture the Host's true Mount Namespace Inode
HOST_MNT=$(readlink /proc/self/ns/mnt)

# 2. Capture the Container's Mount Namespace Inode (with and without --mnt)
CONTAINER_WITH_MNT=$($ENGINE run --mnt $ROOTFS /bin/sh -c "readlink /proc/self/ns/mnt" || echo "ERROR")
CONTAINER_WITHOUT_MNT=$($ENGINE run $ROOTFS /bin/sh -c "readlink /proc/self/ns/mnt" || echo "ERROR")

# 3. Evaluate results safely using string comparison
if [ "$HOST_MNT" = "$CONTAINER_WITHOUT_MNT" ]; then
    WITHOUT_RESULT="PASS (Shared with host: $CONTAINER_WITHOUT_MNT)"
else
    WITHOUT_RESULT="FAIL (Leaked/Mutated namespace: $CONTAINER_WITHOUT_MNT)"
fi

if [ "$HOST_MNT" != "$CONTAINER_WITH_MNT" ] && [ "$CONTAINER_WITH_MNT" != "ERROR" ]; then
    WITH_RESULT="PASS (Isolated: $CONTAINER_WITH_MNT)"
else
    WITH_RESULT="FAIL (Not isolated or Engine crashed)"
fi

# Output clean, descriptive lines
line "Host Namespace Inode:" "$HOST_MNT"
line "With --mnt implementation:"    "$WITH_RESULT"
line "Without --mnt implementation:" "$WITHOUT_RESULT"
echo


############################################################
# 4. NETWORK NAMESPACE
############################################################
echo "[NET] Network isolation"
echo "Context:"
echo " - With --net: container sees minimal interfaces."
echo " - Without --net: container sees host interfaces."
echo

WITH=$($ENGINE run --net $ROOTFS /bin/sh -c "ip link show | wc -l")
WITHOUT=$($ENGINE run $ROOTFS /bin/sh -c "ip link show | wc -l")

line "With --net:" "$WITH interfaces"
line "Without --net:" "$WITHOUT interfaces"
echo

############################################################
# 5. IPC NAMESPACE
############################################################
echo "[IPC] IPC isolation"
echo "Context:"
echo " - With --ipc: container sees empty IPC tables."
echo " - Without --ipc: container sees host IPC queues."
echo


WITH=$($ENGINE run --ipc $ROOTFS /bin/sh -c "ipcs -q | awk 'NR>2 && \$1 ~ /^0x/' | wc -l")
WITHOUT=$($ENGINE run $ROOTFS /bin/sh -c "ipcs -q | awk 'NR>2 && \$1 ~ /^0x/' | wc -l")

line "With --ipc:" "$WITH queues"
line "Without --ipc:" "$WITHOUT queues"
echo

############################################################
# 6. USER NAMESPACE (INODE & MAP TEST)
############################################################
echo "[USER] UID/GID mapping isolation"
echo "Context:"
echo " - With --user: user namespace inode changes AND a mapping exists."
echo " - Without --user: user namespace matches host inode."
echo

# 1. Capture the Host's true User Namespace Inode
HOST_USER_NS=$(readlink /proc/self/ns/user)

# 2. Capture the Container's User Namespace details (with and without --user)
CONTAINER_WITH_USER=$($ENGINE run --user $ROOTFS /bin/sh -c "readlink /proc/self/ns/user" || echo "ERROR")
CONTAINER_WITH_MAP=$($ENGINE run --user $ROOTFS /bin/sh -c "cat /proc/self/uid_map" || echo "ERROR")
CONTAINER_WITHOUT_USER=$($ENGINE run $ROOTFS /bin/sh -c "readlink /proc/self/ns/user" || echo "ERROR")

# 3. Evaluate results securely using string comparison
if [ "$HOST_USER_NS" = "$CONTAINER_WITHOUT_USER" ]; then
    WITHOUT_RESULT="PASS (Shared with host: $CONTAINER_WITHOUT_USER)"
else
    WITHOUT_RESULT="FAIL (Leaked/Mutated namespace: $CONTAINER_WITHOUT_USER)"
fi

if [ "$HOST_USER_NS" != "$CONTAINER_WITH_USER" ] && [ "$CONTAINER_WITH_USER" != "ERROR" ]; then
    # Ensure uid_map is populated and valid (not empty or errored)
    if [ -n "$CONTAINER_WITH_MAP" ] && [ "$CONTAINER_WITH_MAP" != "ERROR" ]; then
        WITH_RESULT="PASS (Isolated User NS: $CONTAINER_WITH_USER)"
    else
        WITH_RESULT="FAIL (Namespace changed but uid_map missing/empty)"
    fi
else
    WITH_RESULT="FAIL (Not isolated or Engine crashed)"
fi

# Output clean, descriptive lines
line "Host User Namespace Inode:" "$HOST_USER_NS"
line "With --user implementation:"   "$WITH_RESULT"
line "Without --user implementation:" "$WITHOUT_RESULT"
echo

############################################################
# 7. RESOURCE LIMIT TEST (CGROUP v2 on Linux, RLIMIT on WSL2)
############################################################
echo "[RESOURCE] Memory/CPU limit enforcement"
echo "Context:"
echo " - Real Linux: cgroup v2 limits apply even with --user."
echo " - WSL2: no cgroups; only RLIMITs can be inspected."
echo " - Test always runs inside a fresh container."
echo

# Detect WSL2 by kernel string
KERNEL=$(uname -r | tr '[:upper:]' '[:lower:]')
if echo "$KERNEL" | grep -q "microsoft"; then
    IS_WSL2=1
else
    IS_WSL2=0
fi

if [ "$IS_WSL2" -eq 1 ]; then
    echo "[WSL2 MODE] Inspecting RLIMITs inside container"
    RLIMITS=$($ENGINE run $ROOTFS /bin/sh -c 'cat /proc/self/limits | grep -E "Max cpu time|Max address space|Max processes|Max open files"' 2>/dev/null)

    if [ -z "$RLIMITS" ]; then
        line "RLIMIT read:" "FAIL (container failed)"
    else
        line "RLIMIT read:" "OK (see below)"
        echo "$RLIMITS"
    fi

    echo
    echo "croup tests will be skipped (Not supported in WSL MODE)"
    echo
fi

if [ "$IS_WSL2" -eq 0 ]; then
    echo "[LINUX MODE] Checking cgroup v2 memory.max"

    # Get container's cgroup path from inside
    CGROUP_PATH=$($ENGINE run $ROOTFS /bin/sh -c '
    CG=$(grep "^0::" /proc/self/cgroup | cut -d: -f3)
    echo "$CG"
    ')

    if [ -z "$CGROUP_PATH" ]; then
        line "Container cgroup path:" "FAIL (empty)"
        echo
        return
    fi

    line "Container cgroup path:" "$CGROUP_PATH"

    # Check memory.max on host
    if [ -f "/sys/fs/cgroup/$CGROUP_PATH/memory.max" ]; then
        MEM_LIMIT=$(cat "/sys/fs/cgroup/$CGROUP_PATH/memory.max")
        if [ "$MEM_LIMIT" = "104857600" ]; then
            RESULT="PASS (memory.max = $MEM_LIMIT)"
        else
            RESULT="FAIL (memory.max = $MEM_LIMIT)"
        fi
    else
        RESULT="FAIL (memory.max missing)"
    fi

    line "Cgroup memory limit:" "$RESULT"
    echo
fi

echo "==================== TEST COMPLETE ====================="
