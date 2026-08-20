// Package listener yields the desired runner count over time by wrapping
// github.com/actions/scaleset — the official GitHub Actions Runner Scale Set
// client — which long-polls GitHub for assigned jobs. It is the single external
// dependency of firerunner.
package listener

import "context"

// DesiredFunc is invoked whenever GitHub reports a new desired runner count. It
// returns the number of runners currently in flight, which is reported back to
// GitHub's scale-set protocol.
type DesiredFunc func(ctx context.Context, desired int) (running int)

// Listener drives the scheduler from GitHub's assigned-job signal.
type Listener interface {
	// Run blocks until ctx is cancelled, calling onDesired with the latest
	// desired runner count each time GitHub reports one.
	Run(ctx context.Context, onDesired DesiredFunc) error
}
