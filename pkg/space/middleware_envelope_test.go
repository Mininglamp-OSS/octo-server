package space

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
)

// 这一组用例断言的是**响应体**，不是状态码。
//
// 既有的中间件用例（middleware_test.go）只看状态码，所以这次把三个出口从
// c.AbortWithStatusJSON 迁到错误信封时，它们全程是绿的——包括 msg 文案变化这一条
// 客户端可见的行为改动。信封迁移的全部意义就在响应体上，不断言响应体等于没测。
//
// 用真实的 i18n 渲染器而不是 wkhttp 的 defaultErrorRenderer：后者是给裸 Context
// 兜底的，形状与线上不同，拿它断言等于在测一个生产环境不存在的渲染器。

// envelopeForTest 只解出断言需要的字段。
type envelopeForTest struct {
	Msg    string `json:"msg"`
	Status int    `json:"status"`
	Error  struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

// setupRenderedRouter 建一个装了真实 i18n 渲染器的路由，并挂上本中间件。
// uid 为空串时模拟未登录（不设置 uid），用来命中 401 分支。
//
// 装配顺序照抄 main.go：SetErrorRenderer（:139）+ UseGin(EarlyMiddleware)（:180），
// 且 EarlyMiddleware 在任何模块注册路由之前。顺序是有意义的而非摆设——
// EarlyMiddleware 负责把协商出的语言放进 request context，渲染器再从那里读
// （renderer.go:35）。少挂它，本中间件的错误响应会全部回落到默认语言，而这里的
// 用例仍然全绿：那样就等于把「文案随 Accept-Language 变化」这条声明测没了。
func setupRenderedRouter(checker MembershipChecker, uid string) *wkhttp.WKHttp {
	testCache = NewInMemoryMembershipCache()
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.UseGin(i18n.EarlyMiddleware(i18n.MiddlewareOptions{
		DefaultLanguage: i18n.DefaultLanguage,
	}))
	r.Use(func(c *wkhttp.Context) {
		if uid != "" {
			c.Set("uid", uid)
			c.Set("name", "Test")
		}
		c.Next()
	})
	r.Use(spaceMiddleware(checker, testCache))
	r.GET("/test", func(c *wkhttp.Context) {
		c.JSON(http.StatusOK, map[string]interface{}{"ok": true})
	})
	return r
}

func doRendered(t *testing.T, r *wkhttp.WKHttp, path string) (*httptest.ResponseRecorder, envelopeForTest) {
	t.Helper()
	return doRenderedLang(t, r, path, "")
}

func doRenderedLang(t *testing.T, r *wkhttp.WKHttp, path, acceptLang string) (*httptest.ResponseRecorder, envelopeForTest) {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	if acceptLang != "" {
		req.Header.Set("Accept-Language", acceptLang)
	}
	r.ServeHTTP(w, req)
	var env envelopeForTest
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "响应体应当是 JSON 信封: %s", w.Body.String())
	return w, env
}

// TestSpaceMiddlewareEnvelope_Unauthenticated 未登录出口。
func TestSpaceMiddlewareEnvelope_Unauthenticated(t *testing.T) {
	r := setupRenderedRouter(func(spaceID, uid string) (bool, error) {
		t.Fatal("未登录时不该查库")
		return false, nil
	}, "")

	w, env := doRendered(t, r, "/test?space_id=sp1")

	// 线路状态必须保持 401：既有客户端按状态码分支，钉死成 400（ResponseErrorL 的
	// D14 兼容行为）会打断它们，这正是这里用 WithStatus 的原因。
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "err.shared.auth.required", env.Error.Code)
	assert.Equal(t, http.StatusUnauthorized, env.Error.HTTPStatus)
	assert.Equal(t, http.StatusUnauthorized, env.Status, "legacy status 字段与线路状态一致")
	assert.Equal(t, env.Error.Message, env.Msg, "legacy msg 与 error.message 同源")
	// 钉死确切文案。这次迁移**改变了** msg 的值——字段还在，但内容由错误码的本地化
	// 文案重算，不是原样透传旧的硬编码串（原来是 "请先登录"，没有叹号）。
	// PR 描述里曾把「字段还在」当成「值不变」写成纯增量，就是因为没有任何断言看着它。
	assert.Equal(t, "请先登录！", env.Msg)
}

