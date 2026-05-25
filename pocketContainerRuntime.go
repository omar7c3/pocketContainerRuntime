package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ── Namespace config ──────────────────────────────────────────────────────────

type nsConfig struct {
	uts  bool
	pid  bool
	mnt  bool
	ipc  bool
	net  bool
	user bool
}

// ── Container lifecycle ───────────────────────────────────────────────────────

// ContainerState is the set of states a container can be in.
type ContainerState string

const (
	StateStarting ContainerState = "starting"
	StateRunning  ContainerState = "running"
	StateStopped  ContainerState = "stopped"
	StateCrashed  ContainerState = "crashed"
)

// RestartPolicy controls what the parent does when the child exits.
type RestartPolicy string

const (
	RestartAlways  RestartPolicy = "always"   // restart on non-zero exit (clean exit stops)
	RestartOnCrash RestartPolicy = "on-crash" // restart only on non-zero exit
	RestartNever   RestartPolicy = "never"    // record and stop
	RestartReplace RestartPolicy = "replace"  // spin up a new container with a fresh ID on crash
)

// HealthMode selects which probe to use.
type HealthMode string

const (
	HealthPID  HealthMode = "pid"  // signal(0) check — works for any process
	HealthHTTP HealthMode = "http" // GET /health on a configured port
)

// Container is the state record persisted to disk for every container.
// The parent process is the sole writer; the container process never touches it.
type Container struct {
	ID            string         `json:"id"`
	State         ContainerState `json:"state"`
	PID           int            `json:"pid,omitempty"`
	RootFS        string         `json:"rootfs"`
	Cmd           []string       `json:"cmd"`
	ExitCode      int            `json:"exit_code,omitempty"`
	RestartPolicy RestartPolicy  `json:"restart_policy"`
	StartedAt     time.Time      `json:"started_at,omitempty"`
	FinishedAt    time.Time      `json:"finished_at,omitempty"`
}

// stateDir is intentionally under /var/lib rather than /var/run.
// /var/run is a tmpfs in WSL2 and is wiped when the instance restarts,
// which would lose all state files across PowerShell sessions.
const stateDir = "/var/lib/mycontainer"

// generateID returns an 8-character random hex string used as the container ID.
func generateID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// stateFile returns the path of the JSON state file for a given container ID.
func stateFile(id string) string {
	return filepath.Join(stateDir, id+".json")
}

// writeState serialises c to its state file.
// All lifecycle transitions funnel through here so the file is always consistent.
func writeState(c *Container) {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		log.Printf("writeState: mkdir %s: %v", stateDir, err)
		return
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		log.Printf("writeState: marshal: %v", err)
		return
	}
	if err := os.WriteFile(stateFile(c.ID), data, 0644); err != nil {
		log.Printf("writeState: write: %v", err)
	}
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		log.Fatalf("usage: %s <run|status|list|clean|child|child-debug|debug-user> [args...]", os.Args[0])
	}

	switch os.Args[1] {
	case "run":
		run()
	case "status":
		status()
	case "list":
		list()
	case "clean":
		clean()
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

// ── run ───────────────────────────────────────────────────────────────────────

// runConfig bundles every flag parsed from the "run" sub-command.
type runConfig struct {
	ns            nsConfig
	restartPolicy RestartPolicy
	healthMode    HealthMode
	healthPort    int // only used when healthMode == HealthHTTP
}

// parseRunArgs parses the arguments after "run":
//
//	mycontainer run [ns flags] [lifecycle flags] <rootfs> <cmd> [args...]
func parseRunArgs(args []string) (runConfig, string, []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)

	// Namespace flags
	var ns nsConfig
	fs.BoolVar(&ns.uts, "uts", false, "enable UTS namespace")
	fs.BoolVar(&ns.pid, "pid", false, "enable PID namespace")
	fs.BoolVar(&ns.mnt, "mnt", false, "enable mount namespace")
	fs.BoolVar(&ns.ipc, "ipc", false, "enable IPC namespace")
	fs.BoolVar(&ns.net, "net", false, "enable network namespace")
	fs.BoolVar(&ns.user, "user", false, "enable user namespace")

	// Lifecycle flags.
	restart := fs.String("restart", "never", "restart policy: always|on-crash|never|replace")
	health := fs.String("health", "pid", "health probe: pid  or  http:<port>  (e.g. http:8080)")

	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) < 2 {
		log.Fatalf("usage: mycontainer run [flags] <rootfs> <cmd> [args...]")
	}

	// Parse health probe into mode + optional port.
	hMode := HealthPID
	hPort := 0
	if *health != "pid" {
		if strings.HasPrefix(*health, "http:") {
			hMode = HealthHTTP
			_, err := fmt.Sscanf(*health, "http:%d", &hPort)
			if err != nil || hPort == 0 {
				log.Fatalf("invalid --health value %q: expected http:<port>", *health)
			}
		} else {
			log.Fatalf("unknown --health value %q: use pid or http:<port>", *health)
		}
	}

	cfg := runConfig{
		ns:            ns,
		restartPolicy: RestartPolicy(*restart),
		healthMode:    hMode,
		healthPort:    hPort,
	}

	return cfg, rest[0], rest[1:]
}

