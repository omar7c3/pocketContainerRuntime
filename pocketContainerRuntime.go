package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type nsConfig struct {
	uts  bool
	pid  bool
	mnt  bool
	ipc  bool
	net  bool
	user bool
}

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		log.Fatalf("usage: %s run [ns flags] <rootfs> <cmd> [args...]", os.Args[0])
	}

	switch os.Args[1] {
	case "run":
		run()
	case "child":
		child()
	case "child-debug":
		childDebug()
	case "debug-user":
		debugUser()
	default:
		log.Fatalf("unknown command: %s", os.Args[1])
	}
}

// parseRunArgs parses the arguments after "run":
//
//	mycontainer run [ns flags] <rootfs> <cmd> [args...]
func parseRunArgs(args []string) (nsConfig, string, []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var cfg nsConfig
	fs.BoolVar(&cfg.uts, "uts", false, "enable UTS namespace")
	fs.BoolVar(&cfg.pid, "pid", false, "enable PID namespace")
	fs.BoolVar(&cfg.mnt, "mnt", false, "enable mount namespace")
	fs.BoolVar(&cfg.ipc, "ipc", false, "enable IPC namespace")
	fs.BoolVar(&cfg.net, "net", false, "enable network namespace")
	fs.BoolVar(&cfg.user, "user", false, "enable user namespace")

	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 2 {
		log.Fatalf("usage: mycontainer run [ns flags] <rootfs> <cmd> [args...]")
	}

	rootfs := rest[0]
	cmdArgs := rest[1:]
	return cfg, rootfs, cmdArgs
}

func run() {
	cfg, rootfs, cmdArgs := parseRunArgs(os.Args[2:])

	// Remember the real host UID/GID before we enter any namespaces,
	// so we can write the correct uid/gid mappings below.
	hostUID := os.Getuid()
	hostGID := os.Getgid()

	// A user namespace without a mount namespace is mostly useless for
	// our purposes — pivot_root requires calling process is in its own
	// mount namespace so enable it automatically.
	if cfg.user && !cfg.mnt {
		log.Printf("info: --user requested, enabling --mnt automatically")
		cfg.mnt = true
	}

	// Translate our config struct into the CLONE_NEW* bitmask that
	// SysProcAttr.Cloneflags expects.
	var cloneFlags uintptr
	if cfg.uts {
		cloneFlags |= syscall.CLONE_NEWUTS
	}
	if cfg.pid {
		cloneFlags |= syscall.CLONE_NEWPID
	}
	if cfg.mnt {
		cloneFlags |= syscall.CLONE_NEWNS
	}
	if cfg.ipc {
		cloneFlags |= syscall.CLONE_NEWIPC
	}
	if cfg.net {
		cloneFlags |= syscall.CLONE_NEWNET
	}
	if cfg.user {
		cloneFlags |= syscall.CLONE_NEWUSER
	}

	// We can't configure namespaces after exec, so encode the ns config
	// as a string and pass it to the child process via argv.
	nsSpec := encodeNsConfig(cfg)

	args := []string{"child", nsSpec, rootfs}
	args = append(args, cmdArgs...)

	// Re-exec ourselves as "child" — this is the standard Go trick for
	// running code inside a new namespace after the kernel has set it up.
	cmd := exec.Command("/proc/self/exe", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	sysAttr := &syscall.SysProcAttr{
		Cloneflags: cloneFlags,
	}

	// When using a user namespace, map the container's root (uid 0) back
	// to our real host uid/gid. This keeps file ownership sane while
	// still giving the container process apparent root inside the ns.
	if cfg.user {
		sysAttr.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: hostUID, Size: 1},
		}
		sysAttr.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: hostGID, Size: 1},
		}
		// Disabling setgroups is required by the kernel before we're
		// allowed to write gid_map from an unprivileged process.
		sysAttr.GidMappingsEnableSetgroups = false
	}

	cmd.SysProcAttr = sysAttr

	if err := cmd.Run(); err != nil {
		log.Fatalf("run: %v", err)
	}
}

// encodeNsConfig serialises the enabled namespaces into a comma-separated
// string (e.g. "uts,pid,mnt") that we can pass safely through argv.
func encodeNsConfig(cfg nsConfig) string {
	var parts []string
	if cfg.uts {
		parts = append(parts, "uts")
	}
	if cfg.pid {
		parts = append(parts, "pid")
	}
	if cfg.mnt {
		parts = append(parts, "mnt")
	}
	if cfg.ipc {
		parts = append(parts, "ipc")
	}
	if cfg.net {
		parts = append(parts, "net")
	}
	if cfg.user {
		parts = append(parts, "user")
	}
	return strings.Join(parts, ",")
}

