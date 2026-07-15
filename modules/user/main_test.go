package user

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
)

// TestMain 确保集成测试启动前 OCTO_MASTER_KEY 已就位。
//
// testutil.NewTestServer → module.Setup → common.Setup 在 2026-05 后强制
// 要求该 env 非空才能加密 IM 私钥写 app_config。user package 下已有若干
// DB-backed 测试(external_login_test.go / api_manager_test.go /
// db_verification_test.go 等)都依赖 NewTestServer,集中在这里一次性兜底,
// 避免每个测试文件各自 setenv。
//
// 语义:已存在不覆盖,允许 CI / dev shell 注入固定密钥;仅在未设置时
// 随机生成一个 16 字节 hex 作为本次进程内的占位密钥(进程退出即失效)。
func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		_ = os.Setenv("OCTO_MASTER_KEY", hex.EncodeToString(key))
	}
	os.Exit(m.Run())
}
