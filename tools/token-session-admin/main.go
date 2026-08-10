package main

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
	"github.com/Mininglamp-OSS/octo-server/internal/tokenlifecycle"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/spf13/viper"
)

type adminCommandKind string

const (
	adminCommandAdvanceFloor adminCommandKind = "advance-floor"
	adminCommandMigrate      adminCommandKind = "migrate"
)

type adminCommand struct {
	kind              adminCommandKind
	configPath        string
	floor             auth.SessionMode
	observationMinGap time.Duration
	migration         auth.LegacyMigrationOptions
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	command, err := parseAdminCommand(args, time.Now)
	if err != nil {
		return err
	}
	cfg, err := loadAdminConfig(command.configPath)
	if err != nil {
		return err
	}
	client := octoredis.NewInstrumentedClient(cfg, func(options *rd.Options) {
		options.MaxRetries = 1
		options.PoolSize = 2
		options.DialTimeout = 2 * time.Second
		options.ReadTimeout = 2 * time.Second
		options.WriteTimeout = 2 * time.Second
		options.PoolTimeout = time.Second
	})
	defer client.Close()
	store := auth.NewRedisSessionStore(
		client,
		cfg.Cache.TokenCachePrefix,
		cfg.Cache.UIDTokenCachePrefix,
		cfg.Cache.TokenExpire,
	)

	switch command.kind {
	case adminCommandAdvanceFloor:
		if err := store.AdvanceRolloutControl(ctx, command.floor, command.observationMinGap); err != nil {
			return fmt.Errorf("advance session rollout floor: %w", err)
		}
		control, err := store.RolloutControl(ctx)
		if err != nil {
			return fmt.Errorf("read advanced session rollout floor: %w", err)
		}
		if err := json.NewEncoder(output).Encode(control); err != nil {
			return fmt.Errorf("encode rollout control: %w", err)
		}
		return nil
	case adminCommandMigrate:
		result, migrationErr := store.MigrateLegacySessions(ctx, command.migration)
		if err := json.NewEncoder(output).Encode(result); err != nil {
			return fmt.Errorf("encode migration result: %w", err)
		}
		if migrationErr != nil {
			return fmt.Errorf("migrate legacy token sessions: %w", migrationErr)
		}
		return nil
	default:
		return errors.New("unsupported token session admin command")
	}
}

func parseAdminCommand(args []string, now func() time.Time) (adminCommand, error) {
	if len(args) == 0 {
		return adminCommand{}, errors.New("usage: token-session-admin <advance-floor|migrate> [flags]")
	}
	if now == nil {
		now = time.Now
	}
	switch args[0] {
	case string(adminCommandAdvanceFloor):
		return parseAdvanceFloorCommand(args[1:])
	case string(adminCommandMigrate):
		return parseMigrateCommand(args[1:], now)
	default:
		return adminCommand{}, fmt.Errorf("unknown token session admin command %q", args[0])
	}
}

func parseAdvanceFloorCommand(args []string) (adminCommand, error) {
	flags := flag.NewFlagSet(string(adminCommandAdvanceFloor), flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "octo-server config file")
	target := flags.String("to", "", "next rollout floor")
	observationMinGap := flags.Duration("observation-min-gap", 0, "approved minimum duration between rollout observations")
	if err := flags.Parse(args); err != nil {
		return adminCommand{}, err
	}
	if flags.NArg() != 0 {
		return adminCommand{}, errors.New("advance-floor does not accept positional arguments")
	}
	if strings.TrimSpace(*configPath) == "" {
		return adminCommand{}, errors.New("advance-floor requires --config")
	}
	floor := auth.SessionMode(strings.TrimSpace(*target))
	switch floor {
	case auth.SessionModeV3Write, auth.SessionModeRevoke, auth.SessionModeBounded, auth.SessionModeEnforce:
	default:
		return adminCommand{}, errors.New("advance-floor --to must be one of v3-write, revoke, bounded, enforce")
	}
	if *observationMinGap < auth.MinimumRolloutObservationGap {
		return adminCommand{}, fmt.Errorf("advance-floor requires --observation-min-gap of at least %s", auth.MinimumRolloutObservationGap)
	}
	return adminCommand{
		kind:              adminCommandAdvanceFloor,
		configPath:        *configPath,
		floor:             floor,
		observationMinGap: *observationMinGap,
	}, nil
}

