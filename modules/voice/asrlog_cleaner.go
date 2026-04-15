package voice

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// ASRLogCleaner periodically removes old date directories
type ASRLogCleaner struct {
	baseDir       string
	retentionDays int
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	log.Log
}

// NewASRLogCleaner creates a cleaner
func NewASRLogCleaner(baseDir string, retentionDays int) *ASRLogCleaner {
	ctx, cancel := context.WithCancel(context.Background())
	return &ASRLogCleaner{
		baseDir:       baseDir,
		retentionDays: retentionDays,
		ctx:           ctx,
		cancel:        cancel,
		Log:           log.NewTLog("ASRLogCleaner"),
	}
}

// StartAsync starts the cleaner in a background goroutine.
// Encapsulates wg.Add(1) + go start() to avoid caller forgetting wg.Add.
func (c *ASRLogCleaner) StartAsync() {
	c.wg.Add(1)
	go c.start()
}

func (c *ASRLogCleaner) start() {
	defer c.wg.Done()

	c.clean() // run once at startup
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.clean()
		case <-c.ctx.Done():
			return
		}
	}
}

// Close stops the cleaner gracefully
func (c *ASRLogCleaner) Close() {
	c.cancel()
	c.wg.Wait()
}

func (c *ASRLogCleaner) clean() {
	entries, err := os.ReadDir(c.baseDir)
	if err != nil {
		if !os.IsNotExist(err) {
			c.Error("read asr log dir failed", zap.Error(err))
		}
		return
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -c.retentionDays)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t, err := time.Parse("2006-01-02", entry.Name())
		if err != nil {
			continue // skip non-date directories
		}
		if t.Before(cutoff) {
			dir := filepath.Join(c.baseDir, entry.Name())
			if err := os.RemoveAll(dir); err != nil {
				c.Error("remove old asr log dir failed",
					zap.Error(err), zap.String("dir", dir))
			} else {
				c.Info("removed old asr log dir", zap.String("dir", dir))
			}
		}
	}
}
