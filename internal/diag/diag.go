// Package diag implements firerunner's operator-facing "status" and "doctor"
// subcommands. Both work purely by reading host state (files, /dev/kvm, sysctls,
// network interfaces, the work dir) so they can be run standalone on a runner
// host, alongside a live daemon, without any IPC or extra dependencies.
package diag

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/solcreek/firerunner/internal/config"
	"github.com/solcreek/firerunner/internal/provisioner"
)

// Status writes a human-readable snapshot of the deployment: resolved config,
// golden/kernel images, network wiring and the microVMs currently on the host.
func Status(cfg *config.Config, version string, w io.Writer) error {
	fc := cfg.Firecracker
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)

	fmt.Fprintf(tw, "firerunner\t%s\n", version)
	fmt.Fprintf(tw, "scale set\t%s  (group %s)\n", or(cfg.ScaleSetName, "-"), or(cfg.RunnerGroup, "-"))
	fmt.Fprintf(tw, "url\t%s\n", or(cfg.URL, "(unset)"))
	labels := append([]string{cfg.ScaleSetName}, cfg.Labels...)
	fmt.Fprintf(tw, "runs-on labels\t%s\n", strings.Join(nonEmpty(labels), ", "))
	fmt.Fprintf(tw, "capacity\twarm=%d  max=%d  vcpu=%d  mem=%dMiB\n", cfg.MinRunners, cfg.MaxRunners, cfg.VCPU, cfg.MemMiB)

	fmt.Fprintf(tw, "kernel\t%s\n", describeFile(fc.KernelImage))
	fmt.Fprintf(tw, "golden\t%s\n", describeFile(fc.GoldenRootFS))
	if fc.ToolCacheImage != "" {
		fmt.Fprintf(tw, "toolcache\t%s\n", describeFile(fc.ToolCacheImage))
	} else {
		fmt.Fprintf(tw, "toolcache\t(none; jobs download tools on demand)\n")
	}

	mode := "direct"
	if fc.Jailer {
		mode = "jailer"
		if fc.NetNS {
			mode = "jailer+netns"
		}
	}
	fmt.Fprintf(tw, "isolation\t%s\n", mode)
	fmt.Fprintf(tw, "network\text-iface=%s  egress=%s  subnet=172.%d.0.0/16  tap=%s*  nft-table=%s\n",
		ifaceState(fc.ExtIface), egressDesc(fc.Egress.Categories), fc.NetBase, fc.TapPrefix, fc.NFTTable)
	fmt.Fprintf(tw, "workdir\t%s (%s)\n", fc.WorkDir, fsKind(fc.WorkDir))
	tw.Flush()

	// Live microVMs on the host, cross-checked three ways: work dirs (the
	// per-job rootfs clone + socket), tap devices, and firecracker processes.
	vms := activeVMs(fc)
	procs := firecrackerProcs()
	taps := countTaps(fc.TapPrefix)
	fmt.Fprintf(w, "\nmicroVMs: %d active  (firecracker procs=%d, taps=%d, max=%d)\n",
		len(vms), procs, taps, cfg.MaxRunners)
	if len(vms) > 0 {
		vtw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(vtw, "  RUNNER\tROOTFS\tAGE")
		for _, v := range vms {
			fmt.Fprintf(vtw, "  %s\t%s\t%s\n", v.name, humanBytes(v.rootfsBytes), humanDuration(time.Since(v.started)))
		}
		vtw.Flush()
	}
	return nil
}

