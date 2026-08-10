package user

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
)

// TestMain 确保集成测试启动前必需的加密 env 已就位。
//
//   - OCTO_MASTER_KEY: testutil.NewTestServer → module.Setup → common.Setup 在
//     2026-05 后强制要求该 env 非空才能加密 IM 私钥写 app_config。
//   - OCTO_PII_ENCRYPTION_SECRET: 手机号影子列是 fail-closed 的（见
//     DB.syncPhoneShadow）——任何创建"带手机号用户"的写入在缺密钥时都会失败，
//     这与生产要求一致。包内多处 DB-backed 测试会建带手机号的用户，同样集中兜底。
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
	code := m.Run()
	if err := cleanupTokenHTTPTestDatabases(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
