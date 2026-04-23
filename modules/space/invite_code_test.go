package space

import (
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestGenerateInviteCode 验证邀请码格式：16 hex 字符（64 bit 熵），每次生成不同。
func TestGenerateInviteCode(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{16}$`)
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		code, err := generateInviteCode()
		assert.NoError(t, err)
		assert.True(t, re.MatchString(code), "code %q 不符合 16 hex 格式", code)
		if _, dup := seen[code]; dup {
			t.Fatalf("100 次生成中出现重复: %q", code)
		}
		seen[code] = struct{}{}
	}
}

// TestInviteDefaults_Default 未设置环境变量时：max_uses=0（不限），expires_at=now+72h。
func TestInviteDefaults_Default(t *testing.T) {
	t.Setenv(envInviteDefaultMaxUses, "")
	t.Setenv(envInviteDefaultTTL, "")

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	maxUses, expiresAt := inviteDefaults(now)

	assert.Equal(t, 0, maxUses)
	assert.NotNil(t, expiresAt)
	assert.Equal(t, now.Add(72*time.Hour), *expiresAt)
}

// TestInviteDefaults_EnvOverride 环境变量覆盖默认值。
func TestInviteDefaults_EnvOverride(t *testing.T) {
	t.Setenv(envInviteDefaultMaxUses, "50")
	t.Setenv(envInviteDefaultTTL, "24h")

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	maxUses, expiresAt := inviteDefaults(now)

	assert.Equal(t, 50, maxUses)
	assert.NotNil(t, expiresAt)
	assert.Equal(t, now.Add(24*time.Hour), *expiresAt)
}

// TestInviteDefaults_InvalidEnv 非法环境变量回落到默认值。
func TestInviteDefaults_InvalidEnv(t *testing.T) {
	t.Setenv(envInviteDefaultMaxUses, "not-a-number")
	t.Setenv(envInviteDefaultTTL, "bogus")

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	maxUses, expiresAt := inviteDefaults(now)

	assert.Equal(t, 0, maxUses)
	assert.NotNil(t, expiresAt)
	assert.Equal(t, now.Add(72*time.Hour), *expiresAt)
}

// TestInsertInvitationWithRetry_Success 正常生成一次成功。
func TestInsertInvitationWithRetry_Success(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)

	seedSpace(t, "inv-retry-ok", "retry ok", "u-owner", SpaceStatusNormal)

	model := &InvitationModel{
		SpaceId: "inv-retry-ok",
		Creator: "u-owner",
		Status:  1,
	}
	code, err := f.insertInvitationWithRetry(model)
	assert.NoError(t, err)
	assert.Len(t, code, 16)

	got, err := f.db.queryInvitationByCode(code)
	assert.NoError(t, err)
	assert.NotNil(t, got)
	assert.Equal(t, "inv-retry-ok", got.SpaceId)
}

// TestInsertInvitationWithRetry_CollisionRecovery 首次碰撞应自动重试并成功。
// 构造法：预先占用一个 code，让 generateInviteCodeFn mock 第一次返回该 code，第二次正常。
func TestInsertInvitationWithRetry_CollisionRecovery(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)

	seedSpace(t, "inv-collide", "collide", "u-owner", SpaceStatusNormal)

	// 预先占位 code
	taken := "aaaaaaaaaaaaaaaa"
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId: "inv-collide", InviteCode: taken, Creator: "u-owner", Status: 1,
	})
	assert.NoError(t, err)

	// mock 生成器：第一次返回已占用的，之后走真实生成
	calls := 0
	origGen := generateInviteCodeFn
	generateInviteCodeFn = func() (string, error) {
		calls++
		if calls == 1 {
			return taken, nil
		}
		return origGen()
	}
	defer func() { generateInviteCodeFn = origGen }()

	code, err := f.insertInvitationWithRetry(&InvitationModel{
		SpaceId: "inv-collide", Creator: "u-owner", Status: 1,
	})
	assert.NoError(t, err)
	assert.NotEqual(t, taken, code)
	assert.GreaterOrEqual(t, calls, 2)
}

// TestInsertInvitationWithRetry_ExhaustRetries 持续碰撞耗尽重试次数时返回错误。
func TestInsertInvitationWithRetry_ExhaustRetries(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)

	seedSpace(t, "inv-exhaust", "exhaust", "u-owner", SpaceStatusNormal)

	taken := "bbbbbbbbbbbbbbbb"
	err = f.db.insertInvitation(&InvitationModel{
		SpaceId: "inv-exhaust", InviteCode: taken, Creator: "u-owner", Status: 1,
	})
	assert.NoError(t, err)

	origGen := generateInviteCodeFn
	generateInviteCodeFn = func() (string, error) { return taken, nil }
	defer func() { generateInviteCodeFn = origGen }()

	_, err = f.insertInvitationWithRetry(&InvitationModel{
		SpaceId: "inv-exhaust", Creator: "u-owner", Status: 1,
	})
	assert.Error(t, err)
}

// TestCreateSpace_InviteCodeIs16Hex 端到端：createSpace 生成的邀请码为 16 hex 且带默认 expires_at。
func TestCreateSpace_InviteCodeIs16Hex(t *testing.T) {
	_, f, err := setup(t)
	assert.NoError(t, err)

	// 确保测试不依赖外部环境变量
	_ = os.Unsetenv(envInviteDefaultMaxUses)
	_ = os.Unsetenv(envInviteDefaultTTL)

	result, err := f.createSpaceCore(createSpaceParams{
		Creator:  "u-new-fmt",
		Name:     "fmt space",
		JoinMode: JoinModeDirect,
	})
	assert.NoError(t, err)
	assert.Len(t, result.InviteCode, 16)

	inv, err := f.db.queryInvitationByCode(result.InviteCode)
	assert.NoError(t, err)
	assert.NotNil(t, inv)
	assert.NotNil(t, inv.ExpiresAt, "默认 TTL 应写入 expires_at")
}