// TestSpaceMiddlewareEnvelope_NotMember 非成员出口。
func TestSpaceMiddlewareEnvelope_NotMember(t *testing.T) {
	r := setupRenderedRouter(func(spaceID, uid string) (bool, error) {
		return false, nil
	}, "testuser")

	w, env := doRendered(t, r, "/test?space_id=sp1")

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "err.server.space.not_member", env.Error.Code)
	assert.Equal(t, http.StatusForbidden, env.Error.HTTPStatus)
	assert.Equal(t, "你不是该空间成员。", env.Msg, `迁移前是 "无权访问该 Space"`)
	assert.NotContains(t, w.Body.String(), "sp1",
		"错误体不该回显 space_id：非成员连该 Space 是否存在都不该被证实")
}

// TestSpaceMiddlewareEnvelope_NotMemberFromCache 否定缓存命中走的是另一个出口，
// 形状必须与查库那条一致。两条分支各写各的响应，正是会漂移的地方。
func TestSpaceMiddlewareEnvelope_NotMemberFromCache(t *testing.T) {
	calls := 0
	r := setupRenderedRouter(func(spaceID, uid string) (bool, error) {
		calls++
		return false, nil
	}, "testuser")

	_, first := doRendered(t, r, "/test?space_id=sp1")
	w, second := doRendered(t, r, "/test?space_id=sp1")

	assert.Equal(t, 1, calls, "第二次应命中否定缓存，不再查库")
	assert.Equal(t, http.StatusForbidden, w.Code)
	// 断言绝对值，不只断两次相等：只比 first == second 的话，把两个出口一起改回
	// 裸 AbortWithStatusJSON 时两边同样相等，这条用例会照常通过——它能发现漂移，
	// 却发现不了同步的回退。
	assert.Equal(t, "err.server.space.not_member", second.Error.Code)
	assert.Equal(t, first, second, "缓存命中与查库两条出口的响应体必须完全一致")
}

// TestSpaceMiddlewareEnvelope_AcceptLanguage 钉住「文案随请求语言变化」。
//
// 这是迁移带来的实质收益之一：迁移前三条出口都是硬编码中文，无论客户端要什么语言。
// 也是一条装配用例——它只有在 EarlyMiddleware 真的排在本中间件之前时才通过，
// 所以能挡住「把 EarlyMiddleware 挪到路由注册之后」这类顺序回退。
func TestSpaceMiddlewareEnvelope_AcceptLanguage(t *testing.T) {
	newRouter := func() *wkhttp.WKHttp {
		return setupRenderedRouter(func(spaceID, uid string) (bool, error) {
			return false, nil
		}, "testuser")
	}

	w, env := doRenderedLang(t, newRouter(), "/test?space_id=sp1", "en-US")
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "err.server.space.not_member", env.Error.Code, "错误码不随语言变")
	assert.Equal(t, "You are not a member of this space.", env.Msg)
	assert.Equal(t, "en-US", w.Header().Get("Content-Language"))

	// 不支持的语言回落到默认，而不是原样透传或返回空文案。
	_, zh := doRenderedLang(t, newRouter(), "/test?space_id=sp1", "fr-FR")
	assert.Equal(t, "你不是该空间成员。", zh.Msg)
}

// TestSpaceMiddlewareEnvelope_CheckerError 校验失败出口。
//
// 这条是三个里客户端可见变化最大的：ErrSpaceQueryFailed 带 Internal=true，渲染器
// 会把消息短路成通用文案，原来那句「校验 Space 成员身份失败」不再出现在响应体里。
// 按仓库的 Internal 约定这是对的（原因只进日志），但它是行为改动，必须被钉住。
func TestSpaceMiddlewareEnvelope_CheckerError(t *testing.T) {
	r := setupRenderedRouter(func(spaceID, uid string) (bool, error) {
		return false, errors.New("db exploded")
	}, "testuser")

	w, env := doRendered(t, r, "/test?space_id=sp1")

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, http.StatusInternalServerError, env.Error.HTTPStatus)
	assert.NotContains(t, w.Body.String(), "db exploded", "内部原因不得出现在响应体里")
	assert.Equal(t, "服务器内部错误。", env.Msg, `迁移前是 "校验 Space 成员身份失败"`)
	assert.NotContains(t, w.Body.String(), "校验 Space 成员身份失败",
		"Internal=true 时消息被短路成通用文案，这是本次迁移的既定行为改动")
}

// TestSpaceMiddlewareEnvelope_MemberPassesThrough 成功路径不受影响：中间件放行，
// handler 自己的 200 响应体原样返回，没有被信封污染。
func TestSpaceMiddlewareEnvelope_MemberPassesThrough(t *testing.T) {
	r := setupRenderedRouter(func(spaceID, uid string) (bool, error) {
		return true, nil
	}, "testuser")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/test?space_id=sp1", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"ok":true}`, w.Body.String())
}
