package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/linyiru/firerunner/internal/core"
)

// FirecrackerConfig configures the direct-Firecracker provisioner. It
// deliberately depends on nothing beyond the firecracker binary and the Linux
// host tools (cp with reflink, ip) — no containerd, CNI or LVM.
type FirecrackerConfig struct {
	// Binary is the path to the firecracker executable.
	Binary string
	// KernelImage is the uncompressed guest kernel (vmlinux) path.
	KernelImage string
	// GoldenRootFS is the immutable base rootfs for the default tier. Each job
	// gets a reflink (copy-on-write) clone of it.
	GoldenRootFS string
	// BootArgs is the kernel command line. `reboot=k` is required so the guest
	// `reboot -f` triggers an i8042 reset that makes the VMM exit.
	BootArgs string
	// WorkDir is where per-job rootfs clones and API sockets are created. It
	// should live on a reflink-capable filesystem (btrfs/XFS) for near-zero
	// clone cost.
	WorkDir string
	// TapPrefix is the prefix for per-job tap device names.
	TapPrefix string
	// GuestMAC is the MAC address assigned to the guest eth0.
	GuestMAC string
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
	cfg FirecrackerConfig
	log *slog.Logger
	run commandRunner
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
	if cfg.GuestMAC == "" {
		cfg.GuestMAC = "06:00:AC:10:00:02"
	}
	return &Firecracker{cfg: cfg, log: log, run: execRun}
}

// Name implements Provisioner.
func (f *Firecracker) Name() string { return "firecracker" }

// Launch implements Provisioner: reflink-clone the golden rootfs, create a tap,
// boot the microVM with the JIT config delivered via MMDS v2, then block until
// the guest self-destructs (reboot -f) and reap everything.
func (f *Firecracker) Launch(ctx context.Context, name, jitConfig string, spec core.RunnerSpec) error {
	jobDir := filepath.Join(f.cfg.WorkDir, name)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return fmt.Errorf("mkdir job dir: %w", err)
	}
	defer os.RemoveAll(jobDir)

	rootfs := filepath.Join(jobDir, "rootfs.ext4")
	if err := f.run(ctx, "cp", "--reflink=auto", spec.RootFS, rootfs); err != nil {
		return fmt.Errorf("reflink golden rootfs: %w", err)
	}

	tap := f.tapName(name)
	if err := f.setupTap(ctx, tap); err != nil {
		return fmt.Errorf("setup tap %s: %w", tap, err)
	}
	defer func() { _ = f.teardownTap(context.WithoutCancel(ctx), tap) }()

	sock := filepath.Join(jobDir, "fc.sock")
	cmd := exec.CommandContext(ctx, f.cfg.Binary, "--api-sock", sock, "--id", name)
	cmd.Stdout = &logWriter{log: f.log, runner: name, stream: "stdout"}
	cmd.Stderr = &logWriter{log: f.log, runner: name, stream: "stderr"}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}

	if err := waitForSocket(ctx, sock, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("wait for api socket: %w", err)
	}

	if err := f.configure(ctx, sock, rootfs, tap, jitConfig, spec); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("configure microVM: %w", err)
	}

	f.log.Info("microVM started", "runner", name, "vcpu", spec.VCPU, "memMiB", spec.MemMiB)
	err := cmd.Wait() // returns when the guest reboots (self-destruct)
	f.log.Info("microVM exited", "runner", name, "err", err)
	// A clean self-destruct makes the VMM exit non-zero on some kernels; that
	// is expected and not treated as a launch failure.
	return nil
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
func buildAPISteps(cfg FirecrackerConfig, rootfs, tap, jit string, spec core.RunnerSpec) []apiStep {
	return []apiStep{
		{"/boot-source", map[string]any{
			"kernel_image_path": cfg.KernelImage,
			"boot_args":         cfg.BootArgs,
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
			"guest_mac":     cfg.GuestMAC,
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
func (f *Firecracker) configure(ctx context.Context, sock, rootfs, tap, jit string, spec core.RunnerSpec) error {
	cl := newUnixClient(sock)
	for _, s := range buildAPISteps(f.cfg, rootfs, tap, jit, spec) {
		if err := putJSON(ctx, cl, s.path, s.body); err != nil {
			return fmt.Errorf("PUT %s: %w", s.path, err)
		}
	}
	return nil
}

func (f *Firecracker) tapName(runner string) string {
	// tap names are limited to 15 chars; keep the prefix + a short suffix.
	suffix := runner
	if len(suffix) > 12 {
		suffix = suffix[len(suffix)-12:]
	}
	return f.cfg.TapPrefix + suffix
}

func (f *Firecracker) setupTap(ctx context.Context, tap string) error {
	if err := f.run(ctx, "ip", "tuntap", "add", "dev", tap, "mode", "tap"); err != nil {
		return err
	}
	if err := f.run(ctx, "ip", "addr", "add", "172.16.0.1/30", "dev", tap); err != nil {
		return err
	}
	return f.run(ctx, "ip", "link", "set", tap, "up")
}

func (f *Firecracker) teardownTap(ctx context.Context, tap string) error {
	return f.run(ctx, "ip", "link", "del", tap)
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
