package provisioner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlotNet(t *testing.T) {
	n := slotNet(5, "fr", 16)
	if n.tap != "fr5" {
		t.Fatalf("tap = %q, want fr5", n.tap)
	}
	if n.hostIP != "172.16.5.1" || n.guestIP != "172.16.5.2" {
		t.Fatalf("ips = host %q guest %q", n.hostIP, n.guestIP)
	}
	if n.netmask != "255.255.255.252" {
		t.Fatalf("netmask = %q", n.netmask)
	}
	if n.guestMAC != "06:00:AC:10:05:02" {
		t.Fatalf("mac = %q, want 06:00:AC:10:05:02", n.guestMAC)
	}
}

func TestSlotNetDistinctSubnets(t *testing.T) {
	a, b := slotNet(0, "fr", 16), slotNet(1, "fr", 16)
	if a.guestIP == b.guestIP || a.tap == b.tap || a.guestMAC == b.guestMAC {
		t.Fatalf("slots overlap: %+v vs %+v", a, b)
	}
}

// TestSlotNetDistinctNetBase asserts that two firerunner instances sharing a
// host but using different network bases never collide on IP, MAC or tap, even
// for the same slot index.
func TestSlotNetDistinctNetBase(t *testing.T) {
	a := slotNet(0, "fr", 16)
	b := slotNet(0, "fn", 17)
	if a.guestIP != "172.16.0.2" || b.guestIP != "172.17.0.2" {
		t.Fatalf("ips = %q / %q, want 172.16.0.2 / 172.17.0.2", a.guestIP, b.guestIP)
	}
	if a.hostIP == b.hostIP || a.guestIP == b.guestIP || a.guestMAC == b.guestMAC || a.tap == b.tap {
		t.Fatalf("instances overlap: %+v vs %+v", a, b)
	}
	if b.guestMAC != "06:00:AC:11:00:02" {
		t.Fatalf("mac = %q, want 06:00:AC:11:00:02", b.guestMAC)
	}
	if got := vmCIDRFor(17); got != "172.17.0.0/16" {
		t.Fatalf("vmCIDRFor(17) = %q", got)
	}
}

func TestComposeBootArgs(t *testing.T) {
	got := composeBootArgs("console=ttyS0 reboot=k", slotNet(3, "fr", 16))
	want := "console=ttyS0 reboot=k ip=172.16.3.2::172.16.3.1:255.255.255.252::eth0:off"
	if got != want {
		t.Fatalf("bootArgs =\n %q\nwant\n %q", got, want)
	}
}

func TestIPAMAcquireRelease(t *testing.T) {
	p := newIPAM(2)
	a, ok := p.acquire()
	if !ok {
		t.Fatal("first acquire failed")
	}
	b, ok := p.acquire()
	if !ok {
		t.Fatal("second acquire failed")
	}
	if a == b {
		t.Fatalf("acquired duplicate slot %d", a)
	}
	if _, ok := p.acquire(); ok {
		t.Fatal("acquire should fail when pool exhausted")
	}
	p.release(a)
	if _, ok := p.acquire(); !ok {
		t.Fatal("acquire should succeed after release")
	}
}

func TestSetupNetworkRequiresExtIface(t *testing.T) {
	f := NewFirecracker(FirecrackerConfig{Egress: EgressConfig{Categories: []string{"open"}}}, testLogger())
	f.run = func(context.Context, string, ...string) error { return nil }
	if err := f.SetupNetwork(context.Background()); err == nil {
		t.Fatal("expected error when ext-iface is empty")
	}
}

func TestSetupNetworkRunsOnce(t *testing.T) {
	var runs int
	f := NewFirecracker(FirecrackerConfig{
		ExtIface: "eth0",
		WorkDir:  t.TempDir(),
		Egress:   EgressConfig{Categories: []string{"open"}},
	}, testLogger())
	f.run = func(context.Context, string, ...string) error {
		runs++
		return nil
	}
	// No ip/filter FORWARD chain on the (fake) host: host-forward accept is a
	// no-op, so only sysctl + nft run.
	f.runOut = func(context.Context, string, ...string) (string, error) {
		return "", errors.New("no such chain")
	}
	if err := f.SetupNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := runs
	// applyNetwork runs sysctl (ip_forward) then nft -f.
	if first != 2 {
		t.Fatalf("ran %d commands, want 2 (sysctl + nft)", first)
	}
	if err := f.SetupNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runs != first {
		t.Fatalf("SetupNetwork ran again: %d commands total", runs)
	}
}

func TestHostForwardScript(t *testing.T) {
	got := hostForwardScript("enp2s0", "fr")
	for _, want := range []string{
		`iifname "enp2s0" oifname "fr*" ct state established,related counter accept comment "firerunner-egress"`,
		`iifname "fr*" oifname "enp2s0" counter accept comment "firerunner-egress"`,
		"insert rule ip filter FORWARD",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("hostForwardScript missing %q in:\n%s", want, got)
		}
	}
}

func TestEnsureHostForward(t *testing.T) {
	const dropChain = "chain FORWARD {\n\ttype filter hook forward priority filter; policy drop;\n}"
	cases := []struct {
		name    string
		out     string
		outErr  error
		wantRun bool
	}{
		{name: "no forward chain", outErr: errors.New("no such chain"), wantRun: false},
		{name: "permissive policy", out: "chain FORWARD { policy accept; }", wantRun: false},
		{name: "drop policy applies rules", out: dropChain, wantRun: true},
		{name: "already present is idempotent", out: dropChain + " firerunner-egress", wantRun: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ranNFT bool
			f := NewFirecracker(FirecrackerConfig{
				ExtIface: "enp2s0",
				WorkDir:  t.TempDir(),
			}, testLogger())
			f.runOut = func(context.Context, string, ...string) (string, error) {
				return tc.out, tc.outErr
			}
			f.run = func(_ context.Context, name string, _ ...string) error {
				if name == "nft" {
					ranNFT = true
				}
				return nil
			}
			if err := f.ensureHostForward(context.Background()); err != nil {
				t.Fatal(err)
			}
			if ranNFT != tc.wantRun {
				t.Fatalf("ran nft = %v, want %v", ranNFT, tc.wantRun)
			}
		})
	}
}

func TestOpenConsole_NoLogDir(t *testing.T) {
	f := NewFirecracker(FirecrackerConfig{}, testLogger())
	w, closeFn, err := f.openConsole("runner-x")
	if err != nil || w == nil {
		t.Fatalf("openConsole: %v", err)
	}
	closeFn()
}

func TestOpenConsole_WritesFile(t *testing.T) {
	dir := t.TempDir()
	f := NewFirecracker(FirecrackerConfig{LogDir: dir}, testLogger())
	w, closeFn, err := f.openConsole("runner-y")
	if err != nil {
		t.Fatalf("openConsole: %v", err)
	}
	if _, err := w.Write([]byte("boot log line\n")); err != nil {
		t.Fatal(err)
	}
	closeFn()

	b, err := os.ReadFile(filepath.Join(dir, "runner-y.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(b), "boot log line") {
		t.Fatalf("log file = %q", b)
	}
}