// Doctor runs preflight health checks and prints one PASS/WARN/FAIL line each.
// It returns a non-nil error if any check FAILs, so the command exits non-zero.
func Doctor(cfg *config.Config, version string, w io.Writer) error {
	fc := cfg.Firecracker
	var checks []check

	// Privileges. firerunner's default path runs unprivileged, but jailer/netns
	// need root (creating a netns/chroot needs CAP_SYS_ADMIN).
	euid := os.Geteuid()
	switch {
	case fc.Jailer && euid != 0:
		checks = append(checks, fail("privileges", "--jailer needs root; running as uid %d", euid))
	case euid == 0:
		checks = append(checks, pass("privileges", "root"))
	default:
		checks = append(checks, pass("privileges", "uid %d (non-root; ok for direct mode)", euid))
	}

	// KVM: the hard requirement. No /dev/kvm, no microVMs.
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		checks = append(checks, fail("kvm", "/dev/kvm not usable: %v", err))
	} else {
		f.Close()
		checks = append(checks, pass("kvm", "/dev/kvm read-write"))
	}

	checks = append(checks, binVersion("firecracker", fc.Binary))
	if fc.Jailer {
		checks = append(checks, binVersion("jailer", fc.JailerBin))
	}

	checks = append(checks, fileCheck("kernel", fc.KernelImage, 1<<20))   // >=1MiB
	checks = append(checks, fileCheck("golden", fc.GoldenRootFS, 64<<20)) // >=64MiB
	if fc.ToolCacheImage != "" {
		checks = append(checks, fileCheck("toolcache", fc.ToolCacheImage, 1<<20))
	}

	// External interface for egress NAT.
	if fc.ExtIface == "" {
		checks = append(checks, fail("ext-iface", "--ext-iface is unset; microVMs cannot reach GitHub"))
	} else if _, err := net.InterfaceByName(fc.ExtIface); err != nil {
		checks = append(checks, fail("ext-iface", "%q not found: %v", fc.ExtIface, err))
	} else {
		checks = append(checks, pass("ext-iface", "%s up", fc.ExtIface))
	}

	// IP forwarding: the host routes guest egress through ext-iface.
	if v := strings.TrimSpace(readFile("/proc/sys/net/ipv4/ip_forward")); v == "1" {
		checks = append(checks, pass("ip_forward", "enabled"))
	} else {
		checks = append(checks, warn("ip_forward", "net.ipv4.ip_forward=%s; enable it (sysctl -w net.ipv4.ip_forward=1) or guest egress fails", or(v, "0")))
	}

	// nftables drives the NAT + egress allowlist.
	if _, err := exec.LookPath("nft"); err != nil {
		checks = append(checks, warn("nftables", "nft not found in PATH; egress NAT rules cannot be applied"))
	} else {
		checks = append(checks, pass("nftables", "nft present"))
	}

	// Work dir: writable and, ideally, reflink-capable so rootfs clones are cheap.
	checks = append(checks, workdirCheck(fc.WorkDir))

	// GitHub auth.
	checks = append(checks, authCheck(cfg))

	// GitHub API reachability (best-effort; WARN offline, never FAIL).
	checks = append(checks, apiReach("https://api.github.com/zen"))

	// Emit.
	var failed int
	for _, c := range checks {
		if c.level == levelFail {
			failed++
		}
		fmt.Fprintf(w, "%s %-12s %s\n", c.level, c.name, c.detail)
	}
	fmt.Fprintf(w, "\n%d checks, %d failed\n", len(checks), failed)
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

// --- checks ---------------------------------------------------------------

const (
	levelPass = "PASS"
	levelWarn = "WARN"
	levelFail = "FAIL"
)

type check struct {
	level  string
	name   string
	detail string
}

func pass(name, f string, a ...any) check { return check{levelPass, name, fmt.Sprintf(f, a...)} }
func warn(name, f string, a ...any) check { return check{levelWarn, name, fmt.Sprintf(f, a...)} }
func fail(name, f string, a ...any) check { return check{levelFail, name, fmt.Sprintf(f, a...)} }

// binVersion resolves an executable and captures its --version banner.
func binVersion(name, bin string) check {
	path, err := exec.LookPath(bin)
	if err != nil {
		return fail(name, "%q not found: %v", bin, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return warn(name, "%s (version probe failed: %v)", path, err)
	}
	return pass(name, "%s", firstLine(string(out)))
}

// fileCheck verifies a path exists, is readable and is at least minBytes.
func fileCheck(name, path string, minBytes int64) check {
	if path == "" {
		return fail(name, "path is unset")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fail(name, "%s: %v", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fail(name, "%s not readable: %v", path, err)
	}
	f.Close()
	if fi.Size() < minBytes {
		return warn(name, "%s is only %s (looks too small)", path, humanBytes(fi.Size()))
	}
	return pass(name, "%s (%s)", path, humanBytes(fi.Size()))
}

func workdirCheck(dir string) check {
	if dir == "" {
		return fail("workdir", "path is unset")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail("workdir", "%s not creatable: %v", dir, err)
	}
	probe := filepath.Join(dir, ".firerunner-doctor")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fail("workdir", "%s not writable: %v", dir, err)
	}
	os.Remove(probe)
	kind := fsKind(dir)
	if !reflinkCapable(kind) {
		return warn("workdir", "%s is %s; reflink clones unsupported, each microVM copies the full golden (slow)", dir, kind)
	}
	return pass("workdir", "%s writable, %s (reflink-capable)", dir, kind)
}

func authCheck(cfg *config.Config) check {
	if cfg.Token != "" {
		return pass("auth", "PAT configured")
	}
	if cfg.AppClientID == "" {
		return fail("auth", "no --token and no GitHub App credentials")
	}
	if cfg.AppInstallID == 0 {
		return fail("auth", "GitHub App set but --app-installation-id is missing")
	}
	if _, err := cfg.ResolvePrivateKey(); err != nil {
		return fail("auth", "GitHub App private key unreadable: %v", err)
	}
	return pass("auth", "GitHub App (client %s, installation %d)", cfg.AppClientID, cfg.AppInstallID)
}

func apiReach(url string) check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return warn("github-api", "%s unreachable: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return warn("github-api", "%s returned HTTP %d", url, resp.StatusCode)
	}
	return pass("github-api", "reachable (HTTP %d)", resp.StatusCode)
}

