package group

import (
	"os"
	"testing"
	"time"
)

func TestLoadReconcileConfig_Issue394(t *testing.T) {
	envs := []string{
		envReconcileEnabled, envReconcileInterval, envReconcileGrace, envReconcileBatchSize,
	}
	// 保存并清空相关 env，测试结束后恢复，避免污染其他用例。
	saved := map[string]string{}
	for _, e := range envs {
		saved[e] = os.Getenv(e)
		os.Unsetenv(e)
	}
	defer func() {
		for _, e := range envs {
			if v, ok := saved[e]; ok && v != "" {
				os.Setenv(e, v)
			} else {
				os.Unsetenv(e)
			}
		}
	}()

	tests := []struct {
		name string
		env  map[string]string
		want ReconcileConfig
	}{
		{
			name: "defaults when unset (disabled by default)",
			env:  map[string]string{},
			want: ReconcileConfig{
				Enabled:   false,
				Interval:  defaultReconcileInterval,
				Grace:     defaultReconcileGrace,
				BatchSize: defaultReconcileBatchSize,
			},
		},
		{
			name: "all valid overrides",
			env: map[string]string{
				envReconcileEnabled:   "true",
				envReconcileInterval:  "30s",
				envReconcileGrace:     "1m",
				envReconcileBatchSize: "50",
			},
			want: ReconcileConfig{
				Enabled:   true,
				Interval:  30 * time.Second,
				Grace:     time.Minute,
				BatchSize: 50,
			},
		},
		{
			name: "enabled=1 alias; invalid/negative fall back to defaults",
			env: map[string]string{
				envReconcileEnabled:   "1",
				envReconcileInterval:  "not-a-duration",
				envReconcileGrace:     "-5s",
				envReconcileBatchSize: "0",
			},
			want: ReconcileConfig{
				Enabled:   true,
				Interval:  defaultReconcileInterval,
				Grace:     defaultReconcileGrace,
				BatchSize: defaultReconcileBatchSize,
			},
		},
		{
			name: "batch size clamped to max",
			env: map[string]string{
				envReconcileBatchSize: "999999",
			},
			want: ReconcileConfig{
				Enabled:   false,
				Interval:  defaultReconcileInterval,
				Grace:     defaultReconcileGrace,
				BatchSize: maxReconcileBatchSize,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, e := range envs {
				os.Unsetenv(e)
			}
			for k, v := range tt.env {
				os.Setenv(k, v)
			}
			got := LoadReconcileConfig()
			if got != tt.want {
				t.Errorf("LoadReconcileConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
