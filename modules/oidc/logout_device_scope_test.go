package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/gin-gonic/gin"
)

// 登出只应断开发起登出的那一端(brief Decision 3)。
//
// 这个行为曾经是坏的,而且坏得很隐蔽:旧实现把 raw token 字符串直接喂给
// auth.Decode 想读出 device_flag,但 raw token 是 UUID、不含任何结构化字段,
// 解析必然失败 → 一律落到 df==0 的兜底分支 → 每次登出都踢掉用户全部设备。
// 手机上点一下退出,桌面端跟着掉线。
//
// 所以这里断言的不是"踢了谁",而是**踢的范围**:KickDevice 与 Kick 必须
// 严格二选一,且携带正确的 device_flag。
// -----------------------------------------------------------------------------

// stubTokenStore 同时实现 currentTokenInvalidator 与 auth.TokenRecordReader,
// 让测试能控制"从 session 缓存里读回来的 payload"这一个输入。
type stubTokenStore struct {
	// records key 是完整缓存 key(prefix + token)。
	records map[string]auth.TokenRecord
	readErr error

	invalidated []string
}

func (s *stubTokenStore) InvalidateCurrentToken(_ context.Context, _, token string) error {
	s.invalidated = append(s.invalidated, token)
	return nil
}

func (s *stubTokenStore) ReadToken(_ context.Context, key string) (auth.TokenRecord, error) {
	if s.readErr != nil {
		return auth.TokenRecord{}, s.readErr
	}
	rec, ok := s.records[key]
	if !ok {
		return auth.TokenRecord{}, errors.New("not found")
	}
	return rec, nil
}

// newLogoutFixture 装一个只关心踢线范围的 logout 环境。
func newLogoutFixture(t *testing.T, store *stubTokenStore) (*OIDC, *fakeKiller, *gin.Engine) {
	t.Helper()
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	killer := &fakeKiller{}
	o.killer = killer
	o.revoker = &fakeRevoker{}
	o.audit = newFakeAudit()
	o.tokenKill = store

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/logout", func(c *gin.Context) {
		c.Set("uid", "u-multi-device")
		o.logout(wrapWk(c))
	})
	return o, killer, r
}

// encodeV3SessionPayload 造一份带 device_flag 的 v3 session payload。
//
// 必须是 v3:v2 的编码只写 uid 与 name 两个字段(见 auth.Encode),device_flag
// 根本不在序列化结果里。这不是测试细节,而是一条产品行为边界 ——
// 见 TestAPI_Logout_V2SessionCannotScopeToOneDevice。
func encodeV3SessionPayload(t *testing.T, uid string, deviceFlag int) string {
	t.Helper()
	payload, err := auth.EncodeV3(auth.TokenInfo{
		UID:        uid,
		Name:       "multi-device-user",
		DeviceFlag: deviceFlag,
		IssuedAt:   time.Now().Add(-time.Minute).Unix(),
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
		// v3 强制要求这两项(EncodeV3 会校验),取值对本测试无影响。
		SessionGeneration: "gen-test",
		SessionRevision:   1,
	})
	if err != nil {
		t.Fatalf("auth.EncodeV3: %v", err)
	}
	return payload
}

// encodeV2SessionPayload 造一份 v2 session payload(不含 device_flag)。
func encodeV2SessionPayload(t *testing.T, uid string) string {
	t.Helper()
	payload, err := auth.Encode(auth.TokenInfo{
		UID:        uid,
		Name:       "multi-device-user",
		DeviceFlag: 2, // 会被 v2 编码丢弃,故意传一个非零值证明这一点
	})
	if err != nil {
		t.Fatalf("auth.Encode: %v", err)
	}
	return payload
}

func TestAPI_Logout_KicksOnlyTheCallingDevice(t *testing.T) {
	// 两端在线:PC(device_flag=2)与 APP(device_flag=0 在 dmwork 里是 APP)。
	// 从 PC 发起登出,APP 必须留在线上。
	const pcDeviceFlag = 2
	const token = "tok-from-pc"

	store := &stubTokenStore{records: map[string]auth.TokenRecord{
		"token:" + token: {Payload: encodeV3SessionPayload(t, "u-multi-device", pcDeviceFlag)},
	}}
	_, killer, r := newLogoutFixture(t, store)

	req := httptest.NewRequest("POST", "/logout", nil)
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// 核心断言:走按端踢,且 device_flag 是发起方那一端。
	devKicks := killer.deviceSnapshot()
	if len(devKicks) != 1 {
		t.Fatalf("KickDevice calls = %d, want 1 (got %+v)", len(devKicks), devKicks)
	}
	if devKicks[0].UID != "u-multi-device" || devKicks[0].DeviceFlag != pcDeviceFlag {
		t.Errorf("KickDevice = %+v, want {u-multi-device %d}", devKicks[0], pcDeviceFlag)
	}

	// 反面断言同等重要:一旦这里非空,就说明其他端也被踢了。
	if all := killer.snapshot(); len(all) != 0 {
		t.Errorf("Kick(all devices) was called with %v; logging out one device "+
			"must not disconnect the user's other sessions", all)
	}

	// 当前端的 HTTP bearer 仍然要作废 —— 收窄的是 IM 踢线范围,不是 token 吊销。
	if len(store.invalidated) != 1 || store.invalidated[0] != token {
		t.Errorf("invalidated tokens = %v, want [%s]", store.invalidated, token)
	}
}

