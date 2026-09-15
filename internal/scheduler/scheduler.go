// Package scheduler reconciles the number of running ephemeral microVMs to the
// demand reported by GitHub, bounded by a per-host capacity.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

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
	// Pending, when set, is shared by every Scheduler on the host so that each
	// tier's Capacity accounts for launches the other tiers have committed to
	// but the provisioner has not yet drawn a slot for. nil gives the
	// Scheduler a private counter.
	Pending *PendingLaunches
}

// PendingLaunches counts microVMs a Scheduler has added to its running total
// whose Launch has not yet been entered, so they are in neither the
// provisioner's free-slot count nor its in-use count. Capacity subtracts them
// from the free count to keep the two numbers a consistent snapshot: between
// Reconcile and the provisioner acquiring a slot (a JIT registration round
// trip to GitHub) a launching VM would otherwise be counted twice.
type PendingLaunches struct{ n atomic.Int64 }

func (p *PendingLaunches) add(n int) { p.n.Add(int64(n)) }
func (p *PendingLaunches) done()     { p.n.Add(-1) }

// Count returns the number of launches committed but not yet entered.
func (p *PendingLaunches) Count() int { return int(p.n.Load()) }

// Scheduler launches one ephemeral microVM per assigned job, up to Max
// concurrently.
type Scheduler struct {
	opts    Options
	mu      sync.Mutex
	running int
	wg      sync.WaitGroup
	// active tracks in-flight microVMs by runner name so a shutdown drain can
	// tell an idle warm-pool VM (cancel it immediately) from one running a job
	// (let it finish). MarkBusy flips a VM to busy when its guest dequeues a job.
	active map[string]*vmHandle
	// failures counts consecutive unhealthy microVM boots so the warm-pool
	// replenish path can back off exponentially instead of hot-looping a broken
	// golden (which burns a JIT registration per attempt). Reset on any healthy
	// launch. Guarded by mu.
	failures int
	// backingOff counts launch goroutines sleeping in backoff after a failed
	// boot: still in running (so the pacing and replenish bookkeeping hold)
	// but with no VM behind them. live leaves them out. Guarded by mu.
	backingOff int
	// pending is this tier's share of opts.Pending: launches in running that
	// have not yet drawn a slot. Guarded by mu.
	pending int
}

// vmHandle is the shutdown-relevant state of one in-flight microVM.
type vmHandle struct {
	cancel context.CancelFunc
	busy   bool
	// cancelled guards against a scale-down (or shutdown) cancelling the same
	// idle VM twice across rapid Reconcile calls before its launch goroutine has
	// removed it from the registry.
	cancelled bool
}

// New returns a Scheduler.
func New(o Options) *Scheduler {
	if o.Pending == nil {
		o.Pending = &PendingLaunches{}
	}
	return &Scheduler{opts: o, active: make(map[string]*vmHandle)}
}

