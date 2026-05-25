# pocketContainerRuntime

A lightweight educational container runtime demonstrating how Linux namespaces, cgroups, and process isolation work under the hood. Built with clarity and minimalism in mind, **pocketContainerRuntime** shows how modern container engines construct secure, isolated environments using only standard Linux primitives.

---

## Features

- **Namespace isolation** — UTS, PID, mount, network, IPC, and user namespaces
- **Cgroup v2 resource limits** — memory, CPU, PIDs, and file descriptor limits
- **Rootless mode** with UID/GID mapping
- **Isolated mount trees** for private filesystem views
- **Container lifecycle state machine** — tracks `starting → running → stopped / crashed`
- **Restart policies** — `always`, `on-crash`, `never`, `replace`
- **Health probes** — PID-based (any process) or HTTP-based (servers)
- **Persistent state files** — survives across terminal sessions on WSL2
- **`status` and `list` commands** — inspect running and historical containers
- **`clean` command** — granular removal of terminal state files
- **Capability dropping** — strips all Linux capabilities via `capset` after exec
- **WSL2 compatibility** — RLIMIT fallback when cgroups are unavailable

---

## Why This Exists

Modern container engines are powerful but complex. pocketContainerRuntime strips containers down to their essentials so developers can learn:

- How namespaces isolate processes
- How cgroups enforce resource limits
- How rootless containers work
- How a container runtime launches, monitors, and restarts processes
- How state is tracked and managed outside the container process

It's ideal for students, educators, and engineers who want to understand containers without the overhead of a full production runtime.

---

## Installation

```bash
git clone https://github.com/omar7c3/pocketContainerRuntime
cd pocketContainerRuntime

# Initialize Go module (required)
go mod init pocketContainerRuntime
go mod tidy

# Build the runtime
go build -o pocketContainerRuntime .

# Make the test script executable
chmod +x ./namespace-cgroup-test.sh

# Download and prepare a minimal Alpine Linux root filesystem
mkdir rootfs
wget https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.9-x86_64.tar.gz
tar -xzf alpine-minirootfs-3.20.9-x86_64.tar.gz -C rootfs
```

---

## Usage

### Basic run

```bash
./pocketContainerRuntime run rootfs /bin/sh
```

### Enable specific namespaces

```bash
./pocketContainerRuntime run --uts --pid --mnt --net --ipc rootfs /bin/sh
```

### Rootless mode

```bash
./pocketContainerRuntime run --user rootfs /bin/sh
```

### Restart policies

Restart only on non-zero exit (crash):
```bash
./pocketContainerRuntime run --restart=on-crash rootfs /bin/sh
```

Restart on any non-zero exit, up to 5 attempts with exponential backoff:
```bash
./pocketContainerRuntime run --restart=always rootfs /bin/sh
```

Spin up a fresh container with a new ID on each crash:
```bash
./pocketContainerRuntime run --restart=replace rootfs /bin/sh
```

Record result and stop, no restart (default):
```bash
./pocketContainerRuntime run --restart=never rootfs /bin/sh
```

> A clean exit (exit code 0) is always treated as a normal stop regardless of restart policy. The runtime will never restart a process that exited intentionally.

### Health probes

PID probe — works for any process, checks the process is still alive every 5 seconds (default):
```bash
./pocketContainerRuntime run --health=pid rootfs /bin/sh
```

HTTP probe — hits `GET /health` on the given port every 5 seconds, expects HTTP 200:
```bash
./pocketContainerRuntime run --health=http:8080 rootfs /usr/bin/myserver
```

---

## Inspecting Containers

### List all containers

```bash
./pocketContainerRuntime list
```

Example output:
```
ID          STATE       PID     CMD
a1b2c3d4    running     1234    /bin/sh
e5f6a7b8    crashed     0       /usr/bin/myserver
c9d0e1f2    stopped     0       /bin/sh
```

### Inspect a single container

```bash
./pocketContainerRuntime status a1b2c3d4
```

Example output:
```json
{
  "id": "a1b2c3d4",
  "state": "running",
  "pid": 1234,
  "rootfs": "rootfs",
  "cmd": ["/bin/sh"],
  "restart_policy": "on-crash",
  "started_at": "2025-01-01T10:00:00Z"
}
```

---

## Cleaning Up State Files

State files persist in `/var/lib/mycontainer/` until explicitly removed. The `clean` command never touches containers in `running` or `starting` state.

### Remove all terminal state files (stopped + crashed)

```bash
./pocketContainerRuntime clean
```

### Remove only crashed containers

```bash
./pocketContainerRuntime clean --state=crashed
```

### Remove only stopped containers

```bash
./pocketContainerRuntime clean --state=stopped
```

### Remove a specific container by ID

```bash
./pocketContainerRuntime clean a1b2c3d4
```

> Attempting to clean a running or starting container will print an error and exit without removing anything.

---

## Container State Machine

```
STARTING → RUNNING → STOPPED   (clean exit)
                   → CRASHED   (non-zero exit or failed health probe)
                        ↓
                   retrying... (if restart policy allows)
                        ↓
                   RUNNING     (restarted)
```

State files are written to `/var/lib/mycontainer/<id>.json` on every transition. This path persists across WSL2 PowerShell sessions unlike `/var/run` which is a tmpfs and is wiped on instance restart.

---

## Test Suite

### `namespace-cgroup-test.sh`

Validates:
- Namespace isolation
- Cgroup v2 limits
- RLIMIT fallback on WSL2
- User namespace mapping
- Mount propagation
- Network isolation

```bash
./namespace-cgroup-test.sh
```

---

## Limitations

- Not a production container runtime
- No OCI image support — requires a pre-extracted rootfs on disk
- No layered filesystem (no overlayfs)
- No networking wiring — `--net` isolates but does not configure veth/bridge
- No container-to-container communication
- WSL2 uses RLIMITs instead of cgroup filesystem
- Cgroup behaviour on a full Linux host may differ from WSL2 results
- No graceful shutdown — `SIGKILL` only on health probe failure

---

## License

[MIT License](LICENSE)
