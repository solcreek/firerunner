package config

import (
	"testing"
)

// baseArgs is a minimal valid flag set; individual tests mutate a copy.
func baseArgs() []string {
	return []string{
		"--url", "https://github.com/org/repo",
		"--kernel", "/vmlinux",
		"--golden", "/golden.ext4",
		"--token", "ghp_x",
	}
}

func TestFromFlags_Valid(t *testing.T) {
	c, err := FromFlags(append(baseArgs(),
		"--name", "fr", "--labels", "a, b ,,c", "--vcpu", "8", "--mem-mib", "16384", "--max-runners", "6"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.ScaleSetName != "fr" {
		t.Fatalf("name = %q", c.ScaleSetName)
	}
	if got := c.Labels; len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("labels = %v, want [a b c]", got)
	}
	spec := c.RunnerSpec()
	if spec.VCPU != 8 || spec.MemMiB != 16384 || spec.RootFS != "/golden.ext4" {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestFromFlags_MissingRequired(t *testing.T) {
	cases := map[string][]string{
		"no url":    {"--kernel", "/k", "--golden", "/g", "--token", "t"},
		"no kernel": {"--url", "u", "--golden", "/g", "--token", "t"},
		"no golden": {"--url", "u", "--kernel", "/k", "--token", "t"},
		"no auth":   {"--url", "u", "--kernel", "/k", "--golden", "/g"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromFlags(args); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestFromFlags_BoundsValidation(t *testing.T) {
	cases := map[string][]string{
		"vcpu":  {"--vcpu", "0"},
		"mem":   {"--mem-mib", "64"},
		"maxrn": {"--max-runners", "0"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromFlags(append(baseArgs(), extra...)); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestFromFlags_EnvFallback(t *testing.T) {
	t.Setenv("FR_URL", "https://github.com/org/repo")
	t.Setenv("FR_KERNEL", "/env-kernel")
	t.Setenv("FR_GOLDEN", "/env-golden")
	t.Setenv("FR_TOKEN", "ghp_env")
	t.Setenv("FR_VCPU", "2")

	c, err := FromFlags(nil)
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.KernelImage != "/env-kernel" || c.VCPU != 2 || c.Token != "ghp_env" {
		t.Fatalf("env fallback not applied: %+v", c)
	}
}

func TestFromFlags_AppCredsSatisfyAuth(t *testing.T) {
	args := []string{
		"--url", "u", "--kernel", "/k", "--golden", "/g",
		"--app-client-id", "Iv1.abc",
	}
	if _, err := FromFlags(args); err != nil {
		t.Fatalf("app creds should satisfy auth: %v", err)
	}
}

func TestParseLevel(t *testing.T) {
	if parseLevel("debug").String() != "DEBUG" {
		t.Fatal("debug")
	}
	if parseLevel("warn").String() != "WARN" {
		t.Fatal("warn")
	}
	if parseLevel("bogus").String() != "INFO" {
		t.Fatal("default should be INFO")
	}
}
