// Package bot_api · YUJ-644 / Mininglamp-OSS#33 / YUJ-660 unit tests for
// PERSONAL DM payload.space_id authoritative injection.
//
// Coverage:
//   - resolveBotActiveSpaceID branch contract (ctx fast path vs DB fallback)
//   - enrichBotPayloadWithSpaceID overrides forged client space_id
//   - Medium-2 fix: dbr.ErrNotFound 不再被当成 DB 错误（无 false-positive warn）
//   - Medium-4 fix: 用 fakeSpaceQuerier 桩 querySpaceIDByRobotID 并断言被调用
//   - R3 Finding A fix: resolver 返回 ""（任何原因）→ enrich 必须 strip client
//     上送的 payload.space_id（fail-closed），不能 preserve。具体覆盖：
//     ErrNotFound + forged client → strip；real DB error + forged client → strip。
package bot_api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
)

// fakeSpaceQuerier records the calls and returns scripted (spaceID, err) per robotID.
type fakeSpaceQuerier struct {
	calls   []string
	results map[string]struct {
		spaceID string
		err     error
	}
	defaultSpace string
	defaultErr   error
	// Mininglamp-OSS/octo-server#36 — multi-Space rows. Per-robot override of
	// the full ordered list; falls back to a single-element list built from
	// `defaultSpace` / per-robot result when absent.
	multiRows map[string][]string

	// YUJ-688 / PR#43 R1 — production-shaped authorization fake.
	//
	// `isBotSpaceAuthorized` mirrors the production OR-of-three rule from
	// modules/bot_api/db.go:
	//   (1) `memberships[robotID][spaceID] == true` → space_member match
	//   (2) `appBots[robotID].publishedPlatform == true` AND
	//       `activeSpaces[spaceID] != false` → published platform App Bot
	//       visible in target active Space
	//   (3) `appBots[robotID].scopeSpaceID == spaceID` AND
	//       `appBots[robotID].published == true` AND
	//       `activeSpaces[spaceID] != false` → scope=space App Bot in own Space
	// `activeSpaces` defaults to true for any spaceID not explicitly set to
	// false, so tests that don't care about Space status can omit it.
	memberships   map[string]map[string]bool
	authCalls     []memberCall
	authDefault   bool
	authErr       error
	appBots       map[string]appBotShape
	activeSpaces  map[string]bool
}

type appBotShape struct {
	publishedPlatform bool   // app_bot row exists with scope='platform' status=1
	scopeSpaceID      string // app_bot.space_id when scope='space' (status=1)
	published         bool   // app_bot.status=1 for the scope=space case
}

type memberCall struct {
	robotID string
	spaceID string
}

func (f *fakeSpaceQuerier) querySpaceIDByRobotID(robotID string) (string, error) {
	f.calls = append(f.calls, robotID)
	if r, ok := f.results[robotID]; ok {
		return r.spaceID, r.err
	}
	return f.defaultSpace, f.defaultErr
}

func (f *fakeSpaceQuerier) querySpaceIDsByRobotID(robotID string) (string, []string, error) {
	// Reuse single-row behaviour for err / empty handling, then layer the
	// multi-row override on top so individual tests stay readable.
	primary, err := f.querySpaceIDByRobotID(robotID)
	if err != nil {
		return "", nil, err
	}
	if rows, ok := f.multiRows[robotID]; ok {
		if len(rows) == 0 {
			return "", nil, dbr.ErrNotFound
		}
		return rows[0], rows, nil
	}
	if primary == "" {
		return "", nil, dbr.ErrNotFound
	}
	return primary, []string{primary}, nil
}

// isBotSpaceAuthorized mirrors the production OR-of-three rule. Tests pick the
// branch they want by populating `memberships` (User Bot path) or `appBots`
// (App Bot platform / scope=space path). `activeSpaces` defaults to active.
func (f *fakeSpaceQuerier) isBotSpaceAuthorized(robotID, spaceID string) (bool, error) {
	f.authCalls = append(f.authCalls, memberCall{robotID: robotID, spaceID: spaceID})
	if f.authErr != nil {
		return false, f.authErr
	}
	// Branch (1): space_member match.
	if m, ok := f.memberships[robotID]; ok {
		if v, ok := m[spaceID]; ok && v {
			return true, nil
		}
	}
	// Target Space must be active for app_bot branches. Default = active when
	// activeSpaces is nil or the entry is missing.
	spaceActive := true
	if f.activeSpaces != nil {
		if v, ok := f.activeSpaces[spaceID]; ok {
			spaceActive = v
		}
	}
	if spaceActive {
		if shape, ok := f.appBots[robotID]; ok {
			// Branch (2): published platform App Bot in any active Space.
			if shape.publishedPlatform {
				return true, nil
			}
			// Branch (3): scope=space App Bot dispatching into its own Space.
			if shape.published && shape.scopeSpaceID != "" && shape.scopeSpaceID == spaceID {
				return true, nil
			}
		}
	}
	// Fall through: explicit `memberships[robotID][spaceID] == false` or
	// nothing matches → use authDefault for tests that don't model the bot.
	if m, ok := f.memberships[robotID]; ok {
		if v, ok := m[spaceID]; ok {
			return v, nil
		}
	}
	return f.authDefault, nil
}

