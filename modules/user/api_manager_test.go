package user

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
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

// 后台需要区分三类账号：普通用户、Bot（user.robot=1）与系统账号
// （pkg/space.SystemBots，如 fileHelper/u_10000/botfather/notification）。
// 之前前端只能靠 username 后缀 _bot 推断 bot，存在撞名/伪装风险，
// 因此 /v1/manager/user/list 必须在响应里显式带 is_bot/is_system，
// 并提供 exclude_bot / exclude_system 查询参数以便后台筛除。
func TestUserListBotAndSystemFlags(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	m := NewManager(ctx)
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	err = ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test@"+string(wkhttp.SuperAdmin))
	assert.NoError(t, err)

	// 普通用户
	err = m.userDB.Insert(&Model{
		UID:      "user_normal_001",
		ShortNo:  util.GenerUUID(),
		Username: "normaluser",
		Name:     "NormalUser",
		Status:   1,
		Password: util.MD5(util.MD5("111")),
	})
	assert.NoError(t, err)
	// Bot：与 /v1/robot/space_bots 判定一致，user.robot=1
	err = m.userDB.Insert(&Model{
		UID:      "user_bot_001",
		ShortNo:  util.GenerUUID(),
		Username: "thirdpartybot",
		Name:     "ThirdPartyBot",
		Status:   1,
		Robot:    1,
		Password: util.MD5(util.MD5("222")),
	})
	assert.NoError(t, err)
	// 系统账号：pkg/space.SystemBots 中固定的 UID
	err = m.userDB.Insert(&Model{
		UID:      "fileHelper",
		ShortNo:  util.GenerUUID(),
		Username: "fileHelper",
		Name:     "FileHelper",
		Status:   1,
		Password: util.MD5(util.MD5("333")),
	})
	assert.NoError(t, err)
	// 系统账号 + Bot 同时成立的关键 case（is_bot=1 & is_system=1）。
	// 生产环境数据印证：botfather/u_10000/notification 都是 robot=1 的系统账号，
	// bot_only=1&system_only=1 必须能精准命中这种交集账号。
	err = m.userDB.Insert(&Model{
		UID:      "botfather",
		ShortNo:  util.GenerUUID(),
		Username: "botfather",
		Name:     "BotFather",
		Status:   1,
		Robot:    1,
		Password: util.MD5(util.MD5("444")),
	})
	assert.NoError(t, err)

	// 响应解析成结构化数据，便于断言 count 与 list 一致 —— PR #62 review
	// 反复强调"count/list 一致性"是这次重构的主旨，所以测试必须显式覆盖。
	type userRow struct {
		UID      string `json:"uid"`
		IsBot    int    `json:"is_bot"`
		IsSystem int    `json:"is_system"`
	}
	type listResp struct {
		Count int64     `json:"count"`
		List  []userRow `json:"list"`
	}
	doList := func(t *testing.T, query string) listResp {
		t.Helper()
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/manager/user/list?"+query, nil)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		var resp listResp
		assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "response is not JSON: %s", w.Body.String())
		return resp
	}
	uidsOf := func(rs []userRow) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.UID)
		}
		return out
	}
	findUID := func(rs []userRow, uid string) (userRow, bool) {
		for _, r := range rs {
			if r.UID == uid {
				return r, true
			}
		}
		return userRow{}, false
	}

	cases := []struct {
		name        string
		query       string
		wantUIDs    []string // 期望响应包含的 UID（顺序无关）
		notWantUIDs []string // 期望响应不应包含的 UID
	}{
		{
			name:     "default returns all four",
			query:    "page_index=1&page_size=10",
			wantUIDs: []string{"user_normal_001", "user_bot_001", "fileHelper", "botfather"},
		},
		{
			name:        "exclude_bot filters user.robot=1",
			query:       "page_index=1&page_size=10&exclude_bot=1",
			wantUIDs:    []string{"user_normal_001", "fileHelper"},
			notWantUIDs: []string{"user_bot_001", "botfather"},
		},
		{
			name:        "exclude_system filters SystemBots UIDs",
			query:       "page_index=1&page_size=10&exclude_system=1",
			wantUIDs:    []string{"user_normal_001", "user_bot_001"},
			notWantUIDs: []string{"fileHelper", "botfather"},
		},
		{
			name:        "exclude_bot + keyword still filters bots",
			query:       "page_index=1&page_size=10&keyword=user_&exclude_bot=1",
			wantUIDs:    []string{"user_normal_001"},
			notWantUIDs: []string{"user_bot_001"},
		},
		{
			name:        "exclude_bot + exclude_system filters both",
			query:       "page_index=1&page_size=10&exclude_bot=1&exclude_system=1",
			wantUIDs:    []string{"user_normal_001"},
			notWantUIDs: []string{"user_bot_001", "fileHelper", "botfather"},
		},
		// bot_only / system_only：前端"只看 Bot""只看系统账号"档位需要的反向筛选。
		// 没有这两个参数时前端只能客户端过滤当前页，会导致 total 与可见行数不一致。
		{
			name:        "bot_only returns all robot=1 (including system bots)",
			query:       "page_index=1&page_size=10&bot_only=1",
			wantUIDs:    []string{"user_bot_001", "botfather"},
			notWantUIDs: []string{"user_normal_001", "fileHelper"},
		},
		{
			name:        "system_only returns all SystemBots UIDs",
			query:       "page_index=1&page_size=10&system_only=1",
			wantUIDs:    []string{"fileHelper", "botfather"},
			notWantUIDs: []string{"user_normal_001", "user_bot_001"},
		},
		{
			name:        "bot_only + exclude_system narrows to user-created bots",
			query:       "page_index=1&page_size=10&bot_only=1&exclude_system=1",
			wantUIDs:    []string{"user_bot_001"},
			notWantUIDs: []string{"user_normal_001", "fileHelper", "botfather"},
		},
		// 交集：既是 Bot 又是系统账号 —— botfather 是这种账号的代表。
		// 这是 bot_only 和 system_only 共存的唯一有意义组合，因此显式覆盖。
		{
			name:        "bot_only + system_only returns intersection",
			query:       "page_index=1&page_size=10&bot_only=1&system_only=1",
			wantUIDs:    []string{"botfather"},
			notWantUIDs: []string{"user_normal_001", "user_bot_001", "fileHelper"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doList(t, tc.query)
			gotUIDs := uidsOf(resp.List)
			for _, uid := range tc.wantUIDs {
				assert.Contains(t, gotUIDs, uid)
			}
			for _, uid := range tc.notWantUIDs {
				assert.NotContains(t, gotUIDs, uid)
			}
			// list/count 一致 —— 测试用例样本量都小于 page_size，所以 count 必须
			// 与返回的 list 长度一致；如果 count 端漏掉了某个 filter，这里会立刻挂掉。
			assert.Equal(t, int64(len(resp.List)), resp.Count, "count must equal list length under the same filter")
		})
	}

	// 字段语义单测：分别校验四类账号的 is_bot/is_system 取值。
	t.Run("flag values per account type", func(t *testing.T) {
		resp := doList(t, "page_index=1&page_size=10")
		normal, ok := findUID(resp.List, "user_normal_001")
		assert.True(t, ok)
		assert.Equal(t, 0, normal.IsBot)
		assert.Equal(t, 0, normal.IsSystem)

		bot, ok := findUID(resp.List, "user_bot_001")
		assert.True(t, ok)
		assert.Equal(t, 1, bot.IsBot)
		assert.Equal(t, 0, bot.IsSystem)

		// fileHelper 是系统账号但没有 user.robot=1 —— 这正是 is_bot 与
		// is_system 设计为独立维度的关键 case（PR #62 review 共识）。
		sys, ok := findUID(resp.List, "fileHelper")
		assert.True(t, ok)
		assert.Equal(t, 0, sys.IsBot)
		assert.Equal(t, 1, sys.IsSystem)

		// botfather：交集账号，is_bot=1 且 is_system=1 同时成立。
		bf, ok := findUID(resp.List, "botfather")
		assert.True(t, ok)
		assert.Equal(t, 1, bf.IsBot)
		assert.Equal(t, 1, bf.IsSystem)
	})

	// 互斥校验：bot_only 与 exclude_bot、system_only 与 exclude_system 同时为 1
	// 是逻辑矛盾，返回 400 比静默返回空更利于前端发现 bug。
	//
	// Phase 2.1 迁移后该 400 走 i18n 错误信封（err.server.user.list_filter_conflict）。
	// 这里只校验 HTTP 状态码不变（回归保护）——此 testutil 服务未装 i18n
	// ErrorRenderer，故仅输出 legacy {msg,status}；error.code / details 的 v2
	// 信封与 zh-CN 文案由 TestRespondUserHelpers（helperHarness 装了 renderer）覆盖。
	conflictCases := []struct {
		name  string
		query string
	}{
		{"bot_only conflicts with exclude_bot", "page_index=1&page_size=10&bot_only=1&exclude_bot=1"},
		{"system_only conflicts with exclude_system", "page_index=1&page_size=10&system_only=1&exclude_system=1"},
	}
	for _, tc := range conflictCases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/v1/manager/user/list?"+tc.query, nil)
			req.Header.Set("token", testutil.Token)
			s.GetRoute().ServeHTTP(w, req)
			assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
		})
	}
}

