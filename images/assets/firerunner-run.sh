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
# Optional pre-seeded tool cache (progressive enhancement). When firerunner
# attaches the shared read-only tool-cache drive it is labelled
# "hostedtoolcache"; mount it and point the runner at it so setup-* actions hit
# the cache instead of downloading. Absent -> actions download exactly as on
# ubuntu-latest, so the same golden works with or without the drive.
#
# The drive is read-only and shared across microVMs, so it is mounted as an
# overlay LOWER layer under a per-VM tmpfs UPPER. Reads of pre-seeded versions
# hit the drive (cache hit); when a job requests a version the drive lacks,
# setup-* downloads and installs it into the writable upper instead of failing
# with EROFS (cache miss). The upper is tmpfs: ephemeral, discarded with the VM.
if [ -n "$(command -v blkid)" ] && blkid -L hostedtoolcache >/dev/null 2>&1; then
	mkdir -p /opt/hostedtoolcache /opt/.htc-lower
	if mount -o ro -L hostedtoolcache /opt/.htc-lower 2>/dev/null; then
		mkdir -p /run/htc
		mount -t tmpfs tmpfs /run/htc 2>/dev/null
		mkdir -p /run/htc/upper /run/htc/work
		if mount -t overlay overlay \
			-o lowerdir=/opt/.htc-lower,upperdir=/run/htc/upper,workdir=/run/htc/work \
			/opt/hostedtoolcache 2>/dev/null; then
			export RUNNER_TOOL_CACHE=/opt/hostedtoolcache
			export AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
			echo "firerunner: mounted pre-seeded tool cache (ro drive + tmpfs upper) at /opt/hostedtoolcache"
		else
			# Kernel without overlayfs: fall back to a read-only bind so cache
			# hits still work. Cache misses download to the runner's own temp
			# dir per setup-* (they will not write into the read-only cache).
			umount /run/htc 2>/dev/null
			if mount --bind /opt/.htc-lower /opt/hostedtoolcache 2>/dev/null; then
				export RUNNER_TOOL_CACHE=/opt/hostedtoolcache
				export AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
				echo "firerunner: mounted pre-seeded tool cache (read-only, no overlay) at /opt/hostedtoolcache"
			fi
		fi
	fi
elif [ -d /opt/hostedtoolcache ] && [ -n "$(ls -A /opt/hostedtoolcache 2>/dev/null)" ]; then
	# No external drive, but the golden baked a tool cache in (the "full"
	# runner-images-parity image). Point the runner at it so setup-* actions
	# resolve versions locally instead of downloading. Base golden has no such
	# directory, so this is a no-op there and boot behaviour is unchanged.
	export RUNNER_TOOL_CACHE=/opt/hostedtoolcache
	export AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
	echo "firerunner: using baked-in tool cache at /opt/hostedtoolcache"
fi
# Optional self-hosted dependency cache (progressive enhancement). firerunner
# publishes a cache config under /cache in MMDS when --cache-port/--cache-url is
# set. Point actions/cache (and the pnpm/go/setup-* caches built on it) at the
# host's cache-server instead of GitHub's hosted cache. Absent -> the runner
# uses GitHub's cache exactly as normal, so the same golden works either way.
#
# NOTE: this only takes effect on a "cache-redirected" golden, whose runner
# binary has ACTIONS_RESULTS_URL renamed so GitHub's job message cannot override
# the value we export here. On an unpatched golden the export is harmless (the
# runner overwrites it with GitHub's URL and caching stays on GitHub).
cache_url="$(curl -sf "http://$MMDS/cache/url" -H "X-metadata-token: $tok" 2>/dev/null || true)"
cache_port="$(curl -sf "http://$MMDS/cache/port" -H "X-metadata-token: $tok" 2>/dev/null || true)"
if [ -z "$cache_url" ] && [ -n "$cache_port" ]; then
	gw="$(ip route | awk '/^default/{print $3; exit}')"
	if [ -n "$gw" ]; then
		cache_url="http://$gw:$cache_port/"
	fi
fi
if [ -n "$cache_url" ]; then
	export ACTIONS_RESULTS_URL="$cache_url"
	export ACTIONS_CACHE_SERVICE_V2=true
	echo "firerunner: using self-hosted dependency cache at $cache_url"
fi
# Ephemeral JIT runner: registers, runs one job, then auto-deregisters.
./run.sh --jitconfig "$jit" || echo "firerunner: runner exited non-zero" >&2