// fakeWkContext creates a minimal wkhttp.Context (gin context wrapper) with
// a real http.Request attached so c.GetHeader is safe.
func fakeWkContext() *wkhttp.Context {
	c, _ := gin.CreateTestContext(nil)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", nil)
	return &wkhttp.Context{Context: c}
}

// fakeWkContextWithHeader is the variant that adds an `X-Space-ID` header for
// Mininglamp-OSS/octo-server#36 Option B coverage.
func fakeWkContextWithHeader(header, value string) *wkhttp.Context {
	c, _ := gin.CreateTestContext(nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", nil)
	if value != "" {
		req.Header.Set(header, value)
	}
	c.Request = req
	return &wkhttp.Context{Context: c}
}

// newTestBotAPI builds a *BotAPI with logger wired and the given spaceQuerier
// stub injected. Avoids nil-panic when the helper calls ba.Warn / ba.Error.
func newTestBotAPI(q botSpaceQuerier) *BotAPI {
	return &BotAPI{Log: log.NewTLog("BotAPI-test"), spaceQuerier: q}
}

func TestResolveBotActiveSpaceID_AppBotScopeSpace_UsesCtxValue(t *testing.T) {
	ba := &BotAPI{}
	c := fakeWkContext()
	c.Set(CtxKeyAppBotScope, "space")
	c.Set(CtxKeyAppBotSpaceID, "spaceA")
	got := ba.resolveBotActiveSpaceID(c, "bot_robot_1")
	assert.Equal(t, "spaceA", got, "App Bot scope=space 应直接使用 ctx 写入的 SpaceID（无 DB）")
}

func TestResolveBotActiveSpaceID_AppBotScopeSpace_MissingValueFallsBackToDB(t *testing.T) {
	// Medium-4 fix：scope=space 但 ctx 缺 SpaceID 值 → 必须 fallback 到 DB。
	// 用 fakeSpaceQuerier 替换 ba.db，断言 query 被以正确 robotID 调用。
	q := &fakeSpaceQuerier{defaultSpace: "spaceFromDB"}
	ba := newTestBotAPI(q)
	c := fakeWkContext()
	c.Set(CtxKeyAppBotScope, "space")
	// CtxKeyAppBotSpaceID 故意不写入 → 必须 fallback 到 DB
	got := ba.resolveBotActiveSpaceID(c, "bot_robot_2")
	assert.Equal(t, "spaceFromDB", got)
	assert.Equal(t, []string{"bot_robot_2"}, q.calls,
		"scope=space 缺 SpaceID 时必须以 robotID fallback 调 querySpaceIDByRobotID")
}

func TestResolveBotActiveSpaceID_NonAppScope_UsesDB(t *testing.T) {
	q := &fakeSpaceQuerier{defaultSpace: "spaceUserBot"}
	ba := newTestBotAPI(q)
	c := fakeWkContext()
	// scope 不是 "space"（User Bot 或 App Bot scope=platform）
	got := ba.resolveBotActiveSpaceID(c, "user_bot_1")
	assert.Equal(t, "spaceUserBot", got)
	assert.Equal(t, []string{"user_bot_1"}, q.calls)
}

func TestResolveBotActiveSpaceID_DBErrNotFound_NoWarnNoSpace(t *testing.T) {
	// Medium-2 fix：querySpaceIDByRobotID 返回 dbr.ErrNotFound 表示 Bot 没归属
	// 任何 Space（孤儿 Bot / 非 Space 部署），不是 DB 错误。helper 必须返回 ""
	// 而不向 ba.Warn 发 false-positive DB-failure 日志。
	q := &fakeSpaceQuerier{defaultErr: dbr.ErrNotFound}
	ba := newTestBotAPI(q)
	c := fakeWkContext()
	got := ba.resolveBotActiveSpaceID(c, "orphan_bot")
	assert.Equal(t, "", got, "ErrNotFound → 空 SpaceID")
}

func TestResolveBotActiveSpaceID_DBRealError_ReturnsEmpty(t *testing.T) {
	q := &fakeSpaceQuerier{defaultErr: errors.New("connection refused")}
	ba := newTestBotAPI(q)
	c := fakeWkContext()
	got := ba.resolveBotActiveSpaceID(c, "bot_with_db_error")
	assert.Equal(t, "", got, "真实 DB 错误也返回 ''，让上层走 fail-open 不阻断发送")
}

func TestEnrichBotPayloadWithSpaceID_AppBotScopeSpace_OverridesClient(t *testing.T) {
	ba := &BotAPI{}
	c := fakeWkContext()
	c.Set(CtxKeyAppBotScope, "space")
	c.Set(CtxKeyAppBotSpaceID, "spaceAuth")
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceForged"}
	got := ba.enrichBotPayloadWithSpaceID(c, "bot_robot_1", payload)
	assert.Equal(t, "spaceAuth", got["space_id"], "PERSONAL 必须用服务端权威 SpaceID 覆盖客户端伪造值")
}

func TestEnrichBotPayloadWithSpaceID_DBPathOverridesClient(t *testing.T) {
	q := &fakeSpaceQuerier{defaultSpace: "spaceDB"}
	ba := newTestBotAPI(q)
	c := fakeWkContext()
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceForged"}
	got := ba.enrichBotPayloadWithSpaceID(c, "user_bot_1", payload)
	assert.Equal(t, "spaceDB", got["space_id"])
}

func TestEnrichBotPayloadWithSpaceID_ErrNotFound_StripsClientSpaceID(t *testing.T) {
	// YUJ-660 R3 Finding A：当 Bot 没有归属 Space（ErrNotFound），enrich 必须 strip
	// 任何 client 上送的 payload.space_id（fail-closed）。message 层的 strip 只在
	// /v1/message/send 路径生效，bot_api 路径必须独立 strip 否则 forged
	// payload.space_id 会跨 Space 派发。
	q := &fakeSpaceQuerier{defaultErr: dbr.ErrNotFound}
	ba := newTestBotAPI(q)
	c := fakeWkContext()
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceForged"}
	got := ba.enrichBotPayloadWithSpaceID(c, "orphan_bot", payload)
	_, ok := got["space_id"]
	assert.False(t, ok,
		"ErrNotFound + forged client space_id：bot_api 必须 strip，否则跨 Space 派发")
}

func TestEnrichBotPayloadWithSpaceID_OrphanBot_NoForgedClient_NoSpaceInjected(t *testing.T) {
	// 孤儿 Bot + client 未上送：不注入 space_id，发 enrich_payload_space_id_empty warn。
	q := &fakeSpaceQuerier{defaultErr: dbr.ErrNotFound}
	ba := newTestBotAPI(q)
	c := fakeWkContext()
	payload := map[string]interface{}{"content": "hi"}
	got := ba.enrichBotPayloadWithSpaceID(c, "orphan_bot", payload)
	_, ok := got["space_id"]
	assert.False(t, ok)
}

func TestEnrichBotPayloadWithSpaceID_RealDBError_StripsClientSpaceID(t *testing.T) {
	// YUJ-660 R3 Finding A：真实 DB 错误（network blip / failover）也走 strip
	// 路径，不能保留 client 上送 payload.space_id。攻击者构造 DB 错误条件 + forged
	// payload 是已知攻击面，本测试是 regression guard。
	q := &fakeSpaceQuerier{defaultErr: errors.New("connection refused")}
	ba := newTestBotAPI(q)
	c := fakeWkContext()
	payload := map[string]interface{}{"content": "hi", "space_id": "spaceVictim"}
	got := ba.enrichBotPayloadWithSpaceID(c, "bot_with_db_error", payload)
	_, ok := got["space_id"]
	assert.False(t, ok,
		"DB 错误 + forged client space_id：bot_api 必须 strip，否则攻击者借 DB blip 跨 Space 派发")
}

func TestEnrichBotPayloadWithSpaceID_NilPayloadInitialized(t *testing.T) {
	ba := &BotAPI{}
	c := fakeWkContext()
	c.Set(CtxKeyAppBotScope, "space")
	c.Set(CtxKeyAppBotSpaceID, "spaceAuth")
	got := ba.enrichBotPayloadWithSpaceID(c, "bot_robot_1", nil)
	assert.NotNil(t, got)
	assert.Equal(t, "spaceAuth", got["space_id"])
}
