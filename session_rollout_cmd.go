package main

// `app session-rollout ...` — the operator surface for the token session
// rollout, folded into the server binary.
//
// It used to be two standalone commands under tools/, which the Dockerfile
// never built: the image carries the root-package binary plus assets/ and
// configs/ and nothing from tools/, so a private deployment was guaranteed to
// arrive without them. Nothing referenced them from CI either.
//
// The operator surface stays deliberately small because the advance predicate
// needs no human judgement and the reconciler runs it:
//
//	status          diagnosis — where the floor is and what is blocking it
//	migrate         the one real decision: cutoff and finite policy, i.e. how
//	                many people get logged out early
//	set-cap         versioned, audited update of the durable session cap
//	pause / resume  the escape hatch
//	advance --force fault channel for when the reconciler itself is broken
//
// The escape hatch can only STOP the reconciler, never wave the predicate
// through: bypassing a fail-closed gate defeats the point of having one. When
// something is stuck the move is to read blocked_by, not to go around it.
//
// Every subcommand prints the Redis endpoint and instance fingerprint it
// resolved before doing anything. On 2026-08-11 a misplaced config key
// (top-level redisAddr, which octo-lib ignores in favour of db.redisAddr) sent
// a tool to 127.0.0.1:6379, where it scanned local test leftovers and reported
// complete: true. Had that run carried --apply it would have rewritten the
// wrong keyspace, and nothing in the output would have said so.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"github.com/spf13/viper"
)

const sessionRolloutCommand = "session-rollout"

// sessionRolloutSubcommands is checked before the config is read and the Redis
// pool is built, so a typo costs a message rather than a dial timeout against
// whatever endpoint the config happens to name.
var sessionRolloutSubcommands = map[string]bool{
	"status":  true,
	"observe": true,
	"migrate": true,
	"set-cap": true,
	"pause":   true,
	"resume":  true,
	"advance": true,
}

