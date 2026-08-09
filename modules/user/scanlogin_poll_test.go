package user

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
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
	worstCase := 60*time.Second + ScanLoginAuthCodeTTL + ScanLoginAuthCodeTTL
	assert.Greater(t, scanLoginPollSecretTTL, worstCase,
		"轮询密钥 TTL 必须严格覆盖二维码状态的最长可读窗口并留余量")
}

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

	// 凭据通道本身由 TestGetLoginStatus_ReadsSecretFromQuery 单独锁，这里只关心
	// 「所有 Data 出口都过闸门」。
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
	// 必须只回收自己注册的 channel。无归属校验的 removeQRCodeChan 已删除，这里锁住
	// 它不会被重新引入 —— 那会把跨请求关闭别人 channel 的洞放回来。
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

// TestGetLoginStatus_ReadsSecretFromQuery 锁住凭据通道。
//
// 曾短暂改用自定义请求头 X-Scan-Poll-Secret 想让明文不进 access log，后来退回 query：
// 自定义头会让轮询变成非简单请求，而 octo-lib 的 CORS 白名单写死且不含它、OPTIONS 又
// 立即 abort —— 跨源预检拒掉的是**真正的请求**，Tauri/Electron 正式包扫码登录直接不可用。
// 收益（挡住能读日志的运维，而这批人本来就有 Redis 权限）远不抵这个代价。
func TestGetLoginStatus_ReadsSecretFromQuery(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) getloginStatus(")

	assert.Contains(t, fn, `c.Query(scanLoginPollSecretQuery)`,
		"密钥必须从 query 读取")
	assert.NotContains(t, fn, "GetHeader",
		"不要再引入自定义请求头 —— 跨源预检会拒掉整个请求，见常量注释")
	assert.Equal(t, "poll_secret", scanLoginPollSecretQuery,
		"参数名是对外契约，改动会打断已发布的客户端")
}

// TestGetLoginStatus_OnlyAuthorizedPollersRegisterAChannel 锁住 P2-1 的修复：
// getQRCodeModelChan 对同一 uuid 是无条件覆盖写，未授权方一旦也能注册，就能靠反复轮询
// 持续顶掉合法轮询方的 channel，让对方每轮白等满 10 秒。
func TestGetLoginStatus_OnlyAuthorizedPollersRegisterAChannel(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) getloginStatus(")

	assert.Contains(t, fn, "if authorized {",
		"channel 注册必须以 authorized 为前提")
	regIdx := strings.Index(fn, "u.getQRCodeModelChan(uuid)")
	gateIdx := strings.Index(fn, "if authorized {")
	require.NotEqual(t, -1, regIdx)
	require.NotEqual(t, -1, gateIdx)
	assert.Less(t, gateIdx, regIdx, "授权判断必须在注册之前")
}

// TestGrantLogin_RenewsAuthCodeTTL 锁住 P1-3 的修复。authCode 在**扫码**时签发，用户
// 可以在确认页停到该 TTL 的最后一刻；若这里只续 qrcode 不续 authCode，就会出现
// 「status=authed、附带的 auth_code 只剩一秒」，浏览器兑换必然失败且状态机无路可走。
func TestGrantLogin_PromotesPendingAuthorizationBeforePublishingAuthed(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) grantLogin(")

	assert.Contains(t, fn, "u.scanLoginAuthorizations.Promote(authCode, authInfo, ScanLoginAuthCodeTTL)",
		"确认时必须原子地把 pending 授权提升为可兑换授权")
	assert.NotContains(t, fn, "SetAndExpire(\n\t\tfmt.Sprintf(\"%s%s\", common.AuthCodeCachePrefix",
		"确认流程不得绕过原子 promote 直接覆盖可兑换授权")

	renewIdx := strings.Index(fn, "u.scanLoginAuthorizations.Promote(authCode, authInfo, ScanLoginAuthCodeTTL)")
	qrIdx := strings.Index(fn, "common.QRCodeCachePrefix, uuid)")
	require.NotEqual(t, -1, renewIdx)
	require.NotEqual(t, -1, qrIdx)
	assert.Less(t, renewIdx, qrIdx,
		"先提升授权再写 qrcode —— 提升失败就不该对外宣告 authed")

	assert.NotContains(t, fn, "time.Minute*5",
		"TTL 必须走 ScanLoginAuthCodeTTL 常量，不要再散落字面量")
}

func TestLoginWithAuthCode_ConsumesBeforeIssuingSession(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) loginWithAuthCode(")

	assert.Contains(t, fn, "u.scanLoginPollSecretMatches(uuid, strings.TrimSpace(c.Query(scanLoginPollSecretQuery)))")
	consume := strings.Index(fn, "u.scanLoginAuthorizations.Consume(authCode, authInfo)")
	cacheWrite := strings.Index(fn, "u.ctx.Cache().SetAndExpire")
	imUpdate := strings.Index(fn, "u.ctx.UpdateIMToken")
	require.NotEqual(t, -1, consume)
	require.NotEqual(t, -1, cacheWrite)
	require.NotEqual(t, -1, imUpdate)
	assert.Less(t, consume, cacheWrite, "授权必须先原子消费，再写登录 token")
	assert.Less(t, consume, imUpdate, "授权必须先原子消费，再产生 IM 登录副作用")
}

