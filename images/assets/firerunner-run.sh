#!/bin/sh
#
# firerunner-run.sh — golden-image boot service body. Fetches the JIT config that
# firerunner published in Firecracker MMDS (v2), runs exactly one ephemeral
# GitHub Actions job, then self-destructs so the host reaps the microVM.
#
# The microVM is single-use and thrown away after the job, so the runner is
# allowed to run as root (RUNNER_ALLOW_RUNASROOT) — this keeps the boot path
# simple and lets the service issue the final reboot.
set -eu

MMDS=169.254.169.254
RUNNER_DIR=/opt/runner

selfdestruct() {
	# reboot -f + `reboot=k` on the kernel cmdline -> i8042 reset -> the
	# Firecracker VMM process exits -> firerunner reaps the job.
	sync
	reboot -f
}
trap selfdestruct EXIT

# MMDS is link-local; make sure it is routed via the guest NIC.
ip route add "$MMDS" dev eth0 2>/dev/null || true

# MMDS v2: obtain a short-lived session token.
tok="$(curl -sf -X PUT "http://$MMDS/latest/api/token" \
	-H "X-metadata-token-ttl-seconds: 60")" || {
	echo "firerunner: could not obtain MMDS token" >&2
	exit 0
}

# Fetch the base64 JIT config firerunner placed at /jitconfig.
jit="$(curl -sf "http://$MMDS/jitconfig" -H "X-metadata-token: $tok")" || {
	echo "firerunner: could not fetch jitconfig from MMDS" >&2
	exit 0
}

if [ -z "$jit" ]; then
	echo "firerunner: empty jitconfig; nothing to run" >&2
	exit 0
fi

cd "$RUNNER_DIR"
export RUNNER_ALLOW_RUNASROOT=1
# systemd oneshot services start with a bare environment; HOME/USER are not
# guaranteed. Tools invoked by the job (e.g. `go env GOPATH`, which derives a
# default GOPATH of $HOME/go) fail when HOME is unset. We run as root in the
# throwaway microVM, so anchor them explicitly.
export HOME=/root
export USER=root
# Ephemeral JIT runner: registers, runs one job, then auto-deregisters.
./run.sh --jitconfig "$jit" || echo "firerunner: runner exited non-zero" >&2