// runSessionRolloutCommand handles `app session-rollout <sub> [flags]`. It is
// dispatched before flag.Parse in main so subcommand flags are not eaten by the
// server's own -config flag.
func runSessionRolloutCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: app session-rollout <status|observe|migrate|set-cap|pause|resume|advance> [flags]")
	}
	sub := args[0]
	rest := args[1:]
	if !sessionRolloutSubcommands[sub] {
		return fmt.Errorf("unknown %s subcommand %q", sessionRolloutCommand, sub)
	}

	flags := flag.NewFlagSet(sessionRolloutCommand+" "+sub, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "configs/tsdd.yaml", "octo-server config file")
	batchSize := flags.Int64("batch-size", 200, "Redis SCAN count hint (1-10000)")
	qps := flags.Float64("qps", 100, "maximum token records processed per second")
	campaign := flags.String("campaign", "", "immutable migration campaign identifier")
	cutoffRaw := flags.String("cutoff", "", "absolute RFC3339 legacy deadline")
	finitePolicy := flags.String("finite-policy", "", "finite legacy policy: natural or cap")
	lease := flags.Duration("lease", 30*time.Second, "single-owner migration lease")
	apply := flags.Bool("apply", false, "apply TTL shortening; omitted means dry-run")
	confirmElapsed := flags.Bool("confirm-elapsed-cutoff", false, "confirm immediate deletion when the cutoff has elapsed")
	force := flags.Bool("force", false, "advance: fault channel for when the reconciler is broken")
	yes := flags.Bool("yes", false, "confirm advance --force or set-cap")
	maxPerUID := flags.Int("max-per-uid", 0, "set-cap: durable per-UID session limit (1-10000)")
	expectWriters := flags.Int("expect-writers", 0, "advance: replica count the deployment intends to run")
	observeWindow := flags.Duration("observe-window", 90*time.Second,
		"advance: how long to watch the writer set hold still before the first v3 floor")
	if err := flags.Parse(rest); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s %s does not accept positional arguments", sessionRolloutCommand, sub)
	}
	if sub == "set-cap" {
		if !*yes {
			return errors.New("set-cap requires --yes")
		}
		if *maxPerUID <= 0 || *maxPerUID > auth.SessionMaxPerUIDLimit {
			return fmt.Errorf("set-cap --max-per-uid must be between 1 and %d", auth.SessionMaxPerUIDLimit)
		}
	}
	// Validated here, once, for every subcommand that scans. observe and migrate
	// each checked --qps and status did not, so `status --qps 0` ran an
	// unthrottled full-keyspace scan against production while the two commands
	// that say they are dangerous refused the same input. --batch-size advertised
	// a range in its own help text that nothing enforced.
	if err := validateScanRate(*qps, *batchSize); err != nil {
		return err
	}
	scanInterval := time.Duration(float64(time.Second) / *qps)

	cfg, err := loadSessionRolloutConfig(*configPath)
	if err != nil {
		return err
	}
	client := octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 2
		o.DialTimeout = 2 * time.Second
		o.ReadTimeout = 2 * time.Second
		o.WriteTimeout = 2 * time.Second
		o.PoolTimeout = time.Second
	})
	defer client.Close()
	store := auth.NewRedisSessionStore(
		client,
		cfg.Cache.TokenCachePrefix,
		cfg.Cache.UIDTokenCachePrefix,
		cfg.Cache.TokenExpire,
	)

	// Say where we are before doing anything. This is the whole fix for the
	// misconfiguration above: the endpoint is no longer implicit.
	instance := "<unreadable>"
	if id, idErr := store.RedisInstanceFingerprint(); idErr == nil {
		instance = shortFingerprint(id)
	}
	fmt.Fprintf(os.Stderr, "redis: %s  db=%d  instance=%s  token_ttl=%s\n",
		client.Options().Addr, client.Options().DB, instance, cfg.Cache.TokenExpire)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	controlDB := sessionRolloutDB(cfg)
	defer controlDB.Close()
	control := auth.NewRolloutControlStore(controlDB)
	state, err := control.Load(ctx)
	if err != nil {
		return err
	}
	if err := store.ApplyRolloutState(state, state.Floor); err != nil {
		return err
	}

	switch sub {
	case "status":
		return sessionRolloutStatus(ctx, store, control, state,
			out, *batchSize, scanInterval, *expectWriters)
	case "observe":
		return sessionRolloutObserve(ctx, store, out, *batchSize, scanInterval)
	case "migrate":
		return sessionRolloutMigrate(ctx, store, out, sessionRolloutMigrateArgs{
			campaign:       *campaign,
			cutoffRaw:      *cutoffRaw,
			finitePolicy:   *finitePolicy,
			batchSize:      *batchSize,
			scanInterval:   scanInterval,
			lease:          *lease,
			apply:          *apply,
			confirmElapsed: *confirmElapsed,
		})
	case "set-cap":
		return sessionRolloutSetCap(ctx, control, state, out, sessionRolloutSetCapArgs{
			maxPerUID: *maxPerUID,
			yes:       *yes,
		})
	case "pause":
		if err := control.SetPaused(ctx, true); err != nil {
			return err
		}
		fmt.Fprintln(out, `{"paused":true}`)
		return nil
	case "resume":
		if err := control.SetPaused(ctx, false); err != nil {
			return err
		}
		fmt.Fprintln(out, `{"paused":false}`)
		return nil
	case "advance":
		return sessionRolloutAdvance(ctx, store, control, state, out, sessionRolloutAdvanceArgs{
			force:         *force,
			yes:           *yes,
			expectWriters: *expectWriters,
			batchSize:     *batchSize,
			scanInterval:  scanInterval,
			observeWindow: *observeWindow,
		})
	default:
		// Unreachable: sessionRolloutSubcommands gates this above. Kept so
		// adding a name to that map without a case here fails loudly.
		return fmt.Errorf("unhandled %s subcommand %q", sessionRolloutCommand, sub)
	}
}

type rolloutCapSetter interface {
	SetMaxPerUID(context.Context, auth.RolloutState, int, string) (auth.RolloutState, error)
}

type sessionRolloutSetCapArgs struct {
	maxPerUID int
	yes       bool
}

