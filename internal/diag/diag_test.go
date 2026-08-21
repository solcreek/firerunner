package diag

import (
	"bytes"
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

	if c := fileCheck("golden", big, 1<<20); c.level != levelPass {
		t.Errorf("big file: level = %s, detail = %s", c.level, c.detail)
	}
	if c := fileCheck("golden", small, 1<<20); c.level != levelWarn {
		t.Errorf("small file: level = %s, want WARN", c.level)
	}
	if c := fileCheck("golden", filepath.Join(dir, "nope"), 1); c.level != levelFail {
		t.Errorf("missing file: level = %s, want FAIL", c.level)
	}
	if c := fileCheck("golden", "", 1); c.level != levelFail {
		t.Errorf("empty path: level = %s, want FAIL", c.level)
	}
}

func TestAuthCheck(t *testing.T) {
	if c := authCheck(&config.Config{Token: "ghp_x"}); c.level != levelPass {
		t.Errorf("token: %s %s", c.level, c.detail)
	}
	if c := authCheck(&config.Config{}); c.level != levelFail {
		t.Errorf("no creds: want FAIL, got %s", c.level)
	}
	if c := authCheck(&config.Config{AppClientID: "id"}); c.level != levelFail {
		t.Errorf("app without installation id: want FAIL, got %s", c.level)
	}
}

// TestDoctor_ReportsFailures checks that Doctor surfaces missing required inputs
// as FAILs, returns an error, and still prints every check line.
func TestDoctor_ReportsFailures(t *testing.T) {
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir()}}
	var buf bytes.Buffer
	err := Doctor(cfg, "test", &buf)
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

// TestStatus_RendersPartialConfig ensures Status never errors on an unset/partial
// config and clearly marks unset images.
func TestStatus_RendersPartialConfig(t *testing.T) {
	cfg := &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16}}
	var buf bytes.Buffer
	if err := Status(cfg, "test", &buf); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"firerunner", "golden", "(unset)", "microVMs: 0 active"} {
		if !strings.Contains(out, want) {
			t.Errorf("Status output missing %q\n%s", want, out)
		}
	}
}