// plan returns how many new runners to launch given the desired count reported
// by GitHub, the number of microVMs able to take a job (live), the number of
// launch goroutines in flight (running, which also counts VMs tearing down and
// launches sleeping in backoff), and the capacity limit. Demand is measured
// against live, so a job is never left waiting on a VM that cannot serve it;
// the launch is bounded by max - running, so a failing golden can hold at most
// max attempts in backoff at once. It returns only the scale-up amount (never
// negative); scaling down when desired drops is handled separately by
// scaleDown, which cancels idle VMs rather than relying solely on them
// self-terminating.
func plan(desired, live, running, max int) int {
	want := desired - live
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

// live returns how many in-flight microVMs can take a job: running minus the
// launches sleeping in backoff (no VM behind them) and the VMs scaleDown has
// cancelled (tearing down). Callers must hold s.mu.
func (s *Scheduler) live() int {
	n := s.running - s.backingOff
	for _, h := range s.active {
		if h.cancelled {
			n--
		}
	}
	return n
}

// Reconcile launches microVMs to meet the desired count, bounded by capacity,
// and cancels idle VMs when desired drops below the number running. It is safe
// for concurrent use; each launched runner handles exactly one job.
func (s *Scheduler) Reconcile(ctx context.Context, desired int) {
	s.mu.Lock()
	n := plan(desired, s.live(), s.running, s.opts.Max)
	s.running += n
	s.pending += n
	s.opts.Pending.add(n)
	running := s.running
	stopped := 0
	if n == 0 {
		stopped = s.scaleDown(desired)
	}
	s.mu.Unlock()

	if n > 0 {
		s.opts.Logger.Info("scaling up", "desired", desired, "launching", n, "running", running, "max", s.opts.Max)
		for i := 0; i < n; i++ {
			s.wg.Add(1)
			go s.launchOne(ctx)
		}
		return
	}
	if stopped > 0 {
		s.opts.Logger.Info("scaling down", "desired", desired, "cancelling", stopped, "running", running, "min", s.opts.Min)
	}
}

// scaleDown cancels idle (non-busy, not already cancelling) microVMs so the total
// running count converges toward max(desired, Min), returning how many it
// cancelled. Callers must hold s.mu; the cancel is invoked under the lock (a
// context.CancelFunc is non-blocking and re-enters the scheduler only
// asynchronously) so a MarkBusy cannot slip between the busy check and the
// cancel. Busy VMs are never cancelled — they are running a job and left to
// finish and self-terminate; the warm-pool floor (Min) is always preserved so a
// scale-to-zero still keeps pre-booted capacity.
func (s *Scheduler) scaleDown(desired int) int {
	floor := desired
	if floor < s.opts.Min {
		floor = s.opts.Min
	}
	excess := s.live() - floor
	if excess <= 0 {
		return 0
	}
	cancelled := 0
	for _, h := range s.active {
		if excess <= 0 {
			break
		}
		if h.busy || h.cancelled {
			continue
		}
		h.cancelled = true
		h.cancel()
		cancelled++
		excess--
	}
	return cancelled
}

func (s *Scheduler) launchOne(ctx context.Context) {
	defer s.wg.Done()
	finished := false
	finish := func() {
		if !finished {
			finished = true
			s.running--
		}
	}
	// This runs before wg.Done above (defers are LIFO), so the WaitGroup counter
	// stays >=1 while maintainMinimum may Add a replacement — avoiding a
	// concurrent-Add-during-Drain race.
	defer func() {
		s.mu.Lock()
		finish()
		s.mu.Unlock()
		s.maintainMinimum(ctx)
	}()

	// The launch stops being pending once it is about to enter the provisioner
	// (Firecracker.Launch draws its slot as its first act) or when we bail
	// before getting there.
	entered := false
	enter := func() {
		if !entered {
			entered = true
			s.mu.Lock()
			s.pending--
			s.mu.Unlock()
			s.opts.Pending.done()
		}
	}
	defer enter()

	// Once shutdown has begun the listener has stopped accepting work and Drain
	// is waiting to reap; don't boot a brand-new microVM into a draining host.
	if ctx.Err() != nil {
		return
	}

	name, jit, err := s.opts.JIT.Generate(context.WithoutCancel(ctx), s.opts.Spec)
	if err != nil {
		s.opts.Logger.Error("generate JIT config", "err", err)
		// A failed JIT generation is a GitHub API error; without pacing, the
		// maintainMinimum defer relaunches at once and spins a tight retry loop.
		// Settle the reservation first: backoff already keeps this launch out of
		// Capacity, and a slot it will never draw must not stay spoken for.
		enter()
		s.backoff(ctx)
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
	// block the drain, whereas a busy VM (MarkBusy flipped it when the guest
	// dequeued its job) is left to finish and is reaped by Drain when it
	// self-destructs.
	// systemd's TimeoutStopSec (with KillMode=mixed) is the final backstop for a
	// job that overruns the stop timeout.
	vmCtx, vmCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer vmCancel()
	// Settle the reservation before the handle becomes visible to scaleDown:
	// a handle that is both pending and cancelled would be discounted twice.
	enter()
	s.track(name, vmCancel)
	defer s.untrack(name)

	go func() {
		select {
		case <-ctx.Done():
			s.cancelIfIdle(name)
		case <-vmCtx.Done():
		}
	}()

	launchErr := s.opts.Provisioner.Launch(vmCtx, name, jit, s.opts.Spec, func() { s.MarkBusy(name) })
	// The VM is gone and its slot is back in the pool. Settle the count and
	// drop the handle in one critical section, so the two never disagree and a
	// scale-down during the backoff below cannot mark a dead launch cancelled.
	// The provisioner released the slot just before returning, so a Capacity
	// sample between that release and this lock still sees the VM and its
	// freed slot together; the window is a few instructions and closing it
	// would tie the provisioner's slot lifecycle to this lock. A failed launch
	// keeps its place in running through backoff so plan bounds the retries.
	s.mu.Lock()
	delete(s.active, name)
	failed := launchErr != nil && vmCtx.Err() == nil
	if !failed {
		finish()
	}
	s.mu.Unlock()
	switch {
	case vmCtx.Err() != nil:
		// We cancelled this VM ourselves (scale-down or shutdown); any error it
		// returns is expected, so treat it as healthy churn.
		s.resetBackoff()
	case launchErr != nil:
		s.opts.Logger.Error("launch microVM", "runner", name, "err", launchErr)
		s.backoff(ctx)
	default:
		s.resetBackoff()
	}
}

// backoff paces warm-pool replenishment after an unhealthy microVM boot so a
// persistently failing golden (or a GitHub API error during JIT generation)
// cannot spin a tight relaunch loop that burns a runner registration per
// attempt. The delay grows exponentially with the consecutive-failure count and
// returns early if shutdown begins, so it never delays Drain.
func (s *Scheduler) backoff(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.failures++
	s.backingOff++
	n := s.failures
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.backingOff--
		s.mu.Unlock()
	}()
	d := backoffDelay(n)
	s.opts.Logger.Warn("microVM boot failed; backing off before replenishing", "consecutiveFailures", n, "delay", d)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// resetBackoff clears the consecutive-failure count after a healthy launch.
func (s *Scheduler) resetBackoff() {
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()
}

const (
	backoffBase = 500 * time.Millisecond
	backoffMax  = 30 * time.Second
)

// backoffDelay returns the delay for the nth consecutive failure: backoffBase
// doubled per failure, capped at backoffMax (a loop, not a shift, so a long
// failure streak cannot overflow the duration).
func backoffDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := backoffBase
	for i := 1; i < failures && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
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

// MarkBusy records that the named runner has dequeued a job, so a shutdown
// drain lets it finish instead of cancelling it as an idle warm-pool VM. It is
// driven by the guest console "Running job:" marker (see the provisioner) and
// no-ops for a runner that has already exited.
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
	// Cancel under the lock (the CancelFunc only closes a channel, so it never
	// blocks) so a MarkBusy cannot slip between the busy check and the cancel
	// and get an in-flight job SIGKILLed — the same invariant scaleDown holds.
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.active[name]
	if h == nil || h.busy || h.cancelled {
		return
	}
	h.cancelled = true
	h.cancel()
}

// maintainMinimum tops the warm pool back up to Min after a microVM exits. GitHub
// only pushes a desired-count message when job demand changes, so without this a
// burst that drains the pool would leave it below Min until the next job arrives —
// exactly when the following burst is most likely and the warm-pool latency win
// matters most. It only ever launches (never cancels): it must not trigger the
// scale-down path, or every VM exit during a scale-down would keep collapsing the
// pool toward Min instead of the listener's actual desired count. It no-ops
// during shutdown (ctx cancelled) so it never relaunches VMs that Drain is
// trying to reap.
func (s *Scheduler) maintainMinimum(ctx context.Context) {
	if s.opts.Min <= 0 || ctx.Err() != nil {
		return
	}
	// The floor is measured against running rather than live on purpose: a
	// launch sleeping in backoff is the pool's pending replenishment, and
	// counting it keeps a failing golden from being retried faster than backoff
	// allows. Demand-driven launches (Reconcile) are the ones that must not
	// wait on it.
	s.mu.Lock()
	n := plan(s.opts.Min, s.running, s.running, s.opts.Max)
	s.running += n
	s.pending += n
	s.opts.Pending.add(n)
	running := s.running
	s.mu.Unlock()
	if n == 0 {
		return
	}
	s.opts.Logger.Info("replenishing warm pool", "min", s.opts.Min, "launching", n, "running", running)
	for i := 0; i < n; i++ {
		s.wg.Add(1)
		go s.launchOne(ctx)
	}
}

// Running returns the current number of in-flight microVMs.
func (s *Scheduler) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Capacity returns how many jobs this tier can hold concurrently right now.
// It is what the listener advertises to GitHub as the scale set's capacity, so
// it must match what Reconcile can actually serve — otherwise GitHub assigns
// the job here and it queues while a sibling host sits idle. Three parts add
// up, mirroring plan:
//
//   - VMs that hold a slot and can take a job (live minus this tier's pending
//     launches);
//   - pending launches that will find a slot (the shared pool has room for
//     them; those it does not have room for will fail and are not counted);
//   - launches Reconcile could still make: bounded both by Max - running, as
//     plan is, and by the slots the pool has left once every tier's pending
//     launches have drawn theirs.
//
// Launches sleeping in backoff and VMs tearing down after a cancel are in
// running but not in live: they cannot take a job, and their slot is either
// absent or not yet back in the pool. A provisioner without a shared pool has
// no slot constraint, so only Max - running bounds the last term — which is
// still below Max while such entries hold places in running, exactly as plan
// would refuse to launch into them.
//
// Every tier on the host sees the same free count, so the tiers' advertised
// values can sum to more than the pool while several are idle, and a burst
// that lands on two tiers within one poll can still be over-assigned by up to
// the free count. That is deliberate: dividing the free slots between tiers
// would leave a slot unadvertised whenever the tier that wants it has used up
// its share, stranding a job on a single host that has room for it. The
// overlap only lasts until the next poll, and the sum is never larger than the
// static maxima advertised before.
func (s *Scheduler) Capacity() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	room := max(0, s.opts.Max-s.running)
	committed := max(0, s.live()-s.pending)
	sr, ok := s.opts.Provisioner.(provisioner.SlotReporter)
	if !ok {
		return committed + s.pending + room
	}
	free := sr.FreeSlots()
	willBoot := min(s.pending, free)
	headroom := max(0, min(room, free-s.opts.Pending.Count()))
	return committed + willBoot + headroom
}

// Drain blocks until all in-flight microVMs have exited.
func (s *Scheduler) Drain() { s.wg.Wait() }