// decodeNsConfig is the inverse of encodeNsConfig.
func decodeNsConfig(s string) nsConfig {
	var cfg nsConfig
	if s == "" {
		return cfg
	}
	for _, p := range strings.Split(s, ",") {
		switch p {
		case "uts":
			cfg.uts = true
		case "pid":
			cfg.pid = true
		case "mnt":
			cfg.mnt = true
		case "ipc":
			cfg.ipc = true
		case "net":
			cfg.net = true
		case "user":
			cfg.user = true
		}
	}
	return cfg
}

// child is the re-exec'd entry point that runs inside the new namespace(s).
// argv layout: mycontainer child <nsSpec> <rootfs> <cmd> [args...]
func child() {
	if len(os.Args) < 5 {
		log.Fatalf("usage: %s child <nsSpec> <rootfs> <cmd> [args...]", os.Args[0])
	}

	cfg := decodeNsConfig(os.Args[2])
	rootfs := os.Args[3]
	cmdArgs := os.Args[4:]

	if cfg.uts {
		if err := syscall.Sethostname([]byte("edu-container")); err != nil {
			log.Fatalf("sethostname: %v", err)
		}
	}

	if cfg.mnt {
		// Make the mount namespace truly private so that nothing we do
		// here leaks back to the host's mount table.
		if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
			log.Fatalf("remount / private: %v", err)
		}
	}

	if err := setupCgroupV2(); err != nil {
		log.Printf("warning: cgroup v2 setup failed: %v", err)
	}

	// Choose the right filesystem isolation strategy:
	//   mnt only  → full pivot_root (cleanest; old root is unmounted)
	//   mnt+user  → simple chroot (pivot_root needs a real mount ns we
	//               can't always get inside an unprivileged user ns)
	//   neither   → simple chroot (best we can do without a mount ns)
	switch {
	case cfg.mnt && !cfg.user:
		if err := pivotRoot(rootfs); err != nil {
			log.Fatalf("pivot_root: %v", err)
		}
	default:
		if err := pivotRootSimple(rootfs); err != nil {
			log.Fatalf("simple chroot: %v", err)
		}
	}

	if err := syscall.Chdir("/"); err != nil {
		log.Fatalf("chdir /: %v", err)
	}

	// Mount a fresh /proc so tools like ps and top see the container's
	// PID namespace rather than the host's. Inside an unprivileged user
	// namespace this can fail on some kernels, so we only treat it as
	// fatal when we're not in a user namespace.
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		if !cfg.user {
			log.Fatalf("mount /proc: %v", err)
		}
		// Best-effort: the process still works, it just sees the host /proc.
	}

	dropCapabilities()

	if err := syscall.Exec(cmdArgs[0], cmdArgs, os.Environ()); err != nil {
		log.Fatalf("exec failed: %v", err)
	}
}

func pivotRoot(rootfs string) error {
	absRoot, err := filepath.Abs(rootfs)
	if err != nil {
		return fmt.Errorf("abs rootfs: %w", err)
	}

	// Bind-mount the rootfs onto itself so it becomes a mount point,
	// which is a prerequisite for pivot_root.
	if err := syscall.Mount(absRoot, absRoot, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount rootfs: %w", err)
	}

	// pivot_root needs a directory inside the new root to park the old root.
	oldRoot := filepath.Join(absRoot, ".oldroot")
	if err := os.MkdirAll(oldRoot, 0700); err != nil {
		return fmt.Errorf("mkdir oldroot: %w", err)
	}

	if err := syscall.PivotRoot(absRoot, oldRoot); err != nil {
		return fmt.Errorf("pivot_root failed: %w", err)
	}

	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}

	// Detach the old root so the container can't access the host filesystem.
	if err := syscall.Unmount("/.oldroot", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount oldroot: %w", err)
	}

	if err := os.RemoveAll("/.oldroot"); err != nil {
		return fmt.Errorf("remove oldroot: %w", err)
	}

	return nil
}

