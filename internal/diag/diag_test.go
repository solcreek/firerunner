package diag

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solcreek/firerunner/internal/config"
	"github.com/solcreek/firerunner/internal/provisioner"
)

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:          "0B",
		512:        "512B",
		1024:       "1.0KiB",
		1536:       "1.5KiB",
		1 << 20:    "1.0MiB",
		9993822208: "9.3GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestIsPID(t *testing.T) {
	for _, s := range []string{"1", "2474324"} {
		if !isPID(s) {
			t.Errorf("isPID(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "cpuinfo", "12a", "self"} {
		if isPID(s) {
			t.Errorf("isPID(%q) = true, want false", s)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("Firecracker v1.10.1\nextra\n"); got != "Firecracker v1.10.1" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("  one line  "); got != "one line" {
		t.Errorf("firstLine (single) = %q", got)
	}
}

func TestEgressDesc(t *testing.T) {
	if got := egressDesc(nil); got != "none (deny-all)" {
		t.Errorf("egressDesc(nil) = %q, want deny-all (empty list drops all new egress)", got)
	}
	if got := egressDesc([]string{}); got != "none (deny-all)" {
		t.Errorf("egressDesc([]) = %q, want deny-all", got)
	}
	if got := egressDesc([]string{"open"}); got != "open" {
		t.Errorf("egressDesc(open) = %q", got)
	}
	if got := egressDesc([]string{"api", "git"}); got != "api+git" {
		t.Errorf("egressDesc(api,git) = %q", got)
	}
}

func TestProcMatchesInstance(t *testing.T) {
	direct := provisioner.FirecrackerConfig{WorkDir: "/var/tmp/firerunner"}
	jail := provisioner.FirecrackerConfig{WorkDir: "/var/tmp/firerunner", Jailer: true, ChrootBase: "/srv/jailer"}

	// Direct mode: this instance's VMM carries the work dir in its cmdline.
	if !procMatchesInstance("/usr/bin/firecracker --api-sock /var/tmp/firerunner/fr-abc/fc.sock --id fr-abc", "/", direct) {
		t.Error("own direct-mode VMM not matched")
	}
	// A peer instance with a different work dir must not match.
	if procMatchesInstance("/usr/bin/firecracker --api-sock /var/tmp/other/fk-xyz/fc.sock --id fk-xyz", "/", direct) {
		t.Error("peer direct-mode VMM wrongly matched")
	}
	// Jailer mode: the chrooted VMM's root resolves under ChrootBase.
	if !procMatchesInstance("/usr/bin/firecracker --api-sock /run/firecracker.socket --id fr-abc", "/srv/jailer/firecracker/fr-abc/root", jail) {
		t.Error("own jailer-mode VMM not matched")
	}
	if procMatchesInstance("/usr/bin/firecracker --api-sock /run/firecracker.socket --id fk-xyz", "/srv/other/firecracker/fk-xyz/root", jail) {
		t.Error("peer jailer-mode VMM wrongly matched")
	}
}

func TestFileCheck(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(dir, "small.bin")
	if err := os.WriteFile(small, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if c := fileCheck("golden", big, 1<<20); c.Level != levelPass {
		t.Errorf("big file: level = %s, detail = %s", c.Level, c.Detail)
	}
	if c := fileCheck("golden", small, 1<<20); c.Level != levelWarn {
		t.Errorf("small file: level = %s, want WARN", c.Level)
	}
	if c := fileCheck("golden", filepath.Join(dir, "nope"), 1); c.Level != levelFail {
		t.Errorf("missing file: level = %s, want FAIL", c.Level)
	}
	if c := fileCheck("golden", "", 1); c.Level != levelFail {
		t.Errorf("empty path: level = %s, want FAIL", c.Level)
	}
}

func TestAuthCheck(t *testing.T) {
	if c := authCheck(&config.Config{Token: "ghp_x"}); c.Level != levelPass {
		t.Errorf("token: %s %s", c.Level, c.Detail)
	}
	if c := authCheck(&config.Config{}); c.Level != levelFail {
		t.Errorf("no creds: want FAIL, got %s", c.Level)
	}
	if c := authCheck(&config.Config{AppClientID: "id"}); c.Level != levelFail {
		t.Errorf("app without installation id: want FAIL, got %s", c.Level)
	}
}

// TestDoctor_ReportsFailures checks that Doctor surfaces missing required inputs
// as FAILs, returns an error, and still prints every check line.
func TestDoctor_ReportsFailures(t *testing.T) {
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir()}}
	var buf bytes.Buffer
	err := Doctor(cfg, "test", &buf, false)
	if err == nil {
		t.Fatal("Doctor returned nil error despite missing kernel/golden/ext-iface/auth")
	}
	out := buf.String()
	for _, want := range []string{"FAIL kernel", "FAIL golden", "FAIL ext-iface", "FAIL auth"} {
		if !strings.Contains(out, want) {
			t.Errorf("Doctor output missing %q\n%s", want, out)
		}
	}
}

// TestDoctor_ChecksPerTierGolden verifies that in tier-catalog mode Doctor
// checks each tier's golden (and toolcache) by name instead of the top-level
// --golden, which is legitimately empty when a tier catalog is configured.
func TestDoctor_ChecksPerTierGolden(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "golden.ext4")
	if err := os.WriteFile(big, make([]byte, 65<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Firecracker: provisioner.FirecrackerConfig{WorkDir: dir},
		Tiers: []config.Tier{
			{Name: "base", Golden: big},
			{Name: "missing", Golden: filepath.Join(dir, "nope.ext4")},
		},
	}
	r := runDoctor(cfg, "test")
	var names []string
	var basePass, missingFail bool
	for _, c := range r.Checks {
		names = append(names, c.Name)
		if c.Name == "golden[base]" && c.Level == levelPass {
			basePass = true
		}
		if c.Name == "golden[missing]" && c.Level == levelFail {
			missingFail = true
		}
		if c.Name == "golden" {
			t.Errorf("tier mode must not check the empty top-level golden; got %v", c)
		}
	}
	if !basePass {
		t.Errorf("expected golden[base] to pass; checks=%v", names)
	}
	if !missingFail {
		t.Errorf("expected golden[missing] to fail; checks=%v", names)
	}
}

// TestStatus_RendersPartialConfig ensures Status never errors on an unset/partial
// config and clearly marks unset images.
func TestStatus_RendersPartialConfig(t *testing.T) {
	cfg := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16}}
	var buf bytes.Buffer
	if err := Status(cfg, "test", &buf, false); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"firerunner", "golden", "(unset)", "microVMs: 0 active"} {
		if !strings.Contains(out, want) {
			t.Errorf("Status output missing %q\n%s", want, out)
		}
	}
}

