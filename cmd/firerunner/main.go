// Command firerunner runs ephemeral Firecracker microVM GitHub Actions runners.
//
// It long-polls GitHub for queued jobs (via the official actions/scaleset
// control plane), and for each job boots a fresh, single-use microVM that
// registers a JIT/ephemeral runner, executes exactly one job, then self-
// destructs. See README.md for the architecture and security model.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/actions/scaleset"

	"github.com/solcreek/firerunner/internal/config"
	"github.com/solcreek/firerunner/internal/diag"
	"github.com/solcreek/firerunner/internal/listener"
	"github.com/solcreek/firerunner/internal/provisioner"
	"github.com/solcreek/firerunner/internal/scheduler"
)

// version is overridable at build time via -ldflags.
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "status", "doctor":
			if err := diagnose(args[0], args[1:]); err != nil {
				os.Exit(1)
			}
			return
		case "version", "--version", "-version":
			fmt.Println("firerunner", version)
			return
		case "help", "-h", "--help":
			usage(os.Stdout)
			return
		}
	}

	cfg, err := config.FromFlags(args)
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(2)
	}

	log := config.NewLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("firerunner failed", "err", err)
		os.Exit(1)
	}
}

// diagnose runs the read-only "status" and "doctor" subcommands. It parses
// config leniently (config.Parse) so it can report on a partial or broken
// deployment instead of refusing to start on a missing required field.
func diagnose(cmd string, args []string) error {
	asJSON := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" || a == "-json" {
			asJSON = true
			continue
		}
		rest = append(rest, a)
	}
	cfg, err := config.Parse(rest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return err
	}
	switch cmd {
	case "status":
		return diag.Status(cfg, version, os.Stdout, asJSON)
	case "doctor":
		return diag.Doctor(cfg, version, os.Stdout, asJSON)
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, `firerunner - ephemeral Firecracker microVM GitHub Actions runners

Usage:
  firerunner [flags]      run the daemon (default; see --help output of flags)
  firerunner status       show config, images, network and live microVMs
  firerunner doctor       run preflight health checks (exits non-zero on failure)
  firerunner version      print the version
  firerunner help         show this help

status and doctor read the same FR_* env / flags as the daemon, so run them
with the same environment (e.g. the systemd EnvironmentFile) as the service.
Pass --json to either for machine-readable output.
`)
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	privateKey, err := cfg.ResolvePrivateKey()
	if err != nil {
		return err
	}

	owner, err := os.Hostname()
	if err != nil || owner == "" {
		owner = "firerunner"
	}

	app := scaleset.GitHubAppAuth{
		ClientID:       cfg.AppClientID,
		InstallationID: cfg.AppInstallID,
		PrivateKey:     privateKey,
	}

	// One provisioner backs every tier: they share the host kernel, tool cache,
	// network and the MaxRunners slot budget. Each tier is its own GitHub scale
	// set with its own microVM shape and golden image; developers pick one with
	// runs-on: <tier name>.
	prov := provisioner.NewFirecracker(cfg.Firecracker, log)
	tiers := cfg.EffectiveTiers()

	// Register every tier's scale set up front so a failure aborts cleanly
	// before we start booting microVMs.
	var (
		listeners []*listener.ScaleSet
		scheds    []*scheduler.Scheduler
	)
	closeAll := func() {
		for _, l := range listeners {
			_ = l.Close(context.WithoutCancel(ctx))
		}
	}
	for _, t := range tiers {
		tlog := log.With("tier", t.Name)
		lis, err := listener.New(ctx, listener.Config{
			URL:         cfg.URL,
			Name:        t.Name,
			RunnerGroup: cfg.RunnerGroup,
			Labels:      t.Labels,
			MaxRunners:  t.Max,
			MinRunners:  t.Min,
			Token:       cfg.Token,
			App:         app,
			Version:     version,
			Logger:      tlog,
		}, owner)
		if err != nil {
			closeAll()
			return fmt.Errorf("tier %q: %w", t.Name, err)
		}
		listeners = append(listeners, lis)
		scheds = append(scheds, scheduler.New(scheduler.Options{
			Max:         t.Max,
			Min:         t.Min,
			Spec:        t.Spec(),
			Provisioner: prov,
			JIT:         lis.JIT(),
			Logger:      tlog,
		}))
	}
	defer closeAll()

	log.Info("firerunner starting",
		"provisioner", prov.Name(),
		"url", cfg.URL,
		"tiers", len(tiers),
		"maxRunners", cfg.MaxRunners,
	)
	for _, t := range tiers {
		log.Info("serving tier",
			"name", t.Name, "vcpu", t.VCPU, "memMiB", t.MemMiB,
			"golden", t.Golden, "min", t.Min, "max", t.Max)
	}

	// Reclaim taps and job dirs orphaned by a previous unclean exit before we
	// start allocating new slots, so a crash-restart cycle can't exhaust them.
	prov.CleanupStale(ctx)

	if err := prov.SetupNetwork(ctx); err != nil {
		return fmt.Errorf("setup network: %w", err)
	}
	go prov.RefreshNetwork(ctx)

	// Drive every tier's listener concurrently. runCtx lets the first fatal
	// listener error tear down the rest; on SIGTERM the parent ctx cancels it.
	// microVMs and the warm-pool top-up bind to runCtx too, so both a signal and
	// a fatal error unwind identically (listeners stop, Reconcile no-ops, and
	// Drain settles) — matching the single-tier lifecycle.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(listeners))
	for i := range listeners {
		lis, sched := listeners[i], scheds[i]
		onDesired := func(_ context.Context, desired int) int {
			sched.Reconcile(runCtx, desired)
			return sched.Running()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lis.Run(runCtx, onDesired); err != nil && runCtx.Err() == nil {
				errCh <- err
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errCh)

	log.Info("shutdown signalled, draining in-flight microVMs")
	for _, sched := range scheds {
		sched.Drain()
	}
	log.Info("firerunner stopped")

	if err := <-errCh; err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
