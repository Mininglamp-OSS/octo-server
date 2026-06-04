package robot

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// bot_mention_pref —— bot 群级免@偏好（octo-server#237 / YUJ-2836）。
//
// 稀疏覆盖表：维度 (robot_id, group_no) → no_mention，只存「偏离账号级默认」的项。
// 无记录时调用方回退账号级 requireMention（零回归）。
//
// owner 端点（user-session + creator 校验）在本文件实现；adapter 读端点
// （botToken + membership）在 modules/bot_api/mention_pref.go。

const (
	groupsListDefaultLimit = 30
	groupsListMaxLimit     = 100
)

// ownershipResult is the pure outcome of the creator-ownership check, so the
// decision (404 not-found vs 403 forbidden vs OK) is unit-testable without a DB.
type ownershipResult int

const (
	ownershipOK        ownershipResult = iota // login user is the creator
	ownershipNotFound                         // robot missing / no creator_uid → 404
	ownershipForbidden                        // creator != loginUID → 403
)

// decideOwnership maps (creatorUID, loginUID) to an ownershipResult. Mirrors
// the setAutoApprove guard (api.go:1199-1230): empty creator → not found,
// mismatch → forbidden.
func decideOwnership(creatorUID, loginUID string) ownershipResult {
	if creatorUID == "" {
		return ownershipNotFound
	}
	if creatorUID != loginUID {
		return ownershipForbidden
	}
	return ownershipOK
}

// clampGroupsLimit parses/normalizes the ?limit= query value: blank/invalid →
// default 30, capped at 100, floored at 1.
func clampGroupsLimit(raw string) int {
	limit := groupsListDefaultLimit
	if v := strings.TrimSpace(raw); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > groupsListMaxLimit {
		limit = groupsListMaxLimit
	}
	return limit
}