// GH issue #54: 历史迁移 20220222000001_user_legacy01.sql 用裸 MODIFY 把
// user.phone / user.zone 改成了 nullable，导致任何外部 INSERT 漏列就会让这
// 两列出现 NULL，进而让 /v1/manager/user/list 等所有 SELECT phone/zone 到
// string 字段的接口因 "converting NULL to string is unsupported" 报 400。
//
// 修复迁移 20260516000001_user_legacy01.sql 把列改回 NOT NULL DEFAULT ”。
// 本测试通过 INFORMATION_SCHEMA 校验 schema、并直接走 raw SQL 尝试 NULL
// 写入来验证约束是否真生效（修复前是 NULL 通过、读取报错；修复后是写入直接被
// 拒，读取永远安全）。
func TestUserTablePhoneZoneNotNullable(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 1. INFORMATION_SCHEMA 直接校验 schema。
	type colMeta struct {
		ColumnName    string         `db:"COLUMN_NAME"`
		IsNullable    string         `db:"IS_NULLABLE"`
		ColumnDefault sql.NullString `db:"COLUMN_DEFAULT"`
	}
	var cols []*colMeta
	_, err = ctx.DB().SelectBySql(
		"SELECT COLUMN_NAME, IS_NULLABLE, COLUMN_DEFAULT " +
			"FROM INFORMATION_SCHEMA.COLUMNS " +
			"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME='user' " +
			"AND COLUMN_NAME IN ('phone','zone') ORDER BY COLUMN_NAME",
	).Load(&cols)
	assert.NoError(t, err)
	assert.Len(t, cols, 2, "expected phone+zone columns to exist")
	for _, c := range cols {
		assert.Equal(t, "NO", c.IsNullable, "%s must be NOT NULL", c.ColumnName)
		assert.True(t, c.ColumnDefault.Valid, "%s must have a non-NULL DEFAULT", c.ColumnName)
		assert.Equal(t, "", c.ColumnDefault.String, "%s DEFAULT should be empty string", c.ColumnName)
	}

	// 2. 显式 NULL 写入应被数据库拒绝（NOT NULL 约束实际生效，而不只是元数据声明）。
	// short_no 走自定义值避免与系统账号或下面的 case 撞 unique 索引。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, role, username, short_no, phone, zone) VALUES (?, ?, ?, ?, ?, NULL, NULL)",
		"test_null_phone", "X", "admin", "test_null_phone", "test_short_1",
	).Exec()
	assert.Error(t, err, "INSERT with explicit NULL phone/zone must be rejected")

	// 3. 漏列写入应该走 DEFAULT '' —— 这是 issue 报告的"手插 admin 行"场景。
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, role, username, short_no) VALUES (?, ?, ?, ?, ?)",
		"test_missing_phone", "Y", "admin", "test_missing_phone", "test_short_2",
	).Exec()
	assert.NoError(t, err, "INSERT without phone/zone columns should succeed via DEFAULT ''")

	// 4. 验证 DEFAULT 实际值确是 ''。
	type row struct {
		Phone string `db:"phone"`
		Zone  string `db:"zone"`
	}
	var r row
	_, err = ctx.DB().SelectBySql("SELECT phone, zone FROM `user` WHERE uid=?", "test_missing_phone").Load(&r)
	assert.NoError(t, err)
	assert.Equal(t, "", r.Phone)
	assert.Equal(t, "", r.Zone)
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