// 解析不出 device_flag 时必须降级为踢全部,不能变成 no-op。
//
// 宁可多踢:登出的语义底线是"这个凭据之后不能再用",做不到精确就必须做保守。
func TestAPI_Logout_FallsBackToAllDevicesWhenFlagUnknown(t *testing.T) {
	cases := map[string]*stubTokenStore{
		// 缓存里没有这个 token(已过期/被清)
		"cache miss": {records: map[string]auth.TokenRecord{}},
		// 读缓存报错
		"read error": {readErr: errors.New("redis down")},
		// payload 是空串
		"empty payload": {records: map[string]auth.TokenRecord{
			"token:tok-x": {Payload: "   "},
		}},
		// payload 不可解析
		"undecodable payload": {records: map[string]auth.TokenRecord{
			"token:tok-x": {Payload: "not-a-valid-payload"},
		}},
	}
	for name, store := range cases {
		t.Run(name, func(t *testing.T) {
			_, killer, r := newLogoutFixture(t, store)
			req := httptest.NewRequest("POST", "/logout", nil)
			req.Header.Set("token", "tok-x")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if all := killer.snapshot(); len(all) != 1 || all[0] != "u-multi-device" {
				t.Errorf("Kick(all) = %v, want [u-multi-device]; an unresolvable "+
					"device flag must fall back to kicking every device", all)
			}
			if dev := killer.deviceSnapshot(); len(dev) != 0 {
				t.Errorf("KickDevice = %+v, want none when the flag is unknown", dev)
			}
		})
	}
}

// 没带 token 头时同样降级踢全部(而不是把 df=0 当成"APP 端"去精确踢)。
func TestAPI_Logout_NoTokenHeaderKicksAllDevices(t *testing.T) {
	_, killer, r := newLogoutFixture(t, &stubTokenStore{records: map[string]auth.TokenRecord{}})
	req := httptest.NewRequest("POST", "/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if all := killer.snapshot(); len(all) != 1 {
		t.Errorf("Kick(all) calls = %d, want 1", len(all))
	}
	if dev := killer.deviceSnapshot(); len(dev) != 0 {
		t.Errorf("KickDevice = %+v, want none", dev)
	}
}

// 踢线失败不能阻断 logout:handler 仍返 200(best-effort 语义),
// 否则客户端会以为没登出成功而重试,反而放大故障。
func TestAPI_Logout_DeviceKickFailureStillReturns200(t *testing.T) {
	const token = "tok-kick-fail"
	store := &stubTokenStore{records: map[string]auth.TokenRecord{
		"token:" + token: {Payload: encodeV3SessionPayload(t, "u-multi-device", 2)},
	}}
	_, killer, r := newLogoutFixture(t, store)
	killer.deviceErr = errors.New("IM unreachable")

	req := httptest.NewRequest("POST", "/logout", nil)
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when the IM kick fails", w.Code)
	}
	if dev := killer.deviceSnapshot(); len(dev) != 1 {
		t.Fatalf("KickDevice calls = %d, want 1", len(dev))
	}
}

// v2 session 下无法把登出收窄到单端 —— 这是编码格式决定的,不是 bug,
// 但它是一条必须被记录下来的行为边界。
//
// auth.Encode(v2) 只序列化 uid 与 name;device_flag 不在 payload 里,所以
// deviceFlagFromRequest 必然解不出来,登出会降级为踢全部设备。也就是说
// "登出仅当前端" 这个特性**只在 v3 session 生效**。部署时如果某个环境还在
// 发 v2 session,那里的用户看到的仍然是"退一个端、全部掉线"。
//
// 断言这个行为而不是修它:降级方向是安全的(宁可多踢),而让 v2 也带
// device_flag 属于 session 编码的演进,不该由 oidc 模块推动。
func TestAPI_Logout_V2SessionCannotScopeToOneDevice(t *testing.T) {
	const token = "tok-v2-session"
	store := &stubTokenStore{records: map[string]auth.TokenRecord{
		"token:" + token: {Payload: encodeV2SessionPayload(t, "u-multi-device")},
	}}
	_, killer, r := newLogoutFixture(t, store)

	req := httptest.NewRequest("POST", "/logout", nil)
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if all := killer.snapshot(); len(all) != 1 || all[0] != "u-multi-device" {
		t.Errorf("Kick(all) = %v, want [u-multi-device]: a v2 payload carries no "+
			"device_flag, so logout must fall back to kicking every device", all)
	}
	if dev := killer.deviceSnapshot(); len(dev) != 0 {
		t.Errorf("KickDevice = %+v, want none for a v2 session", dev)
	}
}

// tokenKill 不实现 TokenRecordReader 时同样降级踢全部。
//
// deviceFlagFromRequest 靠类型断言拿 reader;换一个不满足该接口的 session store
// 实现(或测试桩)就会走到这里。断言它降级而不是 panic。
func TestAPI_Logout_NonReaderTokenStoreKicksAllDevices(t *testing.T) {
	mp := NewMockProvider(t)
	o := newTestOIDC(t, mp, &fakeUserLookup{}, newFakeIdentityStore())
	killer := &fakeKiller{}
	o.killer = killer
	o.revoker = &fakeRevoker{}
	o.audit = newFakeAudit()
	// 只实现 currentTokenInvalidator,不实现 auth.TokenRecordReader。
	o.tokenKill = invalidatorOnly{}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/logout", func(c *gin.Context) {
		c.Set("uid", "u-multi-device")
		o.logout(wrapWk(c))
	})
	req := httptest.NewRequest("POST", "/logout", nil)
	req.Header.Set("token", "tok-any")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if all := killer.snapshot(); len(all) != 1 {
		t.Errorf("Kick(all) calls = %d, want 1", len(all))
	}
	if dev := killer.deviceSnapshot(); len(dev) != 0 {
		t.Errorf("KickDevice = %+v, want none", dev)
	}
}

// invalidatorOnly 只实现 currentTokenInvalidator。
type invalidatorOnly struct{}

func (invalidatorOnly) InvalidateCurrentToken(_ context.Context, _, _ string) error { return nil }