// --- host probes ----------------------------------------------------------

type vmInfo struct {
	name        string
	rootfsBytes int64
	started     time.Time
}

// activeVMs enumerates per-job work dirs (each holds a rootfs clone + API
// socket), matching the provisioner's own layout.
func activeVMs(fc provisioner.FirecrackerConfig) []vmInfo {
	var out []vmInfo
	socks, _ := filepath.Glob(filepath.Join(fc.WorkDir, "*", "fc.sock"))
	if fc.Jailer {
		js, _ := filepath.Glob(filepath.Join(fc.ChrootBase, "firecracker", "*", "root", "run", "firecracker.socket"))
		socks = append(socks, js...)
	}
	for _, sock := range socks {
		dir := filepath.Dir(sock)
		name := filepath.Base(dir)
		if strings.HasSuffix(sock, "firecracker.socket") {
			// <base>/firecracker/<id>/root/run/firecracker.socket -> id.
			name = filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(sock))))
		}
		v := vmInfo{name: name}
		if fi, err := os.Stat(sock); err == nil {
			v.started = fi.ModTime()
		}
		rootfs := filepath.Join(dir, "rootfs.ext4")
		if st, err := os.Stat(rootfs); err == nil {
			v.rootfsBytes = actualBytes(st)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// firecrackerProcs counts running firecracker processes via /proc/*/comm.
func firecrackerProcs() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !isPID(e.Name()) {
			continue
		}
		if strings.TrimSpace(readFile(filepath.Join("/proc", e.Name(), "comm"))) == "firecracker" {
			n++
		}
	}
	return n
}

func countTaps(prefix string) int {
	ifaces, err := net.Interfaces()
	if err != nil {
		return -1
	}
	n := 0
	for _, i := range ifaces {
		if strings.HasPrefix(i.Name, prefix) {
			n++
		}
	}
	return n
}

// --- small helpers --------------------------------------------------------

func describeFile(path string) string {
	if path == "" {
		return "(unset)"
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("%s  MISSING", path)
	}
	return fmt.Sprintf("%s  (%s, %s)", path, humanBytes(fi.Size()), fi.ModTime().Format("2006-01-02 15:04"))
}

func ifaceState(name string) string {
	if name == "" {
		return "(unset)"
	}
	if _, err := net.InterfaceByName(name); err != nil {
		return name + "(missing)"
	}
	return name
}

func egressDesc(cats []string) string {
	if len(cats) == 0 {
		return "open"
	}
	return strings.Join(cats, "+")
}

func fsKind(dir string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return "unknown"
	}
	switch int64(st.Type) {
	case 0x9123683E:
		return "btrfs"
	case 0x58465342:
		return "xfs"
	case 0xEF53:
		return "ext2/3/4"
	case 0x01021994:
		return "tmpfs"
	case 0x2FC12FC1:
		return "zfs"
	default:
		return fmt.Sprintf("fstype=0x%x", uint64(st.Type))
	}
}

func reflinkCapable(kind string) bool {
	switch kind {
	case "btrfs", "xfs", "zfs":
		return true
	default:
		return false
	}
}

// actualBytes reports on-disk usage (st_blocks*512), so a reflink/sparse clone
// shows what it really consumes rather than its apparent size.
func actualBytes(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// isPID reports whether name is all digits (a /proc PID directory).
func isPID(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
