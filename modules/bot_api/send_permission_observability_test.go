package bot_api

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/reqid"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type permissionLogEntry struct {
	level  string
	msg    string
	fields map[string]interface{}
}

type permissionRecordingLog struct {
	entries []permissionLogEntry
}

var _ log.Log = (*permissionRecordingLog)(nil)

func (l *permissionRecordingLog) Info(msg string, fields ...zap.Field) {
	l.record("info", msg, fields)
}

func (l *permissionRecordingLog) Debug(msg string, fields ...zap.Field) {
	l.record("debug", msg, fields)
}

func (l *permissionRecordingLog) Error(msg string, fields ...zap.Field) {
	l.record("error", msg, fields)
}

func (l *permissionRecordingLog) Warn(msg string, fields ...zap.Field) {
	l.record("warn", msg, fields)
}

func (l *permissionRecordingLog) record(level, msg string, fields []zap.Field) {
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range fields {
		field.AddTo(encoder)
	}
	l.entries = append(l.entries, permissionLogEntry{
		level:  level,
		msg:    msg,
		fields: encoder.Fields,
	})
}

func newPermissionTestContext(t *testing.T, traceID string) *wkhttp.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(recorder)
	gc.Request = httptest.NewRequest("POST", "/v1/bot/sendMessage", nil)
	gc.Set(reqid.GinKey, traceID)
	return &wkhttp.Context{Context: gc}
}

func TestIsGroupDisbandedPreservesLookupErrorIdentity(t *testing.T) {
	lookupErr := fmt.Errorf("group row lookup: %w", dbr.ErrNotFound)
	ba := &BotAPI{
		groupStatusQueryOverride: func(string) (int, error) {
			return 0, lookupErr
		},
	}

	disbanded, err := ba.isGroupDisbanded("sensitive-group")
	require.False(t, disbanded)
	require.ErrorIs(t, err, dbr.ErrNotFound)
}

func TestCheckSendPermissionClassifiesGroupStatusFailures(t *testing.T) {
	tests := []struct {
		name         string
		channelID    string
		channelType  uint8
		lookupErr    error
		wantErr      error
		wantReason   sendPermissionFailureReason
		wantLogLevel string
		wantQueryArg string
	}{
		{
			name:         "missing group",
			channelID:    "sensitive-group",
			channelType:  common.ChannelTypeGroup.Uint8(),
			lookupErr:    dbr.ErrNotFound,
			wantErr:      errBotSendPermGroupNotFound,
			wantReason:   sendPermissionReasonNotFound,
			wantLogLevel: "warn",
			wantQueryArg: "sensitive-group",
		},
		{
			name:         "missing thread parent group",
			channelID:    "sensitive-parent____sensitive-thread",
			channelType:  common.ChannelTypeCommunityTopic.Uint8(),
			lookupErr:    dbr.ErrNotFound,
			wantErr:      errBotSendPermGroupNotFound,
			wantReason:   sendPermissionReasonNotFound,
			wantLogLevel: "warn",
			wantQueryArg: "sensitive-parent",
		},
		{
			name:         "database error",
			channelID:    "sensitive-group",
			channelType:  common.ChannelTypeGroup.Uint8(),
			lookupErr:    errors.New("database unavailable"),
			wantErr:      errBotSendPermCheckFailed,
			wantReason:   sendPermissionReasonQueryError,
			wantLogLevel: "error",
			wantQueryArg: "sensitive-group",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			metrics := newBotSendPermissionMetrics(registry)
			logger := &permissionRecordingLog{}
			var queryArg string
			ba := &BotAPI{
				groupStatusQueryOverride: func(groupNo string) (int, error) {
					queryArg = groupNo
					return 0, tt.lookupErr
				},
				sendPermissionMetrics: metrics,
				Log:                   logger,
			}
			ctx := newPermissionTestContext(t, "trace-safe")

			err := ba.checkSendPermission(ctx, BotKindUser, "sensitive-bot", tt.channelID, tt.channelType, false)
			require.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, tt.wantQueryArg, queryArg)
			assert.Equal(t, float64(1), testutil.ToFloat64(metrics.failures.WithLabelValues(
				string(sendPermissionStageGroupStatus), string(tt.wantReason),
			)))

			require.Len(t, logger.entries, 1)
			entry := logger.entries[0]
			assert.Equal(t, tt.wantLogLevel, entry.level)
			assert.Equal(t, "trace-safe", entry.fields["trace_id"])
			assert.Equal(t, string(sendPermissionStageGroupStatus), entry.fields["permission_stage"])
			assert.Equal(t, string(tt.wantReason), entry.fields["failure_reason"])
			assert.Equal(t, BotKindUser, entry.fields["bot_kind"])
			assert.NotContains(t, entry.fields, "robot_id")
			assert.NotContains(t, entry.fields, "channel_id")
			serialized := fmt.Sprint(entry.fields)
			assert.NotContains(t, serialized, "sensitive-bot")
			assert.NotContains(t, serialized, "sensitive-group")
			assert.NotContains(t, serialized, "sensitive-parent")
			assert.NotContains(t, serialized, "sensitive-thread")
		})
	}
}

