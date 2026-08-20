package provisioner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/solcreek/firerunner/internal/core"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// shortSock returns a short-path unix socket (the sun_path limit is ~104 chars,
// and t.TempDir() paths are often too long on macOS).
func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func TestBuildAPISteps_OrderAndPayloads(t *testing.T) {
	spec := core.RunnerSpec{VCPU: 4, MemMiB: 8192}
	steps := buildAPISteps("/vmlinux", "console=ttyS0 reboot=k ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off",
		"/job/rootfs.ext4", "fr0", "06:00:AC:10:00:02", "JIT-SECRET", spec)

	wantOrder := []string{
		"/boot-source", "/drives/rootfs", "/machine-config",
		"/network-interfaces/eth0", "/mmds/config", "/mmds", "/actions",
	}
	if len(steps) != len(wantOrder) {
		t.Fatalf("got %d steps, want %d", len(steps), len(wantOrder))
	}
	idx := map[string]int{}
	for i, s := range steps {
		if s.path != wantOrder[i] {
			t.Fatalf("step %d = %q, want %q", i, s.path, wantOrder[i])
		}
		idx[s.path] = i
	}

	// Critical invariants.
	if idx["/mmds"] >= idx["/actions"] {
		t.Fatal("/mmds (JIT secret) must be set before InstanceStart")
	}
	if steps[len(steps)-1].path != "/actions" {
		t.Fatal("InstanceStart must be the last step")
	}

	mmds := steps[idx["/mmds"]].body.(map[string]any)
	if mmds["jitconfig"] != "JIT-SECRET" {
		t.Fatalf("mmds jitconfig = %v, want JIT-SECRET", mmds["jitconfig"])
	}
	mc := steps[idx["/machine-config"]].body.(map[string]any)
	if mc["vcpu_count"] != 4 || mc["mem_size_mib"] != 8192 {
		t.Fatalf("machine-config = %v, want vcpu 4 mem 8192", mc)
	}
	boot := steps[idx["/boot-source"]].body.(map[string]any)
	if boot["kernel_image_path"] != "/vmlinux" {
		t.Fatalf("boot kernel = %v", boot["kernel_image_path"])
	}
}

func TestConfigure_SendsSequenceOverUnixSocket(t *testing.T) {
	sock := shortSock(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var paths []string
	bodies := map[string]map[string]any{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies[r.URL.Path] = m
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	f := NewFirecracker(FirecrackerConfig{KernelImage: "/k", GoldenRootFS: "/g"}, testLogger())
	err = f.configure(context.Background(), sock, "/rootfs", slotNet(0, "fr"), "console=ttyS0", "JIT", core.RunnerSpec{VCPU: 2, MemMiB: 512})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}

	want := []string{
		"/boot-source", "/drives/rootfs", "/machine-config",
		"/network-interfaces/eth0", "/mmds/config", "/mmds", "/actions",
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != len(want) {
		t.Fatalf("got %d requests %v, want %d", len(paths), paths, len(want))
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("request %d = %q, want %q", i, paths[i], want[i])
		}
	}
	if bodies["/mmds"]["jitconfig"] != "JIT" {
		t.Fatalf("mmds body = %v", bodies["/mmds"])
	}
}

func TestConfigure_PropagatesAPIError(t *testing.T) {
	sock := shortSock(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	f := NewFirecracker(FirecrackerConfig{KernelImage: "/k"}, testLogger())
	if err := f.configure(context.Background(), sock, "/rootfs", slotNet(0, "fr"), "console=ttyS0", "JIT", core.RunnerSpec{VCPU: 1, MemMiB: 128}); err == nil {
		t.Fatal("expected error from 400 response")
	}
}

func TestWaitForSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = os.WriteFile(sock, nil, 0o600)
	}()
	if err := waitForSocket(context.Background(), sock, time.Second); err != nil {
		t.Fatalf("waitForSocket: %v", err)
	}
}

func TestWaitForSocket_Timeout(t *testing.T) {
	if err := waitForSocket(context.Background(), "/nonexistent/x.sock", 80*time.Millisecond); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitForSocket_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForSocket(ctx, "/nonexistent/x.sock", time.Second); err == nil {
		t.Fatal("expected context error")
	}
}

func TestSetupTap_RunsExpectedCommands(t *testing.T) {
	var calls [][]string
	f := NewFirecracker(FirecrackerConfig{}, testLogger())
	f.run = func(ctx context.Context, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	if err := f.setupTap(context.Background(), slotNet(0, "fr")); err != nil {
		t.Fatalf("setupTap: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("got %d commands, want 3: %v", len(calls), calls)
	}
	if calls[0][0] != "ip" || calls[0][1] != "tuntap" || calls[0][2] != "add" {
		t.Fatalf("first command = %v, want ip tuntap add", calls[0])
	}
	if calls[2][len(calls[2])-1] != "up" {
		t.Fatalf("last command = %v, want link set up", calls[2])
	}
}

func TestName(t *testing.T) {
	if got := NewFirecracker(FirecrackerConfig{}, testLogger()).Name(); got != "firecracker" {
		t.Fatalf("Name = %q", got)
	}
}

func TestTeardownTap(t *testing.T) {
	var got []string
	f := NewFirecracker(FirecrackerConfig{}, testLogger())
	f.run = func(ctx context.Context, name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}
	if err := f.teardownTap(context.Background(), "frtap0"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != "ip" || got[1] != "link" || got[2] != "del" || got[3] != "frtap0" {
		t.Fatalf("teardown ran %v, want ip link del frtap0", got)
	}
}

func TestLogWriter(t *testing.T) {
	w := &logWriter{log: testLogger(), runner: "r", stream: "stdout"}
	n, err := w.Write([]byte("hello\n"))
	if err != nil || n != 6 {
		t.Fatalf("Write = %d, %v", n, err)
	}
}

func TestCleanupStaleJobDirs(t *testing.T) {
	work := t.TempDir()
	// Two stale job dirs (each identified by an fc.sock) plus artefacts that must
	// survive: the nft rulesets and the logs dir.
	for _, name := range []string{"firerunner-aaaa1111", "firerunner-bbbb2222"} {
		dir := filepath.Join(work, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "fc.sock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(work, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"firerunner.nft", "firerunner-forward.nft"} {
		if err := os.WriteFile(filepath.Join(work, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// MaxVMs=1 with an unusual prefix keeps the tap sweep from touching any real
	// host interface.
	f := NewFirecracker(FirecrackerConfig{WorkDir: work, TapPrefix: "frunittest", MaxVMs: 1}, testLogger())
	f.CleanupStale(context.Background())

	for _, name := range []string{"firerunner-aaaa1111", "firerunner-bbbb2222"} {
		if _, err := os.Stat(filepath.Join(work, name)); !os.IsNotExist(err) {
			t.Errorf("job dir %s not removed (err=%v)", name, err)
		}
	}
	for _, keep := range []string{"logs", "firerunner.nft", "firerunner-forward.nft"} {
		if _, err := os.Stat(filepath.Join(work, keep)); err != nil {
			t.Errorf("%s should be preserved: %v", keep, err)
		}
	}
}
