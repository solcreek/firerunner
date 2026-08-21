// Package config loads firerunner configuration from flags and environment.
// It intentionally uses only the standard library (flag/os) to keep the
// dependency surface minimal.
package config

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/solcreek/firerunner/internal/core"
	"github.com/solcreek/firerunner/internal/provisioner"
)

// Config is the fully-resolved firerunner configuration.
type Config struct {
	// GitHub scale-set registration.
	URL          string
	ScaleSetName string
	RunnerGroup  string
	Labels       []string
	MaxRunners   int
	MinRunners   int

	// Auth: either a PAT or a GitHub App (App preferred, per GitHub guidance).
	Token         string
	AppClientID   string
	AppInstallID  int64
	AppPrivateKey string

	// Runner microVM shape.
	VCPU   int
	MemMiB int

	Firecracker provisioner.FirecrackerConfig

	LogLevel  string
	LogFormat string
}

// RunnerSpec derives the microVM spec from the config.
func (c *Config) RunnerSpec() core.RunnerSpec {
	return core.RunnerSpec{
		Labels: c.Labels,
		VCPU:   c.VCPU,
		MemMiB: c.MemMiB,
		RootFS: c.Firecracker.GoldenRootFS,
	}
}

// FromFlags parses configuration from the given argument list, falling back to
// FR_* environment variables. It validates required fields.
func FromFlags(args []string) (*Config, error) {
	fs := flag.NewFlagSet("firerunner", flag.ContinueOnError)
	c := &Config{}
	var labels string

	fs.StringVar(&c.URL, "url", env("FR_URL", ""), "GitHub org or repo URL to register the scale set (required)")
	fs.StringVar(&c.ScaleSetName, "name", env("FR_NAME", "firerunner"), "scale set name; also the runs-on label")
	fs.StringVar(&c.RunnerGroup, "runner-group", env("FR_RUNNER_GROUP", "default"), "runner group name")
	fs.StringVar(&labels, "labels", env("FR_LABELS", ""), "extra comma-separated runs-on labels")
	fs.IntVar(&c.MaxRunners, "max-runners", envInt("FR_MAX_RUNNERS", 4), "max concurrent microVMs")
	fs.IntVar(&c.MinRunners, "min-runners", envInt("FR_MIN_RUNNERS", 0), "warm pool size: microVMs kept pre-booted and registered so jobs start without a cold boot (0 = on-demand; higher = faster pickup, more idle RAM)")

	fs.StringVar(&c.Token, "token", env("FR_TOKEN", ""), "GitHub PAT (use a GitHub App in production)")
	fs.StringVar(&c.AppClientID, "app-client-id", env("FR_APP_CLIENT_ID", ""), "GitHub App client id")
	fs.Int64Var(&c.AppInstallID, "app-installation-id", int64(envInt("FR_APP_INSTALLATION_ID", 0)), "GitHub App installation id")
	fs.StringVar(&c.AppPrivateKey, "app-private-key", env("FR_APP_PRIVATE_KEY", ""), "GitHub App private key (PEM path or contents)")

	fs.IntVar(&c.VCPU, "vcpu", envInt("FR_VCPU", 4), "vCPUs per microVM")
	fs.IntVar(&c.MemMiB, "mem-mib", envInt("FR_MEM_MIB", 8192), "guest memory (MiB) per microVM")

	fs.StringVar(&c.Firecracker.Binary, "firecracker-bin", env("FR_FIRECRACKER_BIN", "firecracker"), "path to firecracker binary")
	fs.StringVar(&c.Firecracker.KernelImage, "kernel", env("FR_KERNEL", ""), "path to guest kernel (vmlinux) (required)")
	fs.StringVar(&c.Firecracker.GoldenRootFS, "golden", env("FR_GOLDEN", ""), "path to immutable golden rootfs (required)")
	fs.StringVar(&c.Firecracker.WorkDir, "workdir", env("FR_WORKDIR", "/var/tmp/firerunner"), "reflink-capable work dir for per-job rootfs")
	fs.StringVar(&c.Firecracker.TapPrefix, "tap-prefix", env("FR_TAP_PREFIX", "fr"), "prefix for per-job tap devices; must be unique per instance when several firerunners share a host")
	fs.IntVar(&c.Firecracker.NetBase, "net-base", envInt("FR_NET_BASE", 16), "second octet of the per-microVM /16 (172.<base>.x); must differ per instance sharing a host")
	fs.StringVar(&c.Firecracker.NFTTable, "nft-table", env("FR_NFT_TABLE", "firerunner"), "nftables table name; must be unique per instance when several firerunners share a host")
	fs.StringVar(&c.Firecracker.BootArgs, "boot-args", env("FR_BOOT_ARGS", ""), "kernel command line (default keeps reboot=k)")
	fs.StringVar(&c.Firecracker.ExtIface, "ext-iface", env("FR_EXT_IFACE", ""), "host external interface for microVM egress NAT (required)")
	fs.StringVar(&c.Firecracker.LogDir, "log-dir", env("FR_LOG_DIR", ""), "directory for per-runner console logs (off-VM log forwarding)")

	fs.BoolVar(&c.Firecracker.Jailer, "jailer", envBool("FR_JAILER", false), "run each microVM under the Firecracker jailer (chroot + PID ns + privilege drop); opt-in, requires a root launcher and --jail-uid/--jail-gid")
	fs.StringVar(&c.Firecracker.JailerBin, "jailer-bin", env("FR_JAILER_BIN", "jailer"), "path to the jailer binary (must match the firecracker version)")
	fs.StringVar(&c.Firecracker.ChrootBase, "chroot-base", env("FR_CHROOT_BASE", "/srv/jailer"), "jailer chroot base dir")
	fs.IntVar(&c.Firecracker.JailUID, "jail-uid", envInt("FR_JAIL_UID", 0), "uid the jailer drops Firecracker to (required with --jailer)")
	fs.IntVar(&c.Firecracker.JailGID, "jail-gid", envInt("FR_JAIL_GID", 0), "gid the jailer drops Firecracker to (required with --jailer)")
	var jailerCgroup string
	fs.StringVar(&jailerCgroup, "jailer-cgroup", env("FR_JAILER_CGROUP", ""), "semicolon-separated cgroup v2 limits applied to each microVM via the jailer, each <file>=<value> (e.g. \"memory.max=2147483648;cpu.max=200000;pids.max=512\"); requires --jailer")
	fs.BoolVar(&c.Firecracker.NetNS, "netns", envBool("FR_NETNS", false), "run each microVM in its own network namespace (tap lives in the netns, veth uplink to the host); strongest network isolation, requires --jailer")
	fs.StringVar(&c.Firecracker.ToolCacheImage, "toolcache", env("FR_TOOLCACHE", ""), "path to a read-only ext4 image (labelled \"hostedtoolcache\", mirroring GitHub's tool-cache layout) attached to every microVM so setup-* actions hit a pre-seeded cache instead of downloading; opt-in accelerator, jobs fall back to downloading when unset")

	var egress, dnsServers string
	var metaRefresh time.Duration
	fs.StringVar(&egress, "egress", env("FR_EGRESS", "api,actions,git,dns,packages,ntp"), "comma-separated egress allowlist: GitHub /meta categories (api,actions,git,packages) plus dns,ntp; or 'open' for no allowlist")
	fs.StringVar(&dnsServers, "dns-servers", env("FR_DNS_SERVERS", "1.1.1.1,8.8.8.8"), "comma-separated resolver IPs guests may reach")
	fs.DurationVar(&metaRefresh, "meta-refresh", envDuration("FR_META_REFRESH", 24*time.Hour), "how often to refresh GitHub /meta ranges (0 disables)")

	fs.StringVar(&c.LogLevel, "log-level", env("FR_LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", env("FR_LOG_FORMAT", "text"), "log format: text or json")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if labels != "" {
		for _, l := range strings.Split(labels, ",") {
			if l = strings.TrimSpace(l); l != "" {
				c.Labels = append(c.Labels, l)
			}
		}
	}
	c.Firecracker.CgroupLimits = splitSemi(jailerCgroup)
	if err := c.validate(); err != nil {
		return nil, err
	}
	// The provisioner's per-VM network pool is bounded by the runner capacity.
	c.Firecracker.MaxVMs = c.MaxRunners
	c.Firecracker.Egress = provisioner.EgressConfig{
		Categories:      splitCSV(egress),
		DNSServers:      splitCSV(dnsServers),
		RefreshInterval: metaRefresh,
	}
	return c, nil
}

// splitCSV splits a comma-separated list, trimming spaces and dropping empties.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitSemi splits a semicolon-separated list, trimming spaces and dropping
// empties. Semicolons (not commas) separate cgroup limits because cgroup values
// such as cpuset.cpus=0-3,5 legitimately contain commas.
func splitSemi(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *Config) validate() error {
	switch {
	case c.URL == "":
		return fmt.Errorf("--url is required")
	case c.Firecracker.KernelImage == "":
		return fmt.Errorf("--kernel is required")
	case c.Firecracker.GoldenRootFS == "":
		return fmt.Errorf("--golden is required")
	case c.Firecracker.ExtIface == "":
		return fmt.Errorf("--ext-iface is required for microVM egress")
	case c.MaxRunners < 1:
		return fmt.Errorf("--max-runners must be >= 1")
	case c.VCPU < 1:
		return fmt.Errorf("--vcpu must be >= 1")
	case c.MemMiB < 128:
		return fmt.Errorf("--mem-mib must be >= 128")
	case c.Token == "" && c.AppClientID == "":
		return fmt.Errorf("provide --token or GitHub App credentials")
	case c.Firecracker.Jailer && (c.Firecracker.JailUID < 1 || c.Firecracker.JailGID < 1):
		return fmt.Errorf("--jail-uid and --jail-gid (>0) are required when --jailer is set")
	case len(c.Firecracker.CgroupLimits) > 0 && !c.Firecracker.Jailer:
		return fmt.Errorf("--jailer-cgroup requires --jailer")
	case c.Firecracker.NetNS && !c.Firecracker.Jailer:
		return fmt.Errorf("--netns requires --jailer")
	}
	if c.Firecracker.ToolCacheImage != "" {
		if _, err := os.Stat(c.Firecracker.ToolCacheImage); err != nil {
			return fmt.Errorf("--toolcache image %q not accessible: %w", c.Firecracker.ToolCacheImage, err)
		}
	}
	for _, l := range c.Firecracker.CgroupLimits {
		if !strings.Contains(l, "=") {
			return fmt.Errorf("--jailer-cgroup entry %q must be <file>=<value>", l)
		}
	}
	return nil
}

// ResolvePrivateKey returns the GitHub App private key in PEM form. The
// configured value may be either the PEM contents or a path to a PEM file.
func (c *Config) ResolvePrivateKey() (string, error) {
	if c.AppPrivateKey == "" {
		return "", nil
	}
	if strings.Contains(c.AppPrivateKey, "PRIVATE KEY") {
		return c.AppPrivateKey, nil
	}
	b, err := os.ReadFile(c.AppPrivateKey)
	if err != nil {
		return "", fmt.Errorf("read app private key: %w", err)
	}
	return string(b), nil
}

// NewLogger builds a slog.Logger from the level and format strings.
func NewLogger(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
