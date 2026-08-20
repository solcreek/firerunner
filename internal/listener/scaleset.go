package listener

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/actions/scaleset"
	sslistener "github.com/actions/scaleset/listener"

	"github.com/solcreek/firerunner/internal/core"
)

// Config configures the scale-set listener.
type Config struct {
	URL         string
	Name        string
	RunnerGroup string
	Labels      []string
	MaxRunners  int
	MinRunners  int

	// Auth: a personal access token, or a GitHub App (preferred, per GitHub).
	Token string
	App   scaleset.GitHubAppAuth

	// SystemInfo metadata reported to GitHub.
	Version   string
	CommitSHA string

	Logger *slog.Logger
}

// ScaleSet is the production Listener. Constructing it registers an ephemeral
// runner scale set with GitHub; Close deregisters it.
type ScaleSet struct {
	cfg        Config
	log        *slog.Logger
	client     *scaleset.Client
	session    *scaleset.MessageSessionClient
	scaleSetID int
}

// New builds a scaleset client, registers the runner scale set, and opens a
// message session. Call Close to deregister.
func New(ctx context.Context, cfg Config, owner string) (*ScaleSet, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.RunnerGroup == "" {
		cfg.RunnerGroup = scaleset.DefaultRunnerGroup
	}

	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}

	groupID, err := runnerGroupID(ctx, client, cfg.RunnerGroup)
	if err != nil {
		return nil, err
	}

	desired := &scaleset.RunnerScaleSet{
		Name:          cfg.Name,
		RunnerGroupID: groupID,
		Labels:        buildLabels(cfg.Name, cfg.Labels),
		// Ephemeral golden images are rebuilt out of band; the runner agent must
		// not self-update (GitHub official guidance for image-based runners).
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}

	// Get-or-create: a scale set with this name may already exist from a prior
	// (possibly unclean) run. Reuse it so restarts are idempotent instead of
	// failing with "runner scale set already exists".
	existing, err := client.GetRunnerScaleSet(ctx, groupID, cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("look up runner scale set: %w", err)
	}
	var scaleSet *scaleset.RunnerScaleSet
	reused := existing != nil
	if reused {
		scaleSet, err = client.UpdateRunnerScaleSet(ctx, existing.ID, desired)
		if err != nil {
			return nil, fmt.Errorf("update runner scale set: %w", err)
		}
	} else {
		scaleSet, err = client.CreateRunnerScaleSet(ctx, desired)
		if err != nil {
			return nil, fmt.Errorf("create runner scale set: %w", err)
		}
	}
	client.SetSystemInfo(systemInfo(cfg, scaleSet.ID))

	session, err := openSessionWithRetry(ctx, client, scaleSet.ID, owner, cfg.Logger)
	if err != nil {
		if !reused {
			// Best-effort cleanup of the scale set we just created.
			_ = client.DeleteRunnerScaleSet(context.WithoutCancel(ctx), scaleSet.ID)
		}
		return nil, fmt.Errorf("open message session: %w", err)
	}

	cfg.Logger.Info("registered runner scale set",
		"name", cfg.Name, "scaleSetID", scaleSet.ID, "group", cfg.RunnerGroup, "reused", reused)

	return &ScaleSet{cfg: cfg, log: cfg.Logger, client: client, session: session, scaleSetID: scaleSet.ID}, nil
}

// openSessionWithRetry opens a message session, retrying while GitHub reports a
// session conflict. After an unclean shutdown (SIGKILL, OOM, power loss) the
// previous session stays active server-side until it times out (~1 min), so a
// restarting service would otherwise crash-loop. Retrying lets it self-heal.
// Any non-conflict error (e.g. bad auth) fails fast.
func openSessionWithRetry(ctx context.Context, client *scaleset.Client, scaleSetID int, owner string, log *slog.Logger) (*scaleset.MessageSessionClient, error) {
	const attempts = 12
	const delay = 10 * time.Second

	var err error
	for i := 0; i < attempts; i++ {
		var session *scaleset.MessageSessionClient
		session, err = client.MessageSessionClient(ctx, scaleSetID, owner)
		if err == nil {
			return session, nil
		}
		if !isSessionConflict(err) {
			return nil, err
		}
		log.Warn("scale set session still held by a previous run; waiting for it to expire",
			"attempt", i+1, "maxAttempts", attempts)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, err
}

// isSessionConflict reports whether err is GitHub's "scale set already has an
// active session" (HTTP 409) response. The library surfaces it only as a
// wrapped string, so we match on the conflict marker.
func isSessionConflict(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "conflict")
}

