package listener

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

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