func run() {
	cfg, rootfs, cmdArgs := parseRunArgs(os.Args[2:])

	// Remember the real host UID/GID before we enter any namespaces,
	// so we can write the correct uid/gid mappings below.
	hostUID := os.Getuid()
	hostGID := os.Getgid()

	// A user namespace without a mount namespace is mostly useless for
	// our purposes — pivot_root requires a mount namespace. Enable it
	// automatically so the user doesn't have to remember the pairing.
	if cfg.ns.user && !cfg.ns.mnt {
		log.Printf("info: --user requested, enabling --mnt automatically")
		cfg.ns.mnt = true
	}

	// Assign a unique ID and create the initial state file before forking,
	// so any observer can see the container is starting.
	id := generateID()
	c := &Container{
		ID:            id,
		State:         StateStarting,
		RootFS:        rootfs,
		Cmd:           cmdArgs,
		RestartPolicy: cfg.restartPolicy,
		StartedAt:     time.Now(),
	}
	writeState(c)
	log.Printf("container %s starting", id)

	// Translate ns config into CLONE_NEW* bitmask.
	var cloneFlags uintptr
	if cfg.ns.uts {
		cloneFlags |= syscall.CLONE_NEWUTS
	}
	if cfg.ns.pid {
		cloneFlags |= syscall.CLONE_NEWPID
	}
	if cfg.ns.mnt {
		cloneFlags |= syscall.CLONE_NEWNS
	}
	if cfg.ns.ipc {
		cloneFlags |= syscall.CLONE_NEWIPC
	}
	if cfg.ns.net {
		cloneFlags |= syscall.CLONE_NEWNET
	}
	if cfg.ns.user {
		cloneFlags |= syscall.CLONE_NEWUSER
	}

	// We can't configure namespaces after exec, so encode the ns config
	// as a string and pass it to the child process via argv.
	nsSpec := encodeNsConfig(cfg.ns)

	childArgs := []string{"child", nsSpec, rootfs}
	childArgs = append(childArgs, cmdArgs...)

	// runWithPolicy drives the restart loop; the actual fork/exec lives there.
	runWithPolicy(cfg, c, cloneFlags, childArgs, hostUID, hostGID)
}

