//go:build e2e

// Package e2e exercises a real Firecracker microVM boot. It is compiled only
// with the `e2e` build tag and skips unless run on a KVM-capable host with the
// required kernel and golden rootfs provided via FR_KERNEL and FR_GOLDEN.
//
// Run with: make e2e   (equivalently: go test -tags e2e ./test/e2e/...)
package e2e

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/solcreek/firerunner/internal/core"
	"github.com/solcreek/firerunner/internal/provisioner"
)

func requireKVM(t *testing.T) (kernel, golden string) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("skipping: /dev/kvm not available")
	}
	kernel = os.Getenv("FR_KERNEL")
	golden = os.Getenv("FR_GOLDEN")
	if kernel == "" || golden == "" {
		t.Skip("skipping: set FR_KERNEL and FR_GOLDEN to a guest kernel and golden rootfs")
	}
	return kernel, golden
}

// TestLaunchBootsAndSelfDestructs boots a real microVM and expects Launch to
// return once the guest self-destructs (reboot -f). Without a runner agent
// baked into the golden image the guest is expected to reboot on its own; the
// context timeout bounds the test.
func TestLaunchBootsAndSelfDestructs(t *testing.T) {
	kernel, golden := requireKVM(t)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	extIface := os.Getenv("FR_EXT_IFACE")
	if extIface == "" {
		t.Skip("skipping: set FR_EXT_IFACE to the host egress interface")
	}
	f := provisioner.NewFirecracker(provisioner.FirecrackerConfig{
		KernelImage:  kernel,
		GoldenRootFS: golden,
		WorkDir:      t.TempDir(),
		ExtIface:     extIface,
		LogDir:       t.TempDir(),
	}, log)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	spec := core.RunnerSpec{VCPU: 2, MemMiB: 1024, RootFS: golden}
	if err := f.Launch(ctx, "e2e-smoke", "", spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}
}
