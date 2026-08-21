// Package scheduler reconciles the number of running ephemeral microVMs to the
// demand reported by GitHub, bounded by a per-host capacity.
package scheduler

import (
	"context"
	"log/slog"
	"sync"

	"github.com/solcreek/firerunner/internal/core"
	"github.com/solcreek/firerunner/internal/provisioner"
)

// JITSource generates a just-in-time runner registration for a new microVM.
// The actions/scaleset listener implements this against the GitHub API.
type JITSource interface {
	Generate(ctx context.Context, spec core.RunnerSpec) (name, jitConfig string, err error)
}

// Options configures a Scheduler.
type Options struct {
	Max         int
	Min         int
	Spec        core.RunnerSpec
	Provisioner provisioner.Provisioner
	JIT         JITSource
	Logger      *slog.Logger
}

// Scheduler launches one ephemeral microVM per assigned job, up to Max
// concurrently.
type Scheduler struct {
	opts    Options
	mu      sync.Mutex
	running int
	wg      sync.WaitGroup
	// active tracks in-flight microVMs by runner name so a shutdown drain can
	// tell an idle warm-pool VM (cancel it immediately) from one running a job
	// (let it finish). MarkBusy flips a VM to busy when GitHub assigns it a job.
	active map[string]*vmHandle
}

// vmHandle is the shutdown-relevant state of one in-flight microVM.
type vmHandle struct {
	cancel context.CancelFunc
	busy   bool
}

// New returns a Scheduler.
func New(o Options) *Scheduler {
	return &Scheduler{opts: o, active: make(map[string]*vmHandle)}
}

// plan returns how many new runners to launch given the desired count reported
// by GitHub, the number already running, and the capacity limit. It never
// returns negative (we rely on ephemeral runners self-terminating) and never
// exceeds available capacity.
func plan(desired, running, max int) int {
	want := desired - running
	if want < 0 {
		want = 0
	}
	if avail := max - running; want > avail {
		want = avail
	}
	if want < 0 {
		want = 0
	}
	return want
}

// Reconcile launches microVMs to meet the desired count, bounded by capacity.
// It is safe for concurrent use; each launched runner handles exactly one job.
func (s *Scheduler) Reconcile(ctx context.Context, desired int) {
	s.mu.Lock()
	n := plan(desired, s.running, s.opts.Max)
	s.running += n
	running := s.running
	s.mu.Unlock()

	if n == 0 {
		return
	}
	s.opts.Logger.Info("scaling up", "desired", desired, "launching", n, "running", running, "max", s.opts.Max)
	for i := 0; i < n; i++ {
		s.wg.Add(1)
		go s.launchOne(ctx)
	}
}

func (s *Scheduler) launchOne(ctx context.Context) {
	defer s.wg.Done()
	// This runs before wg.Done above (defers are LIFO), so the WaitGroup counter
	// stays >=1 while maintainMinimum may Add a replacement — avoiding a
	// concurrent-Add-during-Drain race.
	defer func() {
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
		s.maintainMinimum(ctx)
	}()

	// Once shutdown has begun the listener has stopped accepting work and Drain
	// is waiting to reap; don't boot a brand-new microVM into a draining host.
	if ctx.Err() != nil {
		return
	}

	name, jit, err := s.opts.JIT.Generate(context.WithoutCancel(ctx), s.opts.Spec)
	if err != nil {
		s.opts.Logger.Error("generate JIT config", "err", err)
		return
	}

	// Detach the microVM's lifetime from ctx: once a runner is booting or running
	// its single job we must let it finish rather than kill it mid-job. The
	// provisioner boots Firecracker via exec.CommandContext, which SIGKILLs the
	// VMM the instant its context is cancelled — so if we passed ctx straight
	// through, a SIGTERM would abort every in-flight job.
	//
	// A watcher still cancels the VM on shutdown, but ONLY while it is idle (no
	// job assigned yet): an idle warm-pool VM has nothing to lose and must not
	// block the drain, whereas a busy VM (MarkBusy flipped it when GitHub started
	// its job) is left to finish and is reaped by Drain when it self-destructs.
	// systemd's TimeoutStopSec (with KillMode=mixed) is the final backstop for a
	// job that overruns the stop timeout.
	vmCtx, vmCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer vmCancel()
	s.track(name, vmCancel)
	defer s.untrack(name)

	go func() {
		select {
		case <-ctx.Done():
			s.cancelIfIdle(name)
		case <-vmCtx.Done():
		}
	}()

	if err := s.opts.Provisioner.Launch(vmCtx, name, jit, s.opts.Spec); err != nil {
		s.opts.Logger.Error("launch microVM", "runner", name, "err", err)
	}
}

// track registers an in-flight microVM so a shutdown drain can find and, if it
// is still idle, cancel it. It starts life idle (busy=false).
func (s *Scheduler) track(name string, cancel context.CancelFunc) {
	s.mu.Lock()
	s.active[name] = &vmHandle{cancel: cancel}
	s.mu.Unlock()
}

// untrack removes a microVM from the registry once it has exited.
func (s *Scheduler) untrack(name string) {
	s.mu.Lock()
	delete(s.active, name)
	s.mu.Unlock()
}

// MarkBusy records that GitHub has assigned a job to the named runner, so a
// shutdown drain lets it finish instead of cancelling it as an idle warm-pool
// VM. It is driven by the listener's job-started signal and no-ops for a runner
// that has already exited (or belongs to another tier's scheduler).
func (s *Scheduler) MarkBusy(name string) {
	s.mu.Lock()
	if h := s.active[name]; h != nil {
		h.busy = true
	}
	s.mu.Unlock()
}

// cancelIfIdle cancels the named microVM's context only if no job has been
// assigned to it, so shutdown reaps idle warm-pool VMs at once while leaving
// busy VMs to finish their job.
func (s *Scheduler) cancelIfIdle(name string) {
	s.mu.Lock()
	h := s.active[name]
	var cancel context.CancelFunc
	if h != nil && !h.busy {
		cancel = h.cancel
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// maintainMinimum tops the warm pool back up to Min after a microVM exits. GitHub
// only pushes a desired-count message when job demand changes, so without this a
// burst that drains the pool would leave it below Min until the next job arrives —
// exactly when the following burst is most likely and the warm-pool latency win
// matters most. It no-ops during shutdown (ctx cancelled) so it never relaunches
// VMs that Drain is trying to reap.
func (s *Scheduler) maintainMinimum(ctx context.Context) {
	if s.opts.Min <= 0 || ctx.Err() != nil {
		return
	}
	s.Reconcile(ctx, s.opts.Min)
}

// Running returns the current number of in-flight microVMs.
func (s *Scheduler) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Drain blocks until all in-flight microVMs have exited.
func (s *Scheduler) Drain() { s.wg.Wait() }
