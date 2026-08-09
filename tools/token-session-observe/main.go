package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "configs/tsdd.yaml", "octo-server config file")
	batchSize := flag.Int64("batch-size", 200, "Redis SCAN count hint (1-10000)")
	qps := flag.Float64("qps", 100, "maximum token records read per second")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store := auth.NewRedisSessionStore(
		client,
		cfg.Cache.TokenCachePrefix,
		cfg.Cache.UIDTokenCachePrefix,
		cfg.Cache.TokenExpire,
	)
	if *qps <= 0 || *qps > 10_000 {
		return fmt.Errorf("qps must be greater than zero and no more than 10000")
	}
	interval := time.Duration(float64(time.Second) / *qps)
	stats, err := store.ObserveRateLimited(ctx, *batchSize, interval)
	if err != nil {
		return fmt.Errorf("observe token sessions: %w", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(stats); err != nil {
		return fmt.Errorf("encode aggregate observation: %w", err)
	}
	return nil
}

func loadConfig(path string) (*config.Config, error) {
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
