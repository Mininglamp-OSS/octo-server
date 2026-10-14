package drivepolicy

import (
	"testing"

	internalresolve "github.com/Mininglamp-OSS/octo-server/modules/internal_resolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 32-byte token clears minInternalTokenBytes.
const testToken = "0123456789abcdef0123456789abcdef" // 32 bytes

// pinTokenEnv 是供各用例复用的一行式 getenv 构造。
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// 硬钉子：本模块复制的是字面量而非 import，pin 住两处拼写，防止漂移。
func TestDriveInternalTokenEnvLiteralPinned(t *testing.T) {
	assert.Equal(t, "OCTO_DRIVE_INTERNAL_TOKEN", driveInternalTokenEnv)
	assert.Equal(t, internalresolve.DriveInternalTokenEnv, driveInternalTokenEnv,
		"drivepolicy 的 token env 字面量必须与 internal_resolve.DriveInternalTokenEnv 逐字一致")
}

func TestResolveDrivePolicyToken_Unset(t *testing.T) {
	_, err := resolveDrivePolicyToken(envMap(nil))
	require.Error(t, err)
}

func TestResolveDrivePolicyToken_NilGetenv(t *testing.T) {
	_, err := resolveDrivePolicyToken(nil)
	require.Error(t, err)
}

func TestResolveDrivePolicyToken_TooShort(t *testing.T) {
	_, err := resolveDrivePolicyToken(envMap(map[string]string{
		driveInternalTokenEnv: "short",
	}))
	require.Error(t, err)
	// 31 bytes still rejected (floor is inclusive ≥32).
	_, err = resolveDrivePolicyToken(envMap(map[string]string{
		driveInternalTokenEnv: "0123456789abcdef0123456789abcde", // 31 bytes
	}))
	require.Error(t, err)
}

// 与任一兄弟固定 token 冲突即拒绝（对称 fail-closed）。
func TestResolveDrivePolicyToken_SiblingCollisions(t *testing.T) {
	siblings := []string{
		notifyInternalTokenEnv,
		docsNotifyInternalTokenEnv,
		botMentionInternalTokenEnv,
		marketplaceInternalTokenEnv,
	}
	for _, sib := range siblings {
		t.Run(sib, func(t *testing.T) {
			_, err := resolveDrivePolicyToken(envMap(map[string]string{
				driveInternalTokenEnv: testToken,
				sib:                   testToken,
			}))
			require.Error(t, err, "与兄弟 token %s 同值时必须 fail-closed", sib)
		})
	}
}

// 合法：≥32 字节且与所有兄弟不同 → 返回 token 本身。
func TestResolveDrivePolicyToken_Valid(t *testing.T) {
	got, err := resolveDrivePolicyToken(envMap(map[string]string{
		driveInternalTokenEnv:      testToken,
		notifyInternalTokenEnv:     "ffffffffffffffffffffffffffffffff",
		docsNotifyInternalTokenEnv: "gggggggggggggggggggggggggggggggg",
	}))
	require.NoError(t, err)
	assert.Equal(t, testToken, got)
}

// 错误信息必须 logger-safe：不含 token 明文。
func TestResolveDrivePolicyToken_ErrorMessagesAreLoggerSafe(t *testing.T) {
	cases := map[string]func(string) string{
		"unset":     envMap(nil),
		"short":     envMap(map[string]string{driveInternalTokenEnv: "short"}),
		"collision": envMap(map[string]string{driveInternalTokenEnv: testToken, notifyInternalTokenEnv: testToken}),
	}
	for name, getenv := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := resolveDrivePolicyToken(getenv)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), testToken, "错误信息不得回显 token 明文")
		})
	}
}
