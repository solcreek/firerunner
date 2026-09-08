package provisioner

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConnectDetectorFiresOnListening checks the connect detector fires once on
// the runner's registered-and-waiting line and ignores everything before it.
func TestConnectDetectorFiresOnListening(t *testing.T) {
	var n int
	d := newConnectDetector(func() { n++ })
	for _, line := range []string{
		"[    0.5] firerunner-run.sh: firerunner: mounted pre-seeded tool cache\n",
		"firerunner login: ",
		"√ Connected to GitHub\n",
	} {
		if _, err := d.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if n != 0 {
		t.Fatalf("fired %d times before registration, want 0", n)
	}
	if _, err := d.Write([]byte("2026-09-08 13:00:00Z: Listening for Jobs\n")); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fired %d times, want 1", n)
	}
	// A warm runner re-logs the line on reconnect; still once.
	for i := 0; i < 5; i++ {
		_, _ = d.Write([]byte("Listening for Jobs\nRunning job: x\n"))
	}
	if n != 1 {
		t.Fatalf("fired %d times after first, want 1", n)
	}
}

// TestConnectDetectorBusyImpliesConnected checks a job dequeued so fast that the
// idle line never appears still counts as connected.
func TestConnectDetectorBusyImpliesConnected(t *testing.T) {
	var n int
	d := newConnectDetector(func() { n++ })
	if _, err := d.Write([]byte("√ Connected to GitHub\nRunning job: build\n")); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fired %d times on Running job:, want 1", n)
	}
}

