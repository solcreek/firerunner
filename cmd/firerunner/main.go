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

	lis, err := listener.New(ctx, listener.Config{
		URL:         cfg.URL,
		Name:        cfg.ScaleSetName,
		RunnerGroup: cfg.RunnerGroup,
		Labels:      cfg.Labels,
		MaxRunners:  cfg.MaxRunners,
		MinRunners:  cfg.MinRunners,
		Token:       cfg.Token,
		App: scaleset.GitHubAppAuth{
			ClientID:       cfg.AppClientID,
			InstallationID: cfg.AppInstallID,
			PrivateKey:     privateKey,
		},
		Version: version,
		Logger:  log,
	}, owner)
	if err != nil {
		return err
	}
	defer func() { _ = lis.Close(context.WithoutCancel(ctx)) }()

	prov := provisioner.NewFirecracker(cfg.Firecracker, log)

	sched := scheduler.New(scheduler.Options{
		Max:         cfg.MaxRunners,
		Min:         cfg.MinRunners,
		Spec:        cfg.RunnerSpec(),
		Provisioner: prov,
		JIT:         lis.JIT(),
		Logger:      log,
	})

	log.Info("firerunner starting",
		"provisioner", prov.Name(),
		"scaleSet", cfg.ScaleSetName,
		"url", cfg.URL,
		"maxRunners", cfg.MaxRunners,
		"vcpu", cfg.VCPU,
		"memMiB", cfg.MemMiB,
	)

	// microVMs must live and die with the process, not with the per-message
	// context the scale-set listener passes in. That context is detached from
	// cancellation (listener uses context.WithoutCancel so in-flight job
	// handling survives shutdown), so binding a microVM to it would leak idle
	// warm runners on shutdown and hang Drain. Bind to the shutdown ctx instead.
	onDesired := func(_ context.Context, desired int) int {
		sched.Reconcile(ctx, desired)
		return sched.Running()
	}

	// Reclaim taps and job dirs orphaned by a previous unclean exit before we
	// start allocating new slots, so a crash-restart cycle can't exhaust them.
	prov.CleanupStale(ctx)

	if err := prov.SetupNetwork(ctx); err != nil {
		return fmt.Errorf("setup network: %w", err)
	}
	go prov.RefreshNetwork(ctx)

	runErr := lis.Run(ctx, onDesired)

	log.Info("shutdown signalled, draining in-flight microVMs")
	sched.Drain()
	log.Info("firerunner stopped")

	if runErr != nil && ctx.Err() == nil {
		return runErr
	}
	return nil
}
