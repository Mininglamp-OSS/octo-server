package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	pkgspace "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

const (
	// maxTeamGroupMembers 单次建团队群可指定的 bot 数量上限（防滥用）。
	maxTeamGroupMembers = 50
	// idempotencyKeyHeader 客户端可选幂等 header（Stripe 式，缺省不保证幂等）。
	idempotencyKeyHeader = "Idempotency-Key"
	// idempotencyPendingTTL in-flight 占位 TTL；进程在「建群成功」与「写终值」之间崩溃
	// 时最多保留这么久（之后过期，同 key 重试可能再建一个群——已知边界，可接受）。
	idempotencyPendingTTL = 60 * time.Second
	// idempotencyDoneTTL 终值（可回放）记录 TTL。
	idempotencyDoneTTL = 24 * time.Hour
)

const (
	idemStatePending = "pending"
	idemStateDone    = "done"
)

// idemRecord 是幂等 Redis 值的载荷。pending 仅占坑（带请求指纹）；done 携带完整响应用于回放。
type idemRecord struct {
	State string           `json:"state"`
	SHA   string           `json:"sha"`
	Resp  *createGroupResp `json:"resp,omitempty"`
}

// createGroup handles POST /v1/integrations/oidc/groups —— 用 uk_ key 建团队群
// （owner=当前用户，成员=指定的团队 bot；不设 bot_admin）。
func (it *Integration) createGroup(c *wkhttp.Context) {
	key, ok := getUserAPIKey(c)
	if !ok {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}

	var req createGroupReq
	if err := c.BindJSON(&req); err != nil {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "body"})
		return
	}

	// 1. name：trim 后非空，rune 数 <= group.MaxGroupNameLen。service 层是静默截断，
	//    这里前置 reject 超长，给出明确 400（否则会被悄悄截短）。
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > group.MaxGroupNameLen {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "name"})
		return
	}

	// 2. member_robot_ids：非空、去重、数量上限。
	robotIDs := normalizeRobotIDs(req.MemberRobotIDs)
	if len(robotIDs) == 0 || len(robotIDs) > maxTeamGroupMembers {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "member_robot_ids"})
		return
	}

	// 3. owner 在 Space：前置以拿到 403；否则 CreateGroup 内部的同名校验只会冒泡成 500。
	member, err := pkgspace.CheckMembership(it.ctx.DB(), key.SpaceID, key.UID)
	if err != nil {
		it.Error("integration createGroup check membership failed", zap.Error(err), zap.String("uid", key.UID))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	if !member {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
		return
	}

	// 4. owner 必须是人类成员：bot 不走 OIDC exchange、拿不到 uk_ key，且 AuthByKey 只校验
	//    账号活性不看 robot 标记，故这里显式防御，保证群主/创建者恒为真人，绝不把 bot 当 owner。
	human, err := it.db.isHumanUser(key.UID)
	if err != nil {
		it.Error("integration createGroup check human owner failed", zap.Error(err), zap.String("uid", key.UID))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	if !human {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedForbidden, nil, nil)
		return
	}

	// 5. bot 集合校验：member_robot_ids ⊆ 当前用户在该 Space「可真正入群」的 bot 集合
	//    （owned + 在 Space active + 有可用 user 行，口径与 CreateGroup 的插入源一致）。
	//    任一不在集合 → 统一 404（防 ID 枚举，不区分不存在/不归属/不在 Space/不可用）；
	//    且在建群前就拦掉，避免建出群后才发现某 bot 入不了群（孤儿群 + 500）。
	usable, err := it.db.queryOwnedActiveBotIDs(key.UID, key.SpaceID)
	if err != nil {
		it.Error("integration createGroup query bots failed", zap.Error(err), zap.String("uid", key.UID))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	for _, id := range robotIDs {
		if !usable[id] {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
			return
		}
	}

	// 幂等（可选）：仅当客户端带 Idempotency-Key 才启用。
	idemKey := strings.TrimSpace(c.GetHeader(idempotencyKeyHeader))
	payloadSHA := teamGroupPayloadSHA(name, robotIDs)
	var redisKey string
	reserved := false
	if idemKey != "" {
		redisKey = teamGroupIdemRedisKey(key.ClientID, key.UID, key.SpaceID, idemKey)
		handled, holding := it.idemBegin(c, redisKey, payloadSHA)
		if handled {
			return // 响应已写出（回放 200 / 冲突 400 / in-flight 429）
		}
		reserved = holding
	}

	resp, err := it.doCreateTeamGroup(key.UID, key.SpaceID, name, robotIDs)
	if err != nil {
		if reserved {
			it.rateRedis.Del(redisKey) // 放行同 key 重试
		}
		it.Error("integration createGroup failed", zap.Error(err),
			zap.String("uid", key.UID), zap.String("space", key.SpaceID))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}

	if reserved {
		it.idemFinalize(redisKey, payloadSHA, resp)
	}
	c.Response(resp)
}

// doCreateTeamGroup 调底层建群并组装响应。
func (it *Integration) doCreateTeamGroup(uid, spaceID, name string, robotIDs []string) (*createGroupResp, error) {
	createResp, err := it.groupService.CreateGroup(&group.CreateGroupServiceReq{
		Creator: uid,
		Members: robotIDs,
		Name:    name,
		SpaceID: spaceID,
		BotUID:  "", // 不指定 bot_admin
	})
	if err != nil {
		return nil, err
	}

	// 响应只回真正入群的 bot（与 group_member 实况一致），绝不 echo 一个实际没进群的成员。
	// pre-validation（queryOwnedActiveBotIDs）已保证常态下请求的 bot 全部入群；这里读回实况
	// 仅为兜住「校验与建群之间 bot 被注销」这类极窄 TOCTOU 竞态——此时如实少回，而非建出
	// 一个名不副实的群或回 500。
	members, err := it.groupService.GetMembers(createResp.GroupNo)
	if err != nil {
		return nil, fmt.Errorf("verify group members: %w", err)
	}
	joined := make(map[string]bool, len(members))
	for _, m := range members {
		joined[m.UID] = true
	}
	actualBots := make([]string, 0, len(robotIDs))
	for _, id := range robotIDs {
		if joined[id] {
			actualBots = append(actualBots, id)
		}
	}

	createdAt, err := it.db.queryGroupCreatedAt(createResp.GroupNo)
	if err != nil {
		return nil, err
	}

	return &createGroupResp{
		GroupID:        createResp.GroupNo,
		SpaceID:        spaceID,
		OwnerUserID:    uid,
		MemberRobotIDs: actualBots,
		Name:           createResp.Name,
		CreatedAt:      createdAt.Format(time.RFC3339),
	}, nil
}

// groupExists handles GET /v1/integrations/oidc/groups/:group_no —— 用户态存在性检测（恒 200）。
func (it *Integration) groupExists(c *wkhttp.Context) {
	key, ok := getUserAPIKey(c)
	if !ok {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	groupNo := strings.TrimSpace(c.Param("group_no"))
	if groupNo == "" {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, i18n.Details{"field": "group_no"})
		return
	}

	exists, err := it.teamGroupExists(groupNo, key.SpaceID, key.UID)
	if err != nil {
		it.Error("integration groupExists check failed", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
		return
	}
	c.Response(groupExistsResp{GroupID: groupNo, Exists: exists})
}

// teamGroupExists 判定 owner 当前是否还能访问该群：群属于 key 绑定的 Space、状态 normal，且
// owner 仍是活跃成员（未被移出 / 拉黑）。按 spaceID 限定，避免一把绑定 Space A 的 uk_ key 借
// 用户在 Space B 的成员身份探测 Space B 的群（与建群同源的 Space 隔离）。真 DB 错误向上冒泡
// （→ 500），仅「不在本 Space / 不存在 / 非成员」返回 exists=false。
func (it *Integration) teamGroupExists(groupNo, spaceID, uid string) (bool, error) {
	status, found, err := it.db.queryGroupStatus(groupNo, spaceID)
	if err != nil {
		return false, err
	}
	if !found || status != group.GroupStatusNormal {
		return false, nil
	}
	active, err := it.groupService.ExistMemberActive(groupNo, uid)
	if err != nil {
		return false, err
	}
	return active, nil
}

// idemBegin 尝试占坑。返回 (handled, reserved)：
//   - handled=true  → 响应已写出（回放 / 冲突 / in-flight），调用方应直接 return。
//   - handled=false → 调用方继续建群；reserved 表示是否持有 pending 锁（需 finalize/release）。
//
// Redis 故障一律 fail-open（handled=false, reserved=false）：退化为不保证幂等而非阻断建群，
// 与 integration 其余链路（限流 fail-open）一致。
func (it *Integration) idemBegin(c *wkhttp.Context, redisKey, payloadSHA string) (handled, reserved bool) {
	pending, _ := json.Marshal(idemRecord{State: idemStatePending, SHA: payloadSHA})
	set, err := it.rateRedis.SetNX(redisKey, pending, idempotencyPendingTTL).Result()
	if err != nil {
		it.Warn("integration idempotency SETNX failed, proceeding without idempotency", zap.Error(err))
		return false, false
	}
	if set {
		return false, true // 首次，持锁
	}

	cur, err := it.rateRedis.Get(redisKey).Result()
	if err != nil {
		it.Warn("integration idempotency GET failed, proceeding", zap.Error(err))
		return false, false
	}
	var existing idemRecord
	if err := json.Unmarshal([]byte(cur), &existing); err != nil {
		it.Warn("integration idempotency record corrupt, proceeding", zap.Error(err))
		return false, false
	}
	switch existing.State {
	case idemStatePending:
		// in-flight：同 key 仍在处理 → 409 + Retry-After（可重试，区别于终态冲突）。
		c.Header("Retry-After", "2")
		httperr.ResponseErrorLWithStatus(c, errcode.ErrIntegrationIdempotencyInFlight, nil, nil)
		return true, false
	case idemStateDone:
		if existing.SHA == payloadSHA && existing.Resp != nil {
			c.Response(existing.Resp) // 回放
			return true, false
		}
		// 同 key 不同 payload → 409 终态冲突（不可重试）。
		httperr.ResponseErrorLWithStatus(c, errcode.ErrIntegrationIdempotencyConflict, nil, nil)
		return true, false
	default:
		return false, false
	}
}

// idemFinalize 用终值（含完整响应）覆写 pending 占位，TTL 24h；失败仅告警（不影响已成功的建群）。
func (it *Integration) idemFinalize(redisKey, payloadSHA string, resp *createGroupResp) {
	done, err := json.Marshal(idemRecord{State: idemStateDone, SHA: payloadSHA, Resp: resp})
	if err != nil {
		it.Warn("integration idempotency marshal done record failed", zap.Error(err))
		return
	}
	if err := it.rateRedis.Set(redisKey, done, idempotencyDoneTTL).Err(); err != nil {
		it.Warn("integration idempotency finalize failed", zap.Error(err))
	}
}

// normalizeRobotIDs trim、去空、按出现顺序去重。
func normalizeRobotIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// teamGroupPayloadSHA 计算请求指纹（与成员顺序无关）：name + 排序后的 robotIDs。
func teamGroupPayloadSHA(name string, robotIDs []string) string {
	sorted := make([]string, len(robotIDs))
	copy(sorted, robotIDs)
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	for _, id := range sorted {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// teamGroupIdemRedisKey namespaces the idempotency record by client/uid/space so a
// key is only ever replayed for the same integration client that created it.
func teamGroupIdemRedisKey(clientID, uid, spaceID, idemKey string) string {
	return fmt.Sprintf("octo:idem:%s:groupcreate:%s:%s:%s", clientID, uid, spaceID, idemKey)
}