// pivotRootSimple falls back to a plain chroot when a full pivot_root isn't
// available (no mount namespace, or running inside a user namespace).
// We intentionally skip the bind-mount here because that would require
// privilege we don't have in the unprivileged-user-ns case.
func pivotRootSimple(rootfs string) error {
	absRoot, err := filepath.Abs(rootfs)
	if err != nil {
		return fmt.Errorf("abs rootfs: %w", err)
	}

	if err := syscall.Chdir(absRoot); err != nil {
		return fmt.Errorf("chdir rootfs: %w", err)
	}

	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}

	return nil
}

// setupCgroupV2 picks the right resource-limiting strategy for the host.
// On WSL2 the cgroup filesystem is either absent or read-only for regular
// users, so we fall back to kernel-level rlimits instead.
func setupCgroupV2() error {
	if isWSL2() {
		return setupCgroupV2Kernel()
	}
	return setupCgroupV2Fs()
}

// setupCgroupV2Kernel applies resource limits using prlimit() syscalls,
// which work without any filesystem access and are safe under WSL2.
func setupCgroupV2Kernel() error {
	pid := os.Getpid()

	// Cap virtual address space at 100 MB.
	mem := uint64(100 * 1024 * 1024)
	if err := unix.Prlimit(pid, unix.RLIMIT_AS, &unix.Rlimit{Cur: mem, Max: mem}, nil); err != nil {
		return fmt.Errorf("set RLIMIT_AS: %w", err)
	}

	// Allow at most 2 seconds of CPU time before SIGXCPU fires.
	if err := unix.Prlimit(pid, unix.RLIMIT_CPU, &unix.Rlimit{Cur: 2, Max: 2}, nil); err != nil {
		return fmt.Errorf("set RLIMIT_CPU: %w", err)
	}

	// Prevent fork bombs by capping the number of child processes.
	if err := unix.Prlimit(pid, unix.RLIMIT_NPROC, &unix.Rlimit{Cur: 64, Max: 64}, nil); err != nil {
		return fmt.Errorf("set RLIMIT_NPROC: %w", err)
	}

	// Limit open file descriptors to a reasonable number.
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 1024, Max: 1024}, nil); err != nil {
		return fmt.Errorf("set RLIMIT_NOFILE: %w", err)
	}

	return nil
}

// setupCgroupV2Fs uses the cgroup v2 filesystem hierarchy to constrain the
// container. This is the standard approach on a real Linux host where we
// have write access to /sys/fs/cgroup.
func setupCgroupV2Fs() error {
	const cgroupRoot = "/sys/fs/cgroup"

	pid := os.Getpid()
	name := fmt.Sprintf("edu-container-%d", pid)
	path := filepath.Join(cgroupRoot, name)

	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("mkdir cgroup v2: %w", err)
	}

	// 100 MB in bytes.
	if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte("104857600"), 0644); err != nil {
		return fmt.Errorf("write memory.max: %w", err)
	}

	// Adding our PID to cgroup.procs moves this process (and its future
	// children) into the new cgroup immediately.
	if err := os.WriteFile(filepath.Join(path, "cgroup.procs"), []byte(fmt.Sprintf("%d", pid)), 0644); err != nil {
		return fmt.Errorf("write cgroup.procs: %w", err)
	}

	return nil
}

// dropCapabilities is a placeholder for dropping Linux capabilities via
// prctl + capset. Filling this in would further reduce what a root process
// inside the container can actually do on the host.
func dropCapabilities() {
}

// isWSL2 checks the kernel release string for Microsoft's WSL2 signature.
// We use this to decide whether cgroup fs writes are likely to work.
func isWSL2() bool {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false
	}
	s := strings.ToLower(string(data))
	return strings.Contains(s, "microsoft") || strings.Contains(s, "wsl")
}

func debugUser() {
	// Spawn a child in a fresh user namespace so we can inspect what the
	// uid/gid mappings look like from inside.
	cmd := exec.Command("/proc/self/exe", "child-debug")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("debug-user: %v", err)
	}
}

// childDebug prints namespace diagnostics — useful for verifying that
// uid/gid mappings were applied correctly when debugging user namespaces.
func childDebug() {
	out, _ := os.ReadFile("/proc/self/uid_map")
	fmt.Println("uid_map:")
	fmt.Print(string(out))

	link, _ := os.Readlink("/proc/self/ns/user")
	fmt.Println("user ns:", link)

	cmd := exec.Command("id")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
