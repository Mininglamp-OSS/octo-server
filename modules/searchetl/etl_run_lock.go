package searchetl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// runLock 抽象 searchetl 的 Redis 分布式互斥锁（由 *etlLock 实现）。抽成接口便于单测注入
// 假锁，验证 C3 三条硬语义：锁横跨整 tick 持有、续租失败立即 abort 在飞批次、失锁不双算。
type runLock interface {
	// Acquire 抢锁：(true,nil)=抢到, (false,nil)=别人持锁, (_,err)=Redis 故障。
	Acquire(token string) (bool, error)
	// Renew 续租：true=仍持有并已续期, false=锁已过期或 owner 变更（须 abort）。
	Renew(token string) (bool, error)
	// Release 释放（CAS-DEL，仅 token 匹配时）。
	Release(token string) error
}

// lockRenewInterval 续租周期：TTL/3，留足余量让单次 Redis 抖动不致误判失锁。
func lockRenewInterval() time.Duration {
	iv := etlRunLockTTL / 3
	if iv < time.Second {
		iv = time.Second
	}
	return iv
}

// randomToken 生成锁持有者 token（每轮新生成，供 CAS-DEL/CAS-PEXPIRE 校验 owner）。
func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// runLockedTick 在持有 Redis run-lock 的前提下跑一轮 run（C3 跨副本互斥编排核心）。
//
// 语义（plan §3.5 / 硬条件 C3）：
//   - 抢锁：抢不到（别的副本在跑）→ 直接跳过本轮返回 nil，不报错（验证门：两副本只一个跑 tick）。
//   - 锁横跨整 tick：后台续租 goroutine 在 run 执行期间周期续租，覆盖「读批 → 投 Kafka →
//     推游标」整个窗口。
//   - 🔴 续租失败立即 abort：Renew 返回 error 或 false（锁过期/被抢）→ 取消 lockCtx，令在飞
//     批次 abort——run 内的 Kafka 投递与游标推进都尊重 lockCtx 取消，绝不在失锁后继续推进游标
//     或投递（验证门：锁失效路径不双算）。
//   - 释放：tick 结束 CAS-DEL 释放（token 不匹配则不误删后继 owner 的锁）。
//
// interval 为续租周期（生产用 lockRenewInterval()，测试可传短值）。run 收到的 ctx 即 lockCtx，
// 失锁时被取消。
func runLockedTick(ctx context.Context, lock runLock, interval time.Duration, lg log.Log, run func(context.Context) error) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	acquired, err := lock.Acquire(token)
	if err != nil {
		// Redis 故障：不强行裸跑（避免多副本同跑），等下个 tick。
		lg.Error("searchetl: acquire run-lock failed, skip tick", zap.Error(err))
		return err
	}
	if !acquired {
		lg.Info("searchetl: run-lock held by another replica, skip tick")
		return nil
	}

	lockCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// renewDone 在续租 goroutine 退出后关闭，供下方 join——必须先停 + 等续租 goroutine 真正退出，
	// 再 Release，避免「Release 后迟到的 Renew 误判 owner 丢失」与 goroutine 泄漏（续租比 tick 长寿）。
	done := make(chan struct{})
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		renewUntilDone(lock, token, interval, cancel, done, lg)
	}()

	defer func() {
		close(done) // 通知续租 goroutine 停止
		<-renewDone // join：等其真正退出，确保此后不再有 Renew 调用，再释放
		if rerr := lock.Release(token); rerr != nil {
			lg.Error("searchetl: release run-lock failed", zap.Error(rerr))
		}
	}()

	return run(lockCtx)
}

// renewUntilDone 周期续租，直到 done 关闭（tick 正常结束）或续租失败。
// 🔴 续租失败（err 或 owner 丢失）→ cancel(lockCtx) 触发在飞批次 abort，并停止续租。
func renewUntilDone(lock runLock, token string, interval time.Duration, cancel context.CancelFunc, done <-chan struct{}, lg log.Log) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			ok, err := lock.Renew(token)
			if err != nil {
				lg.Error("searchetl: renew run-lock failed, abort in-flight tick", zap.Error(err))
				cancel()
				return
			}
			if !ok {
				lg.Error("searchetl: run-lock ownership lost, abort in-flight tick")
				cancel()
				return
			}
		}
	}
}
