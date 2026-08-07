package user

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePollSecretStore 是 pollSecretStore 的内存实现，用于在没有 Redis 的环境下对
// 安全闸门做真实行为测试（key 拼装、存摘要而非明文、写读一致、删除生效、TTL）。
type fakePollSecretStore struct {
	values map[string]string
	ttls   map[string]time.Duration
	getErr error
	setErr error
}

func newFakePollSecretStore() *fakePollSecretStore {
	return &fakePollSecretStore{values: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (f *fakePollSecretStore) SetAndExpire(key string, value interface{}, expire time.Duration) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.values[key] = value.(string)
	f.ttls[key] = expire
	return nil
}

func (f *fakePollSecretStore) GetString(key string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.values[key], nil
}

func (f *fakePollSecretStore) Del(key string) error {
	delete(f.values, key)
	delete(f.ttls, key)
	return nil
}

// ---------------------------------------------------------------------------
// 行为测试：mint / matches / delete 全链路
// ---------------------------------------------------------------------------

func TestScanLoginPollSecret_MintThenMatch(t *testing.T) {
	store := newFakePollSecretStore()
	const uuid = "uuid-1"

	secret, err := mintScanLoginPollSecret(store, uuid)
	require.NoError(t, err)
	require.NotEmpty(t, secret)

	ok, err := scanLoginPollSecretMatches(store, uuid, secret)
	require.NoError(t, err)
	assert.True(t, ok, "刚铸造的密钥必须能通过校验")
}

func TestScanLoginPollSecret_StoresDigestNotPlaintext(t *testing.T) {
	store := newFakePollSecretStore()
	const uuid = "uuid-1"

	secret, err := mintScanLoginPollSecret(store, uuid)
	require.NoError(t, err)

	stored, ok := store.values[scanLoginPollSecretKey(uuid)]
	require.True(t, ok, "必须写在 scanlogin:poll:{uuid} 这个 key 上")
	assert.NotEqual(t, secret, stored, "Redis 里不得出现明文密钥")

	want := sha256.Sum256([]byte(secret))
	assert.Equal(t, hex.EncodeToString(want[:]), stored)
	assert.Equal(t, scanLoginPollSecretTTL, store.ttls[scanLoginPollSecretKey(uuid)])
}

func TestScanLoginPollSecret_RejectsWrongAbsentAndEmpty(t *testing.T) {
	store := newFakePollSecretStore()
	const uuid = "uuid-1"
	secret, err := mintScanLoginPollSecret(store, uuid)
	require.NoError(t, err)

	cases := []struct {
		name      string
		uuid      string
		presented string
	}{
		{"错误密钥", uuid, "not-the-secret"},
		{"空密钥（未升级的旧客户端）", uuid, ""},
		{"密钥对但 uuid 不对", "uuid-other", secret},
		{"uuid 与密钥都为空", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := scanLoginPollSecretMatches(store, tc.uuid, tc.presented)
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}

func TestScanLoginPollSecret_FailsClosedOnStoreError(t *testing.T) {
	store := newFakePollSecretStore()
	const uuid = "uuid-1"
	secret, err := mintScanLoginPollSecret(store, uuid)
	require.NoError(t, err)

	store.getErr = errors.New("redis down")
	ok, err := scanLoginPollSecretMatches(store, uuid, secret)
	// 读失败必须判为「不通过」。任何 fail-open 都会把 auth_code 交给匿名轮询方。
	assert.False(t, ok, "存储故障时必须 fail-closed")
	assert.Error(t, err, "错误要回传给调用方记日志")
}

func TestScanLoginPollSecret_DeleteRevokes(t *testing.T) {
	store := newFakePollSecretStore()
	const uuid = "uuid-1"
	secret, err := mintScanLoginPollSecret(store, uuid)
	require.NoError(t, err)

	require.NoError(t, deleteScanLoginPollSecret(store, uuid))

	ok, err := scanLoginPollSecretMatches(store, uuid, secret)
	require.NoError(t, err)
	assert.False(t, ok, "兑换后密钥必须立即失效，不留残余窗口")
}

func TestScanLoginPollSecret_TTLCoversWorstCaseFlow(t *testing.T) {
	// 最坏路径：mint 后 60s 扫码（qrcode 初始 TTL）→ handleScanLogin 续 5min →
	// grantLogin 再续 5min = 660s。密钥必须活得比这久，否则扫得晚的用户拿不到
	// auth_code。上一版取 6min（=360s）零余量，扫码发生在第 59 秒时必然踩中。
	worstCase := 60*time.Second + scanLoginAuthCodeTTLMirror + scanLoginAuthCodeTTLMirror
	assert.Greater(t, scanLoginPollSecretTTL, worstCase,
		"轮询密钥 TTL 必须严格覆盖二维码状态的最长可读窗口并留余量")
}

// scanLoginAuthCodeTTLMirror 镜像 modules/qrcode.scanLoginAuthCodeTTL（跨包不可见）。
// 两者漂移时上面的 TTL 断言会失效，故在此显式标注依赖。
const scanLoginAuthCodeTTLMirror = 5 * time.Minute

// ---------------------------------------------------------------------------
// 行为测试：字段白名单
// ---------------------------------------------------------------------------

func TestFilterScanLoginPublicFields_DropsEverythingNotAllowlisted(t *testing.T) {
	out := filterScanLoginPublicFields(map[string]interface{}{
		"app_id":    "wukongchat",
		"status":    "authed",
		"uid":       "victim-uid",
		"auth_code": "the-golden-ticket",
		"encrypt":   "signal-key-material",
		// 白名单的意义：未来任何人往 payload 里加字段，默认不下发。
		"scanner_phone": "13800000000",
		"someNewField":  "whatever",
	})

	assert.Equal(t, map[string]interface{}{
		"app_id": "wukongchat",
		"status": "authed",
	}, out, "只有显式列入白名单的键可以回给未授权轮询方")
}

func TestFilterScanLoginPublicFields_AllowlistExcludesCredentials(t *testing.T) {
	for _, k := range []string{"auth_code", "uid", "encrypt"} {
		_, ok := scanLoginPublicDataKeys[k]
		assert.False(t, ok, "%q 是凭据/身份字段，绝不可进白名单", k)
	}
}

func TestFilterScanLoginPublicFields_DoesNotMutateInput(t *testing.T) {
	in := map[string]interface{}{"status": "authed", "auth_code": "abc"}
	_ = filterScanLoginPublicFields(in)
	// 同一个 *common.QRCodeModel 会被长轮询 channel 广播给多个等待者，
	// 原地删除会污染持有正确密钥那一方的视图。
	assert.Equal(t, "abc", in["auth_code"], "过滤必须返回拷贝，不得修改入参")
}

func TestFilterScanLoginPublicFields_NilPassthrough(t *testing.T) {
	assert.Nil(t, filterScanLoginPublicFields(nil))
}

func TestHashScanLoginPollSecret_IsDeterministicSHA256Hex(t *testing.T) {
	const secret = "a-poll-secret"
	want := sha256.Sum256([]byte(secret))
	got := hashScanLoginPollSecret(secret)

	assert.Equal(t, hex.EncodeToString(want[:]), got)
	assert.Equal(t, got, hashScanLoginPollSecret(secret), "同一输入必须稳定")
	assert.NotEqual(t, got, hashScanLoginPollSecret(secret+"x"))
}

// ---------------------------------------------------------------------------
// 行为测试：来源上下文
// ---------------------------------------------------------------------------

func TestScanLoginOrigin_RoundTrip(t *testing.T) {
	store := newFakePollSecretStore()
	const uuid = "uuid-1"
	want := ScanLoginOrigin{IP: "203.0.113.9", DeviceName: "Chrome", DeviceModel: "Windows", UserAgent: "UA/1"}

	require.NoError(t, storeScanLoginOrigin(store, uuid, want))

	got, err := LoadScanLoginOrigin(store, uuid)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want, *got)
}

func TestScanLoginOrigin_MissingIsNotAnError(t *testing.T) {
	// 上下文缺失只该退化为「确认页不展示」，不能阻断扫码。
	got, err := LoadScanLoginOrigin(newFakePollSecretStore(), "absent")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// ---------------------------------------------------------------------------
// 源码契约锁：只锁「无法在无 Redis 环境端到端跑」的 handler 结构不变量
// ---------------------------------------------------------------------------

func readFuncBody(t *testing.T, path string, sig string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(src)
	start := strings.Index(body, sig)
	require.NotEqual(t, -1, start, "%s 必须存在于 %s", sig, path)
	rest := body[start+len(sig):]
	end := strings.Index(rest, "\nfunc ")
	if end == -1 {
		return body[start:]
	}
	return body[start : start+len(sig)+end]
}

func TestGetLoginUUID_KeepsPollSecretOutOfQRCodePayload(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) getLoginUUID(")

	assert.Contains(t, fn, "u.mintScanLoginPollSecret(uuid)")
	assert.Contains(t, fn, `"poll_secret": pollSecret`)

	// 核心不变量：密钥绝不能进 qrcode:{uuid} 的 payload —— 那份 payload 正是
	// getloginStatus 要回显给匿名轮询方的内容。
	modelStart := strings.Index(fn, "common.NewQRCodeModel(")
	require.NotEqual(t, -1, modelStart)
	modelEnd := strings.Index(fn[modelStart:], "}))")
	require.NotEqual(t, -1, modelEnd)
	payload := fn[modelStart : modelStart+modelEnd]
	for _, banned := range []string{"poll_secret", "pollSecret", "Secret"} {
		assert.NotContains(t, payload, banned,
			"密钥不得写入 qrcode payload —— 会被 getloginStatus 原样回显")
	}
}

// TestGetLoginStatus_DataOnlyLeavesThroughTheGate 锁住「所有 Data 出口都经过
// respondStatus」。用「respondStatus 闭包之外不得出现 .Data」来表达，而不是禁止某几个
// 具体变量名的字符串 —— 后者改个变量名就能绕过，等于没锁。
func TestGetLoginStatus_DataOnlyLeavesThroughTheGate(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) getloginStatus(")

	assert.Contains(t, fn, "u.scanLoginPollSecretMatches(uuid, c.GetHeader(ScanLoginPollSecretHeader))",
		"轮询密钥必须从请求头读取，不能走 query（会被 access log 记录）")
	assert.Contains(t, fn, "filterScanLoginPublicFields(model.Data)")

	closureStart := strings.Index(fn, "respondStatus := func(")
	require.NotEqual(t, -1, closureStart, "必须存在 respondStatus 闭包")
	closureEnd := strings.Index(fn[closureStart:], "\n\t}\n")
	require.NotEqual(t, -1, closureEnd)
	outside := fn[:closureStart] + fn[closureStart+closureEnd:]

	assert.NotContains(t, outside, ".Data",
		"二维码状态只能经 respondStatus 下发；闭包之外出现 .Data 意味着绕过了密钥校验")
}

func TestGetLoginStatus_ReleasesOnDisconnectAndOwnsItsChannel(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) getloginStatus(")

	assert.Contains(t, fn, "case <-c.Request.Context().Done():",
		"长轮询必须在客户端断开时立即释放")
	assert.Contains(t, fn, "defer timeout.Stop()")
	// 必须只回收自己注册的 channel：removeQRCodeChan(uuid) 会关掉 map 里当前那个，
	// 而该接口未认证 —— 任何知道 uuid 的人反复连了就断即可踢掉合法轮询方。
	assert.NotContains(t, fn, "u.removeQRCodeChan(uuid)",
		"必须用带归属校验的 removeQRCodeChanOwned")
	assert.Contains(t, fn, "u.removeQRCodeChanOwned(uuid, qrcodeChan)")
}

func TestLoginWithAuthCode_RevokesPollSecret(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) loginWithAuthCode(")
	assert.Contains(t, fn, "u.deleteScanLoginPollSecret(uuid)",
		"兑换完成后必须吊销轮询密钥，与删除 authCode 同理")
}

func TestScanLoginRoutesCarryStrictIPRateLimit(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	assert.Contains(t, body, `v.GET("/user/loginuuid", scanLoginUUIDLimit, u.getLoginUUID)`)
	assert.Contains(t, body, `v.GET("/user/loginstatus", scanLoginStatusLimit, u.getloginStatus)`)
	assert.Contains(t, body, `"scanlogin_uuid"`)
	assert.Contains(t, body, `"scanlogin_status"`)
}
