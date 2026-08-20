# firerunner

Ephemeral [Firecracker](https://firecracker-microvm.github.io/) microVM runners
for GitHub Actions. Every job runs in a fresh, single-use microVM that registers
a just-in-time (JIT) ephemeral runner, executes exactly one job, and then self-
destructs.

Built for **maximum control and a minimal dependency surface**: firerunner talks
to the Firecracker REST API directly over its unix socket using only the Go
standard library — no containerd, no CNI, no LVM, no VM-management daemon. Its
only external dependency is GitHub's official runner scale-set client
(`github.com/actions/scaleset`), which drives the long-poll control plane.

> Status: early. The GitHub control-plane integration
> (`github.com/actions/scaleset`) is wired; the provisioner still needs
> hardening (per-VM NAT/egress, log shipping) before production use.

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
                 │  provisioner     per job: reflink golden.ext4 → tap/NAT → MMDS(JIT) │
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
to the kernel via the `ip=` boot argument (no DHCP needed). The host enables
IPv4 forwarding and installs a single nftables masquerade rule (in a dedicated
`firerunner` table) for `172.16.0.0/16` out the external interface
(`--ext-iface`), giving every guest egress to GitHub. Slot allocation is bounded
by `--max-runners`.

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
- **Per-VM network isolation + egress NAT** — dedicated tap/subnet per microVM.
- **External log forwarding** — serial console shipped off-VM to `--log-dir`.
- Roadmap: egress allowlist (restrict beyond blanket NAT), golden-image rebuild
  pipeline (≤30 days, per GitHub's runner-update policy).

## Requirements

- Linux bare-metal host with KVM (`/dev/kvm`).
- `firecracker` binary, a guest kernel (vmlinux), and a golden rootfs image
  with `actions/runner` + a JIT-reading boot service pre-installed.
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
  --app-client-id ... --app-installation-id ... --app-private-key /path/key.pem
```

All flags can also be set via `FR_*` environment variables (see
`config.example.env`).

## License

[MIT](./LICENSE)