func TestCheckSendPermissionObservesContextAndBotKindFailures(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*wkhttp.Context, *BotAPI)
		botKind    string
		wantStage  sendPermissionFailureStage
		wantReason sendPermissionFailureReason
	}{
		{
			name: "space app missing context space id",
			prepare: func(c *wkhttp.Context, ba *BotAPI) {
				c.Set(CtxKeyAppBotScope, "space")
				ba.friendCheckOverride = func(string, string) (bool, error) { return true, nil }
			},
			botKind:    BotKindApp,
			wantStage:  sendPermissionStageSpaceContext,
			wantReason: sendPermissionReasonMissingContext,
		},
		{
			name: "space app membership query error",
			prepare: func(c *wkhttp.Context, ba *BotAPI) {
				c.Set(CtxKeyAppBotScope, "space")
				c.Set(CtxKeyAppBotSpaceID, "sensitive-space")
				ba.friendCheckOverride = func(string, string) (bool, error) { return true, nil }
				ba.spaceMemberQueryOverride = func(string, string) (bool, error) {
					return false, errors.New("space membership store unavailable")
				}
			},
			botKind:    BotKindApp,
			wantStage:  sendPermissionStageSpaceMember,
			wantReason: sendPermissionReasonQueryError,
		},
		{
			name:       "unknown bot kind",
			prepare:    func(*wkhttp.Context, *BotAPI) {},
			botKind:    "sensitive-unknown-kind",
			wantStage:  sendPermissionStageBotKind,
			wantReason: sendPermissionReasonUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			metrics := newBotSendPermissionMetrics(registry)
			logger := &permissionRecordingLog{}
			ba := &BotAPI{sendPermissionMetrics: metrics, Log: logger}
			ctx := newPermissionTestContext(t, "trace-safe")
			tt.prepare(ctx, ba)

			err := ba.checkSendPermission(ctx, tt.botKind, "sensitive-bot", "sensitive-target", common.ChannelTypePerson.Uint8(), false)
			require.ErrorIs(t, err, errBotSendPermCheckFailed)
			assert.Equal(t, float64(1), testutil.ToFloat64(metrics.failures.WithLabelValues(
				string(tt.wantStage), string(tt.wantReason),
			)))
			require.Len(t, logger.entries, 1)
			entry := logger.entries[0]
			assert.Equal(t, "trace-safe", entry.fields["trace_id"])
			assert.Equal(t, string(tt.wantStage), entry.fields["permission_stage"])
			assert.Equal(t, string(tt.wantReason), entry.fields["failure_reason"])
			assert.NotContains(t, fmt.Sprint(entry.fields), "sensitive-")
		})
	}
}

func TestCheckSendPermissionClassifiesDMRelationshipResults(t *testing.T) {
	tests := []struct {
		name       string
		botKind    string
		friend     bool
		friendErr  error
		wantErr    error
		wantMetric bool
	}{
		{
			name:    "user bot no friend remains not_friend",
			botKind: BotKindUser,
			wantErr: errBotSendPermNotFriend,
		},
		{
			name:    "app bot no relationship remains conversation_not_started",
			botKind: BotKindApp,
			wantErr: errBotSendPermConvNotStarted,
		},
		{
			name:       "user bot friend query error remains internal",
			botKind:    BotKindUser,
			friendErr:  errors.New("friend store unavailable"),
			wantErr:    errBotSendPermCheckFailed,
			wantMetric: true,
		},
		{
			name:       "app bot friend query error remains internal",
			botKind:    BotKindApp,
			friendErr:  errors.New("friend store unavailable"),
			wantErr:    errBotSendPermCheckFailed,
			wantMetric: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			metrics := newBotSendPermissionMetrics(registry)
			logger := &permissionRecordingLog{}
			ba := &BotAPI{
				friendCheckOverride: func(string, string) (bool, error) {
					return tt.friend, tt.friendErr
				},
				sendPermissionMetrics: metrics,
				Log:                   logger,
			}
			ctx := newPermissionTestContext(t, "trace-safe")

			err := ba.checkSendPermission(ctx, tt.botKind, "sensitive-bot", "sensitive-user", common.ChannelTypePerson.Uint8(), false)
			require.ErrorIs(t, err, tt.wantErr)
			gotMetric := testutil.ToFloat64(metrics.failures.WithLabelValues(
				string(sendPermissionStageFriend), string(sendPermissionReasonQueryError),
			))
			if tt.wantMetric {
				assert.Equal(t, float64(1), gotMetric)
				require.Len(t, logger.entries, 1)
				entry := logger.entries[0]
				assert.Equal(t, string(sendPermissionStageFriend), entry.fields["permission_stage"])
				assert.Equal(t, string(sendPermissionReasonQueryError), entry.fields["failure_reason"])
				assert.NotContains(t, fmt.Sprint(entry.fields), "sensitive-")
				return
			}
			assert.Zero(t, gotMetric)
			assert.Empty(t, logger.entries)
		})
	}
}

