package scheduler

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/solcreek/firerunner/internal/core"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type provFunc func(ctx context.Context, name, jit string, spec core.RunnerSpec) error

func (p provFunc) Launch(ctx context.Context, name, jit string, spec core.RunnerSpec) error {
	return p(ctx, name, jit, spec)
}
func (provFunc) Name() string { return "fake" }

type jitStub struct{}

func (jitStub) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	return "runner", "jit", nil
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name                    string
		desired, running, max   int
		want                    int
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
	prov := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec) error {
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
	prov := provFunc(func(context.Context, string, string, core.RunnerSpec) error {
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
