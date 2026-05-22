package conversation_ext

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 单测覆盖 withDeadlockRetry 的三条分支：
//  1. 首次成功 — 不应进入重试 path；
//  2. 持续遇到死锁 — 重试到上限后返回 wrap 后的错误；
//  3. 中途从死锁错误恢复 — 返回 nil；
//  4. 非 MySQL 锁错误 — 立即返回，不重试。
//
// 不依赖 DB（纯 Go 单测，无 //go:build integration）。

func TestWithDeadlockRetry_FirstAttemptSucceeds(t *testing.T) {
	var calls int
	err := withDeadlockRetry(func() error {
		calls++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "首次成功不应触发重试")
}

func TestWithDeadlockRetry_RetriesDeadlock_AndRecovers(t *testing.T) {
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"}
	var calls int
	err := withDeadlockRetry(func() error {
		calls++
		if calls < 2 {
			return deadlock
		}
		return nil
	})
	require.NoError(t, err, "第二次返回 nil 应被识别为恢复成功")
	assert.Equal(t, 2, calls)
}

func TestWithDeadlockRetry_RetriesLockWaitTimeout_AndRecovers(t *testing.T) {
	lockWait := &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	var calls int
	err := withDeadlockRetry(func() error {
		calls++
		if calls < 3 {
			return lockWait
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls, "第三次（也即最后一次允许）才恢复也应成功")
}

func TestWithDeadlockRetry_ExhaustsAfterMaxAttempts(t *testing.T) {
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"}
	var calls int
	err := withDeadlockRetry(func() error {
		calls++
		return deadlock
	})
	require.Error(t, err)
	assert.Equal(t, 3, calls, "重试上限是 3")
	assert.ErrorIs(t, err, deadlock, "最终错误应 wrap 原 MySQL 死锁错误，便于上游 errors.Is 判定")
	assert.Contains(t, err.Error(), "retry exhausted")
}

func TestWithDeadlockRetry_NonLockErrorBubblesImmediately(t *testing.T) {
	otherErr := errors.New("syntax error: not a lock issue")
	var calls int
	err := withDeadlockRetry(func() error {
		calls++
		return otherErr
	})
	require.Error(t, err)
	assert.Equal(t, 1, calls, "非死锁错误不应触发重试")
	assert.ErrorIs(t, err, otherErr, "非死锁错误应原样返回（不被 wrap）")
}

func TestWithDeadlockRetry_WrappedDeadlockStillDetected(t *testing.T) {
	// 业务代码经常用 fmt.Errorf("... %w", err) 包装；retry 必须用 errors.As 透过 wrapping
	// 识别 MySQL 错误码，否则保护就失效。
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}
	wrapped := fmt.Errorf("OnThreadCreated batch: %w", deadlock)
	var calls int
	err := withDeadlockRetry(func() error {
		calls++
		if calls < 2 {
			return wrapped
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "wrap 后的 MySQL 死锁错误也应被识别为可重试")
}
