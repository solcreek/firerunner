package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/solcreek/firerunner/internal/core"
)

// FirecrackerConfig configures the direct-Firecracker provisioner. It
// deliberately depends on nothing beyond the firecracker binary and the Linux
// host tools (cp with reflink, ip, nft, sysctl) — no containerd, CNI or LVM.
type FirecrackerConfig struct {
	// Binary is the path to the firecracker executable.
	Binary string
	// KernelImage is the uncompressed guest kernel (vmlinux) path.
	KernelImage string
	// GoldenRootFS is the immutable base rootfs for the default tier. Each job
	// gets a reflink (copy-on-write) clone of it.
	GoldenRootFS string
	// BootArgs is the base kernel command line. A per-VM `ip=` clause is appended
	// at launch. `reboot=k` is required so the guest `reboot -f` triggers an
	// i8042 reset that makes the VMM exit.
	BootArgs string
	// WorkDir is where per-job rootfs clones and API sockets are created. It
	// should live on a reflink-capable filesystem (btrfs/XFS) for near-zero
	// clone cost.
	WorkDir string
	// TapPrefix is the prefix for per-job tap device names.
	TapPrefix string
	// ExtIface is the host's external interface used for microVM egress NAT
	// (e.g. eth0, enp2s0). Required for the guest to reach GitHub.
	ExtIface string
	// LogDir, when set, receives one <runner>.log file per microVM capturing the
	// guest serial console. Because the microVM is destroyed after its single
	// job, this is how runner logs are forwarded off the ephemeral VM.
	LogDir string
	// MaxVMs bounds concurrent microVMs and sizes the per-VM network pool.
	MaxVMs int
}

// DefaultBootArgs is a minimal, quiet serial console command line. reboot=k is
// mandatory for the self-destruct-on-reboot lifecycle.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"

// commandRunner runs an external command. It is a seam so tests can substitute
// a fake for cp / ip / firecracker without touching the host.
type commandRunner func(ctx context.Context, name string, args ...string) error

// Firecracker is a Provisioner that talks to the Firecracker REST API directly
// over its unix socket using only the standard library.
type Firecracker struct {
	cfg     FirecrackerConfig
	log     *slog.Logger
	run     commandRunner
	ipam    *ipam
	natOnce sync.Once
	natErr  error
}

// NewFirecracker returns a Firecracker provisioner, filling in defaults.
func NewFirecracker(cfg FirecrackerConfig, log *slog.Logger) *Firecracker {
	if cfg.Binary == "" {
		cfg.Binary = "firecracker"
	}
	if cfg.BootArgs == "" {
		cfg.BootArgs = DefaultBootArgs
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "/var/tmp/firerunner"
	}
	if cfg.TapPrefix == "" {
		cfg.TapPrefix = "fr"
	}
	if cfg.MaxVMs < 1 {
		cfg.MaxVMs = 64
	}
	return &Firecracker{cfg: cfg, log: log, run: execRun, ipam: newIPAM(cfg.MaxVMs)}
}

// Name implements Provisioner.
func (f *Firecracker) Name() string { return "firecracker" }

// Launch implements Provisioner: reflink-clone the golden rootfs, allocate a
// per-VM network slot, create a tap, boot the microVM with the JIT config
// delivered via MMDS v2, then block until the guest self-destructs (reboot -f)
// and reap everything.
func (f *Firecracker) Launch(ctx context.Context, name, jitConfig string, spec core.RunnerSpec) error {
	if err := f.ensureNAT(ctx); err != nil {
		return fmt.Errorf("ensure NAT: %w", err)
	}

	slot, ok := f.ipam.acquire()
	if !ok {
		return fmt.Errorf("no free network slot (max %d microVMs)", f.cfg.MaxVMs)
	}
	defer f.ipam.release(slot)
	vnet := slotNet(slot, f.cfg.TapPrefix)

	jobDir := filepath.Join(f.cfg.WorkDir, name)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return fmt.Errorf("mkdir job dir: %w", err)
	}
	defer os.RemoveAll(jobDir)

	rootfs := filepath.Join(jobDir, "rootfs.ext4")
	if err := f.run(ctx, "cp", "--reflink=auto", spec.RootFS, rootfs); err != nil {
		return fmt.Errorf("reflink golden rootfs: %w", err)
	}

	if err := f.setupTap(ctx, vnet); err != nil {
		return fmt.Errorf("setup tap %s: %w", vnet.tap, err)
	}
	defer func() { _ = f.teardownTap(context.WithoutCancel(ctx), vnet.tap) }()

	console, closeConsole, err := f.openConsole(name)
	if err != nil {
		return fmt.Errorf("open console log: %w", err)
	}
	defer closeConsole()

	sock := filepath.Join(jobDir, "fc.sock")
	cmd := exec.CommandContext(ctx, f.cfg.Binary, "--api-sock", sock, "--id", name)
	cmd.Stdout = console
	cmd.Stderr = console
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}

	if err := waitForSocket(ctx, sock, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("wait for api socket: %w", err)
	}

	bootArgs := composeBootArgs(f.cfg.BootArgs, vnet)
	if err := f.configure(ctx, sock, rootfs, vnet, bootArgs, jitConfig, spec); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("configure microVM: %w", err)
	}

	f.log.Info("microVM started", "runner", name, "slot", slot, "guestIP", vnet.guestIP, "vcpu", spec.VCPU, "memMiB", spec.MemMiB)
	err = cmd.Wait() // returns when the guest reboots (self-destruct)
	f.log.Info("microVM exited", "runner", name, "err", err)
	// A clean self-destruct makes the VMM exit non-zero on some kernels; that
	// is expected and not treated as a launch failure.
	return nil
}

