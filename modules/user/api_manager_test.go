package user

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

func TestAddUser(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	w := httptest.NewRecorder()

	req, _ := http.NewRequest("POST", "/v1/manager/adduser", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name":     "张三",
		"zone":     "0086",
		"phone":    "13600000002",
		"password": "1234567",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLogin(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	err = m.userDB.Insert(&Model{
		UID:      "xxx",
		Username: "superAdmin",
		Name:     "超级管理员",
		Password: util.MD5(util.MD5("admiN123456")),
		Role:     string(wkhttp.SuperAdmin),
	})
	assert.NoError(t, err)
	req, _ := http.NewRequest("POST", "/v1/manager/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": "superAdmin",
		"password": "admiN123456",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"uid":"xxx"`))
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"超级管理员"`))
}

func TestBlacklist(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = m.userDB.Insert(&Model{
		UID:      "xxx",
		Username: "111",
		Name:     "111",
		Password: util.MD5(util.MD5("111")),
	})
	assert.NoError(t, err)
	err = m.userDB.Insert(&Model{
		UID:      "sss",
		Username: "222",
		Name:     "222",
		Password: util.MD5(util.MD5("222")),
	})
	assert.NoError(t, err)
	m.userSettingDB.InsertUserSettingModel(&SettingModel{
		UID:       "xxx",
		ToUID:     "sss",
		Blacklist: 1,
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/blacklist?uid=xxx", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
}

func TestUpdatePwd(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = m.userDB.Insert(&Model{
		UID:      testutil.UID,
		Username: "111",
		Name:     "111",
		Password: util.MD5(util.MD5("111")),
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/user/updatepassword", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"new_password": "333333",
		"password":     "111",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}
func TestUserList(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	// m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = m.userDB.Insert(&Model{
		UID:      util.GenerUUID(),
		ShortNo:  util.GenerUUID(),
		Phone:    "13897655629",
		Username: "111",
		Name:     "111",
		Status:   1,
		GiteeUID: "gitee_uid_1",
		Password: util.MD5(util.MD5("111")),
	})
	assert.NoError(t, err)
	err = m.userDB.Insert(&Model{
		UID:       util.GenerUUID(),
		ShortNo:   util.GenerUUID(),
		Phone:     "13567889876",
		Username:  "222",
		Name:      "222",
		Status:    1,
		GithubUID: "github_uid_1",
		Password:  util.MD5(util.MD5("222")),
	})
	assert.NoError(t, err)
	err = m.userDB.Insert(&Model{
		UID:      util.GenerUUID(),
		ShortNo:  util.GenerUUID(),
		Phone:    "13567987658",
		Username: "333",
		Name:     "333",
		Status:   1,
		WXOpenid: "wx_open_id_1",
		Password: util.MD5(util.MD5("333")),
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/user/list?page_index=1&page_size=10&keyword=222", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"222"`))
}
func TestUserListByUsernameKeyword(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test@"+string(wkhttp.SuperAdmin))
	assert.NoError(t, err)

	err = m.userDB.Insert(&Model{
		UID:      util.GenerUUID(),
		ShortNo:  util.GenerUUID(),
		Phone:    "13800000001",
		Username: "alice@example.com",
		Name:     "alice",
		Status:   1,
		Password: util.MD5(util.MD5("111")),
	})
	assert.NoError(t, err)
	err = m.userDB.Insert(&Model{
		UID:      util.GenerUUID(),
		ShortNo:  util.GenerUUID(),
		Phone:    "13800000002",
		Username: "bob@example.com",
		Name:     "bob",
		Status:   1,
		Password: util.MD5(util.MD5("222")),
	})
	assert.NoError(t, err)

	cases := []struct {
		name        string
		keyword     string
		wantSubstr  string
		wantMissing string
	}{
		{"exact username", "alice@example.com", `"username":"alice@example.com"`, `"username":"bob@example.com"`},
		{"partial username", "bob@", `"username":"bob@example.com"`, `"username":"alice@example.com"`},
		{"no match", "nobody@nowhere", "", `"username":"alice@example.com"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/v1/manager/user/list?page_index=1&page_size=10&keyword="+tc.keyword, nil)
			req.Header.Set("token", testutil.Token)
			s.GetRoute().ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
			body := w.Body.String()
			if tc.wantSubstr != "" {
				assert.Contains(t, body, tc.wantSubstr)
			}
			if tc.wantMissing != "" {
				assert.NotContains(t, body, tc.wantMissing)
			}
		})
	}
}

// SSO 用户的 user.email 由 OIDC claim 写入,但 user.username 仅由 phone 兜底
// (api.go:2975)。当 IdP 没下发 phone 时,SSO 用户的 username 落空,管理员
// 只剩 email 作为可识别 ID,因此 /v1/manager/user/list 必须支持按 email 搜索,
// 且响应里必须带 email 字段,否则后台无法定位这类用户。
func TestUserListByEmailKeyword(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test@"+string(wkhttp.SuperAdmin))
	assert.NoError(t, err)

	// 模拟一个典型的 SSO email-only 用户:phone 为空,username 兜底也是空,
	// 只有 email 能用来搜索。
	err = m.userDB.Insert(&Model{
		UID:      util.GenerUUID(),
		ShortNo:  util.GenerUUID(),
		Username: "",
		Name:     "Carol",
		Email:    "carol@example.com",
		Status:   1,
		Password: util.MD5(util.MD5("111")),
	})
	assert.NoError(t, err)
	// 另一个对照用户,确保关键字过滤是真实生效的。
	err = m.userDB.Insert(&Model{
		UID:      util.GenerUUID(),
		ShortNo:  util.GenerUUID(),
		Username: "",
		Name:     "Dave",
		Email:    "dave@example.com",
		Status:   1,
		Password: util.MD5(util.MD5("222")),
	})
	assert.NoError(t, err)

	cases := []struct {
		name        string
		keyword     string
		wantSubstr  string
		wantMissing string
	}{
		{"exact email", "carol@example.com", `"email":"carol@example.com"`, `"email":"dave@example.com"`},
		{"partial email local-part", "dave@", `"email":"dave@example.com"`, `"email":"carol@example.com"`},
		{"partial email domain", "@example.com", `"email":"carol@example.com"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			q := url.Values{}
			q.Set("page_index", "1")
			q.Set("page_size", "10")
			q.Set("keyword", tc.keyword)
			req, _ := http.NewRequest("GET", "/v1/manager/user/list?"+q.Encode(), nil)
			req.Header.Set("token", testutil.Token)
			s.GetRoute().ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
			body := w.Body.String()
			if tc.wantSubstr != "" {
				assert.Contains(t, body, tc.wantSubstr)
			}
			if tc.wantMissing != "" {
				assert.NotContains(t, body, tc.wantMissing)
			}
		})
	}
}

func TestUserDisablelist(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	// m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = m.userDB.Insert(&Model{
		UID:      testutil.UID,
		Phone:    "13897655629",
		Username: "111",
		Name:     "111",
		Status:   0,
		Password: util.MD5(util.MD5("111")),
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/user/disablelist?page_index=1&page_size=10", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, true, strings.Contains(w.Body.String(), `"name":"111"`))
}
func TestAddAdminUser(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/user/admin", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"login_name": "admin1",
		"password":   "111",
		"name":       "管理员",
	}))))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetAdminUser(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	//	m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	err = m.userDB.Insert(&Model{
		UID:      "uid1",
		Name:     "管理员1",
		Role:     "admin",
		Username: "admin",
		ShortNo:  "123",
	})
	assert.NoError(t, err)
	err = m.userDB.Insert(&Model{
		UID:      "uid2",
		Name:     "管理员2",
		Role:     "admin",
		Username: "admin2",
		ShortNo:  "321",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/user/admin", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	// assert.Equal(t, http.StatusOK, w.Code)
	panic(w.Body)
}
func TestDeleteAdminUser(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	// m.Route(s.GetRoute())
	//清除数据
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)
	uid := "uid1"
	err = m.userDB.Insert(&Model{
		UID:      uid,
		Name:     "管理员1",
		Role:     "admin",
		Username: "admin",
	})
	assert.NoError(t, err)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("/v1/manager/user/admin?uid=%s", uid), nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	// assert.Equal(t, http.StatusOK, w.Code)
	panic(w.Body)
}
