package opanalytics

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const statDay = "2026-06-01"

// ---- harness ----

func opaSetup(t *testing.T) (*config.Context, *wkhttp.WKHttp, *ETL) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	route := s.GetRoute()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	require.NoError(t, testutil.CleanAllTables(ctx))
	setSuperAdminToken(t, ctx)
	createMessageShards(t, ctx)
	cleanOpaAndShards(t, ctx)
	resetUIDRateLimit(t, ctx)
	return ctx, route, NewETL(ctx)
}

func opaRouteOnlySetup(t *testing.T) (*config.Context, *wkhttp.WKHttp) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	ctx.SetHttpRoute(route)
	setSuperAdminToken(t, ctx)
	resetUIDRateLimit(t, ctx)
	New(ctx).Route(route)
	return ctx, route
}

// resetUIDRateLimit 清空 SharedUIDRateLimiter 的每-uid 令牌桶(ratelimit:uid:*)。该桶持久在
// Redis、跨测试残留且不被 CleanAllTables 清，须在 setup 重置，否则同一 binary 内靠后的测试会 429。
func resetUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	if keys, err := rds.Keys("ratelimit:uid:*").Result(); err == nil && len(keys) > 0 {
		_ = rds.Del(keys...).Err()
	}
	_ = rds.Del(etlRunLockKey).Err()
}

func setSuperAdminToken(t *testing.T, ctx *config.Context) {
	t.Helper()
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin)))
}

func setPlainUserToken(t *testing.T, ctx *config.Context) {
	t.Helper()
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test"))
}

func shardTables(ctx *config.Context) []string {
	count := ctx.GetConfig().TablePartitionConfig.MessageTableCount
	if count <= 0 {
		return []string{"message"}
	}
	tables := []string{"message"}
	for i := 1; i < count; i++ {
		tables = append(tables, fmt.Sprintf("message%d", i))
	}
	return tables
}

func shardFor(ctx *config.Context, channelID string) string {
	count := ctx.GetConfig().TablePartitionConfig.MessageTableCount
	if count <= 0 {
		return "message"
	}
	idx := crc32.ChecksumIEEE([]byte(channelID)) % uint32(count)
	if idx == 0 {
		return "message"
	}
	return fmt.Sprintf("message%d", idx)
}

