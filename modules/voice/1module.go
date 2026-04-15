package voice

import (
	"embed"
	"os"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"go.uber.org/zap"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		x := ctx.(*config.Context)
		cfg := NewVoiceConfigFromEnv()
		if err := cfg.Validate(); err != nil {
			lg := log.NewTLog("Voice")
			lg.Warn("voice module disabled: " + err.Error())
		}

		// ASR data collection (optional)
		var asrCleaner *ASRLogCleaner
		if logDir := os.Getenv("ASR_LOG_DIR"); logDir != "" {
			bufSize := 256
			if v := os.Getenv("ASR_LOG_BUFFER_SIZE"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					bufSize = n
				}
			}
			globalASRLogger = NewASRLogger(logDir, bufSize)

			if globalASRLogger != nil {
				retentionDays := 7
				if v := os.Getenv("ASR_LOG_RETENTION_DAYS"); v != "" {
					if n, err := strconv.Atoi(v); err == nil && n > 0 {
						retentionDays = n
					}
				}
				asrCleaner = NewASRLogCleaner(logDir, retentionDays)
				asrCleaner.StartAsync()

				lg := log.NewTLog("Voice")
				lg.Info("ASR logging enabled",
					zap.String("dir", logDir),
					zap.Int("retention_days", retentionDays),
					zap.Int("buffer_size", bufSize))
			}
		}

		api := New(x, cfg)
		api.asrLogger = globalASRLogger

		return register.Module{
			Name: "voice",
			SetupAPI: func() register.APIRouter {
				return api
			},
			SQLDir: register.NewSQLFS(sqlFS),
			Stop: func() error {
				if globalASRLogger != nil {
					globalASRLogger.Close()
				}
				if asrCleaner != nil {
					asrCleaner.Close()
				}
				return nil
			},
		}
	})
}