// TestConnectDetectorMatchesMarkerSplitAcrossWrites checks the carryover tail
// bridges a marker delivered one byte at a time — including the longer of the
// two markers, which sizes the tail.
func TestConnectDetectorMatchesMarkerSplitAcrossWrites(t *testing.T) {
	for _, marker := range []string{connectedMarker, busyMarker} {
		var n int
		d := newConnectDetector(func() { n++ })
		for _, b := range []byte("noise " + marker + " tail") {
			if _, err := d.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
		if n != 1 {
			t.Fatalf("%q byte-split fired %d times, want 1", marker, n)
		}
	}
}

// TestConnectDetectorNeverFiresOnWedgedBoot models the 2026-09-03 incident: a
// guest that boots, mounts everything, and then only ever retries DNS. Nothing
// it prints may count as a connect.
func TestConnectDetectorNeverFiresOnWedgedBoot(t *testing.T) {
	var n int
	d := newConnectDetector(func() { n++ })
	for i := 0; i < 200; i++ {
		_, _ = d.Write([]byte("firerunner-run.sh[692]: Http failure when getting runner info: Resource temporarily unavailable\n"))
		_, _ = d.Write([]byte("Ubuntu 24.04.4 LTS firerunner console\n\nfirerunner login: "))
	}
	if n != 0 {
		t.Fatalf("wedged boot fired %d times, want 0", n)
	}
}

// TestMarkerDetectorTailSizedByLongestMarker checks the shared detector keeps
// enough carryover for its longest marker when markers differ in length, so a
// split of the long one is not missed just because a short one was also given.
func TestMarkerDetectorTailSizedByLongestMarker(t *testing.T) {
	var n int
	d := newMarkerDetector(func() { n++ }, "ab", "a-much-longer-marker")
	if d.keep != len("a-much-longer-marker")-1 {
		t.Fatalf("keep = %d, want %d", d.keep, len("a-much-longer-marker")-1)
	}
	long := []byte("a-much-longer-marker")
	if _, err := d.Write(long[:7]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(long[7:]); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("split long marker fired %d times, want 1", n)
	}
}

// TestConnectWatchdogKillsWhenNeverConnected checks the deadline fires the kill
// exactly once and is reported by didKill.
func TestConnectWatchdogKillsWhenNeverConnected(t *testing.T) {
	var w connectWatchdog
	var kills atomic.Int32
	w.arm(30*time.Millisecond, func() { kills.Add(1) })
	deadline := time.Now().Add(2 * time.Second)
	for !w.didKill() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !w.didKill() {
		t.Fatal("watchdog never fired")
	}
	time.Sleep(60 * time.Millisecond)
	if got := kills.Load(); got != 1 {
		t.Fatalf("kill called %d times, want 1", got)
	}
	// A connect that lands after the kill (console lag) does not un-kill.
	w.markConnected()
	if !w.didKill() {
		t.Fatal("late markConnected cleared didKill")
	}
}

// TestConnectWatchdogDisarmedByConnect checks a runner that registers in time is
// never killed, even long after the deadline would have fired.
func TestConnectWatchdogDisarmedByConnect(t *testing.T) {
	var w connectWatchdog
	var kills atomic.Int32
	w.arm(20*time.Millisecond, func() { kills.Add(1) })
	w.markConnected()
	time.Sleep(80 * time.Millisecond)
	if kills.Load() != 0 || w.didKill() {
		t.Fatalf("connected VM was killed (kills=%d didKill=%v)", kills.Load(), w.didKill())
	}
}

// TestConnectWatchdogConnectBeforeArmIsInert covers the wiring order in Launch:
// the console detector exists before the VMM does, so a connect observed before
// arm must leave the watchdog permanently disarmed.
func TestConnectWatchdogConnectBeforeArmIsInert(t *testing.T) {
	var w connectWatchdog
	w.markConnected()
	var kills atomic.Int32
	w.arm(10*time.Millisecond, func() { kills.Add(1) })
	time.Sleep(50 * time.Millisecond)
	if kills.Load() != 0 || w.didKill() {
		t.Fatal("watchdog armed after an early connect still killed")
	}
	if w.timer != nil {
		t.Fatal("arm after connect created a timer")
	}
}

// TestConnectWatchdogZeroTimeoutDisabled checks --vm-connect-timeout 0 installs
// nothing, matching the lifetime backstop's opt-out semantics.
func TestConnectWatchdogZeroTimeoutDisabled(t *testing.T) {
	var w connectWatchdog
	w.arm(0, func() { t.Fatal("kill called with timeout disabled") })
	w.arm(-time.Second, func() { t.Fatal("kill called with negative timeout") })
	if w.timer != nil {
		t.Fatal("disabled watchdog created a timer")
	}
	w.stop() // must be safe with no timer
}

// TestConnectWatchdogStopDisarmsWithoutConnect checks Launch's deferred stop (VM
// exited on its own before the deadline) neither kills nor records a connect.
func TestConnectWatchdogStopDisarmsWithoutConnect(t *testing.T) {
	var w connectWatchdog
	var kills atomic.Int32
	w.arm(20*time.Millisecond, func() { kills.Add(1) })
	w.stop()
	time.Sleep(60 * time.Millisecond)
	if kills.Load() != 0 || w.didKill() {
		t.Fatal("stopped watchdog still killed")
	}
	w.mu.Lock()
	connected := w.connected
	w.mu.Unlock()
	if connected {
		t.Fatal("stop recorded a connect")
	}
}

// TestConnectWatchdogArmIsIdempotent checks a second arm does not start a second
// timer (and so cannot double-kill).
func TestConnectWatchdogArmIsIdempotent(t *testing.T) {
	var w connectWatchdog
	var kills atomic.Int32
	w.arm(20*time.Millisecond, func() { kills.Add(1) })
	first := w.timer
	w.arm(1*time.Millisecond, func() { kills.Add(100) })
	if w.timer != first {
		t.Fatal("second arm replaced the timer")
	}
	time.Sleep(80 * time.Millisecond)
	if got := kills.Load(); got != 1 {
		t.Fatalf("kills = %d, want exactly 1 from the first arm", got)
	}
}

// TestConnectWatchdogConcurrent races connects against the deadline from many
// goroutines (run with -race): the kill runs at most once and didKill is
// consistent with whether the kill ran.
func TestConnectWatchdogConcurrent(t *testing.T) {
	for i := 0; i < 50; i++ {
		var w connectWatchdog
		var kills atomic.Int32
		w.arm(time.Duration(i%5)*time.Millisecond+time.Millisecond, func() { kills.Add(1) })
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Duration(g) * 500 * time.Microsecond)
				w.markConnected()
				_ = w.didKill()
			}()
		}
		wg.Wait()
		time.Sleep(15 * time.Millisecond)
		if k := kills.Load(); k > 1 || (k == 1) != w.didKill() {
			t.Fatalf("iteration %d: kills=%d didKill=%v", i, k, w.didKill())
		}
	}
}

