// Command firerunner runs ephemeral Firecracker microVM GitHub Actions runners.
//
// It long-polls GitHub for queued jobs (via the official actions/scaleset
// control plane), and for each job boots a fresh, single-use microVM that
// registers a JIT/ephemeral runner, executes exactly one job, then self-
// destructs. See README.md for the architecture and security model.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/linyiru/firerunner/internal/config"
	"github.com/linyiru/firerunner/internal/listener"
	"github.com/linyiru/firerunner/internal/provisioner"
	"github.com/linyiru/firerunner/internal/scheduler"
)

func main() {
	cfg, err := config.FromFlags(os.Args[1:])
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(2)
	}

	log := config.NewLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	prov := provisioner.NewFirecracker(cfg.Firecracker, log)

	sched := scheduler.New(scheduler.Options{
		Max:         cfg.MaxRunners,
		Spec:        cfg.RunnerSpec(),
		Provisioner: prov,
		JIT:         listener.StubJIT{}, // TODO(fr-listener): actions/scaleset JIT source
		Logger:      log,
	})

	lis := listener.NewStub(log) // TODO(fr-listener): actions/scaleset listener

	log.Info("firerunner starting",
		"provisioner", prov.Name(),
		"scaleSet", cfg.ScaleSetName,
		"url", cfg.URL,
		"maxRunners", cfg.MaxRunners,
		"vcpu", cfg.VCPU,
		"memMiB", cfg.MemMiB,
	)

	if err := lis.Run(ctx, sched.Reconcile); err != nil && ctx.Err() == nil {
		log.Error("listener stopped unexpectedly", "err", err)
		os.Exit(1)
	}

	log.Info("shutdown signalled, draining in-flight microVMs")
	sched.Drain()
	log.Info("firerunner stopped")
}
