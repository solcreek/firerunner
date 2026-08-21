#!/usr/bin/env bash
#
# build-toolcache.sh — build a standalone, composable hosted tool-cache drive
# (ext4) for firerunner's `--toolcache` / `FR_TOOLCACHE` feature.
#
# firerunner can attach a read-only ext4 labelled `hostedtoolcache` to every
# microVM; the guest mounts it at /opt/hostedtoolcache and exports
# RUNNER_TOOL_CACHE, so `actions/setup-*` find their toolchain already on disk
# instead of downloading it per job (see the "Pre-seeded tool cache" section of
# the top-level README). This script is the operator-side builder for that
# drive: pick exactly the tools + versions your team uses and get one small,
# immutable image, decoupled from the base golden rootfs — so you can re-cut the
# cache (new versions, more tools) without rebuilding an OS image, and share the
# same drive across every tier.
#
# The layout mirrors GitHub's hosted tool cache so the stock `setup-*` actions
# hit it unmodified:
#     <tool>/<version>/x64/            # the tool tree
#     <tool>/<version>/x64.complete    # the marker @actions/tool-cache looks for
#
# The drive is a PURE ACCELERATOR: if a requested version is absent the setup-*
# action just downloads it and the job still passes. Nothing here is a
# correctness dependency.
#
# Runs on a Linux host. Node and Go need only curl/tar/mkfs.ext4 (no Docker).
# Python is fetched from actions/python-versions and relocated inside an
# ubuntu:24.04 container (matching the guest) so its shebangs/rpaths are correct,
# so `--python` additionally requires Docker.
#
# Usage:
#   sudo ./build-toolcache.sh --out /var/tmp/fr-golden/toolcache.ext4 \
#     --node 22.22.2 --go 1.26.7                 # no Docker needed
#   sudo ./build-toolcache.sh --out toolcache.ext4 \
#     --node 20.18.0,22.22.2 --python 3.12       # Python => needs Docker
#
set -euo pipefail

OUT=""
ARCH="x64"
LABEL="hostedtoolcache"
SIZE_MB=""            # empty => sized from staged du + margin
MARGIN_PCT=25
UBUNTU_TAG="24.04"    # for the Python relocation container (match the guest)
declare -a NODE_VERSIONS=() GO_VERSIONS=() PYTHON_VERSIONS=()

die() { echo "error: $*" >&2; exit 1; }
log() { echo ">> $*"; }

split_csv() { local IFS=','; read -ra _out <<<"$1"; printf '%s\n' "${_out[@]}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)       OUT="$2"; shift 2 ;;
    --node)      mapfile -t -O "${#NODE_VERSIONS[@]}"   NODE_VERSIONS   < <(split_csv "$2"); shift 2 ;;
    --go)        mapfile -t -O "${#GO_VERSIONS[@]}"     GO_VERSIONS     < <(split_csv "$2"); shift 2 ;;
    --python)    mapfile -t -O "${#PYTHON_VERSIONS[@]}" PYTHON_VERSIONS < <(split_csv "$2"); shift 2 ;;
    --arch)      ARCH="$2"; shift 2 ;;
    --label)     LABEL="$2"; shift 2 ;;
    --size-mb)   SIZE_MB="$2"; shift 2 ;;
    --ubuntu-tag) UBUNTU_TAG="$2"; shift 2 ;;
    -h|--help)   sed -n '2,40p' "$0"; exit 0 ;;
    *) die "unknown flag: $1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || die "must run on Linux"