// runWithPolicy wraps cmd.Run() with the configured restart behaviour.
// It is the only place that transitions the container between states.
func runWithPolicy(
	cfg runConfig,
	c *Container,
	cloneFlags uintptr,
	childArgs []string,
	hostUID, hostGID int,
) {
	const maxRetries = 5

	// cleanExit is declared outside the loop so the replace check at the
	// top of each iteration can read the result from the previous one.
	cleanExit := false

	for attempt := 0; ; attempt++ {
		// On a replace restart each attempt gets a brand-new identity.
		// But if the previous run exited cleanly (code 0) we treat it as
		// a normal stop rather than spinning up a replacement.
		if attempt > 0 && cfg.restartPolicy == RestartReplace && cleanExit {
			c.State = StateStopped
			writeState(c)
			log.Printf("container %s stopped cleanly", c.ID)
			return
		}
		if attempt > 0 && cfg.restartPolicy == RestartReplace {
			oldID := c.ID
			c.State = StateCrashed
			c.FinishedAt = time.Now()
			writeState(c)

			c.ID = generateID()
			c.ExitCode = 0
			c.PID = 0
			c.StartedAt = time.Now()
			c.FinishedAt = time.Time{}
			log.Printf("container %s replacing crashed container %s", c.ID, oldID)
		}

		// Build the re-exec command for this attempt.
		cmd := exec.Command("/proc/self/exe", childArgs...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		sysAttr := &syscall.SysProcAttr{Cloneflags: cloneFlags}
		if cfg.ns.user {
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

		// Start the child so we can capture its PID for the state file
		// and the health checker before we block on Wait.
		if err := cmd.Start(); err != nil {
			log.Fatalf("start child: %v", err)
		}

		c.State = StateRunning
		c.PID = cmd.Process.Pid
		writeState(c)
		log.Printf("container %s running (pid %d, attempt %d)", c.ID, c.PID, attempt+1)

		// Health checker runs in a goroutine alongside the process.
		// It sends true when the probe fails so we can record the crash.
		healthDone := make(chan bool, 1)
		go healthCheck(cfg, cmd.Process.Pid, healthDone)

		// Wait for the child to exit or the health check to fire.
		waitDone := make(chan error, 1)
		go func() { waitDone <- cmd.Wait() }()

		var runErr error
		select {
		case runErr = <-waitDone:
			// Process exited on its own; drain healthDone so the goroutine
			// can exit without blocking.
			healthDone <- false
		case unhealthy := <-healthDone:
			if unhealthy {
				// Health probe failed: kill the process and wait for it.
				_ = cmd.Process.Kill()
				runErr = <-waitDone
			}
		}

		c.FinishedAt = time.Now()

		// Determine exit code.
		exitCode := 0
		if runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		c.ExitCode = exitCode

		// Use = not := so we update the outer cleanExit that the replace
		// check reads at the top of the next iteration.
		cleanExit = exitCode == 0

		// Decide what to do based on the restart policy.
		switch cfg.restartPolicy {
		case RestartNever:
			if cleanExit {
				c.State = StateStopped
			} else {
				c.State = StateCrashed
			}
			writeState(c)
			log.Printf("container %s %s (exit %d)", c.ID, c.State, exitCode)
			return

		case RestartOnCrash:
			if cleanExit {
				c.State = StateStopped
				writeState(c)
				log.Printf("container %s stopped cleanly", c.ID)
				return
			}
			// Non-zero exit: fall through to retry logic below.

		case RestartAlways:
			// A clean exit (code 0) means the process finished intentionally;
			// restarting it would be incorrect even under an "always" policy.
			if cleanExit {
				c.State = StateStopped
				writeState(c)
				log.Printf("container %s stopped cleanly", c.ID)
				return
			}
			// Non-zero exit: fall through to retry logic below.

		case RestartReplace:
			// Loop handles replace at the top; just fall through.
		}

		// Never treat a clean exit as a crash regardless of policy.
		// This is a safety net so the crash log never fires on exit code 0.
		if cleanExit {
			c.State = StateStopped
			writeState(c)
			log.Printf("container %s stopped cleanly", c.ID)
			return
		}

		if attempt+1 >= maxRetries {
			c.State = StateCrashed
			writeState(c)
			log.Printf("container %s crashed (exit %d); giving up after %d attempts", c.ID, exitCode, maxRetries)
			return
		}

		c.State = StateCrashed
		writeState(c)
		backoff := time.Duration(1<<uint(attempt)) * time.Second
		log.Printf("container %s crashed (exit %d); retrying in %s (attempt %d/%d)",
			c.ID, exitCode, backoff, attempt+1, maxRetries)
		time.Sleep(backoff)
	}
}

// ── Health checking ───────────────────────────────────────────────────────────

// healthCheck polls the running container according to cfg.
// It sends true on done if the probe fails (container is unhealthy),
// or returns quietly when done receives false (container already exited).
func healthCheck(cfg runConfig, pid int, done chan bool) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ok := <-done:
			// Caller signalled that the process already exited; nothing to do.
			_ = ok
			return
		case <-ticker.C:
			var healthy bool
			switch cfg.healthMode {
			case HealthHTTP:
				healthy = probeHTTP(cfg.healthPort)
			default: // HealthPID
				healthy = probePID(pid)
			}
			if !healthy {
				done <- true
				return
			}
		}
	}
}

// probePID sends signal 0 to pid. This never kills the process; it only
// checks whether the process exists and we have permission to signal it.
func probePID(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// probeHTTP performs a GET /health against localhost on the given port.
// Returns true only when the server responds with HTTP 200.
func probeHTTP(port int) bool {
	url := fmt.Sprintf("http://localhost:%d/health", port)
	resp, err := http.Get(url) //nolint:gosec // intentionally plain HTTP for local probes
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ── Status / list / clean commands ───────────────────────────────────────────

// status prints the state of a single container by ID.
//
//	mycontainer status <id>
func status() {
	if len(os.Args) < 3 {
		log.Fatalf("usage: %s status <id>", os.Args[0])
	}
	id := os.Args[2]
	data, err := os.ReadFile(stateFile(id))
	if err != nil {
		log.Fatalf("status: container %s not found", id)
	}
	fmt.Print(string(data))
}

// list prints a summary line for every container whose state file exists.
//
//	mycontainer list
func list() {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no containers found")
			return
		}
		log.Fatalf("list: %v", err)
	}

	fmt.Printf("%-10s  %-10s  %-6s  %s\n", "ID", "STATE", "PID", "CMD")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, e.Name()))
		if err != nil {
			continue
		}
		var c Container
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}
		fmt.Printf("%-10s  %-10s  %-6d  %s\n", c.ID, c.State, c.PID, strings.Join(c.Cmd, " "))
	}
}