// createMessageShards creates the message/message1..N shard tables (WuKongIM owns
// these in prod; no in-repo migration creates them).
func createMessageShards(t *testing.T, ctx *config.Context) {
	t.Helper()
	for _, tbl := range shardTables(ctx) {
		_, err := ctx.DB().Exec(fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` ("+
			"id BIGINT NOT NULL PRIMARY KEY AUTO_INCREMENT,"+
			"message_id VARCHAR(20) NOT NULL DEFAULT '',"+
			"from_uid VARCHAR(40) NOT NULL DEFAULT '',"+
			"channel_id VARCHAR(100) NOT NULL DEFAULT '',"+
			"channel_type SMALLINT NOT NULL DEFAULT 0,"+
			"`timestamp` BIGINT NOT NULL DEFAULT 0,"+
			"`signal` INT NOT NULL DEFAULT 0,"+
			"payload BLOB,"+
			"is_deleted INT NOT NULL DEFAULT 0,"+
			"created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP"+
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4", tbl))
		require.NoError(t, err)
	}
}

func cleanOpaAndShards(t *testing.T, ctx *config.Context) {
	t.Helper()
	tables := append([]string{
		"octo_dim_member", "octo_dim_channel",
		"octo_fact_member_channel_daily", "octo_fact_channel_daily",
		"octo_etl_message_cursor", "octo_etl_dirty_day",
	}, shardTables(ctx)...)
	for _, tbl := range tables {
		_, err := ctx.DB().Exec(fmt.Sprintf("DELETE FROM `%s`", tbl))
		require.NoError(t, err)
	}
}

// ---- seed helpers ----

func seedUser(t *testing.T, ctx *config.Context, uid, name, email string, robot int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid,name,email,short_no,robot,status,category) VALUES (?,?,?,?,?,1,'')",
		uid, name, email, uid, robot).Exec()
	require.NoError(t, err)
}

// seedUserCat 同 seedUser 但可指定 category(用于 category='system' 测试账号排除)。
func seedUserCat(t *testing.T, ctx *config.Context, uid, name string, robot int, category string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid,name,email,short_no,robot,status,category) VALUES (?,?,'',?,?,1,?)",
		uid, name, uid, robot, category).Exec()
	require.NoError(t, err)
}

func seedSpace(t *testing.T, ctx *config.Context, spaceID, name string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id,name,status) VALUES (?,?,1)", spaceID, name).Exec()
	require.NoError(t, err)
}

func seedSpaceMember(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id,uid,status) VALUES (?,?,1)", spaceID, uid).Exec()
	require.NoError(t, err)
}

func seedGroup(t *testing.T, ctx *config.Context, groupNo, name, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no,name,space_id,status) VALUES (?,?,?,1)", groupNo, name, spaceID).Exec()
	require.NoError(t, err)
}

func seedGroupMember(t *testing.T, ctx *config.Context, groupNo, uid string, robot int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group_member` (group_no,uid,robot,status) VALUES (?,?,?,1)", groupNo, uid, robot).Exec()
	require.NoError(t, err)
}

// seedGroupMemberDeleted 插入一个已退群/被移除(is_deleted=1，status 仍为 1)的群成员。
func seedGroupMemberDeleted(t *testing.T, ctx *config.Context, groupNo, uid string, robot int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group_member` (group_no,uid,robot,status,is_deleted) VALUES (?,?,?,1,1)", groupNo, uid, robot).Exec()
	require.NoError(t, err)
}

var msgSeq int64

// insertMsgs 落 count 条消息。created_at 设为 NOW()-1天，远早于默认 lag(600s)，使其落在
// 稳定前缀内被 ETL 处理(与机器时钟无关)；message.timestamp(发送时间,用于日切分桶)仍取 ts。
func insertMsgs(t *testing.T, ctx *config.Context, fromUID, channelID string, channelType uint8, ts int64, count, deleted int) {
	t.Helper()
	insertMsgsCreated(t, ctx, fromUID, channelID, channelType, ts, count, deleted, "DATE_SUB(NOW(), INTERVAL 1 DAY)")
}

// insertMsgsCreated 同 insertMsgs，但显式指定 created_at 的 SQL 表达式(测稳定性闸门用)。
func insertMsgsCreated(t *testing.T, ctx *config.Context, fromUID, channelID string, channelType uint8, ts int64, count, deleted int, createdAtExpr string) {
	t.Helper()
	tbl := shardFor(ctx, channelID)
	for i := 0; i < count; i++ {
		msgSeq++
		isDel := 0
		if i < deleted {
			isDel = 1
		}
		_, err := ctx.DB().InsertBySql(
			fmt.Sprintf("INSERT INTO `%s` (message_id,from_uid,channel_id,channel_type,`timestamp`,`signal`,is_deleted,created_at) VALUES (?,?,?,?,?,0,?,%s)", tbl, createdAtExpr),
			fmt.Sprintf("m%d", msgSeq), fromUID, channelID, channelType, ts+int64(i), isDel).Exec()
		require.NoError(t, err)
	}
}

// ---- HTTP helpers ----

func opaGet(t *testing.T, route *wkhttp.WKHttp, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("token", testutil.Token)
	rec := httptest.NewRecorder()
	route.ServeHTTP(rec, req)
	return rec
}

func opaPost(t *testing.T, route *wkhttp.WKHttp, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("token", testutil.Token)
	rec := httptest.NewRecorder()
	route.ServeHTTP(rec, req)
	return rec
}

func decodeOK(t *testing.T, rec *httptest.ResponseRecorder, out interface{}) {
	t.Helper()
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out))
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env.Error.Code
}

func waitForFact4Human(t *testing.T, ctx *config.Context, channelID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := fact4Human(t, ctx, channelID); got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Equal(t, want, fact4Human(t, ctx, channelID))
}

// ---- the scenario seed ----

// seedScenario builds: 2 spaces, 4 members(3 human + 1 agent), 2 groups, 1 private chat,
// and a day of messages. Returns the private fakeChannelID.
func seedScenario(t *testing.T, ctx *config.Context) string {
	seedSpace(t, ctx, "s1", "Alpha Space")
	seedSpace(t, ctx, "s2", "Beta Space")

	seedUser(t, ctx, "u_alice", "Alice", "alice@example.com", 0)
	seedUser(t, ctx, "u_bob", "Bob", "bob@example.com", 0)
	seedUser(t, ctx, "u_carol", "Carol", "carol@example.com", 0)
	seedUser(t, ctx, "u_agent", "AgentX", "", 1)

	seedSpaceMember(t, ctx, "s1", "u_alice")
	seedSpaceMember(t, ctx, "s1", "u_bob")
	seedSpaceMember(t, ctx, "s1", "u_agent")
	seedSpaceMember(t, ctx, "s2", "u_carol")

	seedGroup(t, ctx, "g1", "Product Group", "s1")
	seedGroup(t, ctx, "g2", "Beta Group", "s2")
	seedGroupMember(t, ctx, "g1", "u_alice", 0)
	seedGroupMember(t, ctx, "g1", "u_bob", 0)
	seedGroupMember(t, ctx, "g1", "u_agent", 1)
	seedGroupMember(t, ctx, "g2", "u_carol", 0)

	start, _, err := dayWindowUnix(statDay)
	require.NoError(t, err)
	base := start + 3600 // 当日 01:00，远离边界

	// g1：alice 3(含1撤回)、bob 2、agent 5
	insertMsgs(t, ctx, "u_alice", "g1", channelTypeGroup, base, 3, 1)
	insertMsgs(t, ctx, "u_bob", "g1", channelTypeGroup, base+10, 2, 0)
	insertMsgs(t, ctx, "u_agent", "g1", channelTypeGroup, base+20, 5, 0)
	// g2：carol 2
	insertMsgs(t, ctx, "u_carol", "g2", channelTypeGroup, base, 2, 0)
	// 私聊 alice & bob
	fc := common.GetFakeChannelIDWith("u_alice", "u_bob")
	insertMsgs(t, ctx, "u_alice", fc, channelTypePerson, base, 4, 0)
	insertMsgs(t, ctx, "u_bob", fc, channelTypePerson, base+5, 1, 0)
	return fc
}

// ---- tests ----

func TestOpanalyticsETLAndDB(t *testing.T) {
	ctx, _, etl := opaSetup(t)
	fc := seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())

	// ③ alice 在 g1 含撤回 = 3
	var aliceMsg int
	_, err := ctx.DB().Select("msg_count").From("octo_fact_member_channel_daily").
		Where("channel_id='g1' AND sender_uid='u_alice' AND stat_date=?", statDay).Load(&aliceMsg)
	require.NoError(t, err)
	assert.Equal(t, 3, aliceMsg, "撤回消息必须计入")

	// ④ g1
	var g1 struct {
		HumanMsg    int    `db:"human_msg_count"`
		AgentMsg    int    `db:"agent_msg_count"`
		ActiveHuman int    `db:"active_human_members"`
		ActiveAgent int    `db:"active_agent_members"`
		ConvType    uint8  `db:"conv_type"`
		SpaceID     string `db:"space_id"`
	}
	_, err = ctx.DB().Select("human_msg_count", "agent_msg_count", "active_human_members", "active_agent_members", "conv_type", "space_id").
		From("octo_fact_channel_daily").Where("channel_id='g1' AND stat_date=?", statDay).Load(&g1)
	require.NoError(t, err)
	assert.Equal(t, 5, g1.HumanMsg)
	assert.Equal(t, 5, g1.AgentMsg)
	assert.Equal(t, 2, g1.ActiveHuman)
	assert.Equal(t, 1, g1.ActiveAgent)
	assert.Equal(t, convTypeHAGroup, g1.ConvType)
	assert.Equal(t, "s1", g1.SpaceID)

	// dim_channel g1 成员数
	var dimG1 struct {
		MemberCount int   `db:"member_count"`
		Human       int   `db:"human_member_count"`
		Agent       int   `db:"agent_member_count"`
		LastActive  int64 `db:"last_active_at"`
	}
	_, err = ctx.DB().Select("member_count", "human_member_count", "agent_member_count", "last_active_at").
		From("octo_dim_channel").Where("channel_id='g1'").Load(&dimG1)
	require.NoError(t, err)
	assert.Equal(t, 3, dimG1.MemberCount)
	assert.Equal(t, 2, dimG1.Human)
	assert.Equal(t, 1, dimG1.Agent)
	assert.Greater(t, dimG1.LastActive, int64(0))

	// dim_channel 私聊：space_id='' 且 member_a/b 规范化
	var dimFC struct {
		ChannelType uint8  `db:"channel_type"`
		SpaceID     string `db:"space_id"`
		ConvType    uint8  `db:"conv_type"`
		MemberA     string `db:"member_a_uid"`
		MemberB     string `db:"member_b_uid"`
	}
	_, err = ctx.DB().Select("channel_type", "space_id", "conv_type", "member_a_uid", "member_b_uid").
		From("octo_dim_channel").Where("channel_id=?", fc).Load(&dimFC)
	require.NoError(t, err)
	assert.Equal(t, channelTypePerson, dimFC.ChannelType)
	assert.Equal(t, "", dimFC.SpaceID, "私聊不进空间维度")
	assert.Equal(t, convTypeHHPrivate, dimFC.ConvType)
	assert.Equal(t, "u_alice", dimFC.MemberA)
	assert.Equal(t, "u_bob", dimFC.MemberB)
}

func TestOpanalyticsETLIdempotent(t *testing.T) {
	ctx, _, etl := opaSetup(t)
	seedScenario(t, ctx)

	require.NoError(t, etl.RunIncremental())
	rows1, fact1 := countFacts(t, ctx)

	require.NoError(t, etl.RunIncremental()) // 重跑(游标保证精确一次)
	rows2, fact2 := countFacts(t, ctx)

	assert.Equal(t, rows1, rows2, "重跑后 ③ 行数应一致")
	assert.Equal(t, fact1, fact2, "重跑后 ④ 行数应一致")

	// 关键聚合值不变
	var g1Human int
	_, err := ctx.DB().Select("human_msg_count").From("octo_fact_channel_daily").
		Where("channel_id='g1' AND stat_date=?", statDay).Load(&g1Human)
	require.NoError(t, err)
	assert.Equal(t, 5, g1Human)
}

func countFacts(t *testing.T, ctx *config.Context) (int64, int64) {
	t.Helper()
	var r3, r4 int64
	_, err := ctx.DB().Select("count(*)").From("octo_fact_member_channel_daily").Load(&r3)
	require.NoError(t, err)
	_, err = ctx.DB().Select("count(*)").From("octo_fact_channel_daily").Load(&r4)
	require.NoError(t, err)
	return r3, r4
}

func compositionByType(items []*messageCompositionItem) map[uint8]*messageCompositionItem {
	out := make(map[uint8]*messageCompositionItem, len(items))
	for _, item := range items {
		out[item.ConvType] = item
	}
	return out
}

func expectedConvTypeOrder() []uint8 {
	return []uint8{convTypeHHGroup, convTypeHAGroup, convTypeHHPrivate, convTypeHAPrivate}
}

func compositionTypes(items []*messageCompositionItem) []uint8 {
	out := make([]uint8, 0, len(items))
	for _, item := range items {
		out = append(out, item.ConvType)
	}
	return out
}

func trendByBucket(items []*trendItem) map[string]*trendItem {
	out := make(map[string]*trendItem, len(items))
	for _, item := range items {
		out[item.Bucket] = item
	}
	return out
}

func trendConvByType(items []*trendConvTypeMsgItem) map[uint8]*trendConvTypeMsgItem {
	out := make(map[uint8]*trendConvTypeMsgItem, len(items))
	for _, item := range items {
		out[item.ConvType] = item
	}
	return out
}

func trendConvTypes(items []*trendConvTypeMsgItem) []uint8 {
	out := make([]uint8, 0, len(items))
	for _, item := range items {
		out = append(out, item.ConvType)
	}
	return out
}

func insertLeakedPrivateFactWithSpace(t *testing.T, ctx *config.Context, channelID string, convType uint8, humanMsg, agentMsg int64) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_fact_channel_daily "+
			"(stat_date,channel_id,channel_type,space_id,conv_type,human_msg_count,agent_msg_count,active_human_members,active_agent_members,last_msg_at) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?)",
		statDay, channelID, channelTypePerson, "s1", convType, humanMsg, agentMsg, 1, 0, 0,
	).Exec()
	require.NoError(t, err)
}

func seedDimMember(t *testing.T, ctx *config.Context, uid, name string, memberType uint8) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_dim_member (uid,name,member_type,is_excluded) VALUES (?,?,?,0)",
		uid, name, memberType,
	).Exec()
	require.NoError(t, err)
}

func insertLeakedPrivateMemberFactWithSpace(t *testing.T, ctx *config.Context, channelID, senderUID string, convType, senderType uint8) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_fact_member_channel_daily "+
			"(stat_date,channel_id,channel_type,space_id,conv_type,content_type,sender_uid,sender_type,msg_count,last_msg_at) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?)",
		statDay, channelID, channelTypePerson, "s1", convType, uint8(0), senderUID, senderType, 1, int64(0),
	).Exec()
	require.NoError(t, err)
}

func TestOpanalyticsEndpoints(t *testing.T) {
	ctx, route, etl := opaSetup(t)
	seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())

	rng := "?start_date=" + statDay + "&end_date=" + statDay

	// ---- overview ----
	var ov overviewResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/overview"+rng), &ov)
	assert.Equal(t, int64(2), ov.SpaceTotal)
	assert.Equal(t, int64(2), ov.GroupTotal)
	assert.Equal(t, int64(3), ov.HumanMemberTotal)
	assert.Equal(t, int64(1), ov.AgentTotal)
	assert.Equal(t, int64(2), ov.ActiveGroups)
	assert.Equal(t, int64(3), ov.ActiveHumanMembers)
	assert.Equal(t, int64(1), ov.ActiveAgentMembers)
	assert.Equal(t, int64(12), ov.HumanMsgCount) // g1:5 + g2:2 + private:5
	assert.Equal(t, int64(5), ov.AgentMsgCount)
	assert.Equal(t, int64(1), ov.PrivateActiveCount)
	assert.Equal(t, expectedConvTypeOrder(), compositionTypes(ov.MessageComposition))
	ovComp := compositionByType(ov.MessageComposition)
	require.Len(t, ovComp, 4, "overview composition must always return conv_type 1..4")
	assert.Equal(t, int64(2), ovComp[convTypeHHGroup].HumanMsgCount)
	assert.Equal(t, int64(0), ovComp[convTypeHHGroup].AgentMsgCount)
	assert.Equal(t, int64(2), ovComp[convTypeHHGroup].TotalMsgCount)
	assert.Equal(t, int64(1), ovComp[convTypeHHGroup].ActiveChannelCount)
	assert.Equal(t, int64(5), ovComp[convTypeHAGroup].HumanMsgCount)
	assert.Equal(t, int64(5), ovComp[convTypeHAGroup].AgentMsgCount)
	assert.Equal(t, int64(10), ovComp[convTypeHAGroup].TotalMsgCount)
	assert.Equal(t, int64(1), ovComp[convTypeHAGroup].ActiveChannelCount)
	assert.Equal(t, int64(5), ovComp[convTypeHHPrivate].HumanMsgCount)
	assert.Equal(t, int64(0), ovComp[convTypeHHPrivate].AgentMsgCount)
	assert.Equal(t, int64(5), ovComp[convTypeHHPrivate].TotalMsgCount)
	assert.Equal(t, int64(1), ovComp[convTypeHHPrivate].ActiveChannelCount)
	assert.Zero(t, ovComp[convTypeHAPrivate].TotalMsgCount)

	// ---- overview 限定 Space=s1：总数也随筛选收敛(否则活跃比例失真) ----
	var ovS1 overviewResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/overview"+rng+"&space_ids=s1"), &ovS1)
	assert.Equal(t, int64(1), ovS1.SpaceTotal, "选中 s1 → 空间总数=1")
	assert.Equal(t, int64(1), ovS1.GroupTotal, "s1 仅 g1")
	assert.Equal(t, int64(2), ovS1.HumanMemberTotal, "s1 在册 human=alice,bob")
	assert.Equal(t, int64(1), ovS1.AgentTotal, "s1 在册 agent=u_agent")
	assert.Equal(t, int64(1), ovS1.ActiveGroups)
	assert.Equal(t, int64(2), ovS1.ActiveHumanMembers)
	assert.Equal(t, int64(1), ovS1.ActiveAgentMembers)
	assert.Equal(t, int64(5), ovS1.HumanMsgCount, "私聊/g2 不计入 s1")
	assert.Equal(t, int64(5), ovS1.AgentMsgCount)
	assert.Equal(t, int64(0), ovS1.PrivateActiveCount, "选中 Space 时私聊数置 0(私聊无 space 归属)")
	assert.Equal(t, expectedConvTypeOrder(), compositionTypes(ovS1.MessageComposition))
	ovS1Comp := compositionByType(ovS1.MessageComposition)
	require.Len(t, ovS1Comp, 4)
	assert.Equal(t, int64(10), ovS1Comp[convTypeHAGroup].TotalMsgCount)
	assert.Equal(t, int64(1), ovS1Comp[convTypeHAGroup].ActiveChannelCount)
	assert.Zero(t, ovS1Comp[convTypeHHGroup].TotalMsgCount)
	assert.Zero(t, ovS1Comp[convTypeHHPrivate].TotalMsgCount, "选中 Space 时私聊 composition 为 0")
	assert.Zero(t, ovS1Comp[convTypeHAPrivate].TotalMsgCount, "选中 Space 时私聊 composition 为 0")

	// ---- overview 空范围：composition 仍固定 4 类且全部补 0 ----
	var empty overviewResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/overview?start_date=2026-05-31&end_date=2026-05-31"), &empty)
	assert.Equal(t, expectedConvTypeOrder(), compositionTypes(empty.MessageComposition))
	emptyComp := compositionByType(empty.MessageComposition)
	require.Len(t, emptyComp, 4)
	for _, item := range emptyComp {
		assert.Zero(t, item.TotalMsgCount)
		assert.Zero(t, item.ActiveChannelCount)
	}

	// ---- spaces (表一) ----
	var spaces struct {
		Count int64           `json:"count"`
		List  []spaceListItem `json:"list"`
	}
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/spaces"+rng), &spaces)
	assert.Equal(t, int64(2), spaces.Count)
	var s1 *spaceListItem
	for i := range spaces.List {
		if spaces.List[i].SpaceID == "s1" {
			s1 = &spaces.List[i]
		}
	}
	require.NotNil(t, s1)
	assert.Equal(t, int64(1), s1.GroupTotal)
	assert.Equal(t, int64(2), s1.HumanMemberTotal)
	assert.Equal(t, int64(1), s1.AgentTotal)
	assert.Equal(t, int64(5), s1.HumanMsgCount)
	assert.Equal(t, int64(5), s1.AgentMsgCount)
	assert.True(t, s1.IsActive)

	// ---- channels (表二，仅群组) ----
	var channels struct {
		Count int64             `json:"count"`
		List  []channelListItem `json:"list"`
	}
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/spaces/s1/channels"+rng), &channels)
	assert.Equal(t, int64(1), channels.Count)
	require.Len(t, channels.List, 1)
	assert.Equal(t, "g1", channels.List[0].ChannelID)
	assert.Equal(t, convTypeHAGroup, channels.List[0].ConvType)
	assert.Equal(t, 3, channels.List[0].MemberCount)
	assert.Equal(t, int64(5), channels.List[0].HumanMsgCount)
	assert.Equal(t, int64(5), channels.List[0].AgentMsgCount)

	// 未知 space → 404 (ResponseErrorL：transport 400 + error.code not_found)
	rec := opaGet(t, route, "/v1/manager/dashboard/spaces/nope/channels"+rng)
	assert.Equal(t, "err.server.opanalytics.not_found", errorCode(t, rec))

	// 软删除(status=0)的 space 视为不存在 → 404(与 /spaces 列表口径一致)
	_, err := ctx.DB().Exec("INSERT INTO space (space_id,name,status) VALUES ('s_dead','Dead Space',0)")
	require.NoError(t, err)
	rec = opaGet(t, route, "/v1/manager/dashboard/spaces/s_dead/channels"+rng)
	assert.Equal(t, "err.server.opanalytics.not_found", errorCode(t, rec), "软删除 space 应 404")

	// ---- global direct chats ----
	var direct struct {
		Count int64            `json:"count"`
		List  []directChatItem `json:"list"`
	}
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/global/direct-chats"+rng), &direct)
	assert.Equal(t, int64(1), direct.Count)
	require.Len(t, direct.List, 1)
	assert.Equal(t, int64(5), direct.List[0].MsgCount)
	assert.Equal(t, "Alice", direct.List[0].MemberAName)
	assert.Equal(t, "Bob", direct.List[0].MemberBName)

	// ---- 非 superAdmin → 403 ----
	setPlainUserToken(t, ctx)
	rec = opaGet(t, route, "/v1/manager/dashboard/overview"+rng)
	assert.Equal(t, "err.server.opanalytics.forbidden", errorCode(t, rec))
}

func TestOpanalyticsTrendEndpoint(t *testing.T) {
	ctx, route, etl := opaSetup(t)
	seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())

	nextDay := "2026-06-02"
	nextStart, _, err := dayWindowUnix(nextDay)
	require.NoError(t, err)
	insertMsgs(t, ctx, "u_alice", "g1", channelTypeGroup, nextStart+3600, 1, 0)
	insertMsgs(t, ctx, "u_agent", "g1", channelTypeGroup, nextStart+3610, 1, 0)
	require.NoError(t, etl.RunIncremental())

	var dayTrend trendResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/trend?start_date=2026-05-31&end_date=2026-06-02&granularity=day"), &dayTrend)
	assert.Equal(t, "day", dayTrend.Granularity)
	require.Len(t, dayTrend.List, 3)
	byDay := trendByBucket(dayTrend.List)

	may31 := byDay["2026-05-31"]
	require.NotNil(t, may31)
	assert.Equal(t, "2026-05-31", may31.StartDate)
	assert.Equal(t, "2026-05-31", may31.EndDate)
	assert.Zero(t, may31.TotalMsgCount, "missing days must be zero-filled")
	assert.Zero(t, may31.ActiveHumanMembers)

	jun1 := byDay[statDay]
	require.NotNil(t, jun1)
	assert.Equal(t, int64(12), jun1.HumanMsgCount)
	assert.Equal(t, int64(5), jun1.AgentMsgCount)
	assert.Equal(t, int64(17), jun1.TotalMsgCount)
	assert.Equal(t, int64(3), jun1.ActiveHumanMembers)
	assert.Equal(t, int64(1), jun1.ActiveAgentMembers)
	assert.Equal(t, int64(2), jun1.ActiveGroups)
	assert.Equal(t, int64(1), jun1.PrivateActiveCount)
	assert.Equal(t, expectedConvTypeOrder(), trendConvTypes(jun1.ConvTypeMsgCounts))
	jun1Conv := trendConvByType(jun1.ConvTypeMsgCounts)
	require.Len(t, jun1Conv, 4)
	assert.Equal(t, int64(2), jun1Conv[convTypeHHGroup].TotalMsgCount)
	assert.Equal(t, int64(10), jun1Conv[convTypeHAGroup].TotalMsgCount)
	assert.Equal(t, int64(5), jun1Conv[convTypeHHPrivate].TotalMsgCount)
	assert.Zero(t, jun1Conv[convTypeHAPrivate].TotalMsgCount)

	jun2 := byDay[nextDay]
	require.NotNil(t, jun2)
	assert.Equal(t, int64(1), jun2.HumanMsgCount)
	assert.Equal(t, int64(1), jun2.AgentMsgCount)
	assert.Equal(t, int64(2), jun2.TotalMsgCount)
	assert.Equal(t, int64(1), jun2.ActiveHumanMembers)
	assert.Equal(t, int64(1), jun2.ActiveAgentMembers)
	assert.Equal(t, int64(1), jun2.ActiveGroups)
	assert.Zero(t, jun2.PrivateActiveCount)

	var weekTrend trendResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/trend?start_date=2026-06-01&end_date=2026-06-07&granularity=week"), &weekTrend)
	assert.Equal(t, "week", weekTrend.Granularity)
	require.Len(t, weekTrend.List, 1)
	week := weekTrend.List[0]
	assert.Equal(t, "2026-06-01", week.Bucket, "week bucket is Monday in report timezone")
	assert.Equal(t, "2026-06-01", week.StartDate)
	assert.Equal(t, "2026-06-07", week.EndDate)
	assert.Equal(t, int64(13), week.HumanMsgCount)
	assert.Equal(t, int64(6), week.AgentMsgCount)
	assert.Equal(t, int64(19), week.TotalMsgCount)
	assert.Equal(t, int64(3), week.ActiveHumanMembers, "week active members must be bucket-level distinct, not daily sums")
	assert.Equal(t, int64(1), week.ActiveAgentMembers)
	assert.Equal(t, int64(2), week.ActiveGroups, "week active groups must be bucket-level distinct, not daily sums")
	assert.Equal(t, int64(1), week.PrivateActiveCount)
	weekConv := trendConvByType(week.ConvTypeMsgCounts)
	assert.Equal(t, int64(2), weekConv[convTypeHHGroup].TotalMsgCount)
	assert.Equal(t, int64(12), weekConv[convTypeHAGroup].TotalMsgCount)
	assert.Equal(t, int64(5), weekConv[convTypeHHPrivate].TotalMsgCount)
	assert.Zero(t, weekConv[convTypeHAPrivate].TotalMsgCount)

	var partialWeekTrend trendResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/trend?start_date=2026-06-02&end_date=2026-06-09&granularity=week"), &partialWeekTrend)
	assert.Equal(t, "week", partialWeekTrend.Granularity)
	require.Len(t, partialWeekTrend.List, 2)
	firstPartial := partialWeekTrend.List[0]
	assert.Equal(t, "2026-06-01", firstPartial.Bucket, "bucket remains the report-timezone Monday")
	assert.Equal(t, "2026-06-02", firstPartial.StartDate, "partial week start is clipped to the request")
	assert.Equal(t, "2026-06-07", firstPartial.EndDate)
	assert.Equal(t, int64(2), firstPartial.TotalMsgCount)
	assert.Equal(t, int64(1), firstPartial.ActiveHumanMembers)
	assert.Equal(t, int64(1), firstPartial.ActiveAgentMembers)
	assert.Equal(t, int64(1), firstPartial.ActiveGroups)
	secondPartial := partialWeekTrend.List[1]
	assert.Equal(t, "2026-06-08", secondPartial.Bucket)
	assert.Equal(t, "2026-06-08", secondPartial.StartDate)
	assert.Equal(t, "2026-06-09", secondPartial.EndDate, "partial week end is clipped to the request")
	assert.Zero(t, secondPartial.TotalMsgCount)

	var s1Trend trendResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/trend?start_date="+statDay+"&end_date="+statDay+"&space_ids=s1"), &s1Trend)
	require.Len(t, s1Trend.List, 1)
	s1Day := s1Trend.List[0]
	assert.Equal(t, int64(5), s1Day.HumanMsgCount)
	assert.Equal(t, int64(5), s1Day.AgentMsgCount)
	assert.Equal(t, int64(1), s1Day.ActiveGroups)
	assert.Zero(t, s1Day.PrivateActiveCount, "选中 Space 时私聊趋势置 0")
	s1Conv := trendConvByType(s1Day.ConvTypeMsgCounts)
	assert.Equal(t, int64(10), s1Conv[convTypeHAGroup].TotalMsgCount)
	assert.Zero(t, s1Conv[convTypeHHPrivate].TotalMsgCount, "选中 Space 时私聊 conv_type 置 0")

	insertLeakedPrivateFactWithSpace(t, ctx, "leaked_hh_private", convTypeHHPrivate, 7, 0)
	insertLeakedPrivateFactWithSpace(t, ctx, "leaked_ha_private", convTypeHAPrivate, 3, 4)
	seedDimMember(t, ctx, "u_private_only_human", "PrivateOnlyHuman", memberTypeHuman)
	seedDimMember(t, ctx, "u_private_only_agent", "PrivateOnlyAgent", memberTypeAgent)
	seedSpaceMember(t, ctx, "s1", "u_private_only_human")
	seedSpaceMember(t, ctx, "s1", "u_private_only_agent")
	insertLeakedPrivateMemberFactWithSpace(t, ctx, "leaked_hh_private", "u_private_only_human", convTypeHHPrivate, memberTypeHuman)
	insertLeakedPrivateMemberFactWithSpace(t, ctx, "leaked_ha_private", "u_private_only_agent", convTypeHAPrivate, memberTypeAgent)

	var guardedOverview overviewResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/overview?start_date="+statDay+"&end_date="+statDay+"&space_ids=s1"), &guardedOverview)
	assert.Equal(t, int64(5), guardedOverview.HumanMsgCount, "service must not mix leaked private rows into space overview totals")
	assert.Equal(t, int64(5), guardedOverview.AgentMsgCount, "service must not mix leaked private rows into space overview totals")
	assert.Equal(t, int64(2), guardedOverview.ActiveHumanMembers, "space overview active members must exclude leaked private member facts")
	assert.Equal(t, int64(1), guardedOverview.ActiveAgentMembers, "space overview active members must exclude leaked private member facts")
	assert.Zero(t, guardedOverview.PrivateActiveCount)
	guardedComp := compositionByType(guardedOverview.MessageComposition)
	assert.Zero(t, guardedComp[convTypeHHPrivate].TotalMsgCount, "service must zero private composition under space filter")
	assert.Zero(t, guardedComp[convTypeHAPrivate].TotalMsgCount, "service must zero private composition under space filter")

	var guardedTrend trendResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/trend?start_date="+statDay+"&end_date="+statDay+"&space_ids=s1"), &guardedTrend)
	require.Len(t, guardedTrend.List, 1)
	guardedDay := guardedTrend.List[0]
	assert.Equal(t, int64(5), guardedDay.HumanMsgCount, "service must not mix leaked private rows into space trend totals")
	assert.Equal(t, int64(5), guardedDay.AgentMsgCount, "service must not mix leaked private rows into space trend totals")
	assert.Equal(t, int64(10), guardedDay.TotalMsgCount)
	assert.Equal(t, int64(2), guardedDay.ActiveHumanMembers, "space trend active members must exclude leaked private member facts")
	assert.Equal(t, int64(1), guardedDay.ActiveAgentMembers, "space trend active members must exclude leaked private member facts")
	assert.Zero(t, guardedDay.PrivateActiveCount, "service must zero private trend metrics under space filter")
	guardedTrendConv := trendConvByType(guardedDay.ConvTypeMsgCounts)
	assert.Zero(t, guardedTrendConv[convTypeHHPrivate].TotalMsgCount, "service must zero private trend conv_type under space filter")
	assert.Zero(t, guardedTrendConv[convTypeHAPrivate].TotalMsgCount, "service must zero private trend conv_type under space filter")

	rec := opaGet(t, route, "/v1/manager/dashboard/trend?start_date="+statDay+"&end_date="+statDay+"&granularity=month")
	assert.Equal(t, "err.server.opanalytics.request_invalid", errorCode(t, rec))

	setPlainUserToken(t, ctx)
	rec = opaGet(t, route, "/v1/manager/dashboard/trend?start_date="+statDay+"&end_date="+statDay)
	assert.Equal(t, "err.server.opanalytics.forbidden", errorCode(t, rec))
}

func TestOpanalyticsManualETLRunEndpoint(t *testing.T) {
	ctx, route, _ := opaSetup(t)
	seedScenario(t, ctx)

	rec := opaPost(t, route, "/v1/manager/dashboard/etl/run")
	require.Equalf(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	var accepted struct {
		Status int    `json:"status"`
		State  string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &accepted))
	assert.Equal(t, http.StatusAccepted, accepted.Status)
	assert.Equal(t, "accepted", accepted.State)

	waitForFact4Human(t, ctx, "g1", 5)
	assert.Equal(t, 3, fact3Msg(t, ctx, "g1", "u_alice"))
}

func TestOpanalyticsManualETLRunForbiddenAndAlreadyRunning(t *testing.T) {
	ctx, route := opaRouteOnlySetup(t)

	setPlainUserToken(t, ctx)
	rec := opaPost(t, route, "/v1/manager/dashboard/etl/run")
	assert.Equal(t, "err.server.opanalytics.forbidden", errorCode(t, rec))

	setSuperAdminToken(t, ctx)
	lock := newETLLock(ctx)
	defer lock.Close()
	token := "manual-etl-test-lock"
	acquired, err := lock.Acquire(token)
	require.NoError(t, err)
	require.True(t, acquired)
	defer func() { require.NoError(t, lock.Release(token)) }()

	rec = opaPost(t, route, "/v1/manager/dashboard/etl/run")
	assert.Equal(t, "err.server.opanalytics.etl_already_running", errorCode(t, rec))
	acquired, err = lock.Acquire("second-token")
	require.NoError(t, err)
	assert.False(t, acquired, "already-running path must not drop or replace the held lock")
}

func TestETLLockRenewRequiresMatchingToken(t *testing.T) {
	ctx, _ := opaRouteOnlySetup(t)
	lock := newETLLock(ctx)
	defer lock.Close()

	token := "renew-token"
	acquired, err := lock.Acquire(token)
	require.NoError(t, err)
	require.True(t, acquired)

	renewed, err := lock.Renew(token)
	require.NoError(t, err)
	require.True(t, renewed)

	renewed, err = lock.Renew("other-token")
	require.NoError(t, err)
	require.False(t, renewed)

	acquired, err = lock.Acquire("second-token")
	require.NoError(t, err)
	require.False(t, acquired, "wrong-token renew must not drop the held lock")

	require.NoError(t, lock.Release(token))
	acquired, err = lock.Acquire("second-token")
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, lock.Release("second-token"))
}

// TestOpanalyticsETLExclusion 验收①：系统机器人(pkg/space.SystemBots，含 notification)与
// category=system 测试账号，不进总人数/消息/活跃/私聊等核心指标；注入它们后各项与基线一致。
func TestOpanalyticsETLExclusion(t *testing.T) {
	ctx, route, etl := opaSetup(t)
	fc := seedScenario(t, ctx)

	// 注入排除账号：botfather(系统bot，agent) + u_test(category=system，human)。
	seedUserCat(t, ctx, "botfather", "BotFather", 1, "")
	seedUserCat(t, ctx, "u_test", "Tester", 0, "system")
	seedGroupMember(t, ctx, "g1", "botfather", 1) // 系统bot 是群成员，但不应计入成员数

	start, _, err := dayWindowUnix(statDay)
	require.NoError(t, err)
	base := start + 7200

	insertMsgs(t, ctx, "botfather", "g1", channelTypeGroup, base, 4, 0) // agent 系统bot → 不计
	insertMsgs(t, ctx, "u_test", "g1", channelTypeGroup, base+10, 3, 0) // 测试账号 → 不计
	// 私聊 alice & botfather：一方系统bot → 整条会话丢弃
	fcBot := common.GetFakeChannelIDWith("u_alice", "botfather")
	insertMsgs(t, ctx, "u_alice", fcBot, channelTypePerson, base, 2, 0)
	insertMsgs(t, ctx, "botfather", fcBot, channelTypePerson, base+5, 2, 0)

	require.NoError(t, etl.RunIncremental())

	// ③ 不含被排除账号
	var excludedRows int64
	_, err = ctx.DB().Select("count(*)").From("octo_fact_member_channel_daily").
		Where("sender_uid IN ('botfather','u_test')").Load(&excludedRows)
	require.NoError(t, err)
	assert.Equal(t, int64(0), excludedRows, "系统/测试账号不得进入 ③")

	// alice@botfather 私聊整条丢弃(③ 无行 + dim 无行)
	var botDMRows, botDMDim int64
	_, err = ctx.DB().Select("count(*)").From("octo_fact_member_channel_daily").
		Where("channel_id=?", fcBot).Load(&botDMRows)
	require.NoError(t, err)
	assert.Equal(t, int64(0), botDMRows, "含系统bot的私聊不得进入 ③")
	_, err = ctx.DB().Select("count(*)").From("octo_dim_channel").Where("channel_id=?", fcBot).Load(&botDMDim)
	require.NoError(t, err)
	assert.Equal(t, int64(0), botDMDim, "含系统bot的私聊不得进入会话维表")

	// ④ g1 各项与基线一致(botfather 的 4 条 + u_test 的 3 条均被排除)
	var g1 struct {
		HumanMsg    int `db:"human_msg_count"`
		AgentMsg    int `db:"agent_msg_count"`
		ActiveHuman int `db:"active_human_members"`
		ActiveAgent int `db:"active_agent_members"`
	}
	_, err = ctx.DB().Select("human_msg_count", "agent_msg_count", "active_human_members", "active_agent_members").
		From("octo_fact_channel_daily").Where("channel_id='g1' AND stat_date=?", statDay).Load(&g1)
	require.NoError(t, err)
	assert.Equal(t, 5, g1.HumanMsg, "u_test 的群消息不得计入")
	assert.Equal(t, 5, g1.AgentMsg, "botfather 的群消息不得计入")
	assert.Equal(t, 2, g1.ActiveHuman)
	assert.Equal(t, 1, g1.ActiveAgent)

	// dim_channel g1 成员数剔除系统bot
	var dimG1 struct {
		MemberCount int `db:"member_count"`
		Agent       int `db:"agent_member_count"`
	}
	_, err = ctx.DB().Select("member_count", "agent_member_count").
		From("octo_dim_channel").Where("channel_id='g1'").Load(&dimG1)
	require.NoError(t, err)
	assert.Equal(t, 3, dimG1.MemberCount, "botfather 不计入群成员数")
	assert.Equal(t, 1, dimG1.Agent, "botfather 不计入 agent 成员数")

	// overview / 私聊：与基线一致
	rng := "?start_date=" + statDay + "&end_date=" + statDay
	var ov overviewResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/overview"+rng), &ov)
	assert.Equal(t, int64(3), ov.HumanMemberTotal, "u_test 不计入总人数")
	assert.Equal(t, int64(1), ov.AgentTotal, "botfather 不计入 agent 总数")
	assert.Equal(t, int64(12), ov.HumanMsgCount)
	assert.Equal(t, int64(5), ov.AgentMsgCount)
	assert.Equal(t, int64(1), ov.PrivateActiveCount, "含系统bot的私聊不计入活跃私聊数")

	var direct struct {
		Count int64            `json:"count"`
		List  []directChatItem `json:"list"`
	}
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/global/direct-chats"+rng), &direct)
	assert.Equal(t, int64(1), direct.Count)
	require.Len(t, direct.List, 1)
	assert.Equal(t, fc, direct.List[0].ChannelID, "仅 alice&bob 私聊在列")
}

// TestOpanalyticsETLIncremental 验收③：水位增量——二次运行只累加新增消息(精确一次，不重复计)。
func TestOpanalyticsETLIncremental(t *testing.T) {
	ctx, _, etl := opaSetup(t)
	seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())

	assert.Equal(t, 3, fact3Msg(t, ctx, "g1", "u_alice"), "首轮 alice=3")
	assert.Equal(t, 5, fact4Human(t, ctx, "g1"), "首轮 g1 human=5")

	// 新增 alice 在 g1 的 2 条(新 id)，增量再跑
	start, _, err := dayWindowUnix(statDay)
	require.NoError(t, err)
	insertMsgs(t, ctx, "u_alice", "g1", channelTypeGroup, start+9000, 2, 0)
	require.NoError(t, etl.RunIncremental())

	assert.Equal(t, 5, fact3Msg(t, ctx, "g1", "u_alice"), "增量后 alice=5(3+2，非6)")
	assert.Equal(t, 7, fact4Human(t, ctx, "g1"), "增量后 g1 human=7")

	// 三跑无新消息 → 不变(游标空操作)
	require.NoError(t, etl.RunIncremental())
	assert.Equal(t, 5, fact3Msg(t, ctx, "g1", "u_alice"), "无新消息时不变")
	assert.Equal(t, 7, fact4Human(t, ctx, "g1"))
}

func fact3Msg(t *testing.T, ctx *config.Context, channelID, senderUID string) int {
	t.Helper()
	var n int
	_, err := ctx.DB().Select("IFNULL(msg_count,0)").From("octo_fact_member_channel_daily").
		Where("channel_id=? AND sender_uid=? AND stat_date=?", channelID, senderUID, statDay).Load(&n)
	require.NoError(t, err)
	return n
}

func fact4Human(t *testing.T, ctx *config.Context, channelID string) int {
	t.Helper()
	var n int
	_, err := ctx.DB().Select("IFNULL(human_msg_count,0)").From("octo_fact_channel_daily").
		Where("channel_id=? AND stat_date=?", channelID, statDay).Load(&n)
	require.NoError(t, err)
	return n
}

// TestOpanalyticsETLWatermarkLag 验收#1：稳定性闸门。created_at 在 lag 窗口内(未稳定)的消息
// 本轮不处理、游标不越过；待其落库满 lag(此处用 UPDATE 模拟时间流逝)后下一轮被精确补齐，不漏不重。
func TestOpanalyticsETLWatermarkLag(t *testing.T) {
	ctx, _, etl := opaSetup(t)
	seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())
	require.Equal(t, 3, fact3Msg(t, ctx, "g1", "u_alice"), "稳定消息首轮入账")

	// 新增 2 条"刚落库"(created_at=NOW())的未稳定消息：默认 lag=600s 内，本轮应被跳过。
	start, _, err := dayWindowUnix(statDay)
	require.NoError(t, err)
	insertMsgsCreated(t, ctx, "u_alice", "g1", channelTypeGroup, start+9000, 2, 0, "NOW()")
	require.NoError(t, etl.RunIncremental())
	assert.Equal(t, 3, fact3Msg(t, ctx, "g1", "u_alice"), "未稳定(lag内)消息本轮必须不入账")

	// 关键反例：游标不得越过未稳定行。把它们 created_at 拨老(模拟过了 lag)后，下一轮必须补上。
	g1tbl := shardFor(ctx, "g1")
	_, err = ctx.DB().Exec(fmt.Sprintf(
		"UPDATE `%s` SET created_at=DATE_SUB(NOW(), INTERVAL 1 DAY) WHERE channel_id='g1' AND from_uid='u_alice' AND created_at >= DATE_SUB(NOW(), INTERVAL 60 SECOND)", g1tbl))
	require.NoError(t, err)
	require.NoError(t, etl.RunIncremental())
	assert.Equal(t, 5, fact3Msg(t, ctx, "g1", "u_alice"), "稳定后必须精确补齐(3+2=5，不漏不重)")
}

// TestOpanalyticsETLRebuild 验收#2：口径回溯逃生门。把某成员事后改成 system 排除账号后，
// 普通增量不回溯历史；Rebuild() 用当前维表全量重算，旧统计被清除。
func TestOpanalyticsETLRebuild(t *testing.T) {
	ctx, _, etl := opaSetup(t)
	seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())
	require.Equal(t, 2, fact3Msg(t, ctx, "g1", "u_bob"), "bob 初始入账 2")
	require.Equal(t, 5, fact4Human(t, ctx, "g1"), "g1 human 初始 5")

	// 事后把 bob 标记为 system 排除账号(口径漂移)。
	_, err := ctx.DB().Exec("UPDATE `user` SET category='system' WHERE uid='u_bob'")
	require.NoError(t, err)

	// 普通增量不回溯：bob 旧统计仍在(event-time 语义)。
	require.NoError(t, etl.RunIncremental())
	assert.Equal(t, 2, fact3Msg(t, ctx, "g1", "u_bob"), "增量不回溯历史口径")
	assert.Equal(t, 5, fact4Human(t, ctx, "g1"))

	// Rebuild 用当前维表全量重算：bob 被排除，其历史统计清除。
	require.NoError(t, etl.Rebuild())
	assert.Equal(t, 0, fact3Msg(t, ctx, "g1", "u_bob"), "Rebuild 后被排除成员的历史行清除")
	assert.Equal(t, 3, fact4Human(t, ctx, "g1"), "Rebuild 后 g1 human=3(alice 3，bob 被排除)")
}

// TestOpanalyticsDimFullRefresh 验收#2(维表全量刷新)：禁用(status≠1)用户从成员总数剔除、
// 硬删除用户不残留；而其历史消息按 event-time 仍保留。
func TestOpanalyticsDimFullRefresh(t *testing.T) {
	ctx, route, etl := opaSetup(t)
	seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())

	getOverview := func() overviewResp {
		var ov overviewResp
		decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/overview?start_date="+statDay+"&end_date="+statDay), &ov)
		return ov
	}
	ov := getOverview()
	require.Equal(t, int64(3), ov.HumanMemberTotal, "初始 human 总数=alice,bob,carol")
	require.Equal(t, int64(3), ov.ActiveHumanMembers, "初始活跃 human=alice,bob,carol")

	// 禁用 carol：全量刷新后从总数与活跃中剔除(活跃成员只算当前在册)，但其 g2 历史消息量(event-time)仍在。
	_, err := ctx.DB().Exec("UPDATE `user` SET status=0 WHERE uid='u_carol'")
	require.NoError(t, err)
	require.NoError(t, etl.RunIncremental())
	ov = getOverview()
	assert.Equal(t, int64(2), ov.HumanMemberTotal, "禁用 carol 后 human 总数=2")
	assert.Equal(t, int64(2), ov.ActiveHumanMembers, "禁用 carol 后活跃 human=2(活跃⊆在册，率≤100%)")
	assert.Equal(t, 2, fact4Human(t, ctx, "g2"), "carol 历史消息量按 event-time 保留")

	// 硬删除 alice：全量替换不残留陈旧 dim 行，总数再降。
	_, err = ctx.DB().Exec("DELETE FROM `user` WHERE uid='u_alice'")
	require.NoError(t, err)
	require.NoError(t, etl.RunIncremental())
	assert.Equal(t, int64(1), getOverview().HumanMemberTotal, "硬删除 alice 后 human 总数=1(仅 bob)")
}

// TestOpanalyticsMemberTotalsConsistency 验证概览(Space 路径)与表一的"成员总数"口径一致：
// 孤儿 space_member(user 表无对应行)在两个接口都不计入(INNER JOIN，而非 LEFT JOIN 误当 human)。
func TestOpanalyticsMemberTotalsConsistency(t *testing.T) {
	ctx, route, etl := opaSetup(t)
	seedScenario(t, ctx)
	seedSpaceMember(t, ctx, "s1", "u_orphan") // user 表不存在的孤儿成员
	require.NoError(t, etl.RunIncremental())

	rng := "?start_date=" + statDay + "&end_date=" + statDay

	var ov overviewResp
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/overview"+rng+"&space_ids=s1"), &ov)

	var spaces struct {
		List []spaceListItem `json:"list"`
	}
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/spaces"+rng), &spaces)
	var s1 *spaceListItem
	for i := range spaces.List {
		if spaces.List[i].SpaceID == "s1" {
			s1 = &spaces.List[i]
		}
	}
	require.NotNil(t, s1)

	assert.Equal(t, int64(2), ov.HumanMemberTotal, "概览：孤儿 space_member 不计入")
	assert.Equal(t, int64(2), s1.HumanMemberTotal, "表一：孤儿 space_member 不计入")
	assert.Equal(t, ov.HumanMemberTotal, s1.HumanMemberTotal, "概览与表一 human 口径一致")
	assert.Equal(t, ov.AgentTotal, s1.AgentTotal, "概览与表一 agent 口径一致")
}

// TestOpanalyticsActiveRatioOnConversion 验收 P1：成员 human↔agent 转换后，活跃成员按**当前**
// 类型计，active_human ≤ human_total(率≤100%)。修复前活跃按 ③ 冻结的 sender_type 拆会撑爆比率。
func TestOpanalyticsActiveRatioOnConversion(t *testing.T) {
	ctx, route, etl := opaSetup(t)
	seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())

	overview := func() overviewResp {
		var o overviewResp
		decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/overview?start_date="+statDay+"&end_date="+statDay), &o)
		return o
	}
	o := overview()
	require.Equal(t, int64(3), o.HumanMemberTotal)
	require.Equal(t, int64(3), o.ActiveHumanMembers)

	// bob 以 human 发言后被转成 agent(robot 0→1)。
	_, err := ctx.DB().Exec("UPDATE `user` SET robot=1 WHERE uid='u_bob'")
	require.NoError(t, err)
	require.NoError(t, etl.RunIncremental())

	o = overview()
	assert.Equal(t, int64(2), o.HumanMemberTotal, "human 总数=alice,carol")
	assert.Equal(t, int64(2), o.AgentTotal, "agent 总数=u_agent,bob")
	assert.Equal(t, int64(2), o.ActiveHumanMembers, "活跃 human 按当前类型=alice,carol(不含已转 agent 的 bob)")
	assert.Equal(t, int64(2), o.ActiveAgentMembers, "活跃 agent=u_agent,bob")
	assert.LessOrEqual(t, o.ActiveHumanMembers, o.HumanMemberTotal, "active_human ≤ human_total")
	assert.LessOrEqual(t, o.ActiveAgentMembers, o.AgentTotal, "active_agent ≤ agent_total")
}

// TestOpanalyticsStaleGroupHidden 验收 P2：已解散(status=0)/硬删除的群不再出现在表二
// (读侧 INNER JOIN 活的 group 表 status=1，dim_channel 只 upsert 不删的陈旧行被挡掉)。
func TestOpanalyticsStaleGroupHidden(t *testing.T) {
	ctx, route, etl := opaSetup(t)
	seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())

	channelsOf := func(spaceID string) int64 {
		var resp struct {
			Count int64 `json:"count"`
		}
		decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/spaces/"+spaceID+"/channels?start_date="+statDay+"&end_date="+statDay), &resp)
		return resp.Count
	}
	require.Equal(t, int64(1), channelsOf("s1"), "初始 s1 有 g1")

	// 解散 g1(group.status=0)：表二不再展示。
	_, err := ctx.DB().Exec("UPDATE `group` SET status=0 WHERE group_no='g1'")
	require.NoError(t, err)
	require.NoError(t, etl.RunIncremental())
	assert.Equal(t, int64(0), channelsOf("s1"), "解散群不再出现在表二")

	// 硬删除 g1：仍不展示(dim_channel 残留行被 group JOIN 挡掉)。
	_, err = ctx.DB().Exec("DELETE FROM `group` WHERE group_no='g1'")
	require.NoError(t, err)
	require.NoError(t, etl.RunIncremental())
	assert.Equal(t, int64(0), channelsOf("s1"), "硬删除群不再出现在表二")
}

// TestOpanalyticsSpaceNameLikeEscape 验收 P2：表一 name 过滤把 % _ 当字面量(转义)而非通配符。
func TestOpanalyticsSpaceNameLikeEscape(t *testing.T) {
	ctx, route, etl := opaSetup(t)
	seedSpace(t, ctx, "s_underscore", "a_b")
	seedSpace(t, ctx, "s_literal", "axb")
	require.NoError(t, etl.RunIncremental())

	var resp struct {
		Count int64           `json:"count"`
		List  []spaceListItem `json:"list"`
	}
	decodeOK(t, opaGet(t, route, "/v1/manager/dashboard/spaces?start_date="+statDay+"&end_date="+statDay+"&name=a_b"), &resp)
	assert.Equal(t, int64(1), resp.Count, "name=a_b 只应精确匹配 'a_b'，不应把 _ 当通配匹配到 'axb'")
	require.Len(t, resp.List, 1)
	assert.Equal(t, "s_underscore", resp.List[0].SpaceID)
}

// TestOpanalyticsGroupMemberDeleted 验收 P0：group_member 有 status 与 is_deleted 两个独立字段，
// 已退群/被移除(is_deleted=1)的成员不得计入 dim_channel 成员数，也不得把群误判为 HA。
func TestOpanalyticsGroupMemberDeleted(t *testing.T) {
	ctx, _, etl := opaSetup(t)
	seedScenario(t, ctx)
	// g2(carol，HH 群)加一个已退群的 agent 成员：不得计入、不得使群变 HA。
	seedUser(t, ctx, "u_leftbot", "LeftBot", "", 1)
	seedGroupMemberDeleted(t, ctx, "g2", "u_leftbot", 1)
	require.NoError(t, etl.RunIncremental())

	var dim struct {
		MemberCount int   `db:"member_count"`
		Human       int   `db:"human_member_count"`
		Agent       int   `db:"agent_member_count"`
		ConvType    uint8 `db:"conv_type"`
	}
	_, err := ctx.DB().Select("member_count", "human_member_count", "agent_member_count", "conv_type").
		From("octo_dim_channel").Where("channel_id='g2'").Load(&dim)
	require.NoError(t, err)
	assert.Equal(t, 1, dim.MemberCount, "已退群成员不计入 member_count")
	assert.Equal(t, 1, dim.Human, "仅 carol")
	assert.Equal(t, 0, dim.Agent, "已退群 agent 不计入")
	assert.Equal(t, convTypeHHGroup, dim.ConvType, "已退群 agent 不得把群误判为 HA")
}

// TestOpanalyticsHugePageIndexNoPanic 验收 P2#1：超大 page_index 不得让分页 offset 溢出成负数
// 进而内存切片越界 panic，应干净返回空列表(封顶 pageIndex)。
func TestOpanalyticsHugePageIndexNoPanic(t *testing.T) {
	ctx, route, etl := opaSetup(t)
	seedScenario(t, ctx)
	require.NoError(t, etl.RunIncremental())

	rec := opaGet(t, route, "/v1/manager/dashboard/spaces?start_date="+statDay+"&end_date="+statDay+"&page_index=9000000000000000000")
	require.Equal(t, http.StatusOK, rec.Code, "超大 page_index 必须干净返回而非 panic→500, body=%s", rec.Body.String())
	var resp struct {
		Count int64           `json:"count"`
		List  []spaceListItem `json:"list"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.List, "越界页返回空列表")
}

// TestOpanalyticsDateRangeCap 验收 P2#5：含两端的自然日数封顶 366(BETWEEN 闭区间)。
func TestOpanalyticsDateRangeCap(t *testing.T) {
	_, route, _ := opaSetup(t)
	// 含两端 366 天(跨度 365 天，2025 非闰年)→ 允许
	ok := opaGet(t, route, "/v1/manager/dashboard/overview?start_date=2025-01-01&end_date=2026-01-01")
	assert.Equal(t, http.StatusOK, ok.Code, "366 个自然日应允许, body=%s", ok.Body.String())
	// 含两端 367 天(跨度 366 天)→ 拒绝
	bad := opaGet(t, route, "/v1/manager/dashboard/overview?start_date=2025-01-01&end_date=2026-01-02")
	assert.Equal(t, "err.server.opanalytics.request_invalid", errorCode(t, bad), "367 个自然日应拒绝")
}
