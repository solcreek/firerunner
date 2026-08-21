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

func TestScalerJobStartedInvokesCallback(t *testing.T) {
	var got string
	a := &scaler{
		minRunners:   0,
		log:          testLogger(),
		onDesired:    func(context.Context, int) int { return 0 },
		onJobStarted: func(name string) { got = name },
	}
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "r"}); err != nil {
		t.Fatal(err)
	}
	if got != "r" {
		t.Fatalf("onJobStarted got %q, want %q", got, "r")
	}
	// A nil onJobStarted must not panic; completion stays a pure log.
	b := &scaler{minRunners: 0, log: testLogger(), onDesired: func(context.Context, int) int { return 0 }}
	if err := b.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "r", Result: "succeeded"}); err != nil {
		t.Fatal(err)
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
