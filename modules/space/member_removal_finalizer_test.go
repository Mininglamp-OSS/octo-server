package space

// 收敛动作（finalizer）的执行契约。
//
// 它存在的理由是**顺序**：步骤的执行顺序就是注册顺序，而注册顺序由 import 方向决定，
// 没有任何地方声明过。有些收敛只有在**所有**步骤都做完之后算出来才是对的——PR #855
// 里的例子是「全员群的群主必须是项目的活跃 owner」：群侧级联按群资历交接群主，项目侧
// 级联关席位，两者谁先谁后不确定，而正确答案只有在两件都发生之后才成立。
//
// 所以这里钉的是三条性质，都是"放成第五个步骤"给不出来的：
//   1. 收敛动作跑在**每一个**步骤之后；
//   2. 有步骤失败时**不跑**（那一轮的前提不成立），工单重排后自然会跑到；
//   3. 收敛动作自己失败也让工单重试，而不是被静默吞掉。

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// swapCleanupFinalizersForTest 与 swapCleanupStepsForTest 同一形状。
func swapCleanupFinalizersForTest(fins []namedCleanupStep) func() {
	cleanupFinalizersMu.Lock()
	prev := cleanupFinalizers
	cleanupFinalizers = fins
	cleanupFinalizersMu.Unlock()
	return func() {
		cleanupFinalizersMu.Lock()
		cleanupFinalizers = prev
		cleanupFinalizersMu.Unlock()
	}
}

// TestFinalizerRunsAfterEveryStep 钉性质 1：无论步骤怎么排，收敛动作都在它们之后。
//
// 记录的是**调用顺序**而不是"是否被调用过"：后者对第五个步骤同样成立，正是这个测试
// 必须区分开的东西。
func TestFinalizerRunsAfterEveryStep(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-fin-order"
	seedMember(t, f, spaceID, "owner-fin", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-fin", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-fin", 1, "owner-fin", MemberRemoveReasonKicked)

	var order []string
	restoreSteps := swapCleanupStepsForTest([]namedCleanupStep{
		{name: "step-a", fn: func(*config.Context, MemberRemoval) error {
			order = append(order, "step-a")
			return nil
		}},
		{name: "step-b", fn: func(*config.Context, MemberRemoval) error {
			order = append(order, "step-b")
			return nil
		}},
	})
	defer restoreSteps()
	var seen []MemberRemoval
	restoreFins := swapCleanupFinalizersForTest([]namedCleanupStep{
		{name: "converge", fn: func(_ *config.Context, removal MemberRemoval) error {
			order = append(order, "converge")
			seen = append(seen, removal)
			return nil
		}},
	})
	defer restoreFins()

	f.processMemberRemovalCleanups()

	assert.Equal(t, []string{"step-a", "step-b", "converge"}, order,
		"收敛动作必须排在全部步骤之后。注册成第五个步骤给不出这条性质：步骤顺序就是 "+
			"import 顺序，没有任何地方声明，于是同一段代码在一半的顺序下读到的是中间态")
	require.Len(t, seen, 1)
	assert.Equal(t, spaceID, seen[0].SpaceID)
	assert.Equal(t, "victim-fin", seen[0].UID)
	assert.Equal(t, MemberRemoveReasonKicked, seen[0].Reason,
		"收敛动作拿到的是与步骤同一份 MemberRemoval")

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupDone, jobs[0].Status)
}

// TestFinalizerDoesNotRunWhenAStepFailed 钉性质 2。
//
// 前提没成立就别算：一个失败的步骤意味着"别的步骤已经做完"这句话是假的，而工单会被
// 重排，下一轮全部成功时收敛动作自然会跑到。
func TestFinalizerDoesNotRunWhenAStepFailed(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-fin-skip"
	seedMember(t, f, spaceID, "owner-fs", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-fs", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-fs", 1, "owner-fs", MemberRemoveReasonKicked)

	restoreSteps := swapCleanupStepsForTest([]namedCleanupStep{
		{name: "broken", fn: func(*config.Context, MemberRemoval) error {
			return errors.New("downstream unavailable")
		}},
	})
	defer restoreSteps()
	ran := 0
	restoreFins := swapCleanupFinalizersForTest([]namedCleanupStep{
		{name: "converge", fn: func(*config.Context, MemberRemoval) error {
			ran++
			return nil
		}},
	})
	defer restoreFins()

	f.processMemberRemovalCleanups()

	assert.Zero(t, ran, "有步骤失败时收敛动作不得运行——它的输入还没安定下来")
	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupPending, jobs[0].Status, "工单仍需重试")
	assert.Contains(t, jobs[0].LastError, "broken")
}

// TestFailedFinalizerRetriesTheJob 钉性质 3：收敛动作失败与步骤失败同一条出路。
//
// 静默吞掉会让"群主没收敛"这件事永远没有第二次机会——本模块没有任何扫描在看
// creator-versus-owner 这条关系。
func TestFailedFinalizerRetriesTheJob(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-fin-fail"
	seedMember(t, f, spaceID, "owner-ff", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-ff", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-ff", 1, "owner-ff", MemberRemoveReasonKicked)

	restoreSteps := swapCleanupStepsForTest([]namedCleanupStep{
		{name: "fine", fn: func(*config.Context, MemberRemoval) error { return nil }},
	})
	defer restoreSteps()
	restoreFins := swapCleanupFinalizersForTest([]namedCleanupStep{
		{name: "converge", fn: func(*config.Context, MemberRemoval) error {
			return errors.New("owner sync unavailable")
		}},
	})
	defer restoreFins()

	f.processMemberRemovalCleanups()

	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupPending, jobs[0].Status,
		"收敛动作失败必须让工单重试，而不是被标成 done——否则这次收敛再也没有第二次机会")
	assert.Contains(t, jobs[0].LastError, "converge",
		"last_error 要指出是哪个收敛动作失败的")
	assert.Empty(t, jobs[0].LeaseOwner, "失败后必须释放租约")
}

// TestFinalizerPanicIsContained 钉每个收敛动作单独兜 panic。
//
// 只靠函数级那个 recover 的话，一次 panic 会跳出循环，排在后面的收敛动作本轮一次
// 都跑不到——与步骤那边同一个理由，同一段注释。
func TestFinalizerPanicIsContained(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const spaceID = "wk-fin-panic"
	seedMember(t, f, spaceID, "owner-fp", 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{SpaceId: spaceID, UID: "victim-fp", Role: 0, Status: 1}))
	mustRemoveMember(t, f, spaceID, "victim-fp", 1, "owner-fp", MemberRemoveReasonKicked)

	restoreSteps := swapCleanupStepsForTest(nil)
	defer restoreSteps()
	second := 0
	restoreFins := swapCleanupFinalizersForTest([]namedCleanupStep{
		{name: "boom", fn: func(*config.Context, MemberRemoval) error {
			panic("finalizer exploded")
		}},
		{name: "after-boom", fn: func(*config.Context, MemberRemoval) error {
			second++
			return nil
		}},
	})
	defer restoreFins()

	f.processMemberRemovalCleanups()

	assert.Equal(t, 1, second, "一个收敛动作 panic 不得让后面的本轮跑不到")
	jobs := cleanupJobs(t, spaceID)
	require.Len(t, jobs, 1)
	assert.Equal(t, removalCleanupPending, jobs[0].Status, "panic 记为失败并重排，而不是停在 running")
	assert.Contains(t, jobs[0].LastError, "boom")
}
