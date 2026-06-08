package opanalytics

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
)

// TestMain 确保集成测试启动前 OCTO_MASTER_KEY 已就位。testutil.NewTestServer
// 触发 common.Setup，后者要求该 env 才能加密 IM 私钥。已存在不覆盖(允许 CI 注入固定密钥)。
func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		os.Setenv("OCTO_MASTER_KEY", hex.EncodeToString(key))
	}
	os.Exit(m.Run())
}