func parseMigrateCommand(args []string, now func() time.Time) (adminCommand, error) {
	flags := flag.NewFlagSet(string(adminCommandMigrate), flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "octo-server config file")
	campaignID := flags.String("campaign", "", "immutable migration campaign identifier")
	cutoffRaw := flags.String("cutoff", "", "absolute RFC3339 legacy deadline")
	finitePolicyRaw := flags.String("finite-policy", "", "finite legacy policy: natural or cap")
	batchSize := flags.Int64("batch-size", 0, "Redis SCAN count hint")
	qps := flags.Float64("qps", 0, "maximum token records processed per second")
	lease := flags.Duration("lease", 0, "single-owner migration lease")
	apply := flags.Bool("apply", false, "apply TTL shortening; omitted means dry-run")
	confirmElapsedCutoff := flags.Bool("confirm-elapsed-cutoff", false, "confirm immediate deletion when the immutable cutoff has elapsed")
	if err := flags.Parse(args); err != nil {
		return adminCommand{}, err
	}
	if flags.NArg() != 0 {
		return adminCommand{}, errors.New("migrate does not accept positional arguments")
	}
	if strings.TrimSpace(*configPath) == "" {
		return adminCommand{}, errors.New("migrate requires --config")
	}
	if strings.TrimSpace(*campaignID) == "" {
		return adminCommand{}, errors.New("migrate requires --campaign")
	}
	if strings.TrimSpace(*cutoffRaw) == "" {
		return adminCommand{}, errors.New("migrate requires --cutoff")
	}
	cutoff, err := time.Parse(time.RFC3339Nano, *cutoffRaw)
	if err != nil {
		return adminCommand{}, fmt.Errorf("invalid --cutoff %q: %w", *cutoffRaw, err)
	}
	finitePolicy := auth.LegacyFinitePolicy(strings.TrimSpace(*finitePolicyRaw))
	if finitePolicy != auth.LegacyFinitePolicyNatural && finitePolicy != auth.LegacyFinitePolicyCap {
		return adminCommand{}, errors.New("migrate --finite-policy must be natural or cap")
	}
	if *batchSize <= 0 || *batchSize > 10_000 {
		return adminCommand{}, errors.New("--batch-size must be between 1 and 10000")
	}
	if *qps <= 0 || *qps > 10_000 {
		return adminCommand{}, errors.New("--qps must be greater than zero and no more than 10000")
	}
	interval := time.Duration(float64(time.Second) / *qps)
	if interval <= 0 {
		return adminCommand{}, errors.New("--qps produces an invalid processing interval")
	}
	if *lease < 5*time.Second || *lease > 5*time.Minute {
		return adminCommand{}, errors.New("--lease must be between 5s and 5m")
	}
	if *apply && cutoff.UTC().UnixMilli() <= now().UTC().UnixMilli() && !*confirmElapsedCutoff {
		return adminCommand{}, errors.New("migrate --apply with elapsed --cutoff requires --confirm-elapsed-cutoff")
	}
	return adminCommand{
		kind:       adminCommandMigrate,
		configPath: *configPath,
		migration: auth.LegacyMigrationOptions{
			CampaignID:           strings.TrimSpace(*campaignID),
			CutoffAt:             cutoff.UTC(),
			FinitePolicy:         finitePolicy,
			BatchSize:            *batchSize,
			Interval:             interval,
			Apply:                *apply,
			ConfirmElapsedCutoff: *confirmElapsedCutoff,
			Lease:                *lease,
		},
	}, nil
}

func loadAdminConfig(path string) (*config.Config, error) {
	vp := viper.New()
	vp.SetConfigFile(path)
	if err := vp.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	vp.SetEnvPrefix("TS")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()
	tokenExpire, err := tokenlifecycle.ValidateTokenExpire(vp)
	if err != nil {
		return nil, err
	}
	cfg := config.New()
	cfg.ConfigureWithViper(vp)
	cfg.Cache.TokenExpire = tokenExpire
	return cfg, nil
}