// TestStatus_JSON ensures --json emits a well-formed StatusReport.
func TestStatus_JSON(t *testing.T) {
	cfg := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16}}
	var buf bytes.Buffer
	if err := Status(cfg, "test", &buf, true); err != nil {
		t.Fatalf("Status --json: %v", err)
	}
	var r StatusReport
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal StatusReport: %v\n%s", err, buf.String())
	}
	if r.Version != "test" {
		t.Errorf("version = %q, want test", r.Version)
	}
	if r.ScaleSet != "firerunner" {
		t.Errorf("scale_set = %q, want firerunner", r.ScaleSet)
	}
}

// TestStatus_Cache ensures a configured dependency cache appears in both
// renderings, and that the default (off) is clearly marked.
func TestStatus_Cache(t *testing.T) {
	// Off by default.
	off := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16}}
	var offBuf bytes.Buffer
	if err := Status(off, "test", &offBuf, false); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(offBuf.String(), "GitHub's hosted cache") {
		t.Errorf("default status should note GitHub's hosted cache\n%s", offBuf.String())
	}

	// Gateway mode (local port).
	gw := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16, CachePort: 8099}}
	var gwText, gwJSON bytes.Buffer
	if err := Status(gw, "test", &gwText, false); err != nil {
		t.Fatalf("Status text: %v", err)
	}
	if !strings.Contains(gwText.String(), "gateway:8099") {
		t.Errorf("gateway status missing port\n%s", gwText.String())
	}
	if err := Status(gw, "test", &gwJSON, true); err != nil {
		t.Fatalf("Status json: %v", err)
	}
	var r StatusReport
	if err := json.Unmarshal(gwJSON.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Cache == nil || r.Cache.Mode != "gateway" || r.Cache.Port != 8099 {
		t.Errorf("cache json = %+v, want gateway/8099", r.Cache)
	}

	// URL mode.
	u := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16, CacheURL: "http://cache.internal:8099"}}
	var uText bytes.Buffer
	if err := Status(u, "test", &uText, false); err != nil {
		t.Fatalf("Status text: %v", err)
	}
	if !strings.Contains(uText.String(), "http://cache.internal:8099") {
		t.Errorf("url status missing URL\n%s", uText.String())
	}
}

// TestStatus_Tiers ensures a configured tier catalog appears in both renderings.
func TestStatus_Tiers(t *testing.T) {
	cfg := &config.Config{
		ScaleSetName: "firerunner",
		MaxRunners:   8,
		Firecracker:  provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16},
		Tiers: []config.Tier{
			{Name: "firerunner", VCPU: 2, MemMiB: 4096, Golden: "/g/base.ext4", Min: 1, Max: 8},
			{Name: "firerunner-8c16g", VCPU: 8, MemMiB: 16384, Golden: "/g/base.ext4", Min: 0, Max: 2},
		},
	}

	var text bytes.Buffer
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatalf("Status text: %v", err)
	}
	for _, want := range []string{"tiers: 2", "firerunner-8c16g", "16384MiB"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text status missing %q\n%s", want, text.String())
		}
	}

	var buf bytes.Buffer
	if err := Status(cfg, "test", &buf, true); err != nil {
		t.Fatalf("Status --json: %v", err)
	}
	var r StatusReport
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Tiers) != 2 || r.Tiers[1].Name != "firerunner-8c16g" || r.Tiers[1].VCPU != 8 {
		t.Errorf("tiers = %+v", r.Tiers)
	}
}

// TestDoctor_JSON ensures --json emits a DoctorReport reflecting failures.
func TestDoctor_JSON(t *testing.T) {
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir()}}
	var buf bytes.Buffer
	if err := Doctor(cfg, "test", &buf, true); err == nil {
		t.Fatal("Doctor --json returned nil error despite missing inputs")
	}
	var r DoctorReport
	if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
		t.Fatalf("unmarshal DoctorReport: %v\n%s", err, buf.String())
	}
	if r.OK {
		t.Error("report OK = true, want false")
	}
	if r.Failed == 0 || r.Failed != countFails(r.Checks) {
		t.Errorf("failed = %d, checks-derived = %d", r.Failed, countFails(r.Checks))
	}
}

func countFails(checks []Check) int {
	n := 0
	for _, c := range checks {
		if c.Level == levelFail {
			n++
		}
	}
	return n
}
