package provisioner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlotNet(t *testing.T) {
	n := slotNet(5, "fr")
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
	a, b := slotNet(0, "fr"), slotNet(1, "fr")
	if a.guestIP == b.guestIP || a.tap == b.tap || a.guestMAC == b.guestMAC {
		t.Fatalf("slots overlap: %+v vs %+v", a, b)
	}
}

func TestComposeBootArgs(t *testing.T) {
	got := composeBootArgs("console=ttyS0 reboot=k", slotNet(3, "fr"))
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