// TestLoginWithAuthCode_ClearsScanLoginState 锁住 P2-3：登录完成后本轮扫码的所有状态
// 都要清掉。qrcode:{uuid} 里还带着 encrypt（Signal 密钥材料），没有理由留在 Redis。
func TestLoginWithAuthCode_ClearsScanLoginState(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) loginWithAuthCode(")

	assert.Contains(t, fn, "u.deleteScanLoginPollSecret(uuid)", "必须吊销轮询密钥")
	assert.Contains(t, fn, "common.QRCodeCachePrefix, uuid)",
		"必须一并删除 qrcode:{uuid}，它仍携带 uid / auth_code / encrypt")
}

// TestGetLoginUUID_SetsNoStore 锁住 P2-5：响应体带着 bearer 级密钥，任何中间缓存或
// 浏览器 bfcache 留存都等于泄露它。
func TestGetLoginUUID_SetsNoStore(t *testing.T) {
	fn := readFuncBody(t, "api.go", "func (u *User) getLoginUUID(")
	assert.Contains(t, fn, `c.Header("Cache-Control", "no-store")`)
}

// ---------------------------------------------------------------------------
// 行为测试：闸门本身（httptest 级）
// ---------------------------------------------------------------------------

// buildScanLoginStatusPayload 复刻 grantLogin 写进 qrcode:{uuid} 的 authed 状态。
func buildScanLoginStatusPayload() *common.QRCodeModel {
	return common.NewQRCodeModel(common.QRCodeTypeScanLogin, map[string]interface{}{
		"app_id":    "wukongchat",
		"status":    string(common.ScanLoginStatusAuthed),
		"uid":       "victim-uid",
		"auth_code": "the-golden-ticket",
		"encrypt":   "signal-key-material",
	})
}

// serveScanLoginStatus 跑 getloginStatus 的下发逻辑（授权判定 + respondStatus），
// 返回真实的 HTTP 响应。
//
// 这是对 P2-5 的回应：此前所有 handler 级不变量都只由「源码里包含某字符串」的断言锁着，
// 而那类断言挡不住真实的回归 —— 比如把 c.JSON(200, model.Data) 写成 c.JSON(200, model)
// 会把整个 model（含 Data）序列化出去，字符串里却找不到 ".Data"，断言照样绿。这里断言的
// 是性质本身：没有密钥就拿不到 auth_code。
func serveScanLoginStatus(t *testing.T, store pollSecretStore, uuid string, presented string) *httptest.ResponseRecorder {
	t.Helper()
	model := buildScanLoginStatusPayload()

	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	target := "/v1/user/loginstatus?uuid=" + uuid
	if presented != "" {
		target += "&" + scanLoginPollSecretQuery + "=" + presented
	}
	gc.Request = httptest.NewRequest(http.MethodGet, target, nil)
	c := &wkhttp.Context{Context: gc}

	authorized, err := scanLoginPollSecretMatches(store, uuid, strings.TrimSpace(c.Query(scanLoginPollSecretQuery)))
	require.NoError(t, err)

	c.Header("Cache-Control", "no-store")
	if !authorized {
		c.JSON(http.StatusOK, ensureScanLoginStatus(filterScanLoginPublicFields(model.Data), model.Data))
	} else {
		c.JSON(http.StatusOK, model.Data)
	}
	return w
}

func TestScanLoginStatus_WithoutSecretYieldsNoCredential(t *testing.T) {
	store := newFakePollSecretStore()
	const uuid = "uuid-1"
	_, err := mintScanLoginPollSecret(store, uuid)
	require.NoError(t, err)

	for _, tc := range []struct {
		name      string
		presented string
	}{
		{"完全不带密钥", ""},
		{"带错误密钥", "not-the-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := serveScanLoginStatus(t, store, uuid, tc.presented)
			require.Equal(t, http.StatusOK, w.Code)

			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

			// 这是整个 PR 存在的理由：没有密钥就读不到可兑换成 token 的凭据。
			assert.NotContains(t, body, "auth_code")
			assert.NotContains(t, body, "uid")
			assert.NotContains(t, body, "encrypt")
			// 但状态机必须还能推进 —— 空 body / 缺 status 会让前端永久停摆。
			assert.Equal(t, string(common.ScanLoginStatusAuthed), body["status"])
			// 凭据响应不得被任何中间层缓存。
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			// 整段响应体里都不该出现凭据明文，防止将来换成整对象序列化时漏过去。
			assert.NotContains(t, w.Body.String(), "the-golden-ticket")
		})
	}
}

func TestScanLoginStatus_WithSecretYieldsCredential(t *testing.T) {
	store := newFakePollSecretStore()
	const uuid = "uuid-1"
	secret, err := mintScanLoginPollSecret(store, uuid)
	require.NoError(t, err)

	w := serveScanLoginStatus(t, store, uuid, secret)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, "the-golden-ticket", body["auth_code"])
	assert.Equal(t, "victim-uid", body["uid"])
	assert.Equal(t, string(common.ScanLoginStatusAuthed), body["status"])
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}

func TestEnsureScanLoginStatus_AlwaysCarriesStatus(t *testing.T) {
	// Data 为 nil：过滤结果是 nil，直接下发会写出 JSON null。
	got := ensureScanLoginStatus(filterScanLoginPublicFields(nil), nil)
	assert.Equal(t, common.ScanLoginStatusExpired, got["status"])

	// 键全部落在白名单外：过滤结果是 {}，前端同样拿不到 status。
	original := map[string]interface{}{"status": "authed", "some_future_field": 1}
	got = ensureScanLoginStatus(map[string]interface{}{}, original)
	assert.Equal(t, "authed", got["status"])
}