[[ $EUID -eq 0 ]] || die "must run as root (mkfs/ownership)"
[[ -n "$OUT" ]] || die "--out is required"
[[ "$ARCH" == "x64" ]] || die "only --arch x64 is supported"
for t in curl tar mkfs.ext4 e2label; do command -v "$t" >/dev/null || die "missing tool: $t"; done
if (( ${#NODE_VERSIONS[@]} + ${#GO_VERSIONS[@]} + ${#PYTHON_VERSIONS[@]} == 0 )); then
  die "nothing to build: pass at least one of --node / --go / --python"
fi
if (( ${#PYTHON_VERSIONS[@]} > 0 )); then
  command -v docker >/dev/null || die "--python requires docker (faithful actions/python-versions relocation)"
  docker info >/dev/null 2>&1 || die "--python requires a running docker daemon"
fi

STAGE="$(mktemp -d)"
cleanup() { rm -rf "$STAGE"; }
trap cleanup EXIT

# ---- Node: official nodejs.org static tarball ------------------------------
for v in "${NODE_VERSIONS[@]}"; do
  dest="$STAGE/node/$v/$ARCH"
  log "node $v"
  mkdir -p "$dest"
  curl -fSL --retry 3 "https://nodejs.org/dist/v${v}/node-v${v}-linux-x64.tar.gz" \
    | tar -xz --strip-components=1 -C "$dest" || die "node $v download/extract failed"
  [[ -x "$dest/bin/node" ]] || die "node $v: bin/node missing after extract"
  : > "$STAGE/node/$v/$ARCH.complete"
done

# ---- Go: official go.dev tarball -------------------------------------------
for v in "${GO_VERSIONS[@]}"; do
  dest="$STAGE/go/$v/$ARCH"
  log "go $v"
  mkdir -p "$dest"
  # The archive's top-level dir is `go/`; strip it so bin/go lands at $dest/bin.
  curl -fSL --retry 3 "https://go.dev/dl/go${v}.linux-amd64.tar.gz" \
    | tar -xz --strip-components=1 -C "$dest" || die "go $v download/extract failed"
  [[ -x "$dest/bin/go" ]] || die "go $v: bin/go missing after extract"
  : > "$STAGE/go/$v/$ARCH.complete"
done

# ---- Python: actions/python-versions, relocated in an ubuntu container ------
# The python-versions release tarballs are prebuilt for a specific Ubuntu and
# ship a setup.sh that relocates the interpreter into $AGENT_TOOLSDIRECTORY with
# correct shebangs + a .complete marker. Run it inside ubuntu:${UBUNTU_TAG} so
# the layout matches the microVM guest exactly.
if (( ${#PYTHON_VERSIONS[@]} > 0 )); then
  log "python ${PYTHON_VERSIONS[*]} (via ubuntu:${UBUNTU_TAG} container)"
  mkdir -p "$STAGE"
  docker run --rm -e AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache \
    -e WANT_VERSIONS="${PYTHON_VERSIONS[*]}" -e UBUNTU_TAG="$UBUNTU_TAG" \
    -v "$STAGE:/opt/hostedtoolcache" ubuntu:"${UBUNTU_TAG}" bash -euo pipefail -c '
      export DEBIAN_FRONTEND=noninteractive
      apt-get update >/dev/null
      apt-get install -y --no-install-recommends curl jq ca-certificates >/dev/null
      manifest="$(curl -fsSL https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json)"
      for want in $WANT_VERSIONS; do
        # Newest release whose version starts with the requested prefix and that
        # ships a linux / ${UBUNTU_TAG} / x64 asset.
        url="$(echo "$manifest" | jq -r --arg v "$want" --arg pv "$UBUNTU_TAG" "
          [ .[]
            | select(.version | startswith(\$v))
            | . as \$rel
            | \$rel.files[]
            | select(.platform==\"linux\" and .arch==\"x64\" and .platform_version==\$pv)
            | {version: \$rel.version, url: .download_url} ]
          | sort_by(.version) | last | .url // empty")"
        [[ -n "$url" ]] || { echo "error: no python-versions asset for $want on ubuntu $UBUNTU_TAG x64" >&2; exit 1; }
        echo ">> python asset: $url"
        tmp="$(mktemp -d)"
        curl -fSL --retry 3 "$url" | tar -xz -C "$tmp"
        ( cd "$tmp" && bash ./setup.sh )
        rm -rf "$tmp"
      done
    ' || die "python relocation failed"
fi

# Read-only in the guest; make sure everything is traversable/readable.
chmod -R a+rX "$STAGE"

# ---- Pack the staged tree into a labelled ext4 -----------------------------
if [[ -z "$SIZE_MB" ]]; then
  used_mb="$(du -sm "$STAGE" | awk '{print $1}')"
  SIZE_MB=$(( used_mb + used_mb * MARGIN_PCT / 100 + 64 ))
fi
log "sizing drive at ${SIZE_MB}MB"

mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"
mkfs.ext4 -q -F -d "$STAGE" "$OUT" "${SIZE_MB}M"
# firerunner mounts by label, so this MUST be `hostedtoolcache` (16-byte cap).
e2label "$OUT" "$LABEL"
sync

log "tool-cache drive written: $OUT ($(du -h "$OUT" | awk '{print $1}'), label=$LABEL)"
log "contents:"
for t in node go Python; do
  [[ -d "$STAGE/$t" ]] && for d in "$STAGE/$t"/*/; do echo "   - $t $(basename "$d")"; done
done
log "next: attach with  firerunner --toolcache $OUT  (or FR_TOOLCACHE=$OUT)"