// decodeGroupsCursor decodes an opaque base64 cursor to a last-seen group_member.id.
// Blank/garbage cursor → 0 (first page), keeping the cursor opaque to clients.
func decodeGroupsCursor(raw string) int64 {
	cur := strings.TrimSpace(raw)
	if cur == "" {
		return 0
	}
	dec, err := base64.StdEncoding.DecodeString(cur)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(string(dec), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// encodeGroupsCursor encodes a group_member.id into an opaque base64 cursor.
func encodeGroupsCursor(id int64) string {
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

// assertRobotOwner 校验登录用户为 robot 创建者。守卫语义照搬 setAutoApprove
// (api.go:1199-1230)，但区分 404（robot 不存在）/ 403（非 owner）/ 500（DB 故障）。
// 返回 true 表示已写出错误响应、调用方应直接 return。
func (rb *Robot) assertRobotOwner(c *wkhttp.Context, robotID, loginUID string) bool {
	var creatorUID string
	err := rb.ctx.DB().Select("IFNULL(creator_uid,'')").
		From("robot").Where("robot_id=? AND status=1", robotID).LoadOne(&creatorUID)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		// 真实 DB/扫描错误不能伪装成 404，否则会掩盖故障。
		rb.Error("查询 robot creator 失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return true
	}
	// ErrNotFound → creatorUID 仍为 ""，decideOwnership 归类为 404。
	switch decideOwnership(creatorUID, loginUID) {
	case ownershipNotFound:
		// Preserve the prior real 404 (these owner endpoints are dmwork-facing
		// and predate this PR returning ResponseErrorWithStatus(404)); D14
		// fixed-400 would drift the wire status for existing clients.
		httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotNotFound, nil, nil)
		return true
	case ownershipForbidden:
		// Preserve the prior real 403 (was ResponseErrorWithStatus(403)).
		httperr.ResponseErrorLWithStatus(c, errcode.ErrRobotCreatorOnly, nil, nil)
		return true
	default:
		return false
	}
}

// setMentionPref 处理 PUT /v1/robot/:robot_id/groups/:group_no/mention_pref。
// body {"no_mention":0|1}，UPSERT 到 bot_mention_pref。
func (rb *Robot) setMentionPref(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")
	groupNo := c.Param("group_no")

	var req struct {
		NoMention int `json:"no_mention"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondRobotRequestInvalid(c, "")
		return
	}
	if req.NoMention != 0 && req.NoMention != 1 {
		respondRobotRequestInvalid(c, "no_mention")
		return
	}

	if rb.assertRobotOwner(c, robotID, loginUID) {
		return
	}

	// dbr 的 InsertStmt 不暴露 Suffix，用 InsertBySql + ON DUPLICATE KEY UPDATE 完成 upsert。
	// updated_at 走列默认 ON UPDATE CURRENT_TIMESTAMP 自动更新。
	_, err := rb.ctx.DB().InsertBySql(
		"INSERT INTO bot_mention_pref (robot_id, group_no, no_mention, updated_by) "+
			"VALUES (?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE no_mention=VALUES(no_mention), updated_by=VALUES(updated_by)",
		robotID, groupNo, req.NoMention, loginUID,
	).Exec()
	if err != nil {
		rb.Error("写入 bot_mention_pref 失败", zap.Error(err),
			zap.String("robot_id", robotID), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	// 写库成功后即时通知对应 bot 失效缓存（best-effort，异步不阻塞主流程；
	// adapter 仍有 TTL 兜底）。octo-server#242 / YUJ-2884。
	go func() {
		defer func() {
			if r := recover(); r != nil {
				rb.Error("sendMentionPrefNotification panic", zap.Any("recover", r))
			}
		}()
		rb.sendMentionPrefNotification(robotID, groupNo, req.NoMention)
	}()
	c.ResponseOK()
}

// deleteMentionPref 处理 DELETE /v1/robot/:robot_id/groups/:group_no/mention_pref。
// 删除记录回退账号级默认。幂等：删不存在记录也返回 200。
func (rb *Robot) deleteMentionPref(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")
	groupNo := c.Param("group_no")

	if rb.assertRobotOwner(c, robotID, loginUID) {
		return
	}

	_, err := rb.ctx.DB().DeleteFrom("bot_mention_pref").
		Where("robot_id=? AND group_no=?", robotID, groupNo).Exec()
	if err != nil {
		rb.Error("删除 bot_mention_pref 失败", zap.Error(err),
			zap.String("robot_id", robotID), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	// 删除回退账号级默认即 no_mention=0；同样推事件让 adapter 即时失效缓存
	// （best-effort，异步不阻塞）。octo-server#242 / YUJ-2884。
	go func() {
		defer func() {
			if r := recover(); r != nil {
				rb.Error("sendMentionPrefNotification panic", zap.Any("recover", r))
			}
		}()
		rb.sendMentionPrefNotification(robotID, groupNo, 0)
	}()
	c.ResponseOK()
}

// sendMentionPrefNotification 在 owner 改群级免@偏好后，给对应 bot 推一条
// mention_pref_updated 事件，让 adapter 即时失效 (bot, group) 缓存，消除偏好
// 改动到生效之间的 TTL 延迟（octo-server#242 / YUJ-2884）。
//
// 结构对齐 group 模块 sendGroupMdNotification（owner 网页 → group 频道）：
//   - 走 group 频道，channel_id=groupNo —— adapter 用 extractParentGroupNo
//     (channel_id) 解析群号，与 GROUP.md 事件同一接收路径；
//   - mention.uids 只放当前 robotID（不广播），群里的其它 bot 不会被惊动，
//     而 robot 模块的事件分发按 mention.uids 把消息精确投递到该 bot 的事件队列；
//   - event 载荷带 group_no + no_mention，adapter 据此精准失效缓存。
//
// best-effort：投递失败仅记日志，不影响写接口返回 200（adapter 有 TTL 兜底）。
//
// FromUID 用 robotID 而非 owner —— bot 必是群成员，WuKongIM 不会因发送方非
// 群成员而拒发；owner 可能已退群，用 owner 会让事件静默失败退化成 TTL
// （对齐 bot_api/botfather 样板）。
func (rb *Robot) sendMentionPrefNotification(robotID, groupNo string, noMention int) {
	payload := buildMentionPrefPayload(robotID, groupNo, noMention)

	err := rb.ctx.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{
			RedDot: 0,
		},
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		FromUID:     robotID,
		Payload:     []byte(util.ToJson(payload)),
	})
	if err != nil {
		rb.Error("send mention_pref notification failed", zap.Error(err),
			zap.String("robot_id", robotID), zap.String("group_no", groupNo))
	}
}

// buildMentionPrefPayload 构造 mention_pref_updated 事件的消息载荷。提取为纯函数
// 便于单测断言事件结构（type / group_no / no_mention / mention.uids）。
func buildMentionPrefPayload(robotID, groupNo string, noMention int) map[string]interface{} {
	return map[string]interface{}{
		"type":    common.Text,
		"content": "mention_pref updated",
		"event": map[string]interface{}{
			"type":       "mention_pref_updated",
			"group_no":   groupNo,
			"no_mention": noMention,
		},
		// 只 @ 目标 bot —— 不广播给群里其它 bot。robot 事件分发按 mention.uids
		// 精确投递到该 bot 的事件队列。
		"mention": map[string]interface{}{
			"uids": []string{robotID},
		},
	}
}

// getMentionPref 处理 GET /v1/robot/:robot_id/groups/:group_no/mention_pref。
// 返回 {"no_mention":0|1, "group_allow_no_mention":0|1}，bot 主人 UI 据此能显示
// 「我开了但群主关了」的状态。bot_mention_pref 无记录 → no_mention=0；group 无记录
// 或 allow_no_mention 缺省 → group_allow_no_mention=1（默认允许，零回归）。
func (rb *Robot) getMentionPref(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")
	groupNo := c.Param("group_no")

	if rb.assertRobotOwner(c, robotID, loginUID) {
		return
	}

	var noMention int
	err := rb.ctx.DB().Select("no_mention").From("bot_mention_pref").
		Where("robot_id=? AND group_no=?", robotID, groupNo).LoadOne(&noMention)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		// dbr 在无记录时返回 ErrNotFound；其它错误才算真失败。
		rb.Error("查询 bot_mention_pref 失败", zap.Error(err),
			zap.String("robot_id", robotID), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}

	groupAllow := 1
	err = rb.ctx.DB().Select("allow_no_mention").From("`group`").
		Where("group_no=?", groupNo).LoadOne(&groupAllow)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			groupAllow = 1
		} else {
			rb.Error("查询 group.allow_no_mention 失败", zap.Error(err),
				zap.String("robot_id", robotID), zap.String("group_no", groupNo))
			httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
			return
		}
	}

	c.Response(map[string]interface{}{
		"no_mention":             noMention,
		"group_allow_no_mention": groupAllow,
	})
}

// groupScanRow is the raw DB row for listGroups. no_mention scans as int
// (TINYINT); database/sql does not reliably convert the driver int into a Go
// bool, so we scan int here and project to bool in groupListItem.
type groupScanRow struct {
	ID                  int64  `db:"id"`
	GroupNo             string `db:"group_no"`
	Name                string `db:"name"`
	NoMention           int    `db:"no_mention"`
	GroupAllowNoMention int    `db:"group_allow_no_mention"`
}

// groupListItem 列群响应单项。
type groupListItem struct {
	GroupNo             string `json:"group_no"`
	Name                string `json:"name"`
	NoMention           bool   `json:"no_mention"`
	GroupAllowNoMention bool   `json:"group_allow_no_mention"`
}

// listGroups 处理 GET /v1/robot/:robot_id/groups?limit=30&cursor=<opaque>&q=<可选>。
// group_member gm JOIN group g WHERE gm.uid=robot_id AND gm.is_deleted=0，
// LEFT JOIN bot_mention_pref 得 no_mention。cursor 为不透明 base64(last_id)。
func (rb *Robot) listGroups(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	robotID := c.Param("robot_id")

	if rb.assertRobotOwner(c, robotID, loginUID) {
		return
	}

	limit := clampGroupsLimit(c.Query("limit"))

	// cursor: 不透明 base64 编码的 last group_member.id；解码失败视为首页。
	lastID := decodeGroupsCursor(c.Query("cursor"))

	q := strings.TrimSpace(c.Query("q"))

	// 多取 1 行判断 has_more。按 gm.id 升序稳定分页。
	sql := "SELECT gm.id AS id, gm.group_no AS group_no, IFNULL(g.name,'') AS name, " +
		"IFNULL(p.no_mention,0) AS no_mention, " +
		"IFNULL(g.allow_no_mention,1) AS group_allow_no_mention " +
		"FROM group_member gm " +
		"INNER JOIN `group` g ON gm.group_no = g.group_no " +
		"LEFT JOIN bot_mention_pref p ON p.robot_id = gm.uid AND p.group_no = gm.group_no " +
		"WHERE gm.uid = ? AND gm.is_deleted = 0 AND gm.id > ?"
	args := []interface{}{robotID, lastID}
	if q != "" {
		sql += " AND g.name LIKE ?"
		args = append(args, "%"+q+"%")
	}
	sql += " ORDER BY gm.id ASC LIMIT ?"
	args = append(args, limit+1)

	var rows []groupScanRow
	_, err := rb.ctx.DB().SelectBySql(sql, args...).Load(&rows)
	if err != nil {
		rb.Error("列群查询失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}

	var nextCursor interface{} // null 当无下一页
	if hasMore && len(rows) > 0 {
		nextCursor = encodeGroupsCursor(rows[len(rows)-1].ID)
	}

	list := make([]groupListItem, 0, len(rows))
	for _, r := range rows {
		list = append(list, groupListItem{
			GroupNo:             r.GroupNo,
			Name:                r.Name,
			NoMention:           r.NoMention == 1,
			GroupAllowNoMention: r.GroupAllowNoMention == 1,
		})
	}

	c.JSON(http.StatusOK, map[string]interface{}{
		"list":        list,
		"next_cursor": nextCursor,
		"has_more":    hasMore,
	})
}
