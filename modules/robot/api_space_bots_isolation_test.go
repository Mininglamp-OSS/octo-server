package robot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
)

// GET /v1/robot/space_bots 的 Space 隔离测试。
//
// 修复前该路由只挂了 AuthMiddleware：handler 从 query 直接取 space_id 查库，
// 全链路没有任何成员身份校验，任何登录用户传一个别的 space_id 就能拿到该空间
// 全部 bot 的 name / description / creator_uid / creator_name / bot_commands。

// seedIsolationSpace 建一个空间，可选地把 memberUID 设为成员，并放一个启用 bot 进去。
func seedIsolationSpace(t *testing.T, ctx *config.Context, spaceID, botUID, memberUID string) {
	t.Helper()
	db := ctx.DB()

	_, err := db.InsertBySql(
		"INSERT INTO space (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, "seed_creator",
	).Exec()
	assert.NoError(t, err)

	// bot：user(robot=1) + robot(status=1) + space_member(status=1)，三者齐全才会
	// 被 spaceBots 的两个 INNER JOIN 选中。
	// short_no 必须显式给且互不相同：真实 user 表在该列上有唯一索引，默认值是
	// 空串，所以同一个库里只能存在一行空 short_no，第二次插入必然撞键。
	_, err = db.InsertBySql(
		"INSERT INTO `user` (uid, name, robot, short_no) VALUES (?, ?, 1, ?)", botUID, "Secret Bot", botUID,
	).Exec()
	assert.NoError(t, err)
	_, err = db.InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, description) VALUES (?, 1, ?, ?)",
		botUID, "seed_creator", "internal roadmap bot",
	).Exec()
	assert.NoError(t, err)
	_, err = db.InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, botUID,
	).Exec()
	assert.NoError(t, err)

	if memberUID != "" {
		_, err = db.InsertBySql(
			"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, memberUID,
		).Exec()
		assert.NoError(t, err)
	}
}

// seedIsolationBot 往已有空间里再放一个启用 bot。用于让排序与排除类断言可证伪。
func seedIsolationBot(t *testing.T, ctx *config.Context, spaceID, botUID, name string) {
	t.Helper()
	db := ctx.DB()
	// 用 IGNORE：botfather 的 user / robot 行由 robot 模块启动时自动创建，
	// 直接 INSERT 会撞唯一键。本助手要能给它补一条 space_member。
	_, err := db.InsertBySql(
		"INSERT IGNORE INTO `user` (uid, name, robot, short_no) VALUES (?, ?, 1, ?)", botUID, name, botUID,
	).Exec()
	assert.NoError(t, err)
	_, err = db.InsertBySql(
		"INSERT IGNORE INTO robot (robot_id, status, creator_uid, description) VALUES (?, 1, ?, ?)",
		botUID, "seed_creator", "another bot",
	).Exec()
	assert.NoError(t, err)
	_, err = db.InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, botUID,
	).Exec()
	assert.NoError(t, err)
}

// getSpaceBots 发一次请求。每次调用前清成员缓存：中间件把校验结果缓存在 Redis
// （正向 60s / 否定 30s），不清会让同一 uid 的后续用例读到上一轮的判定。
// resetUIDRateLimit 清空 UID 令牌桶。桶是进程级共享的（2 rps / burst 60），键存活
// 在 Redis 且不随 CleanAllTables 清理；`go test ./...` 并行跑包、共用同一个 Redis 和
// 同一个 testutil.UID，别处的用例可以把桶抽干，让这里的安全断言收到 429 而假失败。
// 真正的危险不是 flake 本身，而是后来者为「修 flake」把 403 断言放宽成「非 200」。
func resetUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer client.Close()
	keys, err := client.Keys("ratelimit:uid:*").Result()
	if err != nil {
		t.Fatalf("读取 UID 限流桶失败: %v", err)
	}
	if len(keys) > 0 {
		if err := client.Del(keys...).Err(); err != nil {
			t.Fatalf("重置 UID 限流桶失败: %v", err)
		}
	}
}

func getSpaceBots(t *testing.T, s *server.Server, ctx *config.Context, spaceID string) *httptest.ResponseRecorder {
	t.Helper()
	resetUIDRateLimit(t, ctx)
	spacepkg.InvalidateMembershipCache(ctx.GetRedisConn(), spaceID, testutil.UID)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/robot/space_bots?space_id="+spaceID, nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// decodeSpaceBots 把响应体解析成 bot 列表。非数组（错误信封）时返回 nil。
func decodeSpaceBots(body []byte) []map[string]interface{} {
	var rows []map[string]interface{}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil
	}
	return rows
}

