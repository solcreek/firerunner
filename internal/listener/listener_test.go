package listener

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/solcreek/firerunner/internal/core"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestScalerHandleDesiredRunnerCount(t *testing.T) {
	var gotDesired int
	a := &scaler{
		minRunners: 2,
		log:        testLogger(),
		onDesired: func(_ context.Context, desired int) int {
			gotDesired = desired
			return 3
		},
	}
	running, err := a.HandleDesiredRunnerCount(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if gotDesired != 6 {
		t.Fatalf("desired = %d, want minRunners(2)+count(4)=6", gotDesired)
	}
	if running != 3 {
		t.Fatalf("running = %d, want 3 (value returned by onDesired)", running)
	}
}

func TestScalerAdvertisesCapacityAfterEveryDesiredCount(t *testing.T) {
	capacity := 5
	var advertised []int
	a := &scaler{
		maxRunners:  8,
		log:         testLogger(),
		onDesired:   func(context.Context, int) int { return 0 },
		capacity:    func() int { return capacity },
		setCapacity: func(c int) { advertised = append(advertised, c) },
	}
	for _, c := range []int{5, 2, 2, 7} {
		capacity = c
		if _, err := a.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
	}
	// Every callback re-reads capacity and pushes it, so the library's next
	// GetMessage always carries the current value, even when unchanged.
	want := []int{5, 2, 2, 7}
	if len(advertised) != len(want) {
		t.Fatalf("advertised %v, want %v", advertised, want)
	}
	for i := range want {
		if advertised[i] != want[i] {
			t.Fatalf("advertised %v, want %v", advertised, want)
		}
	}
}

func TestScalerClampsAdvertisedCapacity(t *testing.T) {
	cases := []struct {
		name     string
		capacity int
		want     int
	}{
		{"above tier max is capped", 50, 8},
		{"at tier max passes", 8, 8},
		{"zero becomes one", 0, 1},
		{"negative becomes one", -3, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got int
			a := &scaler{
				maxRunners:  8,
				log:         testLogger(),
				onDesired:   func(context.Context, int) int { return 0 },
				capacity:    func() int { return tc.capacity },
				setCapacity: func(c int) { got = c },
			}
			if _, err := a.HandleDesiredRunnerCount(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("advertised %d, want %d", got, tc.want)
			}
		})
	}
}

func TestScalerCapacityReadAfterDesired(t *testing.T) {
	// onDesired reconciles (launching VMs), so the capacity GitHub sees must be
	// sampled after it, not before — otherwise a burst would be advertised
	// against a stale, pre-launch count.
	var order []string
	a := &scaler{
		maxRunners: 4,
		log:        testLogger(),
		onDesired: func(context.Context, int) int {
			order = append(order, "desired")
			return 1
		},
		capacity:    func() int { order = append(order, "capacity"); return 1 },
		setCapacity: func(int) { order = append(order, "set") },
	}
	if _, err := a.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != "desired" || order[1] != "capacity" || order[2] != "set" {
		t.Fatalf("call order = %v, want [desired capacity set]", order)
	}
}

func TestScalerWithoutCapacityFuncNeverAdvertises(t *testing.T) {
	a := &scaler{
		maxRunners:  4,
		log:         testLogger(),
		onDesired:   func(context.Context, int) int { return 0 },
		setCapacity: func(int) { t.Fatal("setCapacity must not be called without a CapacityFunc") },
	}
	if _, err := a.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	// And a CapacityFunc with nowhere to send it is equally inert.
	b := &scaler{
		maxRunners: 4,
		log:        testLogger(),
		onDesired:  func(context.Context, int) int { return 0 },
		capacity:   func() int { t.Fatal("capacity must not be read without a sink"); return 0 },
	}
	if _, err := b.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}

func TestScalerJobCallbacks(t *testing.T) {
	var busy []string
	a := &scaler{
		minRunners: 0,
		log:        testLogger(),
		onDesired:  func(context.Context, int) int { return 0 },
		onBusy:     func(name string) { busy = append(busy, name) },
	}
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "r"}); err != nil {
		t.Fatal(err)
	}
	if len(busy) != 1 || busy[0] != "r" {
		t.Fatalf("HandleJobStarted should mark runner busy: got %v", busy)
	}
	// A nameless JobStarted must not mark anything busy.
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{}); err != nil {
		t.Fatal(err)
	}
	if len(busy) != 1 {
		t.Fatalf("nameless JobStarted should not mark busy: got %v", busy)
	}
	if err := a.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "r", Result: "succeeded"}); err != nil {
		t.Fatal(err)
	}
}

func TestScalerJobStartedNilBusyFunc(t *testing.T) {
	a := &scaler{minRunners: 0, log: testLogger(), onDesired: func(context.Context, int) int { return 0 }}
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "r"}); err != nil {
		t.Fatalf("HandleJobStarted with nil onBusy must not panic: %v", err)
	}
}

func TestBuildLabels(t *testing.T) {
	if got := buildLabels("firerunner", nil); len(got) != 1 || got[0].Name != "firerunner" {
		t.Fatalf("default labels = %v, want [firerunner]", got)
	}
	// The name is always advertised first; extra labels are additive and the
	// name is not duplicated if repeated in the extras.
	got := buildLabels("firerunner-node", []string{"node", "firerunner-node", "gpu"})
	if len(got) != 3 || got[0].Name != "firerunner-node" || got[1].Name != "node" || got[2].Name != "gpu" {
		t.Fatalf("labels = %v, want [firerunner-node node gpu]", got)
	}
}

