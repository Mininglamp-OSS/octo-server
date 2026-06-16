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

	// 幂等（可选，Stripe 式）：仅当客户端带 Idempotency-Key 才启用。关键顺序——先按 body 指纹
	// 查幂等记录（回放/冲突/in-flight），放在下面 membership / human / bot 这些「可变」校验之前：
	// 首次成功后即便调用者成员身份或 bot 状态变了，同 key + 同 body 的重试仍能回放原结果，
	// 而不是撞 403/404（满足回放契约）。
	idemKey := strings.TrimSpace(c.GetHeader(idempotencyKeyHeader))
	var redisKey, payloadSHA string
	if idemKey != "" {
		payloadSHA = teamGroupPayloadSHA(name, robotIDs)
		redisKey = teamGroupIdemRedisKey(key.ClientID, key.UID, key.SpaceID, idemKey)
		if it.idemLookup(c, redisKey, payloadSHA) {
			return // 已写出：回放 200 / 冲突 409 / in-flight 409
		}
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

	// 首次请求才占坑（reserve）：上面 idemLookup 已排除回放/冲突/in-flight；这里 SETNX 兜住
	// 并发首次请求的竞争。
	reserved := false
	if idemKey != "" {
		handled, holding := it.idemReserve(c, redisKey, payloadSHA)
		if handled {
			return // 与并发首次请求竞争失败 → 已写出回放/冲突/in-flight
		}
		reserved = holding
	}

	resp, err := it.doCreateTeamGroup(key.UID, key.SpaceID, name, robotIDs)
	if err != nil {
		// CreateGroup 返回 error **不保证**群未落库：它会在 tx.Commit() 之后调
		// IMCreateOrUpdateChannel，失败时做 best-effort 补偿删除（补偿删除自身失败则群行残留）
		// 再返回 error。因此这里**不**释放 pending 幂等 key —— 释放会让同 key 重试再建一个群
		// （重复 / 孤立群）。让 pending 随 TTL 自然过期后才允许重试，把重复窗口收敛到与进程崩溃
		// 窗口同级（已知可接受边界）。同 key 在此期间重试 → idemLookup 命中 pending → 409 in-flight。
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
//
// 注意 group.CreateGroup 的真实契约（review #377：Jerry-Xin/yujiawei/lml2468 P1）：返回 nil err
// 表示群已提交；但返回**非 nil err 并不保证群未创建**——它在 tx.Commit() 之后还会调
// IMCreateOrUpdateChannel，失败时做 best-effort 补偿删除（补偿删除失败则群行残留）再返回 error。
// 所以本函数：
//   - CreateGroup 返回 error 直接上抛，调用方据此**不会**释放幂等 key（不能把它当「提交前、可安全
//     重试」），见 createGroup 的错误分支；
//   - CreateGroup 成功后的成员实况 / created_at 两次读做 best-effort 降级，绝不因这些读失败而把一个
//     已落库的群当成失败。
func (it *Integration) doCreateTeamGroup(uid, spaceID, name string, robotIDs []string) (*createGroupResp, error) {
	createResp, err := it.groupService.CreateGroup(&group.CreateGroupServiceReq{
		Creator: uid,
		Members: robotIDs,
		Name:    name,
		SpaceID: spaceID,
		BotUID:  "", // 不指定 bot_admin
	})
	if err != nil {
		return nil, err // 注意：不保证群未落库（见上）；调用方不得据此释放幂等 key
	}

	// 响应只回真正入群的 bot（与 group_member 实况一致），绝不 echo 一个实际没进群的成员。
	// pre-validation（queryOwnedActiveBotIDs）已保证常态下请求的 bot 全部入群；读回实况兜住
	// 「校验与建群之间 bot 被注销」这类极窄 TOCTOU 竞态。读失败 → 退回请求集合（best-effort）。
	actualBots := robotIDs
	if members, mErr := it.groupService.GetMembers(createResp.GroupNo); mErr != nil {
		it.Warn("integration createGroup verify members failed; echoing requested",
			zap.Error(mErr), zap.String("groupNo", createResp.GroupNo))
	} else {
		joined := make(map[string]bool, len(members))
		for _, m := range members {
			joined[m.UID] = true
		}
		filtered := make([]string, 0, len(robotIDs))
		for _, id := range robotIDs {
			if joined[id] {
				filtered = append(filtered, id)
			}
		}
		actualBots = filtered
	}

	// created_at 取 DB 真实建群时间；读失败 → 退回服务端当前时刻（best-effort）。两个分支都归一到
	// UTC 再格式化，避免 fallback 与 DB 值在时区/RFC3339 偏移上漂移。
	createdAt := time.Now().UTC()
	if t, cErr := it.db.queryGroupCreatedAt(createResp.GroupNo, spaceID); cErr != nil {
		it.Warn("integration createGroup read created_at failed; using server time",
			zap.Error(cErr), zap.String("groupNo", createResp.GroupNo))
	} else {
		createdAt = t.UTC()
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

// teamGroupExists 判定 owner 当前是否还能访问该群。语义是「创建者态」而非「成员态」：群属于
// key 绑定的 Space、状态 normal、**调用者就是群主（group.creator==uid）**，且其仍是活跃成员
// （未被移出 / 拉黑）。
//
//   - 按 spaceID 限定：避免一把绑定 Space A 的 uk_ key 探测 Space B 的群。
//   - 要求 creator==uid：避免「调用者只是同 Space 某群的普通成员」也被判 true——存在性检测的
//     契约是「我创建的群是否还在」，不是「我是不是某群成员」。
//
// 真 DB 错误向上冒泡（→ 500）；「不在本 Space / 不存在 / 非创建者 / 非活跃成员」均 exists=false。
func (it *Integration) teamGroupExists(groupNo, spaceID, uid string) (bool, error) {
	status, creator, found, err := it.db.queryGroupStatus(groupNo, spaceID)
	if err != nil {
		return false, err
	}
	if !found || status != group.GroupStatusNormal || creator != uid {
		return false, nil
	}
	active, err := it.groupService.ExistMemberActive(groupNo, uid)
	if err != nil {
		return false, err
	}
	return active, nil
}

// idemLookup 只读地查已存在的幂等记录并处理回放/冲突/in-flight，不占坑。返回 handled=true 表示
// 已写出响应（调用方直接 return）。无记录或 Redis 故障 → handled=false（fail-open，继续建群）。
// 放在 mutable 校验之前调用，保证首次成功后即便状态变化，同 key+同 body 仍能回放。
func (it *Integration) idemLookup(c *wkhttp.Context, redisKey, payloadSHA string) (handled bool) {
	cur, err := it.rateRedis.Get(redisKey).Result()
	if err != nil {
		// redis.Nil（无记录）或瞬时错误 → 继续走首次流程。
		return false
	}
	return it.idemHandleRecord(c, cur, payloadSHA)
}

// idemReserve 原子占坑（SETNX pending，带 TTL）。返回：
//   - handled=true  → 与并发首次请求竞争失败，已按现存记录写出回放/冲突/in-flight，调用方 return。
//   - reserved=true → 成功持有 pending 锁（需 finalize 或失败时 release）。
//
// Redis 故障一律 fail-open（handled=false, reserved=false）：退化为不保证幂等而非阻断建群。
func (it *Integration) idemReserve(c *wkhttp.Context, redisKey, payloadSHA string) (handled, reserved bool) {
	pending, _ := json.Marshal(idemRecord{State: idemStatePending, SHA: payloadSHA})
	set, err := it.rateRedis.SetNX(redisKey, pending, idempotencyPendingTTL).Result()
	if err != nil {
		it.Warn("integration idempotency SETNX failed, proceeding without idempotency", zap.Error(err))
		return false, false
	}
	if set {
		return false, true // 首次，持锁
	}
	// 竞争失败：lookup 与此处之间有人占坑/完成。重读并按现存记录处理。
	cur, err := it.rateRedis.Get(redisKey).Result()
	if err != nil {
		it.Warn("integration idempotency GET failed, proceeding", zap.Error(err))
		return false, false
	}
	return it.idemHandleRecord(c, cur, payloadSHA), false
}

// idemHandleRecord 解析一条现存记录并写出对应响应。返回是否写出（true → 调用方 return）。
func (it *Integration) idemHandleRecord(c *wkhttp.Context, cur, payloadSHA string) bool {
	var existing idemRecord
	if err := json.Unmarshal([]byte(cur), &existing); err != nil {
		it.Warn("integration idempotency record corrupt, proceeding", zap.Error(err))
		return false
	}
	switch existing.State {
	case idemStatePending:
		// in-flight：同 key 仍在处理 → 409 + Retry-After（可重试，区别于终态冲突）。
		c.Header("Retry-After", "2")
		httperr.ResponseErrorLWithStatus(c, errcode.ErrIntegrationIdempotencyInFlight, nil, nil)
		return true
	case idemStateDone:
		if existing.SHA == payloadSHA && existing.Resp != nil {
			c.Response(existing.Resp) // 回放
			return true
		}
		// 同 key 不同 payload → 409 终态冲突（不可重试）。
		httperr.ResponseErrorLWithStatus(c, errcode.ErrIntegrationIdempotencyConflict, nil, nil)
		return true
	default:
		return false
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

// teamGroupIdemRedisKey namespaces the idempotency record by client/uid/space so a key is
// only ever replayed for the same integration client that created it. The client-supplied
// Idempotency-Key is hashed (not concatenated raw) so the Redis key has a fixed, bounded
// size regardless of how long a header the caller sends.
func teamGroupIdemRedisKey(clientID, uid, spaceID, idemKey string) string {
	sum := sha256.Sum256([]byte(idemKey))
	return fmt.Sprintf("octo:idem:%s:groupcreate:%s:%s:%s", clientID, uid, spaceID, hex.EncodeToString(sum[:]))
}
