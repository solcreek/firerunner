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
	"strconv"
	"strings"
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
	// NetBase is the second octet of the per-microVM /16 (default 16 ->
	// 172.16.x). Distinct values let multiple firerunner instances share a host
	// with non-overlapping guest subnets.
	NetBase int
	// NFTTable is the nftables table firerunner manages (default "firerunner").
	// A second instance must use a distinct name so the two rulesets do not
	// clobber each other.
	NFTTable string
	// ExtIface is the host's external interface used for microVM egress NAT
	// (e.g. eth0, enp2s0). Required for the guest to reach GitHub.
	ExtIface string
	// Egress restricts what microVM guests may reach on the network.
	Egress EgressConfig
	// LogDir, when set, receives one <runner>.log file per microVM capturing the
	// guest serial console. Because the microVM is destroyed after its single
	// job, this is how runner logs are forwarded off the ephemeral VM.
	LogDir string
	// MaxVMs bounds concurrent microVMs and sizes the per-VM network pool.
	MaxVMs int

	// Jailer, when true, launches every microVM under the Firecracker jailer,
	// which chroots the VMM, gives it its own PID namespace and drops it to an
	// unprivileged uid/gid. It is opt-in: firerunner already runs non-root and
	// Firecracker already installs seccomp filters by default, so the jailer is
	// an incremental defence-in-depth step whose cost is that it needs a root
	// launcher. No network namespace is used, so the host-namespace tap devices
	// work unchanged.
	Jailer bool
	// JailerBin is the path to the jailer executable (must match the firecracker
	// version). Defaults to "jailer".
	JailerBin string
	// ChrootBase is the jailer chroot base dir (default "/srv/jailer"). Each
	// microVM gets <ChrootBase>/firecracker/<id>/root.
	ChrootBase string
	// JailUID and JailGID are the unprivileged uid/gid the jailer drops the VMM
	// to. Required (>0) when Jailer is set.
	JailUID int
	JailGID int
}

// DefaultBootArgs is a minimal, quiet serial console command line. reboot=k is
// mandatory for the self-destruct-on-reboot lifecycle.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"

// commandRunner runs an external command. It is a seam so tests can substitute
// a fake for cp / ip / firecracker without touching the host.
type commandRunner func(ctx context.Context, name string, args ...string) error

// commandOutputRunner runs an external command and captures its combined
// output. It is a seam (like commandRunner) so host-state probes such as
// `nft list chain` can be faked in tests.
type commandOutputRunner func(ctx context.Context, name string, args ...string) (string, error)

// Firecracker is a Provisioner that talks to the Firecracker REST API directly
// over its unix socket using only the standard library.
type Firecracker struct {
	cfg     FirecrackerConfig
	log     *slog.Logger
	run     commandRunner
	runOut  commandOutputRunner
	http    *http.Client
	ipam    *ipam
	netOnce sync.Once
	netErr  error
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
	if cfg.NetBase == 0 {
		cfg.NetBase = 16
	}
	if cfg.NFTTable == "" {
		cfg.NFTTable = natTable
	}
	if cfg.JailerBin == "" {
		cfg.JailerBin = "jailer"
	}
	if cfg.ChrootBase == "" {
		cfg.ChrootBase = "/srv/jailer"
	}
	if cfg.MaxVMs < 1 {
		cfg.MaxVMs = 64
	}
	return &Firecracker{
		cfg:    cfg,
		log:    log,
		run:    execRun,
		runOut: execRunOut,
		http:   &http.Client{Timeout: 30 * time.Second},
		ipam:   newIPAM(cfg.MaxVMs),
	}
}

// Name implements Provisioner.
func (f *Firecracker) Name() string { return "firecracker" }

