package main

// `app session-rollout ...` — the operator surface for the token session
// rollout, folded into the server binary.
//
// It used to be two standalone commands under tools/, which the Dockerfile
// never built: the image ships only /home/app, so a private deployment was
// guaranteed to arrive without them. Nothing referenced them from CI either.
//
// Only three things are left here, because the advance predicate needs no human
// judgement and the reconciler runs it:
//
//	status          diagnosis — where the floor is and what is blocking it
//	migrate         the one real decision: cutoff and finite policy, i.e. how
//	                many people get logged out early
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
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const sessionRolloutCommand = "session-rollout"

// runSessionRolloutCommand handles `app session-rollout <sub> [flags]`. It is
// dispatched before flag.Parse in main so subcommand flags are not eaten by the
// server's own -config flag.
func runSessionRolloutCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: app session-rollout <status|observe|migrate|pause|resume|advance> [flags]")
	}
	sub := args[0]
	rest := args[1:]

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
	yes := flags.Bool("yes", false, "advance --force: confirm")
	expectWriters := flags.Int("expect-writers", 0, "advance: replica count the deployment intends to run")
	maxPerUID := flags.Int("max-per-uid", 0, "advance: bounded sessions per UID to carry into the floor record")
	if err := flags.Parse(rest); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s %s does not accept positional arguments", sessionRolloutCommand, sub)
	}

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
		instance = id[:16]
	}
	fmt.Fprintf(os.Stderr, "redis: %s  db=%d  instance=%s  token_ttl=%s\n",
		client.Options().Addr, client.Options().DB, instance, cfg.Cache.TokenExpire)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "status":
		return sessionRolloutStatus(ctx, store, out, *batchSize)
	case "observe":
		return sessionRolloutObserve(ctx, store, out, *batchSize, *qps)
	case "migrate":
		return sessionRolloutMigrate(ctx, store, out, sessionRolloutMigrateArgs{
			campaign:       *campaign,
			cutoffRaw:      *cutoffRaw,
			finitePolicy:   *finitePolicy,
			batchSize:      *batchSize,
			qps:            *qps,
			lease:          *lease,
			apply:          *apply,
			confirmElapsed: *confirmElapsed,
		})
	case "pause":
		if err := store.SetRolloutPaused(ctx, true); err != nil {
			return err
		}
		fmt.Fprintln(out, `{"paused":true}`)
		return nil
	case "resume":
		if err := store.SetRolloutPaused(ctx, false); err != nil {
			return err
		}
		fmt.Fprintln(out, `{"paused":false}`)
		return nil
	case "advance":
		return sessionRolloutAdvance(ctx, store, out, *force, *yes, *expectWriters, *maxPerUID, *batchSize)
	default:
		return fmt.Errorf("unknown %s subcommand %q", sessionRolloutCommand, sub)
	}
}

func loadSessionRolloutConfig(path string) (*config.Config, error) {
	vp := loadConfigFromFile(path)
	vp.SetEnvPrefix("TS")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()
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
	MaxPerUID   int                          `json:"max_per_uid,omitempty"`
	Paused      bool                         `json:"reconciler_paused"`
	Writers     []auth.WriterEntry           `json:"writers"`
	Tokens      *auth.SessionObservation     `json:"tokens,omitempty"`
	LastAdvance *auth.RolloutAdvanceRecord   `json:"last_advance,omitempty"`
	Next        *auth.RolloutAdvanceDecision `json:"next,omitempty"`
}

func sessionRolloutStatus(ctx context.Context, store *auth.RedisSessionStore, out io.Writer, batchSize int64) error {
	report := sessionRolloutStatusReport{Floor: "none"}
	control, err := store.RolloutControl(ctx)
	if err != nil {
		return err
	}
	if control != nil {
		report.Floor = string(control.ModeFloor)
		report.MaxPerUID = control.MaxPerUID
	}
	if paused, err := store.RolloutPaused(ctx); err == nil {
		report.Paused = paused
	}
	registry := auth.NewWriterRegistry(store.Client(), store.UIDTokenPrefix())
	if live, err := registry.Live(); err == nil {
		report.Writers = live
	}
	if observation, err := store.ObserveRateLimited(ctx, batchSize, 0); err == nil {
		report.Tokens = &observation
	}
	if record, err := store.LastRolloutAdvance(ctx); err == nil {
		report.LastAdvance = record
	}
	decision, err := store.EvaluateRolloutAdvance(ctx, auth.RolloutAdvanceInput{
		Registry:      registry,
		ScanBatchSize: batchSize,
	})
	if err == nil {
		report.Next = &decision
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func sessionRolloutObserve(ctx context.Context, store *auth.RedisSessionStore, out io.Writer, batchSize int64, qps float64) error {
	if qps <= 0 || qps > 10_000 {
		return errors.New("--qps must be greater than zero and no more than 10000")
	}
	stats, err := store.ObserveRateLimited(ctx, batchSize, time.Duration(float64(time.Second)/qps))
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
	qps            float64
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
	if args.qps <= 0 || args.qps > 10_000 {
		return errors.New("--qps must be greater than zero and no more than 10000")
	}
	if args.apply && cutoff.UTC().UnixMilli() <= time.Now().UTC().UnixMilli() && !args.confirmElapsed {
		return errors.New("migrate --apply with elapsed --cutoff requires --confirm-elapsed-cutoff")
	}
	result, migrateErr := store.MigrateLegacySessions(ctx, auth.LegacyMigrationOptions{
		CampaignID:           strings.TrimSpace(args.campaign),
		CutoffAt:             cutoff.UTC(),
		FinitePolicy:         policy,
		BatchSize:            args.batchSize,
		Interval:             time.Duration(float64(time.Second) / args.qps),
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

func sessionRolloutAdvance(
	ctx context.Context,
	store *auth.RedisSessionStore,
	out io.Writer,
	force, yes bool,
	expectWriters, maxPerUID int,
	batchSize int64,
) error {
	if !force || !yes {
		return errors.New("advance is the reconciler's job; use --force --yes only when the reconciler itself is broken")
	}
	registry := auth.NewWriterRegistry(store.Client(), store.UIDTokenPrefix())
	decision, err := store.EvaluateRolloutAdvance(ctx, auth.RolloutAdvanceInput{
		Registry:      registry,
		ExpectWriters: expectWriters,
		MaxPerUID:     maxPerUID,
		ScanBatchSize: batchSize,
	})
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
	if err := store.ForceAdvanceRollout(ctx, decision, registry); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(decision)
}
