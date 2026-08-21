package provisioner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleMeta = `{
	"verifiable_password_authentication": false,
	"api": ["192.30.252.0/22", "2606:50c0::/32"],
	"actions": ["13.64.0.0/16", "192.30.252.0/22"],
	"git": ["140.82.112.0/20"],
	"packages": ["140.82.121.0/24"],
	"web": ["1.2.3.0/24"]
}`

func TestFetchMetaCIDRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleMeta))
	}))
	defer srv.Close()

	cidrs, err := fetchMetaCIDRs(context.Background(), srv.Client(), srv.URL, []string{"api", "actions", "git", "packages"})
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(cidrs, ",")
	for _, want := range []string{"192.30.252.0/22", "13.64.0.0/16", "140.82.112.0/20", "140.82.121.0/24"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing CIDR %q in %v", want, cidrs)
		}
	}
	// IPv6 must be filtered out.
	if strings.Contains(joined, ":") {
		t.Errorf("IPv6 range not filtered: %v", cidrs)
	}
	// "web" was not requested, so must be absent.
	if strings.Contains(joined, "1.2.3.0/24") {
		t.Errorf("unrequested category leaked: %v", cidrs)
	}
	// Dedup: 192.30.252.0/22 appears in both api and actions but once here.
	if n := strings.Count(joined, "192.30.252.0/22"); n != 1 {
		t.Errorf("duplicate CIDR count = %d, want 1", n)
	}
}

func TestFetchMetaCIDRsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := fetchMetaCIDRs(context.Background(), srv.Client(), srv.URL, []string{"api"}); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

// TestBuildNFTRulesetCustomTable asserts a second instance can own an isolated
// nft table so the two rulesets never clobber each other.
func TestBuildNFTRulesetCustomTable(t *testing.T) {
	out := buildNFTRuleset(egressRuleset{
		Table:    "firerunner_node",
		ExtIface: "enp2s0",
		VMCidr:   "172.17.0.0/16",
		Open:     true,
	})
	for _, want := range []string{
		"add table ip firerunner_node",
		"delete table ip firerunner_node",
		"table ip firerunner_node {",
		"172.17.0.0/16",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("ruleset missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "table ip firerunner {") {
		t.Fatalf("custom table leaked the default name:\n%s", out)
	}
}

func TestBuildNFTRulesetAllowlist(t *testing.T) {
	rs := egressRuleset{
		ExtIface:   "enp2s0",
		VMCidr:     vmCIDR,
		Allowed:    []string{"140.82.112.0/20", "13.64.0.0/16"},
		DNSServers: []string{"1.1.1.1", "8.8.8.8"},
		AllowDNS:   true,
		AllowNTP:   true,
	}
	out := buildNFTRuleset(rs)

	// Atomic table-replace idiom.
	for _, want := range []string{
		"add table ip firerunner",
		"delete table ip firerunner",
		"table ip firerunner {",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing table-replace line %q", want)
		}
	}
	// Allowlist set + elements. auto-merge lets nftables fold the overlapping
	// CIDRs GitHub publishes in /meta (real nft rejects conflicting intervals).
	if !strings.Contains(out, "set allowed {") || !strings.Contains(out, "flags interval") || !strings.Contains(out, "auto-merge") {
		t.Errorf("missing allowed set:\n%s", out)
	}
	for _, cidr := range rs.Allowed {
		if !strings.Contains(out, cidr) {
			t.Errorf("allowed set missing %q", cidr)
		}
	}
	// DNS set + rules.
	if !strings.Contains(out, "set dns {") || !strings.Contains(out, "udp dport 53 ip daddr @dns accept") {
		t.Errorf("missing dns handling:\n%s", out)
	}
	// NTP rule.
	if !strings.Contains(out, "udp dport 123 accept") {
		t.Errorf("missing ntp rule:\n%s", out)
	}
	// Forward chain, allowlist accept and default drop.
	if !strings.Contains(out, "hook forward") || !strings.Contains(out, "ip daddr @allowed accept") {
		t.Errorf("missing forward allow rule:\n%s", out)
	}
	if !strings.Contains(out, vmCIDR+" drop") {
		t.Errorf("missing default drop for guests:\n%s", out)
	}
	// Masquerade always present.
	if !strings.Contains(out, "masquerade") || !strings.Contains(out, `oifname "enp2s0"`) {
		t.Errorf("missing masquerade:\n%s", out)
	}
}

func TestBuildNFTRulesetOpen(t *testing.T) {
	out := buildNFTRuleset(egressRuleset{ExtIface: "eth0", VMCidr: vmCIDR, Open: true})
	if strings.Contains(out, "chain forward") {
		t.Errorf("open mode must not install a forward filter:\n%s", out)
	}
	if strings.Contains(out, "@allowed") || strings.Contains(out, "drop") {
		t.Errorf("open mode must not have allowlist/drop:\n%s", out)
	}
	if !strings.Contains(out, "masquerade") {
		t.Errorf("open mode still needs masquerade:\n%s", out)
	}
}

func TestBuildNFTRulesetNoDNSNoNTP(t *testing.T) {
	out := buildNFTRuleset(egressRuleset{
		ExtIface: "eth0",
		VMCidr:   vmCIDR,
		Allowed:  []string{"140.82.112.0/20"},
	})
	if strings.Contains(out, "set dns") || strings.Contains(out, "dport 53") {
		t.Errorf("dns must be absent when not allowed:\n%s", out)
	}
	if strings.Contains(out, "dport 123") {
		t.Errorf("ntp must be absent when not allowed:\n%s", out)
	}
	if !strings.Contains(out, "ip daddr @allowed accept") {
		t.Errorf("allowlist still expected:\n%s", out)
	}
}

func TestEgressConfigHelpers(t *testing.T) {
	e := EgressConfig{Categories: []string{"api", "Actions", "dns", "open"}}
	if !e.open() {
		t.Error("open() should be true")
	}
	if !e.has("DNS") {
		t.Error("has() should be case-insensitive")
	}
	cats := e.metaCats()
	if len(cats) != 2 {
		t.Fatalf("metaCats = %v, want api+actions", cats)
	}
	joined := strings.Join(cats, ",")
	if !strings.Contains(joined, "api") || !strings.Contains(joined, "actions") {
		t.Errorf("metaCats = %v", cats)
	}
}