func TestCheckSendPermissionClassifiesGroupMembershipResults(t *testing.T) {
	tests := []struct {
		name       string
		count      int
		queryErr   error
		wantErr    error
		wantMetric bool
	}{
		{
			name:    "zero active memberships remains not_group_member",
			wantErr: errBotSendPermNotGroupMember,
		},
		{
			name:       "membership query error remains internal",
			queryErr:   errors.New("group membership store unavailable"),
			wantErr:    errBotSendPermCheckFailed,
			wantMetric: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			metrics := newBotSendPermissionMetrics(registry)
			logger := &permissionRecordingLog{}
			ba := &BotAPI{
				groupStatusQueryOverride: func(string) (int, error) { return 0, nil },
				groupMemberCountOverride: func(string, string) (int, error) {
					return tt.count, tt.queryErr
				},
				sendPermissionMetrics: metrics,
				Log:                   logger,
			}
			ctx := newPermissionTestContext(t, "trace-safe")

			err := ba.checkSendPermission(ctx, BotKindUser, "sensitive-bot", "sensitive-group", common.ChannelTypeGroup.Uint8(), false)
			require.ErrorIs(t, err, tt.wantErr)
			gotMetric := testutil.ToFloat64(metrics.failures.WithLabelValues(
				string(sendPermissionStageGroupMember), string(sendPermissionReasonQueryError),
			))
			if tt.wantMetric {
				assert.Equal(t, float64(1), gotMetric)
				require.Len(t, logger.entries, 1)
				entry := logger.entries[0]
				assert.Equal(t, string(sendPermissionStageGroupMember), entry.fields["permission_stage"])
				assert.NotContains(t, fmt.Sprint(entry.fields), "sensitive-")
				return
			}
			assert.Zero(t, gotMetric)
			assert.Empty(t, logger.entries)
		})
	}
}

func TestSendPermissionMetricsHaveOnlyBoundedLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newBotSendPermissionMetrics(registry)
	metrics.observe(sendPermissionStageGroupStatus, sendPermissionReasonNotFound)

	err := testutil.GatherAndCompare(
		registry,
		strings.NewReader(`# HELP dmwork_bot_send_permission_failure_total Total terminal Bot send-permission check failures by bounded stage and reason.
# TYPE dmwork_bot_send_permission_failure_total counter
dmwork_bot_send_permission_failure_total{reason="not_found",stage="group_status"} 1
`),
		"dmwork_bot_send_permission_failure_total",
	)
	require.NoError(t, err)
}

func TestSendPermissionObservationBoundsUnexpectedValues(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newBotSendPermissionMetrics(registry)
	logger := &permissionRecordingLog{}
	ba := &BotAPI{sendPermissionMetrics: metrics, Log: logger}
	ctx := newPermissionTestContext(t, "trace-safe")

	ba.observeSendPermissionFailure(
		ctx,
		"sensitive-kind",
		common.ChannelTypePerson.Uint8(),
		sendPermissionFailureStage("sensitive-stage-id"),
		sendPermissionFailureReason("sensitive-error-text"),
		nil,
	)

	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.failures.WithLabelValues("unknown", "unknown")))
	require.Len(t, logger.entries, 1)
	entry := logger.entries[0]
	assert.Equal(t, "unknown", entry.fields["permission_stage"])
	assert.Equal(t, "unknown", entry.fields["failure_reason"])
	assert.Equal(t, "unknown", entry.fields["bot_kind"])
	assert.NotContains(t, fmt.Sprint(entry.fields), "sensitive-")
}