// JIT returns a JIT source bound to this scale set, satisfying
// scheduler.JITSource.
func (s *ScaleSet) JIT() *JITSource {
	return &JITSource{client: s.client, scaleSetID: s.scaleSetID, namePrefix: s.cfg.Name}
}

// Run implements Listener: long-poll GitHub and drive onDesired.
func (s *ScaleSet) Run(ctx context.Context, onDesired DesiredFunc) error {
	l, err := sslistener.New(s.session, sslistener.Config{
		ScaleSetID: s.scaleSetID,
		MaxRunners: s.cfg.MaxRunners,
		Logger:     s.log.WithGroup("scaleset"),
	})
	if err != nil {
		return fmt.Errorf("create scaleset listener: %w", err)
	}
	return l.Run(ctx, &scaler{onDesired: onDesired, minRunners: s.cfg.MinRunners, log: s.log})
}

// Close deregisters the scale set and closes the message session.
func (s *ScaleSet) Close(ctx context.Context) error {
	_ = s.session.Close(ctx)
	if err := s.client.DeleteRunnerScaleSet(ctx, s.scaleSetID); err != nil {
		return fmt.Errorf("delete runner scale set: %w", err)
	}
	s.log.Info("deregistered runner scale set", "scaleSetID", s.scaleSetID)
	return nil
}

// scaler adapts GitHub's scale-set callbacks onto firerunner's DesiredFunc.
// Because every microVM is ephemeral and self-terminating, job start/completion
// need no action beyond logging.
type scaler struct {
	onDesired  DesiredFunc
	minRunners int
	log        *slog.Logger
}

var _ sslistener.Scaler = (*scaler)(nil)

func (a *scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	desired := a.minRunners + count
	running := a.onDesired(ctx, desired)
	return running, nil
}

func (a *scaler) HandleJobStarted(_ context.Context, j *scaleset.JobStarted) error {
	a.log.Info("job started", "runner", j.RunnerName, "runnerRequestId", j.RunnerRequestID)
	return nil
}

func (a *scaler) HandleJobCompleted(_ context.Context, j *scaleset.JobCompleted) error {
	a.log.Info("job completed", "runner", j.RunnerName, "result", j.Result)
	return nil
}

// JITSource generates just-in-time runner registrations against the scale set.
type JITSource struct {
	client     *scaleset.Client
	scaleSetID int
	namePrefix string
}

// Generate implements scheduler.JITSource.
func (j *JITSource) Generate(ctx context.Context, _ core.RunnerSpec) (name, jitConfig string, err error) {
	name = j.namePrefix + "-" + randSuffix()
	cfg, err := j.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, j.scaleSetID)
	if err != nil {
		return "", "", fmt.Errorf("generate JIT config: %w", err)
	}
	return name, cfg.EncodedJITConfig, nil
}

// --- helpers ---

func newClient(cfg Config) (*scaleset.Client, error) {
	if cfg.App.Validate() == nil {
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: cfg.URL,
			GitHubAppAuth:   cfg.App,
			SystemInfo:      systemInfo(cfg, 0),
		})
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     cfg.URL,
		PersonalAccessToken: cfg.Token,
		SystemInfo:          systemInfo(cfg, 0),
	})
}

func runnerGroupID(ctx context.Context, client *scaleset.Client, group string) (int, error) {
	if group == scaleset.DefaultRunnerGroup {
		return 1, nil
	}
	rg, err := client.GetRunnerGroupByName(ctx, group)
	if err != nil {
		return 0, fmt.Errorf("get runner group %q: %w", group, err)
	}
	return rg.ID, nil
}

func buildLabels(name string, labels []string) []scaleset.Label {
	if len(labels) == 0 {
		return []scaleset.Label{{Name: name}}
	}
	out := make([]scaleset.Label, len(labels))
	for i, l := range labels {
		out[i] = scaleset.Label{Name: l}
	}
	return out
}

func systemInfo(cfg Config, scaleSetID int) scaleset.SystemInfo {
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	commit := cfg.CommitSHA
	if commit == "" {
		commit = "NA"
	}
	return scaleset.SystemInfo{
		System:     "firerunner",
		Subsystem:  "listener",
		Version:    version,
		CommitSHA:  commit,
		ScaleSetID: scaleSetID,
	}
}

func randSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
