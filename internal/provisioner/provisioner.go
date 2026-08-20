// Package provisioner turns a JIT runner config into a running, ephemeral
// microVM and reaps it when the job is done.
package provisioner

import (
	"context"

	"github.com/linyiru/firerunner/internal/core"
)

// Provisioner boots exactly one ephemeral microVM per call and blocks until it
// exits. The GitHub runner inside the guest is JIT/ephemeral: it picks up a
// single job, then the guest issues `reboot -f`, which makes Firecracker exit
// (Firecracker's VMM terminates on guest reboot, not on halt/poweroff).
//
// Implementations MUST clean up all per-job resources (rootfs clone, tap
// device, API socket) before returning, whether the VM exited cleanly or the
// context was cancelled.
type Provisioner interface {
	// Launch boots a microVM for the given runner name and JIT config and
	// blocks until the VM exits or ctx is cancelled.
	Launch(ctx context.Context, name, jitConfig string, spec core.RunnerSpec) error
	// Name returns the provisioner implementation name (for logging).
	Name() string
}
