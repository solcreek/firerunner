# firerunner golden images

firerunner boots each job in a fresh microVM cloned (reflink) from an immutable
**golden rootfs**. The image bakes in the official `actions/runner` agent plus a
tiny boot service that reads its JIT config from MMDS and starts the runner for
exactly one job. This directory holds the (host-side) build tooling and the
rebuild pipeline.

> These scripts run on a Linux KVM host — they cannot build on macOS. Treat this
> as the reference/scaffold; run it on `starship` or the self-hosted `kvm`
> runner.

## Why a golden image (not Docker)

firerunner deliberately avoids OCI/containerd. The rootfs is a plain ext4 file:
no image registry, no layer graph, no container runtime on the critical path.
Cloning is a filesystem reflink (btrfs/XFS), so a new job's disk appears in
milliseconds and is thrown away after the run.

## Tiers

Different workloads need different images. Each tier is a separate golden rootfs
selected by the runner label a workflow requests (`runs-on: <tier>`). In the
scale-set model GitHub routes a job to the scale set whose name matches that
label; one firerunner process serves every tier (each registered as its own
scale set) from a single [tier catalog](../examples/tiers.json).

| Tier                      | vCPU / RAM (suggested) | Contents                                              | Use for                                                     |
| ------------------------- | ---------------------- | ---------------------------------------------------- | ----------------------------------------------------------- |
| `firerunner-4c8g`         | 4 / 8 GiB              | actions/runner, git, curl, jq, build-essential base   | generic jobs; toolchains fetched by `setup-*` per workflow  |
| `firerunner-node`         | 2 / 4 GiB              | everything above **+ Node.js LTS + npm** (baked)      | JS/TS builds where `setup-node` should hit a local toolchain |
| `firerunner-8c16g-docker` | 8 / 16 GiB            | everything above **+ Docker Engine (dind-capable)**  | jobs using `container:`, service containers, `docker build` |
| `firerunner-ubuntu`       | 4 / 8 GiB              | Ubuntu 24.04 (glibc 2.39) **+ hosted tool cache + runner-images installers** (kitchen-sink) | closest parity with GitHub-hosted `ubuntu-latest` |

The first three tiers are lean Debian goldens built by
[`build-rootfs.sh`](build-rootfs.sh); the `firerunner-ubuntu` tier is the
faithful `ubuntu-latest` full image built by
[`build-ubuntu-rootfs.sh --toolset full`](build-ubuntu-rootfs.sh).

The base tier deliberately ships no language toolchain — a workflow's `setup-go`
/ `setup-node` fetches one per run. Baking a toolchain (the `firerunner-node`
tier) removes that download, mirroring GitHub's hosted **tool cache**
(`/opt/hostedtoolcache`). The Docker tier exists because some pipelines rely on
`docker build`, job `container:` and service containers, which need a real
Docker daemon inside the guest.

### Running several tiers on one host

Serve every tier from **one** firerunner process: list them in a JSON tier
catalog and point `--tiers` (or `FR_TIERS`) at it (see
[`examples/tiers.json`](../examples/tiers.json) and the "Runner tiers" section
of the top-level [README](../README.md)). All tiers then share one host
network, tool cache and the `--max-runners` slot budget — `vcpu`/`mem_mib`/
`golden` vary per tier — and developers pick one with `runs-on: <name>`. No
per-instance network identity or extra systemd units are needed.

## What the boot service does (MMDS → JIT)

At boot the guest fetches its runner JIT config from Firecracker MMDS v2. This
is implemented by [`assets/firerunner-run.sh`](assets/firerunner-run.sh), started
by [`assets/firerunner-runner.service`](assets/firerunner-runner.service):

1. Grab an MMDS token: `PUT http://169.254.169.254/latest/api/token`
   (`X-metadata-token-ttl-seconds`).
2. `GET http://169.254.169.254/jitconfig` with `X-metadata-token` → the base64
   JIT config firerunner published (from `{"jitconfig": "<base64>"}`).
3. Run `./run.sh --jitconfig <base64>` (ephemeral; auto-deregisters after one
   job). The runner runs as root (`RUNNER_ALLOW_RUNASROOT`) — the whole microVM
   is disposable, and it lets the boot service issue the final reboot.
4. On exit the boot service issues `reboot -f`; with `reboot=k` on the kernel
   cmdline this makes the VMM exit and the host reaps the microVM.

A static `/etc/resolv.conf` (matching `--dns-servers`) is baked in because the
egress allowlist only permits the configured resolvers.

## Building

```bash
# On a Linux KVM host, as root, with debootstrap installed:
sudo ./build-rootfs.sh \
  --tier firerunner-4c8g \
  --runner-version 2.320.0 \
  --out /var/lib/firerunner/golden-4c8g.ext4

# Node tier (bakes Node.js LTS); --node-version overrides the default:
sudo ./build-rootfs.sh \
  --tier firerunner-node \
  --node-version 22.11.0 \
  --out /var/lib/firerunner/golden-node.ext4
```

`build-rootfs.sh` creates/formats the ext4 image, debootstraps a minimal Debian
base (systemd + git/curl/iproute2), installs `actions/runner` and runs its
`installdependencies.sh`, drops in the boot service + a static resolv.conf,
enables the service, and (per tier) installs Docker or bakes Node.js. The result
is an immutable file firerunner reflink-clones per job. Requires `debootstrap`,
`mkfs.ext4`, `curl` and `tar` on the host.

## Rebuild policy

GitHub only supports self-hosted runner agents released within the **last 30
days**. `.github/workflows/build-image.yml` rebuilds the images on a schedule
(monthly, well within the window) and on manual dispatch, pinning the latest
`actions/runner` release. Rebuilding also picks up base-OS security updates so
every microVM starts from a patched image.