// ensureNAT installs host forwarding + masquerade exactly once per process.
func (f *Firecracker) ensureNAT(ctx context.Context) error {
	f.natOnce.Do(func() {
		if f.cfg.ExtIface == "" {
			f.natErr = errors.New("external interface is required for microVM egress")
			return
		}
		for _, c := range natCommands(f.cfg.ExtIface) {
			if err := f.run(ctx, c[0], c[1:]...); err != nil {
				f.natErr = fmt.Errorf("%v: %w", c, err)
				return
			}
		}
		f.log.Info("host NAT configured", "extIface", f.cfg.ExtIface, "cidr", vmCIDR)
	})
	return f.natErr
}

// openConsole returns the writer for the guest serial console. When LogDir is
// set the console is teed to a per-runner file so logs survive the microVM.
func (f *Firecracker) openConsole(name string) (io.Writer, func(), error) {
	slogWriter := &logWriter{log: f.log, runner: name, stream: "console"}
	if f.cfg.LogDir == "" {
		return slogWriter, func() {}, nil
	}
	if err := os.MkdirAll(f.cfg.LogDir, 0o755); err != nil {
		return nil, nil, err
	}
	file, err := os.Create(filepath.Join(f.cfg.LogDir, name+".log"))
	if err != nil {
		return nil, nil, err
	}
	return io.MultiWriter(file, slogWriter), func() { _ = file.Close() }, nil
}

// apiStep is a single Firecracker configuration API call.
type apiStep struct {
	path string
	body any
}

// buildAPISteps returns the ordered Firecracker API sequence for a microVM.
// It is a pure function so tests can assert the exact order and payloads —
// notably that /mmds (the JIT secret) is set before InstanceStart, and that
// InstanceStart is always last.
func buildAPISteps(kernelImage, bootArgs, rootfs, tap, guestMAC, jit string, spec core.RunnerSpec) []apiStep {
	return []apiStep{
		{"/boot-source", map[string]any{
			"kernel_image_path": kernelImage,
			"boot_args":         bootArgs,
		}},
		{"/drives/rootfs", map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   rootfs,
			"is_root_device": true,
			"is_read_only":   false,
		}},
		{"/machine-config", map[string]any{
			"vcpu_count":   spec.VCPU,
			"mem_size_mib": spec.MemMiB,
		}},
		{"/network-interfaces/eth0", map[string]any{
			"iface_id":      "eth0",
			"host_dev_name": tap,
			"guest_mac":     guestMAC,
		}},
		{"/mmds/config", map[string]any{
			"version":            "V2",
			"network_interfaces": []string{"eth0"},
			"ipv4_address":       "169.254.169.254",
		}},
		{"/mmds", map[string]any{"jitconfig": jit}},
		{"/actions", map[string]any{"action_type": "InstanceStart"}},
	}
}

// configure runs the Firecracker API sequence. MMDS config and data MUST be set
// before InstanceStart so the JIT secret is available to the guest at boot.
func (f *Firecracker) configure(ctx context.Context, sock, rootfs string, n vmNet, bootArgs, jit string, spec core.RunnerSpec) error {
	cl := newUnixClient(sock)
	for _, s := range buildAPISteps(f.cfg.KernelImage, bootArgs, rootfs, n.tap, n.guestMAC, jit, spec) {
		if err := putJSON(ctx, cl, s.path, s.body); err != nil {
			return fmt.Errorf("PUT %s: %w", s.path, err)
		}
	}
	return nil
}

func (f *Firecracker) setupTap(ctx context.Context, n vmNet) error {
	for _, c := range tapUpCommands(n) {
		if err := f.run(ctx, c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

func (f *Firecracker) teardownTap(ctx context.Context, tap string) error {
	for _, c := range tapDownCommands(tap) {
		if err := f.run(ctx, c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// --- small stdlib helpers ---

func execRun(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(out))
	}
	return nil
}

func newUnixClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

func putJSON(ctx context.Context, cl *http.Client, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("firecracker returned %s", resp.Status)
	}
	return nil
}

func waitForSocket(ctx context.Context, sock string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(sock); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s did not appear within %s", sock, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// logWriter adapts firecracker's serial/stdio output to structured logs.
type logWriter struct {
	log    *slog.Logger
	runner string
	stream string
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.log.Debug("firecracker", "runner", w.runner, "stream", w.stream, "line", string(bytes.TrimRight(p, "\n")))
	return len(p), nil
}
