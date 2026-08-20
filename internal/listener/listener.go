// Package listener yields the desired runner count over time. The production
// implementation wraps github.com/actions/scaleset (added in a later step); the
// Stub here keeps the binary buildable and dependency-free until then.
package listener

import (
	"context"
	"log/slog"
	"time"

	"github.com/solcreek/firerunner/internal/core"
)

// DesiredFunc is invoked whenever the desired runner count is known.
type DesiredFunc func(ctx context.Context, desired int)

// Listener drives the scheduler from an external demand signal.
type Listener interface {
	// Run blocks until ctx is cancelled, calling onDesired with the latest
	// desired runner count.
	Run(ctx context.Context, onDesired DesiredFunc) error
}

// Stub is a placeholder Listener used until the actions/scaleset long-poll
// listener is wired in. It reports zero demand.
type Stub struct{ log *slog.Logger }

// NewStub returns a stub listener.
func NewStub(log *slog.Logger) *Stub { return &Stub{log: log} }

// Run implements Listener.
func (s *Stub) Run(ctx context.Context, onDesired DesiredFunc) error {
	s.log.Warn("using stub listener; wire github.com/actions/scaleset to receive real jobs")
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			onDesired(ctx, 0)
		}
	}
}

// StubJIT is a placeholder JITSource. It satisfies scheduler.JITSource.
type StubJIT struct{}

// Generate implements the JIT source interface with a no-op config.
func (StubJIT) Generate(ctx context.Context, spec core.RunnerSpec) (string, string, error) {
	return "firerunner-stub", "", nil
}
