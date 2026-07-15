// Command card-action-dlq inspects or manually replays one card-action DLQ
// event. It intentionally has no bulk/automatic replay mode.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/spf13/viper"
)

func main() {
	var configFile, action string
	var eventID int64
	flag.StringVar(&configFile, "config", "configs/tsdd.yaml", "octo-server config file")
	flag.StringVar(&action, "action", "depth", "depth or replay")
	flag.Int64Var(&eventID, "event-id", 0, "exact event_id to replay")
	flag.Parse()

	cfg, err := loadConfig(configFile)
	if err != nil {
		fatal(err)
	}
	client := octoredis.NewInstrumentedClient(cfg)
	defer client.Close()
	queue, err := cardactiondispatch.NewRedisQueue(client, cardactiondispatch.QueueConfig{
		Prefix: "card_action_dispatch", LiveTTL: cfg.Robot.MessageExpire, DLQRetention: 30 * 24 * time.Hour,
	})
	if err != nil {
		fatal(err)
	}

	switch action {
	case "depth":
		depths, err := queue.Depths()
		if err != nil {
			fatal(err)
		}
		fmt.Printf("ready=%d leased=%d dlq=%d\n", depths.Ready, depths.Leased, depths.DLQ)
	case "replay":
		if eventID <= 0 {
			fatal(fmt.Errorf("-event-id must be a positive exact event id"))
		}
		replayed, err := queue.ReplayDLQ(eventID, time.Now())
		if err != nil {
			fatal(err)
		}
		if !replayed {
			fatal(fmt.Errorf("event_id %d was not present in the DLQ", eventID))
		}
		fmt.Printf("replayed event_id=%d\n", eventID)
	default:
		fatal(fmt.Errorf("unsupported -action %q", action))
	}
}

func loadConfig(path string) (*config.Config, error) {
	vp := viper.New()
	vp.SetConfigFile(path)
	if err := vp.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	vp.SetEnvPrefix("TS")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()
	cfg := config.New()
	cfg.ConfigureWithViper(vp)
	return cfg, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "card-action-dlq:", err)
	os.Exit(1)
}