func TestCheckSendPermissionDisbandedGroupDoesNotEmitInternalFailureMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newBotSendPermissionMetrics(registry)
	logger := &permissionRecordingLog{}
	ba := &BotAPI{
		groupStatusQueryOverride: func(string) (int, error) {
			return group.GroupStatusDisband, nil
		},
		sendPermissionMetrics: metrics,
		Log:                   logger,
	}
	ctx := newPermissionTestContext(t, "trace-safe")

	err := ba.checkSendPermission(ctx, BotKindUser, "sensitive-bot", "sensitive-group", common.ChannelTypeGroup.Uint8(), false)
	require.ErrorIs(t, err, errBotSendPermGroupDisbanded)
	assert.Empty(t, logger.entries)
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.failures.WithLabelValues(
		string(sendPermissionStageGroupStatus), string(sendPermissionReasonQueryError),
	)))
}

func TestCheckSendPermissionKeepsSuccessfulAndBusinessDenialPaths(t *testing.T) {
	tests := []struct {
		name        string
		botKind     string
		channelID   string
		channelType uint8
		prepare     func(*wkhttp.Context, *BotAPI)
		wantErr     error
	}{
		{
			name:        "platform app friend succeeds",
			botKind:     BotKindApp,
			channelID:   "target",
			channelType: common.ChannelTypePerson.Uint8(),
			prepare: func(_ *wkhttp.Context, ba *BotAPI) {
				ba.friendCheckOverride = func(string, string) (bool, error) { return true, nil }
			},
		},
		{
			name:        "space app inactive member remains not_space_member",
			botKind:     BotKindApp,
			channelID:   "target",
			channelType: common.ChannelTypePerson.Uint8(),
			prepare: func(c *wkhttp.Context, ba *BotAPI) {
				c.Set(CtxKeyAppBotScope, "space")
				c.Set(CtxKeyAppBotSpaceID, "space")
				ba.friendCheckOverride = func(string, string) (bool, error) { return true, nil }
				ba.spaceMemberQueryOverride = func(string, string) (bool, error) { return false, nil }
			},
			wantErr: errBotSendPermNotSpaceMember,
		},
		{
			name:        "group member succeeds",
			botKind:     BotKindUser,
			channelID:   "group",
			channelType: common.ChannelTypeGroup.Uint8(),
			prepare: func(_ *wkhttp.Context, ba *BotAPI) {
				ba.groupStatusQueryOverride = func(string) (int, error) { return 0, nil }
				ba.groupMemberCountOverride = func(string, string) (int, error) { return 1, nil }
			},
		},
		{
			name:        "thread parent group member succeeds",
			botKind:     BotKindUser,
			channelID:   "group____thread",
			channelType: common.ChannelTypeCommunityTopic.Uint8(),
			prepare: func(_ *wkhttp.Context, ba *BotAPI) {
				ba.groupStatusQueryOverride = func(string) (int, error) { return 0, nil }
				ba.groupMemberCountOverride = func(string, string) (int, error) { return 1, nil }
			},
		},
		{
			name:        "user bot friend succeeds",
			botKind:     BotKindUser,
			channelID:   "target",
			channelType: common.ChannelTypePerson.Uint8(),
			prepare: func(_ *wkhttp.Context, ba *BotAPI) {
				ba.friendCheckOverride = func(string, string) (bool, error) { return true, nil }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			metrics := newBotSendPermissionMetrics(registry)
			logger := &permissionRecordingLog{}
			ba := &BotAPI{sendPermissionMetrics: metrics, Log: logger}
			ctx := newPermissionTestContext(t, "trace-safe")
			tt.prepare(ctx, ba)

			err := ba.checkSendPermission(ctx, tt.botKind, "bot", tt.channelID, tt.channelType, false)
			if tt.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.wantErr)
			}
			assert.Empty(t, logger.entries)
			families, gatherErr := registry.Gather()
			require.NoError(t, gatherErr)
			assert.Empty(t, families)
		})
	}
}

func TestSendPermissionObservabilityNilSafety(t *testing.T) {
	assert.Nil(t, newBotSendPermissionMetrics(nil))
	(*botSendPermissionMetrics)(nil).observe(sendPermissionStageFriend, sendPermissionReasonQueryError)
	assert.Empty(t, traceIDFromWKContext(nil))
	assert.Empty(t, traceIDFromWKContext(&wkhttp.Context{}))
}
