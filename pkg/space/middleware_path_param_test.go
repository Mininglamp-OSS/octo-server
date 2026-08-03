package space

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// setupPathParamRouter 挂一条带 :space_id 路径参数的路由，模拟
// GET /v1/space/{space_id}/directory 这类「Space 在路径上」的接口。
func setupPathParamRouter(checker MembershipChecker) *gin.Engine {
	testCache = NewInMemoryMembershipCache()
	r := gin.New()
	mw := spaceMiddleware(checker, testCache)
	r.Use(func(c *gin.Context) {
		c.Set("uid", "testuser")
		c.Set("name", "Test")
		c.Next()
	})
	r.Use(wrapWK(mw))
	r.GET("/space/:space_id/directory", func(c *gin.Context) {
		validated, _ := c.Get("space_id")
		c.JSON(http.StatusOK, gin.H{
			"validated": validated,
			"path":      c.Param("space_id"),
		})
	})
	return r
}

// TestSpaceMiddleware_PathParam_Enforced 路径参数上的 space_id 必须触发成员校验。
// 这是新增第三取值源的目的：在此之前 c.Param 不被读取，路径作用域的路由挂了中间件
// 也会被当作「没带 space_id」直接放行。
func TestSpaceMiddleware_PathParam_Enforced(t *testing.T) {
	t.Run("成员放行且 context 里是路径上的 Space", func(t *testing.T) {
		var checked string
		r := setupPathParamRouter(func(spaceID, uid string) (bool, error) {
			checked = spaceID
			return true, nil
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/space/sp-path/directory", nil)
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "sp-path", checked, "校验的应当是路径上的 Space")
		assert.Contains(t, w.Body.String(), `"validated":"sp-path"`)
	})

	t.Run("非成员被拒", func(t *testing.T) {
		r := setupPathParamRouter(func(spaceID, uid string) (bool, error) {
			return false, nil
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/space/sp-path/directory", nil)
		r.ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// TestSpaceMiddleware_PathParam_WinsOverQueryAndHeader 路径参数优先级最高。
//
// 这是安全要求而非风格偏好：handler 读的是 c.Param("space_id")，若中间件转而用
// query / header 里的另一个值完成校验，就会出现「校验 Space A、返回 Space B 数据」
// 的越权缺口。构造一个「query/header 指向调用者有权的 Space，路径指向无权的
// Space」的请求，必须被拒。
func TestSpaceMiddleware_PathParam_WinsOverQueryAndHeader(t *testing.T) {
	var checked string
	r := setupPathParamRouter(func(spaceID, uid string) (bool, error) {
		checked = spaceID
		return spaceID == "sp-allowed", nil
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/space/sp-forbidden/directory?space_id=sp-allowed", nil)
	req.Header.Set("X-Space-ID", "sp-allowed")
	r.ServeHTTP(w, req)

	assert.Equal(t, "sp-forbidden", checked, "必须校验路径上的 Space，而不是 query/header")
	assert.Equal(t, http.StatusForbidden, w.Code, "路径指向的 Space 无权时必须拒绝")
}

// TestSpaceMiddleware_QueryHeaderUnchangedWithoutPathParam 回归断言：既有调用方
// 全部没有 :space_id 路径参数，新增的取值源对它们必须完全无感——query 仍生效，
// header 仍生效，两者都没有时仍走 c.Next() 放行。
func TestSpaceMiddleware_QueryHeaderUnchangedWithoutPathParam(t *testing.T) {
	t.Run("query 仍生效", func(t *testing.T) {
		var checked string
		r := setupRouter(func(spaceID, uid string) (bool, error) {
			checked = spaceID
			return true, nil
		})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test?space_id=sp-query", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "sp-query", checked)
	})

	t.Run("query 优先于 header", func(t *testing.T) {
		var checked string
		r := setupRouter(func(spaceID, uid string) (bool, error) {
			checked = spaceID
			return true, nil
		})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test?space_id=sp-query", nil)
		req.Header.Set("X-Space-ID", "sp-header")
		r.ServeHTTP(w, req)
		assert.Equal(t, "sp-query", checked)
	})

	t.Run("三处都没有则放行且不调用 checker", func(t *testing.T) {
		called := false
		r := setupRouter(func(spaceID, uid string) (bool, error) {
			called = true
			return false, nil
		})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.False(t, called, "无 space_id 时应保持 opt-in 放行语义")
	})
}
