package user

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
)

// TestMain 确保集成测试启动前必需的加密 env 已就位。
//
//   - OCTO_MASTER_KEY: testutil.NewTestServer → module.Setup → common.Setup 在
//     2026-05 后强制要求该 env 非空才能加密 IM 私钥写 app_config。
//   - OCTO_PII_ENCRYPTION_SECRET: 缺密钥时带手机号的写入会降级成只写明文（见
//     DB.syncPhoneShadow），影子列留空。包内多处 DB-backed 测试断言影子列已填齐，
//     所以这里集中兜底注入密钥；需要覆盖降级行为的用例用 withPhoneSecretForTest
//     显式清掉它。
//
// user package 下已有若干 DB-backed 测试(external_login_test.go /
// api_manager_test.go / db_verification_test.go 等)都依赖 NewTestServer,集中在
// 这里一次性兜底,避免每个测试文件各自 setenv。
//
// 语义:已存在不覆盖,允许 CI / dev shell 注入固定密钥;仅在未设置时
// 随机生成占位密钥(进程退出即失效)。
func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		_ = os.Setenv("OCTO_MASTER_KEY", hex.EncodeToString(key))
	}
	// 必须恰好 32 字节：16 字节随机数的 hex 编码正好是 32 个字符。
	if os.Getenv(phoneEncryptionSecretEnv) == "" {
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		_ = os.Setenv(phoneEncryptionSecretEnv, hex.EncodeToString(key))
	}
	// register.GetModules caches module objects from the first Context for the
	// whole process. Make that Context deterministic and run the same rollout
	// startup sequence as main.go so every cached login handler has a live write
	// lease instead of inheriting a pre-migration issuance fence.
	legacyMode, hadLegacyMode := os.LookupEnv("OCTO_AUTH_SESSION_MODE")
	legacyCap, hadLegacyCap := os.LookupEnv("OCTO_AUTH_SESSION_MAX_PER_UID")
	_ = os.Unsetenv("OCTO_AUTH_SESSION_MODE")
	_ = os.Unsetenv("OCTO_AUTH_SESSION_MAX_PER_UID")
	_, rolloutTestContext := testutil.NewTestServer()
	stopRollout, err := startSessionRolloutForTest(rolloutTestContext, "user-package-tests")
	if hadLegacyMode {
		_ = os.Setenv("OCTO_AUTH_SESSION_MODE", legacyMode)
	} else {
		_ = os.Unsetenv("OCTO_AUTH_SESSION_MODE")
	}
	if hadLegacyCap {
		_ = os.Setenv("OCTO_AUTH_SESSION_MAX_PER_UID", legacyCap)
	} else {
		_ = os.Unsetenv("OCTO_AUTH_SESSION_MAX_PER_UID")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	stopRollout()
	if err := cleanupTokenHTTPTestDatabases(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func startSessionRolloutForTest(ctx *config.Context, build string) (context.CancelFunc, error) {
	store, client := auth.SessionStoreAndClientForContext(ctx)
	boot, _, err := auth.InitializeSessionRollout(ctx)
	if err != nil {
		return nil, fmt.Errorf("initialize test session rollout: %w", err)
	}
	rolloutCtx, stop := context.WithCancel(context.Background())
	registry := auth.NewWriterRegistry(client, ctx.GetConfig().Cache.UIDTokenCachePrefix)
	store.UseWriterLease(registry)
	if err := registry.Join(rolloutCtx, build, build, string(store.Mode()), nil); err != nil {
		stop()
		return nil, fmt.Errorf("join test writer registry: %w", err)
	}
	if err := store.ApplyAndPublishRolloutState(registry, auth.RolloutState{
		Floor: boot.Floor, MaxPerUID: boot.MaxPerUID, Version: boot.Version,
	}, store.Mode()); err != nil {
		stop()
		return nil, fmt.Errorf("publish test rollout state: %w", err)
	}
	return stop, nil
}
