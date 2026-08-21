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

// BusyFunc is invoked with a runner's name when GitHub reports that runner has
// started a job, so the shutdown drain lets it finish instead of reaping it as
// an idle warm-pool VM. It is a second, independent busy signal alongside the
// guest console marker; either source flipping a VM to busy is enough.
type BusyFunc func(runnerName string)

// Listener drives the scheduler from GitHub's assigned-job signal.
type Listener interface {
	// Run blocks until ctx is cancelled, calling onDesired with the latest
	// desired runner count each time GitHub reports one and onBusy with a
	// runner's name each time GitHub reports it started a job. onBusy may be nil.
	Run(ctx context.Context, onDesired DesiredFunc, onBusy BusyFunc) error
}