type sessionRolloutSetCapReport struct {
	Floor         string `json:"floor"`
	FromMaxPerUID int    `json:"from_max_per_uid"`
	ToMaxPerUID   int    `json:"to_max_per_uid"`
	Version       int64  `json:"version"`
}

func sessionRolloutSetCap(
	ctx context.Context,
	control rolloutCapSetter,
	current auth.RolloutState,
	out io.Writer,
	args sessionRolloutSetCapArgs,
) error {
	if !args.yes {
		return errors.New("set-cap requires --yes")
	}
	if args.maxPerUID <= 0 || args.maxPerUID > auth.SessionMaxPerUIDLimit {
		return fmt.Errorf("set-cap --max-per-uid must be between 1 and %d", auth.SessionMaxPerUIDLimit)
	}
	if control == nil {
		return errors.New("set-cap requires MySQL rollout control")
	}
	next, err := control.SetMaxPerUID(ctx, current, args.maxPerUID, "operator")
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(sessionRolloutSetCapReport{
		Floor:         string(next.Floor),
		FromMaxPerUID: current.MaxPerUID,
		ToMaxPerUID:   next.MaxPerUID,
		Version:       next.Version,
	})
}

// validateScanRate bounds both throttle knobs. The ceiling on --qps is not
// arbitrary: this scan runs against the shared session pool, and the flag's job
// is to keep an operator from saturating it by hand.
func validateScanRate(qps float64, batchSize int64) error {
	if qps <= 0 || qps > 10_000 {
		return errors.New("--qps must be greater than zero and no more than 10000")
	}
	if batchSize <= 0 || batchSize > 10_000 {
		return errors.New("--batch-size must be between 1 and 10000")
	}
	return nil
}

// sessionRolloutDB opens the control connection lazily: dbr does not dial
// until the first query, so an unreachable MySQL surfaces as a failed stamp
// with a real error rather than as a CLI that cannot start.
func sessionRolloutDB(cfg *config.Config) *dbr.Session {
	return db.NewMySQL(
		cfg.DB.MySQLAddr,
		cfg.DB.MySQLMaxOpenConns,
		cfg.DB.MySQLMaxIdleConns,
		cfg.DB.MySQLConnMaxLifetime,
	)
}

// operatorConfigViper resolves an operator subcommand's config file plus the
// same TS_* environment overrides the server applies.
//
// Shared by `app session-rollout` and `app cutover` so the rules for how a
// config path and its env overrides are resolved have one definition: two
// copies means a change (a new override, a required post-load validation)
// silently lands on one operator command and not the other.
//
// It returns an error where the server's own loader panics: an operator typo in
// -config should be a one-line message, not a stack trace.
func operatorConfigViper(path string, notice io.Writer) (*viper.Viper, error) {
	vp := viper.New()
	vp.SetConfigFile(path)
	if err := vp.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	// The server prints this on startup and the operator commands inherited it
	// until the loader was factored out. It belongs on stderr, not stdout:
	// `session-rollout status` emits JSON that runbooks pipe into jq.
	fmt.Fprintln(notice, "config:", vp.ConfigFileUsed())
	vp.SetEnvPrefix("TS")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()
	return vp, nil
}

func loadSessionRolloutConfig(path string) (*config.Config, error) {
	vp, err := operatorConfigViper(path, os.Stderr)
	if err != nil {
		return nil, err
	}
	tokenExpire, err := validateTokenExpireConfig(vp)
	if err != nil {
		return nil, err
	}
	cfg := config.New()
	cfg.ConfigureWithViper(vp)
	cfg.Cache.TokenExpire = tokenExpire
	return cfg, nil
}

