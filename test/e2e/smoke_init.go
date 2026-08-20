//go:build ignore

// smoke_init is a minimal PID1 for the firerunner smoke microVM. It is NOT part
// of any package build (see the `ignore` constraint); build-smoke-rootfs.sh
// compiles it directly with GOOS=linux and installs it at /sbin/init in the
// smoke rootfs.
//
// On boot it prints a marker, then triggers a kernel restart (the reboot -f
// equivalent). With `reboot=k` on the kernel command line this makes the
// Firecracker VMM process exit — exactly firerunner's self-destruct signal — so
// a passing smoke test proves the whole Launch path (reflink clone, tap/IP,
// nftables egress, the Firecracker API sequence, boot, and teardown).
package main

import (
	"os"
	"syscall"
	"time"
)

func main() {
	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	os.Stdout.WriteString("firerunner-smoke: PID1 up, self-destructing via reboot\n")
	time.Sleep(500 * time.Millisecond)
	syscall.Sync()
	// LINUX_REBOOT_CMD_RESTART + reboot=k -> i8042 reset -> VMM exits.
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	for {
		time.Sleep(time.Second)
	}
}
