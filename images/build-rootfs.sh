#!/usr/bin/env bash
#
# build-rootfs.sh — build a firerunner golden rootfs (ext4) with the official
# actions/runner agent and an MMDS-JIT boot service pre-installed.
#
# This is a SCAFFOLD: it lays out the required steps and fails fast on missing
# host tooling. It must run on a Linux host as root; it cannot run on macOS.
#
# Usage:
#   sudo ./build-rootfs.sh --tier firerunner-4c8g \
#     --runner-version 2.320.0 --out /var/lib/firerunner/golden-4c8g.ext4
#
set -euo pipefail

TIER="firerunner-4c8g"
RUNNER_VERSION=""            # empty => resolve latest from GitHub releases
OUT=""
SIZE_MB=8192
DNS_SERVERS="1.1.1.1 8.8.8.8"

die() { echo "error: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tier)           TIER="$2"; shift 2 ;;
    --runner-version) RUNNER_VERSION="$2"; shift 2 ;;
    --out)            OUT="$2"; shift 2 ;;
    --size-mb)        SIZE_MB="$2"; shift 2 ;;
    --dns-servers)    DNS_SERVERS="${2//,/ }"; shift 2 ;;
    *) die "unknown flag: $1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux (KVM host)"
[[ $EUID -eq 0 ]] || die "must run as root"
[[ -n "$OUT" ]] || die "--out is required"

case "$TIER" in
  firerunner-4c8g|firerunner-8c16g-docker) ;;
  *) die "unknown tier: $TIER (want firerunner-4c8g or firerunner-8c16g-docker)" ;;
esac

for t in mkfs.ext4 curl tar; do
  command -v "$t" >/dev/null || die "missing required tool: $t"
done

# Resolve the latest runner release when not pinned. Kept within GitHub's
# 30-day support window by the scheduled rebuild workflow.
if [[ -z "$RUNNER_VERSION" ]]; then
  RUNNER_VERSION="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest \
    | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n1)"
  [[ -n "$RUNNER_VERSION" ]] || die "could not resolve latest actions/runner version"
fi

echo ">> tier=$TIER runner=$RUNNER_VERSION out=$OUT size=${SIZE_MB}MB"

WORK="$(mktemp -d)"
MNT="$WORK/mnt"
mkdir -p "$MNT"
cleanup() { mountpoint -q "$MNT" && umount "$MNT"; rm -rf "$WORK"; }
trap cleanup EXIT

# 1. Create + format the ext4 image.
truncate -s "${SIZE_MB}M" "$OUT"
mkfs.ext4 -q -F "$OUT"
mount -o loop "$OUT" "$MNT"

# 2. Bootstrap a minimal base filesystem into $MNT.
#    TODO: choose a bootstrapper for the host distro, e.g.
#      Debian/Ubuntu: debootstrap --variant=minbase stable "$MNT"
#      Arch:          pacstrap -c "$MNT" base
#    then install: git, ca-certificates, and (for the docker tier) docker.
echo ">> TODO: bootstrap base rootfs into $MNT"

# 3. Install the official actions/runner agent.
#    TODO:
#      arch=x64
#      url=https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${arch}-${RUNNER_VERSION}.tar.gz
#      install -d "$MNT/opt/runner"
#      curl -fsSL "$url" | tar -xz -C "$MNT/opt/runner"
#      "$MNT/opt/runner/bin/installdependencies.sh"  # via chroot
echo ">> TODO: install actions/runner v$RUNNER_VERSION into /opt/runner"

# 4. Install the MMDS-JIT boot service (reads jitconfig from MMDS, runs one job,
#    then reboot -f to self-destruct). See images/README.md for the contract.
#    TODO: install a systemd unit or init script calling fetch-jit-and-run.
echo ">> TODO: install MMDS-JIT boot service"

# 5. Bake a static resolv.conf matching the egress allowlist's --dns-servers.
{
  for ns in $DNS_SERVERS; do echo "nameserver $ns"; done
} > "$MNT/etc/resolv.conf"

# 6. (docker tier) enable the Docker daemon at boot.
if [[ "$TIER" == "firerunner-8c16g-docker" ]]; then
  echo ">> TODO: enable dockerd in the golden image"
fi

sync
echo ">> golden image written: $OUT"
echo ">> NOTE: complete the TODO steps above before using in production."
