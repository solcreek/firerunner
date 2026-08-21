# firerunner

[![CI](https://github.com/solcreek/firerunner/actions/workflows/ci.yml/badge.svg)](https://github.com/solcreek/firerunner/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/solcreek/firerunner.svg)](https://pkg.go.dev/github.com/solcreek/firerunner)
[![Go Report Card](https://goreportcard.com/badge/github.com/solcreek/firerunner)](https://goreportcard.com/report/github.com/solcreek/firerunner)
[![Go version](https://img.shields.io/github/go-mod/go-version/solcreek/firerunner)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)

Ephemeral [Firecracker](https://firecracker-microvm.github.io/) microVM runners
for GitHub Actions. Every job runs in a fresh, single-use microVM that registers
a just-in-time (JIT) ephemeral runner, executes exactly one job, and then self-
destructs.

Built for **maximum control and a minimal dependency surface**: firerunner talks
to the Firecracker REST API directly over its unix socket using only the Go
standard library — no containerd, no CNI, no LVM, no VM-management daemon. Its
only external dependency is GitHub's official runner scale-set client
(`github.com/actions/scaleset`), which drives the long-poll control plane.

> Status: early, but validated end-to-end on real hardware. The full path —
> GitHub scale-set long-poll, per-VM networking, the nftables egress allowlist,
> off-VM log shipping, golden-image build, and microVM boot → MMDS-JIT →
> self-destruct — has been exercised on a KVM host (Firecracker v1.10.1).
> Running a live production fleet additionally needs a golden image and a GitHub
> App (see below).

## Why microVMs

Self-hosted runners execute untrusted third-party code (dependencies, actions,
fork PRs). Containers share the host kernel, so a container escape reaches the
host — unacceptable when the host holds credentials. A Firecracker microVM has
its own kernel and hardware-enforced (KVM) isolation, at ~1–2% overhead versus
bare metal. See GitHub's guidance on
[self-hosted runner security](https://docs.github.com/en/actions/reference/runners/self-hosted-runners).

## Architecture

```
                 ┌──────────────────── firerunner (single Go binary) ─────────────────┐
GitHub  ◀──────▶ │  listener        long-poll GitHub → desired runner count           │
(scaleset API)   │  scheduler       reconcile running microVMs to desired, ≤ maxRunners│
                 │  provisioner     per job: reflink golden.ext4 → tap+nft → MMDS(JIT) │
                 │                  → firecracker InstanceStart → wait exit → reap      │
                 └────────────────────────────────────────────────────────────────────┘
                                              │  one microVM per job
                                              ▼
                        ┌──────── Firecracker microVM (ephemeral) ────────┐
                        │  actions/runner --jitconfig  → run ONE job      │
                        │  then `reboot -f` → VMM exits → host reaps it    │
                        └─────────────────────────────────────────────────┘
```

The guest self-destructs with `reboot -f` (not `poweroff`): with `reboot=k` on
the kernel command line this triggers an i8042 reset that makes the Firecracker
VMM process exit, which the host detects to reap the job.

### Networking

Each microVM gets its own tap device on a dedicated `/30` subnet
(`172.16.<slot>.0/30`; host gateway `.1`, guest `.2`), so many VMs run in
parallel without collisions. The guest address, gateway and netmask are handed
to the kernel via the `ip=` boot argument (no DHCP needed). Slot allocation is
bounded by `--max-runners`.

Egress is controlled by an **nftables allowlist** (in a dedicated `firerunner`
table). Instead of a blanket masquerade, the host enables IPv4 forwarding and
only forwards guest traffic destined for GitHub's own IP ranges — fetched from
[`api.github.com/meta`](https://docs.github.com/rest/meta/meta#get-github-meta-information)
and refreshed periodically (`--meta-refresh`, default 24h) — plus optional DNS
and NTP. Everything else from `172.16.0.0/16` is dropped, then the allowed
traffic is masqueraded out the external interface (`--ext-iface`). Configure the
allowlist with `--egress` (default `api,actions,git,dns,packages,ntp`); the
GitHub categories are `api`, `actions`, `git`, `packages`, and `dns`/`ntp` are
pseudo-categories. Pass `--egress open` to disable the allowlist and fall back to
blanket NAT.

### Logs

Because the microVM is destroyed after its one job, its serial console (which
carries the runner/job output) is streamed to a per-runner file under
`--log-dir` on the host — off-VM log forwarding, as GitHub recommends for
ephemeral runners.

## Alignment with GitHub's official recommendations

- **Ephemeral / JIT runners** — one job per runner, auto-deregistered (GitHub's
  recommended model for autoscaling).
- **Official runner agent** — uses `actions/runner` inside the golden image.
- **Official scaling API** — built on `github.com/actions/scaleset` (reliable
  long-poll; GitHub warns webhook-based scaling is less reliable).
- **Least-privilege auth** — GitHub App preferred over PAT.
- **Clean environment per job** — reflink-cloned rootfs, destroyed after use.
- **Per-VM network isolation + egress allowlist** — dedicated tap/subnet per
  microVM; guests may reach only GitHub's published IP ranges (plus DNS/NTP).
- **External log forwarding** — serial console shipped off-VM to `--log-dir`.
- **Golden-image rebuild pipeline** — images rebuilt on a schedule so the
  bundled `actions/runner` stays within GitHub's ≤30-day support window (see
  [`images/`](images/README.md)).

## Requirements

- Linux bare-metal host with KVM (`/dev/kvm`).
- `firecracker` binary, a guest kernel (vmlinux), and a golden rootfs image
  with `actions/runner` + a JIT-reading boot service pre-installed (see
  [`images/`](images/README.md) for the build tooling and rebuild policy).
- A reflink-capable filesystem (btrfs or XFS) for the work directory.
- `iproute2` (`ip`) and `nftables` (`nft`) on the host; `CAP_NET_ADMIN` and the
  ability to set `net.ipv4.ip_forward`.

## Build & test

```bash
make build         # build the binary
make test          # unit tests with -race
make cover-check   # unit coverage with a minimum threshold (COVER_MIN)
make e2e           # end-to-end tests (requires a real KVM host; -tags e2e)
```

Tests follow a functional-core / imperative-shell split: pure logic (the
Firecracker API sequence, the scale-up plan, config parsing) is unit-tested to
near-100%, host I/O is exercised through injected seams (a `commandRunner` fake
and an `httptest` server over a unix socket), and real microVM boots live in
`test/e2e` behind the `e2e` build tag. CI (`.github/workflows/ci.yml`) runs
build, vet and the coverage gate on every push and PR.

## Usage

```bash
firerunner \
  --url https://github.com/ORG/REPO \
  --name firerunner \
  --max-runners 4 --vcpu 4 --mem-mib 8192 \
  --kernel /var/lib/firerunner/vmlinux \
  --golden /var/lib/firerunner/golden.ext4 \
  --ext-iface enp2s0 --log-dir /var/log/firerunner \
  --egress api,actions,git,dns,packages,ntp --meta-refresh 24h \
  --app-client-id ... --app-installation-id ... --app-private-key /path/key.pem
```

All flags can also be set via `FR_*` environment variables (see
`config.example.env`).

### Warm pool (`--min-runners`)

Every job runs in a fresh microVM, so by default a runner is cold-booted only
once a job is queued. The microVM itself boots in well under a second, but the
GitHub runner agent still has to start and open its session to GitHub before it
can accept work — so the *first* job after an idle period waits for that
one-time connect.

Set `--min-runners N` (env `FR_MIN_RUNNERS`) to keep `N` microVMs pre-booted and
already registered (`Listening for Jobs`). A queued job is then handed to a warm
runner immediately, and firerunner launches a replacement to refill the pool.
This trades idle capacity for lower pickup latency:

| `--min-runners` | Pickup                                   | Idle cost                              |
| --------------- | ---------------------------------------- | -------------------------------------- |
| `0` (default)   | cold boot + runner connect per job       | none                                   |
| `1`+            | job handed to a waiting runner           | `N ×` (`--vcpu` / `--mem-mib`) held idle |

Warm runners are still single-use and ephemeral: a pre-booted VM that has not
run a job is discarded like any other once it does. Size the pool to your peak
concurrency — `--max-runners` still caps the total.

### Running as a service (systemd)

`deploy/firerunner.service` is a hardened unit template (dedicated `firerunner`
user, least-privilege capabilities). To install:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin firerunner
sudo usermod -aG kvm firerunner
sudo install -m0755 firerunner /usr/local/bin/firerunner
sudo mkdir -p /etc/firerunner
sudo cp config.example.env /etc/firerunner/firerunner.env   # then edit
sudo install -m0644 deploy/firerunner.service /etc/systemd/system/
sudo systemctl enable --now firerunner
```

Stopping the service sends `SIGTERM`; firerunner stops taking new work and
drains in-flight microVMs before exiting (`TimeoutStopSec` bounds the wait).
Restarts are safe: registration is idempotent and a session left behind by an
unclean exit is retried until GitHub expires it.

### Hardening: the Firecracker jailer (`--jailer`, opt-in)

firerunner already runs the VMM **non-root** (a dedicated `firerunner` user with
only `cap_net_admin`) and Firecracker installs **seccomp** filters by default, so
the two biggest sandboxing wins are on out of the box. The
[jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)
adds the rest — it `chroot`s each microVM into `<chroot-base>/firecracker/<id>/root`,
gives it a private PID namespace, creates jail-local `/dev/kvm` and `/dev/net/tun`
nodes, and drops the VMM to an unprivileged uid/gid.

It is **off by default** because it inverts the privilege model: the *launcher*
must run as **root** (to `chroot`, `mknod` and drop privileges), whereas the
default deployment keeps firerunner unprivileged. Enable it for multi-tenant,
untrusted-code or shared-host deployments:

```bash
firerunner ... \
  --jailer --jailer-bin /usr/local/bin/jailer \
  --chroot-base /srv/jailer \
  --jail-uid "$(id -u firerunner)" --jail-gid "$(id -g firerunner)"
```

Notes:

- The `jailer` binary must be the **same version** as `firecracker` (it ships in
  the same release tarball) and Firecracker must be the static musl build.
- No network namespace is used (`--netns` is not passed), so the per-slot
  host-namespace tap devices and the egress allowlist work unchanged.
- Per-launch overhead is ~single-digit milliseconds (chroot + staging the VMM),
  negligible against the microVM boot.
- The systemd unit must run as `root` when the jailer is enabled (the default
  `deploy/firerunner.service` runs as the `firerunner` user for the
  non-jailer mode).

## License

[MIT](./LICENSE)
