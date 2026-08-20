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
selected by the runner labels a workflow requests.

| Tier                      | vCPU / RAM (suggested) | Contents                                              | Use for                                                     |
| ------------------------- | ---------------------- | ---------------------------------------------------- | ----------------------------------------------------------- |
| `firerunner-4c8g`         | 4 / 8 GiB              | actions/runner, git, Node LTS + pnpm, build-essential | typical JS/TS builds, tests, lint                           |
| `firerunner-8c16g-docker` | 8 / 16 GiB            | everything above **+ Docker Engine (dind-capable)**  | jobs using `container:`, service containers, `docker build` |

The Docker tier exists because some pipelines rely on `docker build`, job
`container:` and service containers, which need a real Docker daemon inside the
guest.

## What the boot service does (MMDS → JIT)

At boot the guest fetches its runner JIT config from Firecracker MMDS v2:

1. Grab an MMDS token: `PUT http://169.254.169.254/latest/api/token`
   (`X-metadata-token-ttl-seconds`).
2. `GET http://169.254.169.254/` with `X-metadata-token` → the JSON firerunner
   put there (`{"jitconfig": "<base64>"}`).
3. Run `./run.sh --jitconfig <base64>` (ephemeral; auto-deregisters after one
   job).
4. When the job finishes the runner exits; the boot service issues `reboot -f`
   so the VMM exits and the host reaps the microVM.

A static `/etc/resolv.conf` (matching `--dns-servers`) is baked in because the
egress allowlist only permits the configured resolvers.

## Building

```bash
# On a Linux KVM host:
sudo ./build-rootfs.sh \
  --tier firerunner-4c8g \
  --runner-version 2.320.0 \
  --out /var/lib/firerunner/golden-4c8g.ext4
```

See `build-rootfs.sh` for the (scaffolded) steps: create/format the ext4 image,
bootstrap a minimal base, install `actions/runner`, drop in the boot service +
resolv.conf, then unmount. The result is an immutable file firerunner
reflink-clones per job.

## Rebuild policy

GitHub only supports self-hosted runner agents released within the **last 30
days**. `.github/workflows/build-image.yml` rebuilds the images on a schedule
(monthly, well within the window) and on manual dispatch, pinning the latest
`actions/runner` release. Rebuilding also picks up base-OS security updates so
every microVM starts from a patched image.