// clean removes state files. Never touches running or starting containers.
//
//	mycontainer clean              — remove all terminal state files (stopped + crashed)
//	mycontainer clean --state=crashed  — remove only crashed
//	mycontainer clean --state=stopped  — remove only stopped
//	mycontainer clean <id>         — remove one specific container by ID
func clean() {
	args := os.Args[2:]

	// Single ID provided: remove that one file regardless of state,
	// but refuse if the container is still running.
	if len(args) == 1 && !strings.HasPrefix(args[0], "--") {
		id := args[0]
		data, err := os.ReadFile(stateFile(id))
		if err != nil {
			log.Fatalf("clean: container %s not found", id)
		}
		var c Container
		if err := json.Unmarshal(data, &c); err != nil {
			log.Fatalf("clean: parse state for %s: %v", id, err)
		}
		if c.State == StateRunning || c.State == StateStarting {
			log.Fatalf("clean: container %s is %s — stop it before cleaning", id, c.State)
		}
		if err := os.Remove(stateFile(id)); err != nil {
			log.Fatalf("clean: remove %s: %v", id, err)
		}
		fmt.Printf("removed %s\n", id)
		return
	}

	// Flag-based removal: filter by state.
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	stateFilter := fs.String("state", "", "filter by state: crashed|stopped (default: both)")
	_ = fs.Parse(args)

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("no containers found")
			return
		}
		log.Fatalf("clean: %v", err)
	}

	removed := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, e.Name()))
		if err != nil {
			continue
		}
		var c Container
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}

		// Never remove live containers.
		if c.State == StateRunning || c.State == StateStarting {
			continue
		}

		// Apply state filter if specified.
		if *stateFilter != "" && string(c.State) != *stateFilter {
			continue
		}

		if err := os.Remove(filepath.Join(stateDir, e.Name())); err != nil {
			log.Printf("clean: remove %s: %v", c.ID, err)
			continue
		}
		fmt.Printf("removed %s (%s)\n", c.ID, c.State)
		removed++
	}

	if removed == 0 {
		fmt.Println("nothing to clean")
	}
}

// ── Namespace encode / decode ────────────────────────────────────

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

// ── child (re-exec'd entry point, runs inside namespaces) ────────────────────

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

// ── Filesystem isolation ─────────────────────────────────────────

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

// ── cgroup v2 ─────────────────────────────────────────────────────

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
// which work without any filesystem access and are safe under WSL2 or
// when the process isn't in its own cgroup.
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

// ── Capabilities ──────────────────────────────────────────────────────────────

// dropCapabilities drops all Linux capabilities from the calling process using
// prctl + capset. After this call a process that appears to be root inside a
// user namespace still cannot perform any privileged operation on the host.
//
// We clear every capability in both the effective and permitted sets.
// The inheritable set is also cleared so child execs can't regain anything.
func dropCapabilities() {
	// Step 1: tell the kernel we will not use capabilities across an exec
	// (PR_SET_NO_NEW_PRIVS). This is a defence-in-depth measure that
	// prevents a setuid binary inside the container from elevating.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		log.Printf("warning: PR_SET_NO_NEW_PRIVS: %v", err)
	}

	// Step 2: build an all-zero capability header + data payload and push
	// it into the kernel with capset(). This atomically removes every
	// capability from the effective, permitted, and inheritable sets.
	hdr := unix.CapUserHeader{
		Version: unix.LINUX_CAPABILITY_VERSION_3,
		Pid:     0, // 0 means "current process"
	}
	// unix.CapUserData is an array of two blocks (high + low 32-bit words).
	// Leaving them zero-initialised clears all 64 capability bits.
	var data [2]unix.CapUserData

	if err := unix.Capset(&hdr, &data[0]); err != nil {
		log.Printf("warning: capset (drop all): %v", err)
	}
}

// ── isWSL2  ────────────────────────────────────────────────────────

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

// ── Debug helpers ─────────────────────────────────────────────────

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