// TestConsoleConnectDetectionSurvivesLogWriteError mirrors the busy-detector
// guarantee for the connect signal: a failing per-runner log file must not
// abort the console pipe, or a healthy VM would be killed as never-connected.
func TestConsoleConnectDetectionSurvivesLogWriteError(t *testing.T) {
	var w connectWatchdog
	var kills atomic.Int32
	mw := io.MultiWriter(newConnectDetector(w.markConnected), tolerantWriter{w: errWriter{}})
	w.arm(30*time.Millisecond, func() { kills.Add(1) })
	if _, err := mw.Write([]byte("Listening for Jobs\n")); err != nil {
		t.Fatalf("MultiWriter aborted on log error: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if kills.Load() != 0 {
		t.Fatal("connected VM killed because the console pipe broke on a log error")
	}
}

// TestErrNeverConnectedIsIdentifiable checks the Launch error wraps the sentinel
// so callers and log readers can tell a never-connected kill from a boot crash.
func TestErrNeverConnectedIsIdentifiable(t *testing.T) {
	err := wrapNeverConnected("runner-1", 5*time.Minute)
	if !errors.Is(err, errNeverConnected) {
		t.Fatalf("errors.Is(errNeverConnected) = false for %v", err)
	}
	for _, want := range []string{"runner-1", "5m0s", "never registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

// TestConnectWatchdogTimerLosesRaceToConnect covers the guard inside the timer
// callback: markConnected can land after the timer has already fired but before
// the callback takes the lock (timer.Stop returns false in that window). The
// callback must then see connected and not kill. Simulated by recording the
// connect without stopping the timer.
func TestConnectWatchdogTimerLosesRaceToConnect(t *testing.T) {
	var w connectWatchdog
	var kills atomic.Int32
	w.arm(20*time.Millisecond, func() { kills.Add(1) })
	w.mu.Lock()
	w.connected = true // as markConnected would, minus the (lost) Stop
	w.mu.Unlock()
	time.Sleep(80 * time.Millisecond)
	if kills.Load() != 0 || w.didKill() {
		t.Fatalf("timer callback killed a VM that had connected (kills=%d didKill=%v)", kills.Load(), w.didKill())
	}
}

// TestConnectWatchdogTimerLosesRaceToStop covers the other side of the timer
// race: Launch's deferred stop can run after the timer has fired but before its
// callback takes the lock (timer.Stop returns false). The callback must then see
// stopped and not kill — cmd.Wait has already returned by then, so a kill would
// hit a reaped and possibly recycled PID. Simulated by recording the stop
// without stopping the timer.
func TestConnectWatchdogTimerLosesRaceToStop(t *testing.T) {
	var w connectWatchdog
	var kills atomic.Int32
	w.arm(20*time.Millisecond, func() { kills.Add(1) })
	w.mu.Lock()
	w.stopped = true // as stop would, minus the (lost) timer.Stop
	w.mu.Unlock()
	time.Sleep(80 * time.Millisecond)
	if kills.Load() != 0 || w.didKill() {
		t.Fatalf("timer callback killed after stop (kills=%d didKill=%v)", kills.Load(), w.didKill())
	}
	// And arm after stop stays inert, so a late arm cannot resurrect it either.
	w.arm(time.Millisecond, func() { kills.Add(1) })
	time.Sleep(20 * time.Millisecond)
	if kills.Load() != 0 {
		t.Fatal("arm after stop started a timer")
	}
}

// TestMarkerDetectorTailStaysBoundedOnIdleStream checks the carry-over never
// grows past the longest marker minus one — including the degenerate
// single-byte-marker case, where nothing can straddle a write boundary and the
// tail must stay empty rather than retaining the whole stream.
func TestMarkerDetectorTailStaysBoundedOnIdleStream(t *testing.T) {
	cases := map[string][]string{
		"single byte": {"X"},
		"mixed":       {"ab", "a-much-longer-marker"},
		"connect":     {connectedMarker, busyMarker},
	}
	for name, markers := range cases {
		d := newMarkerDetector(func() {}, markers...)
		for i := 0; i < 10_000; i++ {
			if _, err := d.Write([]byte("idle console noise with no marker in it\n")); err != nil {
				t.Fatal(err)
			}
			if len(d.tail) > d.keep {
				t.Fatalf("%s: tail grew to %d bytes after %d writes (keep=%d)", name, len(d.tail), i+1, d.keep)
			}
		}
	}
	// The single-byte marker is still detected despite carrying nothing over.
	var n int
	d := newMarkerDetector(func() { n++ }, "X")
	_, _ = d.Write([]byte("noise"))
	_, _ = d.Write([]byte("X"))
	if n != 1 {
		t.Fatalf("single-byte marker fired %d times, want 1", n)
	}
}