// TestManager_DeleteAdminUser_RevokesSessionsAndRoleCache pins the issue #363
// fast-follow: deleting an admin must invalidate the user_role:{uid} hot cache
// AND revoke that admin's tokens on every device flag (not just Web), so the
// removal takes effect on the next request instead of waiting out RoleCacheTTL.
func TestManager_DeleteAdminUser_RevokesSessionsAndRoleCache(t *testing.T) {
	// testutil.NewTestServer already registers every module's routes via
	// module.Setup, so the DELETE handler is live — don't call m.Route (double
	// registration panics). NewDB(ctx) is only for seeding rows; the request is
	// served by the module.Setup-constructed Manager, whose roleService shares
	// the same Redis namespace as our assertions.
	s, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	assert.NoError(t, testutil.CleanAllTables(ctx))

	userDB := NewDB(ctx)
	cacheCfg := ctx.GetConfig().Cache

	// caller must be superAdmin (deleteAdminUsers requires it)
	callerTok := "user-mgr-superadmin-caller"
	assert.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+callerTok, "root-uid@root@"+string(wkhttp.SuperAdmin)))

	// victim admin with live sessions on every device flag + a hot role cache
	adminUID := "admin-victim-uid"
	assert.NoError(t, userDB.Insert(&Model{
		UID:      adminUID,
		Name:     "管理员X",
		Role:     string(wkhttp.Admin),
		Username: "admin-victim",
	}))
	deviceTokens := map[config.DeviceFlag]string{
		config.APP: "victim-tok-app",
		config.Web: "victim-tok-web",
		config.PC:  "victim-tok-pc",
	}
	for flag, tok := range deviceTokens {
		assert.NoError(t, ctx.Cache().Set(fmt.Sprintf("%s%d%s", cacheCfg.UIDTokenCachePrefix, flag, adminUID), tok))
		assert.NoError(t, ctx.Cache().Set(cacheCfg.TokenCachePrefix+tok, adminUID+"@管理员X@"+string(wkhttp.Admin)))
	}
	assert.NoError(t, ctx.Cache().Set(RoleCacheKeyPrefix+adminUID, string(wkhttp.Admin)))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/manager/user/admin?uid="+adminUID, nil)
	req.Header.Set("token", callerTok)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// role hot cache invalidated
	roleCached, err := ctx.Cache().Get(RoleCacheKeyPrefix + adminUID)
	assert.NoError(t, err)
	assert.Empty(t, roleCached, "user_role hot cache must be invalidated after deletion")

	// every device session revoked: both the token:{token} payload and the
	// UIDToken:{flag}{uid} reverse mapping
	for flag, tok := range deviceTokens {
		payload, err := ctx.Cache().Get(cacheCfg.TokenCachePrefix + tok)
		assert.NoError(t, err)
		assert.Empty(t, payload, "device token payload (flag %d) must be revoked", flag)

		uidMap, err := ctx.Cache().Get(fmt.Sprintf("%s%d%s", cacheCfg.UIDTokenCachePrefix, flag, adminUID))
		assert.NoError(t, err)
		assert.Empty(t, uidMap, "UIDToken mapping (flag %d) must be cleared", flag)
	}
}

