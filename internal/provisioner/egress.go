package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// natTable is the dedicated nftables table firerunner manages, isolated from any
// other host firewall rules.
const natTable = "firerunner"

// metaURL is GitHub's published inventory of its own IP ranges.
const metaURL = "https://api.github.com/meta"

// metaCategories are the api.github.com/meta keys firerunner understands.
var metaCategories = map[string]bool{"api": true, "actions": true, "git": true, "packages": true}

// EgressConfig describes what microVM guests are permitted to reach.
type EgressConfig struct {
	// Categories mixes GitHub /meta categories (api, actions, git, packages)
	// with pseudo-categories: "dns" (allow resolvers), "ntp" (allow time sync),
	// and "open" (no allowlist — blanket NAT).
	Categories []string
	// DNSServers are the resolver IPs guests may reach on port 53.
	DNSServers []string
	// RefreshInterval controls how often the /meta ranges are re-fetched. Zero
	// disables periodic refresh.
	RefreshInterval time.Duration
}

func (e EgressConfig) has(category string) bool {
	for _, c := range e.Categories {
		if strings.EqualFold(c, category) {
			return true
		}
	}
	return false
}

// open reports whether egress is unrestricted (blanket NAT, no allowlist).
func (e EgressConfig) open() bool { return e.has("open") }

// metaCats returns the GitHub /meta categories selected in the config.
func (e EgressConfig) metaCats() []string {
	var out []string
	for _, c := range e.Categories {
		if metaCategories[strings.ToLower(c)] {
			out = append(out, strings.ToLower(c))
		}
	}
	return out
}

// fetchMetaCIDRs fetches api.github.com/meta and returns the IPv4 CIDRs for the
// requested categories. IPv6 ranges are skipped (guests are IPv4-only).
func fetchMetaCIDRs(ctx context.Context, cl *http.Client, url string, categories []string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("meta API returned %s", resp.Status)
	}

	var meta map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode meta: %w", err)
	}

	seen := map[string]bool{}
	var cidrs []string
	for _, cat := range categories {
		raw, ok := meta[cat]
		if !ok {
			continue
		}
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("decode meta category %q: %w", cat, err)
		}
		for _, c := range list {
			if strings.Contains(c, ":") { // skip IPv6
				continue
			}
			if !seen[c] {
				seen[c] = true
				cidrs = append(cidrs, c)
			}
		}
	}
	sort.Strings(cidrs)
	return cidrs, nil
}

// buildNFTRuleset returns a complete nftables ruleset for the firerunner table.
// It atomically replaces the table (add/delete/define idiom) so it is idempotent
// across runs and refreshes. When open, guests get blanket egress to everything
// except each other (a forward chain drops intra-CIDR guest-to-guest traffic);
// otherwise a forward filter drops any guest traffic not matching the allowlist
// (which also blocks guest-to-guest). It is pure so the generated policy can be
// asserted in tests.
func buildNFTRuleset(cfg egressRuleset) string {
	table := cfg.Table
	if table == "" {
		table = natTable
	}
	var b strings.Builder
	// Atomic replace: ensure the table exists, delete it, recreate fresh.
	fmt.Fprintf(&b, "add table ip %s\n", table)
	fmt.Fprintf(&b, "delete table ip %s\n", table)
	fmt.Fprintf(&b, "table ip %s {\n", table)

	if !cfg.Open {
		fmt.Fprintf(&b, "\tset allowed {\n\t\ttype ipv4_addr\n\t\tflags interval\n\t\tauto-merge\n")
		if len(cfg.Allowed) > 0 {
			fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(cfg.Allowed, ", "))
		}
		b.WriteString("\t}\n")

		if cfg.AllowDNS && len(cfg.DNSServers) > 0 {
			fmt.Fprintf(&b, "\tset dns {\n\t\ttype ipv4_addr\n\t\telements = { %s }\n\t}\n", strings.Join(cfg.DNSServers, ", "))
		}

		b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority 0; policy accept;\n")
		fmt.Fprintf(&b, "\t\tip saddr %s ct state established,related accept\n", cfg.VMCidr)
		fmt.Fprintf(&b, "\t\tip saddr %s ip daddr @allowed accept\n", cfg.VMCidr)
		if cfg.AllowDNS && len(cfg.DNSServers) > 0 {
			fmt.Fprintf(&b, "\t\tip saddr %s udp dport 53 ip daddr @dns accept\n", cfg.VMCidr)
			fmt.Fprintf(&b, "\t\tip saddr %s tcp dport 53 ip daddr @dns accept\n", cfg.VMCidr)
		}
		if cfg.AllowNTP {
			fmt.Fprintf(&b, "\t\tip saddr %s udp dport 123 accept\n", cfg.VMCidr)
		}
		fmt.Fprintf(&b, "\t\tip saddr %s drop\n", cfg.VMCidr)
		b.WriteString("\t}\n")
	} else {
		// Even with unrestricted egress, guests must not reach each other: a
		// per-VM /30 tap already blocks L2 peers, but in open mode the host would
		// otherwise route L3 traffic between guests on the same instance. Drop
		// intra-CIDR traffic while leaving all other forwarding (guest->internet)
		// to the accept policy.
		b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority 0; policy accept;\n")
		fmt.Fprintf(&b, "\t\tip saddr %s ip daddr %s drop\n", cfg.VMCidr, cfg.VMCidr)
		b.WriteString("\t}\n")
	}

	b.WriteString("\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, "\t\tip saddr %s oifname \"%s\" masquerade\n", cfg.VMCidr, cfg.ExtIface)
	b.WriteString("\t}\n}\n")
	return b.String()
}

// egressRuleset is the fully-resolved input to buildNFTRuleset.
type egressRuleset struct {
	// Table is the nftables table name to manage. Empty falls back to natTable
	// so a second firerunner instance can own an isolated table.
	Table      string
	ExtIface   string
	VMCidr     string
	Allowed    []string
	DNSServers []string
	AllowDNS   bool
	AllowNTP   bool
	Open       bool
}
