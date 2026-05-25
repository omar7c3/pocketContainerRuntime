# pocketContainerRuntime

A lightweight educational container runtime demonstrating how Linux namespaces, cgroups, and process isolation work under the hood. Built with clarity and minimalism in mind, **pocketContainerRuntime** shows how modern container engines construct secure, isolated environments using only standard Linux primitives.

---

## Features

- **Namespace isolation** — UTS, PID, mount, network, IPC, and user namespaces
- **Cgroup v2 resource limits** — memory, CPU, PIDs, and file descriptor limits
- **Rootless mode** with UID/GID mapping
- **Isolated mount trees** for private filesystem views
- **Simple CLI** for launching isolated processes
- **WSL2 compatibility** with RLIMIT fallback when cgroups are unavailable

---

## Why This Exists

Modern container engines are powerful but complex. pocketContainerRuntime strips containers down to their essentials so developers can learn:

- How namespaces isolate processes
- How cgroups enforce resource limits
- How rootless containers work
- How a container runtime launches and manages processes

It's ideal for students, educators, and engineers who want to understand containers without the overhead of a full production runtime.

---

## Installation

```bash
git clone https://github.com/omar7c3/pocketContainerRuntime
cd pocketContainerRuntime

# Initialize Go module (required)
go mod init pocketctr
go mod tidy

# Build the runtime
go build -o pocketctr .

# Download and prepare a minimal Alpine Linux root filesystem (required)
# This rootfs will serve as the container's filesystem environment
mkdir rootfs
wget https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.9-x86_64.tar.gz
tar -xzf alpine-minirootfs-3.20.9-x86_64.tar.gz -C rootfs
```

---

## Usage

Run a command inside an isolated container environment:

```bash
./pocketctr run rootfs /bin/sh
```

Enable specific namespaces:

```bash
./pocketctr run --uts --pid --mnt --net --ipc rootfs /bin/sh
```

Run in rootless mode:

```bash
./pocketctr run --user rootfs /bin/sh
```

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

Run it with:

```bash
./namespace-cgroup-test.sh
```

---

## Limitations

- Not a production container runtime
- No OCI image support
- No layered filesystem
- No seccomp or capabilities filtering
- WSL2 uses RLIMITs instead of cgroups
- Cgroup behavior on a full Linux system has not yet been tested and may differ from WSL2 results

---

## License

[MIT License](LICENSE)