func TestSystemInfoDefaults(t *testing.T) {
	si := systemInfo(Config{}, 7)
	if si.System != "firerunner" || si.Subsystem != "listener" {
		t.Fatalf("system info = %+v", si)
	}
	if si.Version != "dev" || si.CommitSHA != "NA" || si.ScaleSetID != 7 {
		t.Fatalf("defaults not applied: %+v", si)
	}
}

func TestRandSuffixUnique(t *testing.T) {
	if a, b := randSuffix(), randSuffix(); a == b {
		t.Fatalf("randSuffix collided: %q", a)
	}
	if got := randSuffix(); len(got) != 8 {
		t.Fatalf("randSuffix len = %d, want 8", len(got))
	}
}

// Compile-time proof the JIT source satisfies scheduler's expectation.
var _ interface {
	Generate(context.Context, core.RunnerSpec) (string, string, error)
} = (*JITSource)(nil)

func TestIsSessionConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"conflict 409", errors.New(`unexpected status code 409 Conflict: RunnerScaleSetSessionConflictException`), true},
		{"conflict mixed case", errors.New("The scaleset already has an active session (CONFLICT)"), true},
		{"unrelated", errors.New("401 Unauthorized: bad credentials"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSessionConflict(tc.err); got != tc.want {
				t.Fatalf("isSessionConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type fakeRemover struct {
	ref       *scaleset.RunnerReference
	lookupErr error
	removeErr error
	removed   []int64
}

func (f *fakeRemover) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	return f.ref, f.lookupErr
}

func (f *fakeRemover) RemoveRunner(_ context.Context, id int64) error {
	f.removed = append(f.removed, id)
	return f.removeErr
}

// TestJITSourceRemoveRunnerMapsErrors pins the mapping the scheduler relies on:
// only JobStillRunning reads as busy, and a runner GitHub no longer knows is
// already gone.
func TestJITSourceRemoveRunnerMapsErrors(t *testing.T) {
	ref := &scaleset.RunnerReference{ID: 42, Name: "r"}
	cases := []struct {
		name       string
		fake       fakeRemover
		wantErr    bool
		wantBusy   bool
		wantRemove bool
	}{
		{"removed", fakeRemover{ref: ref}, false, false, true},
		{"unknown runner", fakeRemover{}, false, false, false},
		{"not found on remove", fakeRemover{ref: ref, removeErr: fmt.Errorf("req: %w: gone", scaleset.RunnerNotFoundError)}, false, false, true},
		{"job assigned", fakeRemover{ref: ref, removeErr: fmt.Errorf("req: %w: busy", scaleset.JobStillRunningError)}, true, true, true},
		{"other remove failure", fakeRemover{ref: ref, removeErr: errors.New("503")}, true, false, true},
		{"lookup failure", fakeRemover{lookupErr: errors.New("503")}, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fake
			j := &JITSource{remover: &f, removeSlot: make(chan struct{}, 1)}
			err := j.RemoveRunner(context.Background(), "r")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got := errors.Is(err, core.ErrRunnerBusy); got != tc.wantBusy {
				t.Fatalf("errors.Is(err, ErrRunnerBusy)=%v want %v (err=%v)", got, tc.wantBusy, err)
			}
			if got := len(f.removed) == 1 && f.removed[0] == 42; got != tc.wantRemove {
				t.Fatalf("removed=%v wantRemove=%v", f.removed, tc.wantRemove)
			}
		})
	}
}

// blockingRemover holds each RemoveRunner until release, honouring ctx the way
// scaleset's requests do once they are running.
type blockingRemover struct {
	release chan struct{}
	started chan struct{}
}

func (b *blockingRemover) GetRunnerByName(context.Context, string) (*scaleset.RunnerReference, error) {
	return &scaleset.RunnerReference{ID: 1}, nil
}

func (b *blockingRemover) RemoveRunner(ctx context.Context, _ int64) error {
	b.started <- struct{}{}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestRemoveRunnerTimeoutStartsWhenItRuns verifies a removal queued behind
// another gets its full removeOpTimeout once it runs, instead of spending it
// waiting in the queue.
func TestRemoveRunnerTimeoutStartsWhenItRuns(t *testing.T) {
	old := removeOpTimeout
	removeOpTimeout = 200 * time.Millisecond
	defer func() { removeOpTimeout = old }()

	b := &blockingRemover{release: make(chan struct{}), started: make(chan struct{}, 2)}
	j := &JITSource{remover: b, removeSlot: make(chan struct{}, 1)}

	first := make(chan error, 1)
	go func() { first <- j.RemoveRunner(context.Background(), "a") }()
	<-b.started
	second := make(chan error, 1)
	go func() { second <- j.RemoveRunner(context.Background(), "b") }()

	// The first holds the slot for most of a timeout while the second waits.
	time.Sleep(150 * time.Millisecond)
	b.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("first removal: %v", err)
	}
	<-b.started
	time.Sleep(100 * time.Millisecond) // ~250 ms after it queued, ~100 ms after it ran
	b.release <- struct{}{}
	if err := <-second; err != nil {
		t.Fatalf("queued removal timed out before it ran: %v", err)
	}
}

// TestRemoveRunnerGivesUpWaitingOnCtx verifies a caller whose ctx ends while
// queued returns instead of piling up behind the slot.
func TestRemoveRunnerGivesUpWaitingOnCtx(t *testing.T) {
	j := &JITSource{remover: &fakeRemover{}, removeSlot: make(chan struct{}, 1)}
	j.removeSlot <- struct{}{} // slot held elsewhere
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := j.RemoveRunner(ctx, "r"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v want DeadlineExceeded", err)
	}
}
