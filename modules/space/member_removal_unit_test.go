package space

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件只放**不依赖 MySQL / Redis / WuKongIM** 的单元测试，
// 保证清理工单的枚举、退避、注册表这几层逻辑在没有基础设施的环境里也能验证。
// 端到端行为（真的退群、真的摘白名单）在 member_removal_test.go 里，需要 CI 的服务容器。

func TestIsMemberRemoveReason(t *testing.T) {
	for _, reason := range []string{
		MemberRemoveReasonKicked,
		MemberRemoveReasonLeft,
		MemberRemoveReasonForceRemoved,
		MemberRemoveReasonSpaceDisbanded,
	} {
		assert.True(t, IsMemberRemoveReason(reason), "%s 必须是合法原因", reason)
	}
	// 入库前拦住拼错的字面量：工单原因是低基数枚举，写进去就再也匹配不上了。
	for _, bad := range []string{"", "KICKED", "removed", "space_disband", "left "} {
		assert.False(t, IsMemberRemoveReason(bad), "%q 不该被当成合法原因", bad)
	}
}

func TestMemberRemovalRetryDelayBacksOffAndCaps(t *testing.T) {
	// 退避必须单调不减，否则失败的工单会以固定间隔猛打下游 IM。
	prev := time.Duration(0)
	for attempt := uint32(1); attempt <= 12; attempt++ {
		got := memberRemovalRetryDelay(attempt)
		assert.GreaterOrEqual(t, got, prev, "attempt %d 的退避不能比上一次短", attempt)
		assert.LessOrEqual(t, got, 5*time.Minute, "attempt %d 的退避必须封顶在 5 分钟", attempt)
		prev = got
	}
	assert.Equal(t, 2*time.Second, memberRemovalRetryDelay(1))
	assert.Equal(t, 4*time.Second, memberRemovalRetryDelay(2))
	// 封顶之后继续加 attempt 不再增长，避免一条坏工单被推到几小时后才重试。
	assert.Equal(t, memberRemovalRetryDelay(9), memberRemovalRetryDelay(100))
}

func TestTruncateCleanupErrorFitsColumn(t *testing.T) {
	// last_error 是 VARCHAR(255)：超长会让整条 UPDATE 报错，等于把重试链路自己弄断。
	long := make([]byte, 1000)
	for i := range long {
		long[i] = 'x'
	}
	assert.Len(t, truncateCleanupError(string(long)), 255)
	assert.Equal(t, "short", truncateCleanupError("short"))
	assert.Equal(t, "", truncateCleanupError(""))
}

func TestRegisterMemberRemovalCleanupStepOverridesByName(t *testing.T) {
	restore := swapCleanupStepsForTest(nil)
	defer restore()

	calls := make([]string, 0, 4)
	RegisterMemberRemovalCleanupStep("a", func(*config.Context, MemberRemoval) error {
		calls = append(calls, "a-v1")
		return nil
	})
	RegisterMemberRemovalCleanupStep("b", func(*config.Context, MemberRemoval) error {
		calls = append(calls, "b")
		return nil
	})
	// 同名重注册必须覆盖而不是追加，否则测试替身会和真身一起跑。
	RegisterMemberRemovalCleanupStep("a", func(*config.Context, MemberRemoval) error {
		calls = append(calls, "a-v2")
		return nil
	})

	steps := snapshotCleanupSteps()
	require.Len(t, steps, 2, "同名注册不得新增条目")
	for _, step := range steps {
		require.NoError(t, step.fn(nil, MemberRemoval{}))
	}
	assert.Equal(t, []string{"a-v2", "b"}, calls, "覆盖后必须只跑新版本，且保持注册顺序")
}

func TestRegisterMemberRemovalCleanupStepIgnoresEmpty(t *testing.T) {
	restore := swapCleanupStepsForTest(nil)
	defer restore()

	// 空名字 / nil 函数静默忽略：注册进去会在 worker 里变成一次 nil 调用 panic。
	RegisterMemberRemovalCleanupStep("", func(*config.Context, MemberRemoval) error { return nil })
	RegisterMemberRemovalCleanupStep("named", nil)
	assert.Empty(t, snapshotCleanupSteps())
}

// swapCleanupStepsForTest 换掉全局注册表并返回还原函数。
// 注册表是包级状态，测试之间必须隔离，否则先跑的用例会污染后跑的。
func swapCleanupStepsForTest(steps []namedCleanupStep) func() {
	cleanupStepsMu.Lock()
	prev := cleanupSteps
	cleanupSteps = steps
	cleanupStepsMu.Unlock()
	return func() {
		cleanupStepsMu.Lock()
		cleanupSteps = prev
		cleanupStepsMu.Unlock()
	}
}
