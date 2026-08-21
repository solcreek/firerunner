#!/usr/bin/env bash
#
# build-ubuntu-rootfs.sh — build a kitchen-sink, ubuntu-latest-parity firerunner
# rootfs (ext4) via Docker (OCI -> ext4), the on-prem equivalent of the
# Ubicloud approach (actions/runner-images provisioning, minus the Azure Packer
# flow). Produces a BOOTABLE microVM image: systemd init + actions/runner +
# firerunner MMDS-JIT boot service.
#
# Runs on a Linux KVM host with Docker (tested on the Arch/Omarchy 'starship').
#
# Usage:
#   sudo ./build-ubuntu-rootfs.sh --out /var/tmp/fr-golden/ubuntu-rootfs.ext4 \
#     --toolset base            # base = curated kitchen-sink (Stage 1)
#                               # full = + actions/runner-images toolset (Stage 2)
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ASSETS="$HERE/assets"

OUT=""
TOOLSET="base"
RUNNER_VERSION=""
NODE_VERSION="22.11.0"
UBUNTU_TAG="24.04"
DNS_SERVERS="1.1.1.1 8.8.8.8"
SIZE_MB=""              # empty => sized from rootfs du + margin
MARGIN_PCT=35
IMAGE_TAG="firerunner-ubuntu-rootfs:latest"

die() { echo "error: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)            OUT="$2"; shift 2 ;;
    --toolset)        TOOLSET="$2"; shift 2 ;;
    --runner-version) RUNNER_VERSION="$2"; shift 2 ;;
    --node-version)   NODE_VERSION="$2"; shift 2 ;;
    --ubuntu-tag)     UBUNTU_TAG="$2"; shift 2 ;;
    --dns-servers)    DNS_SERVERS="${2//,/ }"; shift 2 ;;
    --size-mb)        SIZE_MB="$2"; shift 2 ;;
    *) die "unknown flag: $1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux (KVM host)"
[[ $EUID -eq 0 ]] || die "must run as root"
[[ -n "$OUT" ]] || die "--out is required"
case "$TOOLSET" in base|full) ;; *) die "--toolset must be base or full" ;; esac
for t in docker mkfs.ext4 e2label; do command -v "$t" >/dev/null || die "missing tool: $t"; done
docker info >/dev/null 2>&1 || die "docker daemon not available"

if [[ -z "$RUNNER_VERSION" ]]; then
  RUNNER_VERSION="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest \
    | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n1)"
  [[ -n "$RUNNER_VERSION" ]] || die "could not resolve latest actions/runner version"
fi

echo ">> toolset=$TOOLSET ubuntu=$UBUNTU_TAG runner=$RUNNER_VERSION out=$OUT"

# Stage 2 (actions/runner-images toolset scripts) is not wired up yet; be honest
# about it rather than silently shipping the base set under a "full" label.
if [[ "$TOOLSET" == "full" ]]; then
  echo ">> WARNING: --toolset full is not implemented yet; building the curated"
  echo ">>          base set (Stage 1). Track Stage 2 before relying on 'full'."
fi

WORK="$(mktemp -d)"
CTX="$WORK/ctx"
ROOT="$WORK/root"
mkdir -p "$CTX" "$ROOT"
cleanup() { rm -rf "$WORK"; docker rm -f fr-rootfs-export >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Boot assets go into the build context.
cp "$ASSETS/firerunner-run.sh" "$CTX/firerunner-run.sh"
cp "$ASSETS/firerunner-runner.service" "$CTX/firerunner-runner.service"

# resolv.conf matching the egress allowlist's resolvers.
{ for ns in $DNS_SERVERS; do echo "nameserver $ns"; done; } > "$CTX/resolv.conf"

# ---- Dockerfile ------------------------------------------------------------
cat > "$CTX/Dockerfile" <<DOCKERFILE
FROM ubuntu:${UBUNTU_TAG}
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8

# init + base runtime the microVM and the runner need.
RUN apt-get update && apt-get install -y --no-install-recommends \\
      systemd systemd-sysv ca-certificates curl wget git tar xz-utils gzip unzip zip \\
      jq sudo iproute2 iputils-ping openssh-client rsync gnupg lsb-release \\
      software-properties-common locales tzdata \\
    && rm -rf /var/lib/apt/lists/*

# curated kitchen-sink toolset (Stage 1: ubuntu-latest-ish, reliable + bootable).
RUN apt-get update && apt-get install -y --no-install-recommends \\
      build-essential pkg-config make cmake \\
      python3 python3-pip python3-venv python-is-python3 \\
      default-jdk ruby-full golang-go \\
      docker.io \\
      libssl-dev libffi-dev zlib1g-dev libbz2-dev libreadline-dev libsqlite3-dev \\
    && rm -rf /var/lib/apt/lists/*

# Node.js LTS from the official static tarball (no nodesource) into /usr/local.
RUN curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz" \\
      | tar -xz -C /usr/local --strip-components=1 && node --version

# GitHub Actions runner agent.
RUN install -d /opt/runner \\
    && curl -fsSL "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" \\
      | tar -xz -C /opt/runner \\
    && /opt/runner/bin/installdependencies.sh \\
    && rm -rf /var/lib/apt/lists/*

# firerunner MMDS-JIT boot service (fetch jitconfig -> run one job -> reboot -f).
COPY firerunner-run.sh /usr/local/bin/firerunner-run.sh
COPY firerunner-runner.service /etc/systemd/system/firerunner-runner.service
COPY resolv.conf /etc/resolv.conf
RUN chmod 0755 /usr/local/bin/firerunner-run.sh \\
    && systemctl enable firerunner-runner.service docker.service \\
    && systemctl mask serial-getty@ttyS0.service \\
    && ln -sf /lib/systemd/systemd /sbin/init
DOCKERFILE

echo ">> docker build ($TOOLSET)"
docker build -t "$IMAGE_TAG" "$CTX"

# ---- OCI -> rootfs dir -> ext4 --------------------------------------------
echo ">> exporting container filesystem"
docker create --name fr-rootfs-export "$IMAGE_TAG" true >/dev/null
docker export fr-rootfs-export | tar -C "$ROOT" -xf -
docker rm -f fr-rootfs-export >/dev/null

# systemd inside a VM needs these dirs; docker export omits some.
mkdir -p "$ROOT/proc" "$ROOT/sys" "$ROOT/dev" "$ROOT/run" "$ROOT/tmp"
chmod 1777 "$ROOT/tmp"

if [[ -z "$SIZE_MB" ]]; then
  used_mb="$(du -sm "$ROOT" | awk '{print $1}')"
  SIZE_MB=$(( used_mb + used_mb * MARGIN_PCT / 100 + 512 ))
fi
echo ">> rootfs sized ${SIZE_MB}MB"

mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"
mkfs.ext4 -q -F -d "$ROOT" "$OUT" "${SIZE_MB}M"
# ext4 volume labels are capped at 16 bytes, so keep this short (avoid silent
# truncation). It is cosmetic — firerunner boots the rootfs via is_root_device /
# root=/dev/vda, not by label — so a stable role name is enough for every tier.
e2label "$OUT" firerunner-root || true

sync
echo ">> rootfs image written: $OUT ($(du -h "$OUT" | awk '{print $1}'))"
echo ">> next: boot-test in firecracker, then deploy as a tier image"
