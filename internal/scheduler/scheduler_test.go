package scheduler

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solcreek/firerunner/internal/core"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type provFunc func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error

func (p provFunc) Launch(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
	return p(ctx, name, jit, spec, onBusy)
}
func (provFunc) Name() string { return "fake" }

type jitStub struct{}

func (jitStub) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	return "runner", "jit", nil
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name                  string
		desired, running, max int
		want                  int
	}{
		{"none wanted", 0, 0, 4, 0},
		{"scale from zero", 3, 0, 4, 3},
		{"bounded by max", 10, 0, 4, 4},
		{"partial capacity", 10, 3, 4, 1},
		{"at capacity", 4, 4, 4, 0},
		{"desired below running", 1, 3, 4, 0},
		{"over capacity running", 10, 5, 4, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := plan(tc.desired, tc.running, tc.max); got != tc.want {
				t.Fatalf("plan(%d,%d,%d)=%d want %d", tc.desired, tc.running, tc.max, got, tc.want)
			}
		})
	}
}

func TestReconcileRespectsMaxAndDrains(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan string, 16)
	prov := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- name
		<-release
		return nil
	})
	s := New(Options{Max: 2, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(context.Background(), 5) // demand 5, capacity 2
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2", got)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for launch to enter")
		}
	}
	// Further reconcile at capacity launches nothing.
	s.Reconcile(context.Background(), 5)
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2 after saturated reconcile", got)
	}

	close(release)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

func TestReconcileZeroDoesNothing(t *testing.T) {
	prov := provFunc(func(context.Context, string, string, core.RunnerSpec, func()) error {
		t.Fatal("Launch should not be called for desired=0")
		return nil
	})
	s := New(Options{Max: 4, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})
	s.Reconcile(context.Background(), 0)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0", got)
	}
}

func TestMaintainsMinimumAfterExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	launched := make(chan string, 32)
	block := make(chan struct{})
	var count int32
	prov := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		launched <- name
		if atomic.AddInt32(&count, 1) == 1 {
			return nil // first microVM finishes its job and exits immediately
		}
		<-block // replacement stays "running" so we can observe it
		return nil
	})
	s := New(Options{Max: 4, Min: 1, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 1) // warm pool of 1; that VM exits and must be replenished

	for i := 0; i < 2; i++ {
		select {
		case <-launched:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected a replacement launch after exit; got %d launches", i)
		}
	}
	if got := s.Running(); got != 1 {
		t.Fatalf("running=%d want 1 (pool refilled to Min)", got)
	}

	// Shutdown must stop replenishment, not fight the drain.
	cancel()
	close(block)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

// TestBusyVMOutlivesCancel is the core of the graceful-drain fix: a SIGTERM
// (modelled by cancelling the parent context) must not cancel the context of a
// microVM that is running its job, or the provisioner's exec.CommandContext
// would SIGKILL Firecracker mid-job.
func TestBusyVMOutlivesCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan context.Context, 1)
	busyCh := make(chan func(), 1)
	release := make(chan struct{})
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- vmCtx
		busyCh <- onBusy
		<-release
		return nil
	})
	s := New(Options{Max: 1, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 1)

	var vmCtx context.Context
	select {
	case vmCtx = <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("launch never started")
	}

	(<-busyCh)() // guest console reported "Running job:" — mark the VM busy
	cancel()     // simulate SIGTERM while the job is in flight

	select {
	case <-vmCtx.Done():
		t.Fatal("busy microVM context was cancelled by shutdown; the job would be killed mid-run")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

// TestIdleVMCancelledOnShutdown verifies the other half: an idle warm-pool VM
// (no job assigned) is cancelled at once on shutdown so it never blocks the
// drain waiting for a job that will never come.
func TestIdleVMCancelledOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 1)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-vmCtx.Done() // stays "running" until cancelled
		return nil
	})
	s := New(Options{Max: 1, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 1)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("launch never started")
	}

	cancel() // shutdown with an idle warm VM in the pool

	done := make(chan struct{})
	go func() { s.Drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("idle warm VM was not cancelled on shutdown; drain hung")
	}
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

// TestReconcileAfterShutdownLaunchesNothing verifies we stop accepting new work
// once shutdown has begun: a Reconcile racing in after the context is cancelled
// must not boot a fresh microVM into a draining host.
func TestReconcileAfterShutdownLaunchesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	prov := provFunc(func(context.Context, string, string, core.RunnerSpec, func()) error {
		t.Fatal("Launch must not run once shutdown has begun")
		return nil
	})
	s := New(Options{Max: 4, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0", got)
	}
}

// jitSeq hands out a unique runner name per call so each microVM gets its own
// entry in the scheduler's active registry (the shared-name jitStub would
// collide for multi-VM scenarios).
type jitSeq struct{ n atomic.Int64 }

func (j *jitSeq) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	return "runner-" + strconv.FormatInt(j.n.Add(1), 10), "jit", nil
}

func waitRunning(t *testing.T, s *Scheduler, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if got := s.Running(); got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("running=%d want %d", s.Running(), want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestScaleDownCancelsIdle verifies that when demand drops below the number
// running, the surplus idle VMs are cancelled rather than left squatting slots.
func TestScaleDownCancelsIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-vmCtx.Done() // idle VM stays running until cancelled
		return nil
	})
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("launch never started")
		}
	}
	waitRunning(t, s, 3)

	s.Reconcile(ctx, 1) // demand falls to 1; two idle VMs must be cancelled
	waitRunning(t, s, 1)
}

// TestScaleDownLeavesBusy verifies a busy VM (running a job) is never cancelled
// by scale-down; only idle VMs are reaped.
func TestScaleDownLeavesBusy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	busyCh := make(chan func(), 8)
	release := make(chan struct{})
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		busyCh <- onBusy
		select {
		case <-vmCtx.Done():
		case <-release:
		}
		return nil
	})
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 2)
	var onBusy1 func()
	select {
	case onBusy1 = <-busyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first launch never started")
	}
	select {
	case <-busyCh: // second VM's onBusy (unused)
	case <-time.After(2 * time.Second):
		t.Fatal("second launch never started")
	}
	waitRunning(t, s, 2)

	onBusy1()            // one VM dequeues a job
	s.Reconcile(ctx, 0)  // demand drops to 0: only the idle VM may be cancelled
	waitRunning(t, s, 1) // the busy VM survives

	close(release)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

// TestScaleDownPreservesMin verifies scale-to-zero still keeps the warm-pool
// floor (Min) of pre-booted VMs.
func TestScaleDownPreservesMin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-vmCtx.Done()
		return nil
	})
	s := New(Options{Max: 4, Min: 1, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("launch never started")
		}
	}
	waitRunning(t, s, 3)

	s.Reconcile(ctx, 0) // demand 0, but Min=1 keeps one warm
	waitRunning(t, s, 1)
}
