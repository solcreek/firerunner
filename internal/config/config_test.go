package config

import (
	"os"
	"path/filepath"
	"testing"
)

// baseArgs is a minimal valid flag set; individual tests mutate a copy.
func baseArgs() []string {
	return []string{
		"--url", "https://github.com/org/repo",
		"--kernel", "/vmlinux",
		"--golden", "/golden.ext4",
		"--ext-iface", "eth0",
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
		"no url":    {"--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0", "--token", "t"},
		"no kernel": {"--url", "u", "--golden", "/g", "--ext-iface", "eth0", "--token", "t"},
		"no golden": {"--url", "u", "--kernel", "/k", "--ext-iface", "eth0", "--token", "t"},
		"no iface":  {"--url", "u", "--kernel", "/k", "--golden", "/g", "--token", "t"},
		"no auth":   {"--url", "u", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0"},
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
	t.Setenv("FR_EXT_IFACE", "eth0")
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
		"--url", "u", "--kernel", "/k", "--golden", "/g", "--ext-iface", "eth0",
		"--app-client-id", "Iv1.abc",
	}
	if _, err := FromFlags(args); err != nil {
		t.Fatalf("app creds should satisfy auth: %v", err)
	}
}

func TestFromFlags_JailerCgroup(t *testing.T) {
	c, err := FromFlags(append(baseArgs(),
		"--jailer", "--jail-uid", "955", "--jail-gid", "954",
		"--jailer-cgroup", "memory.max=2147483648;cpu.max=200000 ; ;pids.max=512"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	got := c.Firecracker.CgroupLimits
	want := []string{"memory.max=2147483648", "cpu.max=200000", "pids.max=512"}
	if len(got) != len(want) {
		t.Fatalf("cgroup limits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("limit %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFromFlags_JailerCgroupErrors(t *testing.T) {
	cases := map[string][]string{
		"without jailer": {"--jailer-cgroup", "memory.max=1"},
		"bad format": {
			"--jailer", "--jail-uid", "955", "--jail-gid", "954",
			"--jailer-cgroup", "memory.max=1;bogus",
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FromFlags(append(baseArgs(), extra...)); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestFromFlags_NetNS(t *testing.T) {
	c, err := FromFlags(append(baseArgs(),
		"--jailer", "--jail-uid", "955", "--jail-gid", "954", "--netns"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if !c.Firecracker.NetNS {
		t.Fatal("NetNS should be true")
	}
}

func TestFromFlags_NetNSRequiresJailer(t *testing.T) {
	if _, err := FromFlags(append(baseArgs(), "--netns")); err == nil {
		t.Fatal("expected error: --netns requires --jailer")
	}
}

func TestFromFlags_ToolCache(t *testing.T) {
	img := filepath.Join(t.TempDir(), "toolcache-go.ext4")
	if err := os.WriteFile(img, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := FromFlags(append(baseArgs(), "--toolcache", img))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if c.Firecracker.ToolCacheImage != img {
		t.Fatalf("ToolCacheImage = %q, want %q", c.Firecracker.ToolCacheImage, img)
	}
}

func TestFromFlags_ToolCacheMissing(t *testing.T) {
	if _, err := FromFlags(append(baseArgs(), "--toolcache", "/no/such/toolcache.ext4")); err == nil {
		t.Fatal("expected error for a missing --toolcache image")
	}
}

func TestEffectiveTiers_DefaultSingleTier(t *testing.T) {
	c, err := FromFlags(append(baseArgs(), "--name", "fr", "--vcpu", "2", "--mem-mib", "4096", "--max-runners", "8", "--min-runners", "1"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	tiers := c.EffectiveTiers()
	if len(tiers) != 1 {
		t.Fatalf("want 1 synthesized tier, got %d", len(tiers))
	}
	tr := tiers[0]
	if tr.Name != "fr" || tr.VCPU != 2 || tr.MemMiB != 4096 || tr.Golden != "/golden.ext4" || tr.Min != 1 || tr.Max != 8 {
		t.Fatalf("synthesized tier = %+v", tr)
	}
	if spec := tr.Spec(); spec.VCPU != 2 || spec.RootFS != "/golden.ext4" {
		t.Fatalf("tier spec = %+v", spec)
	}
}

func writeTiers(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tiers.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFromFlags_TierCatalog(t *testing.T) {
	p := writeTiers(t, `[
	  {"name":"firerunner","vcpu":2,"mem_mib":4096,"golden":"/g/base.ext4","min":1,"max":8},
	  {"name":"firerunner-8c16g","vcpu":8,"mem_mib":16384,"golden":"/g/base.ext4","min":0,"max":2},
	  {"name":"firerunner-node","labels":["node"],"vcpu":2,"mem_mib":4096,"golden":"/g/node.ext4","min":0,"max":4}
	]`)
	c, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "8"))
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	tiers := c.EffectiveTiers()
	if len(tiers) != 3 {
		t.Fatalf("want 3 tiers, got %d", len(tiers))
	}
	if tiers[1].Name != "firerunner-8c16g" || tiers[1].VCPU != 8 || tiers[1].MemMiB != 16384 {
		t.Fatalf("tier[1] = %+v", tiers[1])
	}
	if tiers[2].Golden != "/g/node.ext4" || len(tiers[2].Labels) != 1 || tiers[2].Labels[0] != "node" {
		t.Fatalf("tier[2] = %+v", tiers[2])
	}
}

func TestFromFlags_TierCatalogSatisfiesGolden(t *testing.T) {
	// A tier catalog replaces the top-level --golden requirement.
	p := writeTiers(t, `[{"name":"firerunner","vcpu":2,"mem_mib":4096,"golden":"/g/base.ext4","min":0,"max":4}]`)
	args := []string{"--url", "u", "--kernel", "/k", "--ext-iface", "eth0", "--token", "t", "--tiers", p}
	if _, err := FromFlags(args); err != nil {
		t.Fatalf("tier catalog should satisfy the golden requirement: %v", err)
	}
}

func TestFromFlags_TierCatalogErrors(t *testing.T) {
	cases := map[string]string{
		"empty array":     `[]`,
		"missing name":    `[{"vcpu":2,"mem_mib":4096,"golden":"/g","max":2}]`,
		"low vcpu":        `[{"name":"t","vcpu":0,"mem_mib":4096,"golden":"/g","max":2}]`,
		"low mem":         `[{"name":"t","vcpu":2,"mem_mib":64,"golden":"/g","max":2}]`,
		"no golden":       `[{"name":"t","vcpu":2,"mem_mib":4096,"max":2}]`,
		"zero max":        `[{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","max":0}]`,
		"min over max":    `[{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","min":3,"max":2}]`,
		"duplicate names": `[{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","max":2},{"name":"t","vcpu":2,"mem_mib":4096,"golden":"/g","max":2}]`,
		"bad json":        `{not json}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeTiers(t, body)
			if _, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "8")); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestFromFlags_TierWarmPoolsExceedBudget(t *testing.T) {
	// Warm pools sum to 5 but the shared slot budget is 4.
	p := writeTiers(t, `[
	  {"name":"a","vcpu":2,"mem_mib":4096,"golden":"/g","min":3,"max":4},
	  {"name":"b","vcpu":2,"mem_mib":4096,"golden":"/g","min":2,"max":4}
	]`)
	if _, err := FromFlags(append(baseArgs(), "--tiers", p, "--max-runners", "4")); err == nil {
		t.Fatal("expected error: warm pools exceed the shared slot budget")
	}
}

func TestFromFlags_TierFileMissing(t *testing.T) {
	if _, err := FromFlags(append(baseArgs(), "--tiers", "/no/such/tiers.json")); err == nil {
		t.Fatal("expected error for a missing tiers file")
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

func TestResolvePrivateKey(t *testing.T) {
	// Inline PEM contents are returned as-is.
	pem := "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----"
	c := &Config{AppPrivateKey: pem}
	if got, err := c.ResolvePrivateKey(); err != nil || got != pem {
		t.Fatalf("inline PEM: got %q err %v", got, err)
	}

	// Empty stays empty.
	if got, err := (&Config{}).ResolvePrivateKey(); err != nil || got != "" {
		t.Fatalf("empty: got %q err %v", got, err)
	}

	// A path is read from disk.
	dir := t.TempDir()
	path := dir + "/key.pem"
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		t.Fatal(err)
	}
	c = &Config{AppPrivateKey: path}
	if got, err := c.ResolvePrivateKey(); err != nil || got != pem {
		t.Fatalf("path: got %q err %v", got, err)
	}

	// A missing path errors.
	if _, err := (&Config{AppPrivateKey: dir + "/missing.pem"}).ResolvePrivateKey(); err == nil {
		t.Fatal("expected error for missing key file")
	}
}
