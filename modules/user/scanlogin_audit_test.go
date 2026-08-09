package user

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type capturedScanLoginWarning struct {
	message string
	fields  []zap.Field
}

type scanLoginAuditTestLogger struct {
	warnings []capturedScanLoginWarning
}

func (l *scanLoginAuditTestLogger) Warn(message string, fields ...zap.Field) {
	l.warnings = append(l.warnings, capturedScanLoginWarning{message: message, fields: fields})
}

func TestScanLoginRedemptionAuditWarnsOnlyWhenLoginIsIncomplete(t *testing.T) {
	tests := []struct {
		name         string
		completed    bool
		wantWarnings int
	}{
		{name: "consumed authorization without login completion", wantWarnings: 1},
		{name: "successful login", completed: true, wantWarnings: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := &scanLoginAuditTestLogger{}
			audit := newScanLoginRedemptionAudit(logger, "qr-uuid", "scanner-uid")
			if tt.completed {
				audit.MarkCompleted()
			}

			audit.WarnIfIncomplete()

			require.Len(t, logger.warnings, tt.wantWarnings)
			if tt.wantWarnings == 0 {
				return
			}
			warning := logger.warnings[0]
			assert.Equal(t, "扫码授权已消费但登录未完成", warning.message)
			require.Len(t, warning.fields, 2)
			assert.Equal(t, "uuid", warning.fields[0].Key)
			assert.Equal(t, "qr-uuid", warning.fields[0].String)
			assert.Equal(t, "scanner_uid", warning.fields[1].Key)
			assert.Equal(t, "scanner-uid", warning.fields[1].String)
		})
	}
}