// TestRevokeDeviceTokensInCache_BestEffort pins that a Redis error on one device
// flag does not abort revocation of the others (PR #364 review F2): the loop
// attempts all three flags and reports a failure per error rather than bailing
// on the first. Uses the shared fakeLangCache (implements cache.Cache).
func TestRevokeDeviceTokensInCache_BestEffort(t *testing.T) {
	c := newFakeLangCache()
	const uidTokenPrefix, tokenPrefix, uid = "UIDTOKEN:", "TOKEN:", "u1"
	for _, fl := range []config.DeviceFlag{config.APP, config.Web, config.PC} {
		tok := fmt.Sprintf("tok-%d", fl)
		c.store[fmt.Sprintf("%s%d%s", uidTokenPrefix, fl, uid)] = tok
		c.store[tokenPrefix+tok] = uid + "@x@admin"
	}
	// Every Delete fails: a fail-fast loop would stop after flag 0; best-effort
	// must still attempt all three.
	c.delErr = errors.New("redis down")

	failures := revokeDeviceTokensInCache(c, uidTokenPrefix, tokenPrefix, uid)
	if len(failures) != 3 {
		t.Fatalf("best-effort revoke must attempt all 3 device flags despite errors, got %d failures", len(failures))
	}
	if len(c.deletes) < 3 {
		t.Fatalf("expected a Delete attempt per device flag, got %d", len(c.deletes))
	}
}
