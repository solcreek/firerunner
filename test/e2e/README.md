# firerunner end-to-end tests

These tests boot **real Firecracker microVMs**, so they only run on a Linux host
with `/dev/kvm`, and are gated behind the `e2e` build tag (they are skipped
otherwise). They also need `CAP_NET_ADMIN` (tap devices, nftables, sysctl), so
run them as root.

## Smoke test (no golden image, no GitHub App)

`TestLaunchBootsAndSelfDestructs` validates the entire `Launch` path — reflink
clone, per-VM tap/IP, the nftables egress allowlist, the Firecracker API
sequence, guest boot, and teardown — against a tiny **self-destructing rootfs**.
That rootfs is just a static Go PID1 (`smoke_init.go`) at `/sbin/init` that
reboots immediately; with `reboot=k` on the cmdline the VMM exits, which is
exactly firerunner's self-destruct signal. If `Launch` returns quickly, the
whole path works.

```bash
# 1. Build the smoke rootfs (Linux + root):
sudo ./build-smoke-rootfs.sh          # -> /var/tmp/fr-smoke/rootfs.ext4

# 2. Compile the e2e binary as your user, run it as root:
go test -c -tags e2e -o /tmp/fr-e2e.test ./test/e2e/
sudo env \
  FR_KERNEL=/path/to/vmlinux \
  FR_GOLDEN=/var/tmp/fr-smoke/rootfs.ext4 \
  FR_EXT_IFACE=enp2s0 \
  /tmp/fr-e2e.test -test.run TestLaunchBootsAndSelfDestructs -test.v
```

Expected: `--- PASS ... (~1s)`. A hang until the context timeout means the guest
never reset (check the kernel `reboot=k` arg and that `/sbin/init` is the static
init).

Compiling the test as your user and running the binary as root keeps root out of
your Go module/build caches.

## Required environment

| Var           | Meaning                                             |
| ------------- | --------------------------------------------------- |
| `FR_KERNEL`   | uncompressed guest kernel (vmlinux)                 |
| `FR_GOLDEN`   | rootfs image to boot (smoke rootfs, or a real one)  |
| `FR_EXT_IFACE`| host egress interface (e.g. `enp2s0`)               |

The tests skip (not fail) when `/dev/kvm` or these variables are missing.

## Full job test

Running an actual GitHub Actions job end-to-end additionally needs a
firerunner-compatible golden image (`actions/runner` + the MMDS-JIT boot service;
see [`../../images/`](../../images/README.md)) and a GitHub App or PAT pointed at
a test repository. That is exercised by running the `firerunner` binary itself,
not by this package.
