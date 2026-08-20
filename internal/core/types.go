// Package core holds the shared domain types for firerunner.
package core

// RunnerSpec describes the shape of the ephemeral microVM that will host a
// single GitHub Actions job. One spec maps to one scale-set tier (e.g. the
// runs-on label "firerunner-4c8g" -> 4 vCPU / 8192 MiB / a Docker-less golden
// image).
type RunnerSpec struct {
	// Labels are the additional runs-on labels advertised for this tier.
	Labels []string
	// VCPU is the number of virtual CPUs given to the microVM.
	VCPU int
	// MemMiB is the guest memory size in MiB.
	MemMiB int
	// RootFS is the path to the immutable golden ext4 image for this tier. It
	// is never mutated; each job gets a reflink (copy-on-write) clone.
	RootFS string
}
