package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

const (
	defaultVersionSyncURL      = "https://im-data-1255521909.cos.ap-beijing.myqcloud.com/version-sync/version.json"
	defaultVersionSyncInterval = 5 * time.Minute
	versionSyncHTTPTimeout     = 30 * time.Second
)

type versionSyncJSON struct {
	UpdatedAt  string                            `json:"updated_at"`
	Components map[string]versionSyncComponentJSON `json:"components"`
}

type versionSyncComponentJSON struct {
	LatestVersion string          `json:"latest_version"`
	ReleaseMeta   json.RawMessage `json:"release_meta"`
}

type versionSyncer struct {
	log.Log
	db       *runtimeDB
	url      string
	interval time.Duration
	client   *http.Client
}

func newVersionSyncer(db *runtimeDB, cfgFile string) *versionSyncer {
	url, interval := loadVersionSyncConfig(cfgFile)
	return &versionSyncer{
		Log:      log.NewTLog("VersionSync"),
		db:       db,
		url:      url,
		interval: interval,
		client:   &http.Client{Timeout: versionSyncHTTPTimeout},
	}
}

func loadVersionSyncConfig(cfgFile string) (string, time.Duration) {
	url := defaultVersionSyncURL
	interval := defaultVersionSyncInterval
	if cfgFile == "" {
		return url, interval
	}
	v := viper.New()
	v.SetConfigFile(cfgFile)
	if err := v.ReadInConfig(); err != nil {
		return url, interval
	}
	if s := v.GetString("versionSync.url"); s != "" {
		url = s
	}
	if d := v.GetDuration("versionSync.interval"); d > 0 {
		interval = d
	}
	return url, interval
}

func (s *versionSyncer) run(ctx context.Context) {
	if s.url == "" {
		s.Info("version sync disabled (empty url)")
		return
	}
	s.Info("version sync started",
		zap.String("url", s.url),
		zap.Duration("interval", s.interval),
	)

	// Run immediately on start, then tick.
	s.syncOnce(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncOnce(ctx)
		}
	}
}

func (s *versionSyncer) syncOnce(ctx context.Context) {
	data, err := s.fetch(ctx)
	if err != nil {
		s.Warn("fetch version.json failed", zap.Error(err))
		return
	}

	var payload versionSyncJSON
	if err := json.Unmarshal(data, &payload); err != nil {
		s.Warn("parse version.json failed", zap.Error(err))
		return
	}

	if len(payload.Components) == 0 {
		s.Warn("version.json has no components, skip")
		return
	}

	var updated, skipped int
	for name, c := range payload.Components {
		if c.LatestVersion == "" {
			skipped++
			continue
		}
		releaseMeta := ""
		if len(c.ReleaseMeta) > 0 {
			releaseMeta = string(c.ReleaseMeta)
		}
		if err := s.db.upsertLatestVersion(name, c.LatestVersion, releaseMeta); err != nil {
			s.Error("upsert latest version failed",
				zap.String("component", name),
				zap.Error(err),
			)
			continue
		}
		updated++
	}

	s.Info("version sync completed",
		zap.String("updated_at", payload.UpdatedAt),
		zap.Int("updated", updated),
		zap.Int("skipped", skipped),
	)
}

func (s *versionSyncer) fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, errors.New("empty body")
	}
	return body, nil
}
