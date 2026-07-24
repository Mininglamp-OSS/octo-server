package bot_api

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/reqid"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

type sendPermissionFailureStage string

const (
	sendPermissionStageFriend       sendPermissionFailureStage = "friend"
	sendPermissionStageSpaceContext sendPermissionFailureStage = "space_context"
	sendPermissionStageSpaceMember  sendPermissionFailureStage = "space_member"
	sendPermissionStageGroupStatus  sendPermissionFailureStage = "group_status"
	sendPermissionStageGroupMember  sendPermissionFailureStage = "group_member"
	sendPermissionStageBotKind      sendPermissionFailureStage = "bot_kind"
	sendPermissionStageUnknown      sendPermissionFailureStage = "unknown"
)

type sendPermissionFailureReason string

const (
	sendPermissionReasonQueryError     sendPermissionFailureReason = "query_error"
	sendPermissionReasonNotFound       sendPermissionFailureReason = "not_found"
	sendPermissionReasonMissingContext sendPermissionFailureReason = "missing_context"
	sendPermissionReasonUnknown        sendPermissionFailureReason = "unknown"
)

type botSendPermissionMetrics struct {
	failures *prometheus.CounterVec
}

func newBotSendPermissionMetrics(reg prometheus.Registerer) *botSendPermissionMetrics {
	if reg == nil {
		return nil
	}
	metrics := &botSendPermissionMetrics{
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_bot_send_permission_failure_total",
			Help: "Total terminal Bot send-permission check failures by bounded stage and reason.",
		}, []string{"stage", "reason"}),
	}
	reg.MustRegister(metrics.failures)
	return metrics
}

var defaultBotSendPermissionMetrics = newBotSendPermissionMetrics(prometheus.DefaultRegisterer)

func (m *botSendPermissionMetrics) observe(stage sendPermissionFailureStage, reason sendPermissionFailureReason) {
	if m == nil {
		return
	}
	stage = stage.bounded()
	reason = reason.bounded()
	m.failures.WithLabelValues(string(stage), string(reason)).Inc()
}

func (ba *BotAPI) observeSendPermissionFailure(
	c *wkhttp.Context,
	botKind string,
	channelType uint8,
	stage sendPermissionFailureStage,
	reason sendPermissionFailureReason,
) {
	stage = stage.bounded()
	reason = reason.bounded()
	metrics := ba.sendPermissionMetrics
	if metrics == nil {
		metrics = defaultBotSendPermissionMetrics
	}
	metrics.observe(stage, reason)

	if ba.Log == nil {
		return
	}
	fields := []zap.Field{
		zap.String("trace_id", traceIDFromWKContext(c)),
		zap.String("permission_stage", string(stage)),
		zap.String("failure_reason", string(reason)),
		zap.String("bot_kind", safeBotKind(botKind)),
		zap.Uint8("channel_type", channelType),
	}
	if reason == sendPermissionReasonNotFound {
		ba.Warn("bot send permission check denied", fields...)
		return
	}
	ba.Error("bot send permission check failed", fields...)
}

func (stage sendPermissionFailureStage) bounded() sendPermissionFailureStage {
	switch stage {
	case sendPermissionStageFriend,
		sendPermissionStageSpaceContext,
		sendPermissionStageSpaceMember,
		sendPermissionStageGroupStatus,
		sendPermissionStageGroupMember,
		sendPermissionStageBotKind,
		sendPermissionStageUnknown:
		return stage
	default:
		return sendPermissionStageUnknown
	}
}

func (reason sendPermissionFailureReason) bounded() sendPermissionFailureReason {
	switch reason {
	case sendPermissionReasonQueryError,
		sendPermissionReasonNotFound,
		sendPermissionReasonMissingContext,
		sendPermissionReasonUnknown:
		return reason
	default:
		return sendPermissionReasonUnknown
	}
}

func traceIDFromWKContext(c *wkhttp.Context) string {
	if c == nil || c.Context == nil {
		return ""
	}
	return c.GetString(reqid.GinKey)
}

func safeBotKind(kind string) string {
	switch kind {
	case BotKindUser, BotKindApp:
		return kind
	default:
		return "unknown"
	}
}
