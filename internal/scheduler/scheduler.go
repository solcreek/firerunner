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
}

// New returns a Scheduler.
func New(o Options) *Scheduler { return &Scheduler{opts: o} }

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
	defer func() {
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}()

	name, jit, err := s.opts.JIT.Generate(ctx, s.opts.Spec)
	if err != nil {
		s.opts.Logger.Error("generate JIT config", "err", err)
		return
	}
	if err := s.opts.Provisioner.Launch(ctx, name, jit, s.opts.Spec); err != nil {
		s.opts.Logger.Error("launch microVM", "runner", name, "err", err)
	}
}

// Running returns the current number of in-flight microVMs.
func (s *Scheduler) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Drain blocks until all in-flight microVMs have exited.
func (s *Scheduler) Drain() { s.wg.Wait() }
