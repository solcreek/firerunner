package listener

import (
	"context"
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

func TestScalerJobCallbacksAreNoOps(t *testing.T) {
	a := &scaler{minRunners: 0, log: testLogger(), onDesired: func(context.Context, int) int { return 0 }}
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "r", Result: "succeeded"}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildLabels(t *testing.T) {
	if got := buildLabels("firerunner", nil); len(got) != 1 || got[0].Name != "firerunner" {
		t.Fatalf("default labels = %v, want [firerunner]", got)
	}
	got := buildLabels("firerunner", []string{"a", "b"})
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("labels = %v, want [a b]", got)
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
