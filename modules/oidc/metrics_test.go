package oidc

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// metricsAreRegistered 调用 Inc/Observe 并断言通过 testutil 能拿到非零样本,
// 间接验证 collectors 注册到默认 Registry 成功。
func TestMetrics_AuthorizeTotal(t *testing.T) {
	before := testutil.ToFloat64(metricAuthorizeTotal)
	metricAuthorizeTotal.Inc()
	assert.Equal(t, before+1, testutil.ToFloat64(metricAuthorizeTotal))
}

func TestMetrics_CallbackTotal_AllResultLabelsExist(t *testing.T) {
	for _, result := range callbackResultLabels() {
		c, err := metricCallbackTotal.GetMetricWithLabelValues(result)
		assert.NoError(t, err, "label %s must be valid", result)
		assert.NotNil(t, c)
	}
}

func TestMetrics_CallbackDuration_HistogramRegistered(t *testing.T) {
	metricCallbackDuration.Observe(0.05)
	// CollectAndCount 用样本数判断 collector 已注册并接收样本
	count := testutil.CollectAndCount(metricCallbackDuration)
	assert.Greater(t, count, 0)
}

func TestMetrics_StateConsumeTotal_Labels(t *testing.T) {
	for _, result := range []string{"ok", "miss"} {
		_, err := metricStateConsumeTotal.GetMetricWithLabelValues(result)
		assert.NoError(t, err)
	}
}

func TestMetrics_LogoutTotal_Labels(t *testing.T) {
	for _, result := range []string{"ok", "kick_fail", "token_fail", "revoke_fail"} {
		_, err := metricLogoutTotal.GetMetricWithLabelValues(result)
		assert.NoError(t, err)
	}
}

func TestMetrics_SyncTickTotal_Labels(t *testing.T) {
	for _, result := range []string{"ran", "lock_held", "lock_err"} {
		_, err := metricSyncTickTotal.GetMetricWithLabelValues(result)
		assert.NoError(t, err)
	}
}

func TestMetrics_SyncProcessedTotal_Labels(t *testing.T) {
	for _, result := range []string{"ok", "invalid_grant", "transient", "panic"} {
		_, err := metricSyncProcessedTotal.GetMetricWithLabelValues(result)
		assert.NoError(t, err)
	}
}

// 重复 New 不能再次注册到默认 Registry —— 通过 prometheus.Register 显式调用应当返
// AlreadyRegisteredError,确认 init() 只注册了一次。
func TestMetrics_NoDoubleRegistration(t *testing.T) {
	err := prometheus.Register(metricAuthorizeTotal)
	assert.Error(t, err)
	_, ok := err.(prometheus.AlreadyRegisteredError)
	assert.True(t, ok, "expected AlreadyRegisteredError, got %T", err)
}