type sessionRolloutStatusReport struct {
	Floor       string                       `json:"floor"`
	Version     int64                        `json:"version"`
	MaxPerUID   int                          `json:"max_per_uid,omitempty"`
	Paused      bool                         `json:"reconciler_paused"`
	Writers     []auth.WriterEntry           `json:"writers"`
	Tokens      *auth.SessionObservation     `json:"tokens,omitempty"`
	LastAdvance *auth.RolloutAdvanceRecord   `json:"last_advance,omitempty"`
	Next        *auth.RolloutAdvanceDecision `json:"next,omitempty"`
	// NotDeterminableHere lists blockers this one-shot command cannot evaluate,
	// moved out of next.blocked_by so they are not read as real obstacles. The
	// convergence window is a statement about a set holding still over a lease
	// TTL, which a single invocation has no way to have observed — reporting it
	// alongside genuine blockers made status disagree with the reconciler it is
	// meant to diagnose.
	NotDeterminableHere []string `json:"not_determinable_here,omitempty"`
}

func sessionRolloutStatus(
	ctx context.Context,
	store *auth.RedisSessionStore,
	control *auth.RolloutControlStore,
	state auth.RolloutState,
	out io.Writer,
	batchSize int64,
	interval time.Duration,
	expectWriters int,
) error {
	report := sessionRolloutStatusReport{
		Floor: string(state.Floor), Version: state.Version, MaxPerUID: state.MaxPerUID, Paused: state.Paused,
	}
	registry := auth.NewWriterRegistry(store.Client(), store.UIDTokenPrefix())
	live, err := registry.Live()
	if err != nil {
		return fmt.Errorf("status writer registry: %w", err)
	}
	report.Writers = live
	// One scan, reused. This used to scan here and again inside the predicate,
	// and ignore its own --qps while doing it.
	//
	// The predicate gets the same inputs the server's reconciler passes, or the
	// blocker list here describes a different question than the loop an operator
	// is trying to diagnose — and the runbook makes blocked_by the starting point
	// for everything.
	decisionInput := auth.RolloutAdvanceInput{
		State:         state,
		ScanBatchSize: batchSize,
		ScanInterval:  interval,
		ExpectWriters: expectWriters,
	}
	record, err := control.LastAdvance(ctx)
	if err != nil {
		return fmt.Errorf("status last rollout advance: %w", err)
	}
	report.LastAdvance = record
	decisionInput.Registry = registry
	decision, err := store.EvaluateRolloutAdvance(ctx, decisionInput)
	if err != nil {
		return fmt.Errorf("status evaluate rollout advance: %w", err)
	}
	report.NotDeterminableHere, decision.BlockedBy = auth.SplitUndeterminableBlockers(decision.BlockedBy)
	decision.Allowed = len(decision.BlockedBy) == 0 && len(report.NotDeterminableHere) == 0
	report.Next = &decision
	report.Tokens = decision.Observation
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func sessionRolloutObserve(ctx context.Context, store *auth.RedisSessionStore, out io.Writer, batchSize int64, interval time.Duration) error {
	stats, err := store.ObserveRateLimited(ctx, batchSize, interval)
	if encodeErr := json.NewEncoder(out).Encode(stats); encodeErr != nil {
		return encodeErr
	}
	return err
}

type sessionRolloutMigrateArgs struct {
	campaign       string
	cutoffRaw      string
	finitePolicy   string
	batchSize      int64
	scanInterval   time.Duration
	lease          time.Duration
	apply          bool
	confirmElapsed bool
}

func sessionRolloutMigrate(ctx context.Context, store *auth.RedisSessionStore, out io.Writer, args sessionRolloutMigrateArgs) error {
	if strings.TrimSpace(args.campaign) == "" {
		return errors.New("migrate requires --campaign")
	}
	if strings.TrimSpace(args.cutoffRaw) == "" {
		return errors.New("migrate requires --cutoff")
	}
	cutoff, err := time.Parse(time.RFC3339Nano, args.cutoffRaw)
	if err != nil {
		return fmt.Errorf("invalid --cutoff %q: %w", args.cutoffRaw, err)
	}
	policy := auth.LegacyFinitePolicy(strings.TrimSpace(args.finitePolicy))
	if policy != auth.LegacyFinitePolicyNatural && policy != auth.LegacyFinitePolicyCap {
		return errors.New("migrate --finite-policy must be natural or cap")
	}
	if args.apply && cutoff.UTC().UnixMilli() <= time.Now().UTC().UnixMilli() && !args.confirmElapsed {
		return errors.New("migrate --apply with elapsed --cutoff requires --confirm-elapsed-cutoff")
	}
	result, migrateErr := store.MigrateLegacySessions(ctx, auth.LegacyMigrationOptions{
		CampaignID:           strings.TrimSpace(args.campaign),
		CutoffAt:             cutoff.UTC(),
		FinitePolicy:         policy,
		BatchSize:            args.batchSize,
		Interval:             args.scanInterval,
		Apply:                args.apply,
		ConfirmElapsedCutoff: args.confirmElapsed,
		Lease:                args.lease,
	})
	if err := json.NewEncoder(out).Encode(result); err != nil {
		return err
	}
	if migrateErr != nil {
		return fmt.Errorf("migrate legacy token sessions: %w", migrateErr)
	}
	return nil
}

type sessionRolloutAdvanceArgs struct {
	force         bool
	yes           bool
	expectWriters int
	batchSize     int64
	scanInterval  time.Duration
	// observeWindow bounds how long to watch the writer set settle. Zero means
	// evaluate once and report — used by tests and by anyone who wants the
	// current verdict without waiting.
	observeWindow time.Duration
}

// sessionRolloutObserveUntilConverged re-evaluates until the decision stops
// being blocked purely on the convergence window, or the window budget runs out.
//
// Only that one blocker is worth waiting on: it is a statement about elapsed
// time and nothing else, so re-asking is what resolves it. Every other blocker
// describes the state of the fleet or the keyspace, which will not change
// because this process asked twice — waiting on those would just be a slower
// refusal.
func sessionRolloutObserveUntilConverged(
	ctx context.Context,
	store *auth.RedisSessionStore,
	input auth.RolloutAdvanceInput,
	window time.Duration,
	out io.Writer,
) (auth.RolloutAdvanceDecision, error) {
	deadline := time.Now().Add(window)
	for attempt := 0; ; attempt++ {
		decision, err := store.EvaluateRolloutAdvance(ctx, input)
		if err != nil {
			return decision, err
		}
		if decision.Allowed || !decision.BlockedOnlyOnConvergence() || !time.Now().Before(deadline) {
			return decision, nil
		}
		if attempt == 0 {
			fmt.Fprintf(os.Stderr, "waiting up to %s for the writer set to hold still: %s\n",
				window, decision.BlockedSummary())
		}
		select {
		case <-ctx.Done():
			return decision, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func sessionRolloutAdvance(
	ctx context.Context,
	store *auth.RedisSessionStore,
	control *auth.RolloutControlStore,
	state auth.RolloutState,
	out io.Writer,
	args sessionRolloutAdvanceArgs,
) error {
	if !args.force || !args.yes {
		return errors.New("advance is the reconciler's job; use --force --yes only when the reconciler itself is broken")
	}
	registry := auth.NewWriterRegistry(store.Client(), store.UIDTokenPrefix())
	input := auth.RolloutAdvanceInput{
		State:         state,
		Registry:      registry,
		ExpectWriters: args.expectWriters,
		ScanBatchSize: args.batchSize,
		// Throttled like every other scan. This used to hardcode 1ms — a rate
		// the --qps ceiling does not permit — so the one subcommand that runs
		// unattended-grade work on an operator's say-so was also the one that
		// ignored the throttle.
		ScanInterval: args.scanInterval,
		// The first v3 floor requires the writer set to hold still for a lease
		// TTL, which a single evaluation cannot establish. Rather than making
		// break-glass structurally impossible for that one transition, watch for
		// real: evaluate repeatedly and let the window mature.
		Convergence: auth.NewWriterConvergence(),
	}
	decision, err := sessionRolloutObserveUntilConverged(ctx, store, input, args.observeWindow, out)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		// --force bypasses the reconciler, never the predicate. Waving a
		// fail-closed gate through by hand removes the only reason it exists.
		if encodeErr := json.NewEncoder(out).Encode(decision); encodeErr != nil {
			return encodeErr
		}
		return fmt.Errorf("refusing to advance to %s: %s", decision.Target, decision.BlockedSummary())
	}
	if err := store.ForceAdvanceRollout(ctx, decision, control); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(decision)
}