// TestSpaceBots_NonMemberRefused 是这次修复的核心断言。
//
// 断言落在响应体上而不是只看状态码：状态码变了但 bot 列表照给，同样是越权。
func TestSpaceBots_NonMemberRefused(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	const spaceID = "sb_iso_foreign"
	// 调用者（uid）不是这个空间的成员。
	seedIsolationSpace(t, ctx, spaceID, "sb_iso_bot", "")

	w := getSpaceBots(t, s, ctx, spaceID)

	assert.Equal(t, http.StatusForbidden, w.Code, "非成员必须被拒: %s", w.Body.String())
	assert.Empty(t, decodeSpaceBots(w.Body.Bytes()), "非成员不得拿到任何 bot 行: %s", w.Body.String())
	// 连 bot 的存在性都不该泄露。
	assert.NotContains(t, w.Body.String(), "sb_iso_bot")
	assert.NotContains(t, w.Body.String(), "internal roadmap bot")
}

// TestSpaceBots_MemberUnaffected 合法调用方的响应必须与修复前完全一致——
// 这次改动只加门，不动门后的任何东西。
func TestSpaceBots_MemberUnaffected(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	const spaceID = "sb_iso_mine"
	seedIsolationSpace(t, ctx, spaceID, "sb_iso_bot_mine", testutil.UID)
	// 只有一个 bot 时，ORDER BY u.created_at DESC 与 botfather 排除都恒真、
	// 无法证伪——而这两件正是本用例声称要钉住的。补第二个 bot（created_at 更晚，
	// 因此必须排在前面）和一条 botfather 记录，让断言真的能失败。
	seedIsolationBot(t, ctx, spaceID, "sb_iso_bot_newer", "Newer Bot")
	seedIsolationBot(t, ctx, spaceID, "botfather", "BotFather")
	// 显式拉开 created_at：两行同秒插入时 ORDER BY u.created_at DESC 的结果不确定，
	// 断言会随机翻车。把时间钉死，排序断言才真正在验证排序而不是碰运气。
	for uid, at := range map[string]string{
		"sb_iso_bot_mine":  "2026-01-01 00:00:00",
		"sb_iso_bot_newer": "2026-06-01 00:00:00",
	} {
		_, err := ctx.DB().UpdateBySql("UPDATE `user` SET created_at = ? WHERE uid = ?", at, uid).Exec()
		assert.NoError(t, err)
	}

	w := getSpaceBots(t, s, ctx, spaceID)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	rows := decodeSpaceBots(w.Body.Bytes())
	assert.Len(t, rows, 2, "botfather 必须被排除: %s", w.Body.String())
	uids := []string{rows[0]["uid"].(string), rows[1]["uid"].(string)}
	assert.NotContains(t, uids, "botfather")
	// u.created_at DESC：后建的排在前面。
	assert.Equal(t, []string{"sb_iso_bot_newer", "sb_iso_bot_mine"}, uids, "排序应为 created_at DESC")
	// 字段集保持原样：这几个是客户端在读的。
	for _, field := range []string{"uid", "name", "description", "creator_uid", "creator_name", "bot_commands", "auto_approve", "status"} {
		assert.Contains(t, rows[0], field, "响应字段 %s 不应消失", field)
	}
	assert.Equal(t, "not_added", rows[0]["status"], "未加好友时关系态仍是 not_added")
}