// Launch implements Provisioner: reflink-clone the golden rootfs, allocate a
// per-VM network slot, create a tap, boot the microVM with the JIT config
// delivered via MMDS v2, then block until the guest self-destructs (reboot -f)
// and reap everything.
func (f *Firecracker) Launch(ctx context.Context, name, jitConfig string, spec core.RunnerSpec) error {
	if err := f.SetupNetwork(ctx); err != nil {
		return fmt.Errorf("setup network: %w", err)
	}

	slot, ok := f.ipam.acquire()
	if !ok {
		return fmt.Errorf("no free network slot (max %d microVMs)", f.cfg.MaxVMs)
	}
	defer f.ipam.release(slot)
	vnet := slotNet(slot, f.cfg.TapPrefix, f.cfg.NetBase)

	if err := f.setupTap(ctx, vnet); err != nil {
		return fmt.Errorf("setup tap %s: %w", vnet.tap, err)
	}
	defer func() { _ = f.teardownTap(context.WithoutCancel(ctx), vnet.tap) }()

	console, closeConsole, err := f.openConsole(name)
	if err != nil {
		return fmt.Errorf("open console log: %w", err)
	}
	defer closeConsole()

	cmd, sock, kernelPath, rootfsPath, cleanup, err := f.prepare(ctx, name, console, spec)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	if err := waitForSocket(ctx, sock, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("wait for api socket: %w", err)
	}

	bootArgs := composeBootArgs(f.cfg.BootArgs, vnet)
	if err := f.configure(ctx, sock, kernelPath, rootfsPath, vnet, bootArgs, jitConfig, spec); err != nil {
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

// SetupNetwork installs host forwarding, the egress allowlist and NAT exactly
// once per process. It is safe to call from Launch and from main.
func (f *Firecracker) SetupNetwork(ctx context.Context) error {
	f.netOnce.Do(func() { f.netErr = f.applyNetwork(ctx) })
	return f.netErr
}

// RefreshNetwork periodically re-fetches GitHub's /meta ranges and re-applies
// the egress allowlist, until ctx is cancelled. It is a no-op when egress is
// open or refresh is disabled.
func (f *Firecracker) RefreshNetwork(ctx context.Context) {
	if f.cfg.Egress.open() || f.cfg.Egress.RefreshInterval <= 0 {
		return
	}
	t := time.NewTicker(f.cfg.Egress.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := f.applyNetwork(ctx); err != nil {
				f.log.Error("refresh egress allowlist", "err", err)
			}
		}
	}
}

// applyNetwork enables IPv4 forwarding and applies the nftables ruleset (egress
// allowlist + masquerade) atomically via `nft -f`.
func (f *Firecracker) applyNetwork(ctx context.Context) error {
	if f.cfg.ExtIface == "" {
		return errors.New("external interface is required for microVM egress")
	}

	rs := egressRuleset{
		Table:      f.cfg.NFTTable,
		ExtIface:   f.cfg.ExtIface,
		VMCidr:     vmCIDRFor(f.cfg.NetBase),
		DNSServers: f.cfg.Egress.DNSServers,
		AllowDNS:   f.cfg.Egress.has("dns"),
		AllowNTP:   f.cfg.Egress.has("ntp"),
		Open:       f.cfg.Egress.open(),
	}
	if !rs.Open {
		cidrs, err := fetchMetaCIDRs(ctx, f.http, metaURL, f.cfg.Egress.metaCats())
		if err != nil {
			return fmt.Errorf("fetch GitHub meta ranges: %w", err)
		}
		rs.Allowed = cidrs
	}

	if err := f.run(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}

	if err := os.MkdirAll(f.cfg.WorkDir, 0o755); err != nil {
		return fmt.Errorf("mkdir work dir: %w", err)
	}
	path := filepath.Join(f.cfg.WorkDir, "firerunner.nft")
	if err := os.WriteFile(path, []byte(buildNFTRuleset(rs)), 0o600); err != nil {
		return fmt.Errorf("write nft ruleset: %w", err)
	}
	if err := f.run(ctx, "nft", "-f", path); err != nil {
		return err
	}
	if err := f.ensureHostForward(ctx); err != nil {
		return err
	}
	f.log.Info("egress network applied", "extIface", f.cfg.ExtIface, "open", rs.Open, "allowedCIDRs", len(rs.Allowed))
	return nil
}

// forwardComment tags the FORWARD accept rules firerunner adds to a foreign
// (ufw/docker/hardened) filter chain, so refreshes stay idempotent. It is keyed
// by tap prefix so multiple firerunner instances sharing a host each manage —
// and detect — their own accept rules instead of mistaking a peer's rules for
// their own and never inserting accepts for their tap devices.
func forwardComment(tapPrefix string) string {
	return "firerunner-egress-" + tapPrefix
}

// hostForwardScript returns an nft script that inserts accept rules for
// firerunner's tap interfaces into the host's ip/filter FORWARD chain.
//
// It is needed on hosts whose forward policy defaults to drop (ufw, docker,
// hardened hosts). firerunner's own table only masquerades in postrouting; a
// separate base chain's `accept` cannot override another base chain's `drop`
// verdict at the same hook, so the accept must live in the dropping chain
// itself. The allowlist `drop` in firerunner's own table still applies (drop is
// terminal across chains), so this does not widen egress beyond the policy.
func hostForwardScript(extIface, tapPrefix string) string {
	tap := tapPrefix + "*"
	comment := forwardComment(tapPrefix)
	return fmt.Sprintf(
		"insert rule ip filter FORWARD iifname %q oifname %q ct state established,related counter accept comment %q\n"+
			"insert rule ip filter FORWARD iifname %q oifname %q counter accept comment %q\n",
		extIface, tap, comment,
		tap, extIface, comment,
	)
}

// ensureHostForward keeps a foreign default-drop forward policy from silently
// blocking masqueraded microVM egress. It is best-effort and idempotent: it is
// a no-op when there is no ip/filter FORWARD chain (default-accept host), when
// the forward policy is not drop, or when firerunner's accept rules are already
// present.
func (f *Firecracker) ensureHostForward(ctx context.Context) error {
	out, err := f.runOut(ctx, "nft", "list", "chain", "ip", "filter", "FORWARD")
	if err != nil {
		// No ip/filter FORWARD chain (host without ufw/iptables-nft). A
		// default-accept forward policy needs nothing beyond our masquerade.
		f.log.Debug("no ip/filter FORWARD chain; skipping host forward accept", "err", err)
		return nil
	}
	if strings.Contains(out, forwardComment(f.cfg.TapPrefix)) {
		return nil // our accept rules are already present
	}
	if !strings.Contains(out, "policy drop") {
		return nil // permissive forward policy; masquerade is sufficient
	}
	path := filepath.Join(f.cfg.WorkDir, "firerunner-forward.nft")
	if err := os.WriteFile(path, []byte(hostForwardScript(f.cfg.ExtIface, f.cfg.TapPrefix)), 0o600); err != nil {
		return fmt.Errorf("write host forward ruleset: %w", err)
	}
	if err := f.run(ctx, "nft", "-f", path); err != nil {
		return fmt.Errorf("apply host forward accept: %w", err)
	}
	f.log.Info("added host forward accept for microVM egress",
		"extIface", f.cfg.ExtIface, "tapPrefix", f.cfg.TapPrefix)
	return nil
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
// before InstanceStart so the JIT secret is available to the guest at boot. The
// kernel and rootfs paths are given as Firecracker will see them (host paths for
// a direct launch, in-jail paths such as /vmlinux under the jailer).
func (f *Firecracker) configure(ctx context.Context, sock, kernelPath, rootfs string, n vmNet, bootArgs, jit string, spec core.RunnerSpec) error {
	cl := newUnixClient(sock)
	for _, s := range buildAPISteps(kernelPath, bootArgs, rootfs, n.tap, n.guestMAC, jit, spec) {
		if err := putJSON(ctx, cl, s.path, s.body); err != nil {
			return fmt.Errorf("PUT %s: %w", s.path, err)
		}
	}
	return nil
}

// jailChrootRoot returns the host path to a microVM's jail root, where its
// kernel, rootfs and API socket live: <base>/firecracker/<id>/root.
func jailChrootRoot(base, id string) string {
	return filepath.Join(base, "firecracker", id, "root")
}

// buildJailerArgs returns the jailer argv that launches one jailed Firecracker.
// The jailer chroots into <ChrootBase>/firecracker/<id>/root, drops to
// JailUID:JailGID and execs Firecracker (passing --id itself). Firecracker's API
// socket defaults to /run/firecracker.socket inside the jail. No --netns is
// passed: the jailed VMM stays in the host network namespace so the per-slot
// host tap devices attach unchanged. cgroup-version 2 with no --cgroup means the
// jailer creates no cgroup (systemd already scopes the service). It is a pure
// function so the argv can be asserted in tests.
func buildJailerArgs(cfg FirecrackerConfig, id string) []string {
	return []string{
		"--id", id,
		"--exec-file", cfg.Binary,
		"--uid", strconv.Itoa(cfg.JailUID),
		"--gid", strconv.Itoa(cfg.JailGID),
		"--cgroup-version", "2",
		"--chroot-base-dir", cfg.ChrootBase,
	}
}

// prepare stages a microVM's rootfs and returns the command to launch it, the
// host path to its API socket, the kernel and rootfs paths as Firecracker will
// see them, and a cleanup that removes everything staged. When Jailer is set it
// builds the chroot and stages the kernel and rootfs inside it (owned by the
// jail user); otherwise it uses a flat per-job work dir and launches Firecracker
// directly.
func (f *Firecracker) prepare(ctx context.Context, name string, console io.Writer, spec core.RunnerSpec) (cmd *exec.Cmd, sock, kernelPath, rootfsPath string, cleanup func(), err error) {
	if f.cfg.Jailer {
		idDir := filepath.Join(f.cfg.ChrootBase, "firecracker", name)
		root := jailChrootRoot(f.cfg.ChrootBase, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, "", "", "", nil, fmt.Errorf("mkdir jail root: %w", err)
		}
		cleanup = func() { _ = os.RemoveAll(idDir) }
		kernel := filepath.Join(root, "vmlinux")
		rootfs := filepath.Join(root, "rootfs.ext4")
		if err := f.run(ctx, "cp", "--reflink=auto", f.cfg.KernelImage, kernel); err != nil {
			cleanup()
			return nil, "", "", "", nil, fmt.Errorf("stage kernel into jail: %w", err)
		}
		if err := f.run(ctx, "cp", "--reflink=auto", spec.RootFS, rootfs); err != nil {
			cleanup()
			return nil, "", "", "", nil, fmt.Errorf("reflink golden rootfs into jail: %w", err)
		}
		// The dropped-privilege Firecracker must own the writable rootfs and be
		// able to read the kernel; the jailer only chowns the chroot dir and the
		// /dev nodes it creates, not the files we stage.
		for _, p := range []string{kernel, rootfs} {
			if err := os.Chown(p, f.cfg.JailUID, f.cfg.JailGID); err != nil {
				cleanup()
				return nil, "", "", "", nil, fmt.Errorf("chown %s to jail user: %w", p, err)
			}
		}
		sock = filepath.Join(root, "run", "firecracker.socket")
		cmd = exec.CommandContext(ctx, f.cfg.JailerBin, buildJailerArgs(f.cfg, name)...)
		cmd.Stdout, cmd.Stderr = console, console
		return cmd, sock, "/vmlinux", "/rootfs.ext4", cleanup, nil
	}

	jobDir := filepath.Join(f.cfg.WorkDir, name)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return nil, "", "", "", nil, fmt.Errorf("mkdir job dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(jobDir) }
	rootfs := filepath.Join(jobDir, "rootfs.ext4")
	if err := f.run(ctx, "cp", "--reflink=auto", spec.RootFS, rootfs); err != nil {
		cleanup()
		return nil, "", "", "", nil, fmt.Errorf("reflink golden rootfs: %w", err)
	}
	sock = filepath.Join(jobDir, "fc.sock")
	cmd = exec.CommandContext(ctx, f.cfg.Binary, "--api-sock", sock, "--id", name)
	cmd.Stdout, cmd.Stderr = console, console
	return cmd, sock, f.cfg.KernelImage, rootfs, cleanup, nil
}

// CleanupStale reclaims host resources left behind by microVMs from a previous,
// uncleanly terminated run (SIGKILL, OOM, power loss). It is safe to call once at
// startup because the process owns no microVMs yet, so any matching tap device or
// job dir is definitively stale. Orphaned firecracker processes are reaped by the
// systemd cgroup on crash; this closes what survives that: per-slot tap devices
// (a host-namespace resource whose lifetime isn't bound to the VMM, so leaks here
// eventually exhaust the slot space) and per-job work dirs (reflink rootfs clones
// and sockets whose teardown defer never ran).
func (f *Firecracker) CleanupStale(ctx context.Context) {
	for slot := 0; slot < f.cfg.MaxVMs; slot++ {
		tap := fmt.Sprintf("%s%d", f.cfg.TapPrefix, slot)
		if _, err := net.InterfaceByName(tap); err != nil {
			continue // not present
		}
		if err := f.teardownTap(ctx, tap); err != nil {
			f.log.Warn("remove stale tap", "tap", tap, "err", err)
			continue
		}
		f.log.Info("removed stale tap", "tap", tap)
	}

	// A job dir is uniquely identified by its firecracker API socket, so globbing
	// for fc.sock avoids ever touching the nft rulesets or logs dir in WorkDir.
	socks, _ := filepath.Glob(filepath.Join(f.cfg.WorkDir, "*", "fc.sock"))
	for _, sock := range socks {
		dir := filepath.Dir(sock)
		if err := os.RemoveAll(dir); err != nil {
			f.log.Warn("remove stale job dir", "dir", dir, "err", err)
			continue
		}
		f.log.Info("removed stale job dir", "dir", dir)
	}

	// Under the jailer each microVM leaves a chroot at <base>/firecracker/<id>;
	// any that survive startup are stale (the process owns no microVMs yet).
	if f.cfg.Jailer {
		jails, _ := filepath.Glob(filepath.Join(f.cfg.ChrootBase, "firecracker", "*"))
		for _, dir := range jails {
			if err := os.RemoveAll(dir); err != nil {
				f.log.Warn("remove stale jail dir", "dir", dir, "err", err)
				continue
			}
			f.log.Info("removed stale jail dir", "dir", dir)
		}
	}
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

func execRunOut(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(out))
	}
	return string(out), nil
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