// TestSpaceBots_RemovedMemberLosesAccess 成员被移出空间后应当失去访问权。
//
// 中间件的否定缓存 TTL 是 30s、正向 60s，所以这里显式失效一次再断言：
// 用例验证的是「移除后判定会翻转」，而不是「翻转是瞬时的」——后者中间件本就
// 不保证，写成瞬时断言会变成一条依赖缓存实现细节的脆弱用例。
func TestSpaceBots_RemovedMemberLosesAccess(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	const spaceID = "sb_iso_removed"
	seedIsolationSpace(t, ctx, spaceID, "sb_iso_bot_rm", testutil.UID)

	assert.Equal(t, http.StatusOK, getSpaceBots(t, s, ctx, spaceID).Code, "移除前应可访问")

	_, err := ctx.DB().UpdateBySql(
		"UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?", spaceID, testutil.UID,
	).Exec()
	assert.NoError(t, err)

	w := getSpaceBots(t, s, ctx, spaceID)
	assert.Equal(t, http.StatusForbidden, w.Code, "移除后必须被拒: %s", w.Body.String())
	// decodeSpaceBots 在 body 非数组时返回 nil，assert.Empty(nil) 恒真——
	// 所以必须同时断言 body 里不出现 bot 标识，否则响应形状一变就静默失守。
	assert.NotContains(t, w.Body.String(), "sb_iso_bot_rm")
	assert.NotContains(t, w.Body.String(), "internal roadmap bot")
}

// TestSpaceBots_MissingSpaceIDKeepsBusinessError 不传 space_id 时必须仍走 handler
// 原有的「参数缺失」业务错误，而不是被中间件拦成 403。
//
// SpaceMiddleware 对「请求里没有 space_id」是 opt-in 放行的（c.Next()），所以这条
// 路径不变——这里把它钉住，避免日后有人给中间件加上「必须带 space_id」的语义时
// 悄悄改掉这个端点面向客户端的错误形状。
func TestSpaceBots_MissingSpaceIDKeepsBusinessError(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	resetUIDRateLimit(t, ctx)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/robot/space_bots", nil)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	// 断言停在「不是 403、是 400 业务错误」这一层，不去比对错误体里的 field 明细：
	// 明细要经 i18n ErrorRenderer 渲染才会出现，而 robot 包的测试 harness 没装
	// renderer（space 包用的是自己的 newRenderedTestServer）。把断言绑到渲染装配上
	// 会让这条用例在验证中间件行为之外，还悄悄依赖一件无关的事。
	assert.NotEqual(t, http.StatusForbidden, w.Code, "缺参数不该被中间件拦成 403: %s", w.Body.String())
	assert.Equal(t, http.StatusBadRequest, w.Code, "仍应是 handler 原有的参数缺失业务错误: %s", w.Body.String())
	assert.Empty(t, decodeSpaceBots(w.Body.Bytes()), "错误响应不该是 bot 数组")

	// 真实客户端不会发裸请求：octo-web 的 APIClient 拦截器给每个请求注入
	// X-Space-Id。此时中间件会拿 header 值去校验，成员仍得到 handler 的 400，
	// 非成员则是 403。补上成员变体，免得这条用例钉的是客户端发不出的形状。
	const spaceID = "sb_iso_hdr"
	seedIsolationSpace(t, ctx, spaceID, "sb_iso_bot_hdr", testutil.UID)
	resetUIDRateLimit(t, ctx)
	spacepkg.InvalidateMembershipCache(ctx.GetRedisConn(), spaceID, testutil.UID)

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodGet, "/v1/robot/space_bots", nil)
	req2.Header.Set("token", testutil.Token)
	req2.Header.Set("X-Space-Id", spaceID)
	s.GetRoute().ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusBadRequest, w2.Code, "成员带 header 但缺 query 时仍是参数错误: %s", w2.Body.String())
}

// TestSpaceBots_DisbandedSpaceRefusesMember 钉住这次改动对**合法调用方**的行为变更。
//
// CheckMembership 除了 space_member.status=1，还额外要求 space.status=1
// （pkg/space/membership.go），而 handler 自己从不校验 space 行。所以已解散空间的
// 老成员从「200 + bot 列表」变成 403。这是正确的收紧，但它确实是对一个合法成员的
// 行为变更——brief 记了，之前却没有任何测试固定它。
func TestSpaceBots_DisbandedSpaceRefusesMember(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	const spaceID = "sb_iso_disbanded"
	seedIsolationSpace(t, ctx, spaceID, "sb_iso_bot_dis", testutil.UID)

	assert.Equal(t, http.StatusOK, getSpaceBots(t, s, ctx, spaceID).Code, "解散前成员可访问")

	_, err := ctx.DB().UpdateBySql("UPDATE space SET status = 0 WHERE space_id = ?", spaceID).Exec()
	assert.NoError(t, err)

	w := getSpaceBots(t, s, ctx, spaceID)
	assert.Equal(t, http.StatusForbidden, w.Code, "空间解散后成员也应被拒: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "sb_iso_bot_dis")
}
