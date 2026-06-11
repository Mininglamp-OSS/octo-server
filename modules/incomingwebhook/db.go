package incomingwebhook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/gocraft/dbr/v2"
)

// ErrQuotaExceeded 创建时群级配额已满，由 insertWithQuota 在事务内原子判定。
var ErrQuotaExceeded = errors.New("incomingwebhook: per-group quota exceeded")

// ErrCreatorQuotaExceeded 创建者个人配额已满（仅普通成员/bot 受限，管理员豁免），
// 同样由 insertWithQuota 在事务内原子判定。
var ErrCreatorQuotaExceeded = errors.New("incomingwebhook: per-creator quota exceeded")

type incomingWebhookDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newDB(ctx *config.Context) *incomingWebhookDB {
	return &incomingWebhookDB{ctx: ctx, session: ctx.DB()}
}

// insertWithQuota 在事务内做"配额校验 + 写入"原子操作：
//  1. SELECT id FROM `group` ... FOR UPDATE：对父群行加 X 记录锁（group_no 命中
//     UNIQUE 索引 group_groupNo，锁的是必然存在的单行 record lock）。并发的同 group
//     创建请求在此串行化，逐个进入配额校验+写入。
//  2. SELECT count(*) FROM incoming_webhook WHERE group_no=?：父群行锁已串行化，
//     此处普通读即可；count >= max 返回 ErrQuotaExceeded，事务回滚。
//  3. 显式回填 CreatedAt：dbr 的 InsertInto.Record 不会从 DB 默认值回读时间，
//     不写就会导致响应里的 created_at = epoch(0)。
//
// 为何锁父群行而非 `SELECT count(*) FROM incoming_webhook ... FOR UPDATE`：后者在
// 空群首次插入时只命中 0 行 → 纯 gap lock。gap-X 锁互相兼容，并发事务会全部通过
// count 检查、各自 INSERT 抢 insert-intention lock 互等 → InnoDB 死锁(1213)，且无
// 重试，合法并发创建会以不透明的"创建失败"500 收场。锁父群这一必然存在的单行可彻底
// 串行化而不触发 gap-lock 死锁（PR #31 yujiawei / Jerry-Xin review）。
// maxPerCreator <= 0 表示不启用个人配额（管理员创建路径）；> 0 时在同一事务内对
// creator_uid 维度再做一次计数校验，超限返回 ErrCreatorQuotaExceeded。个人配额与
// 群级配额共享同一把父群行锁，无需额外加锁。
func (d *incomingWebhookDB) insertWithQuota(m *incomingWebhookModel, max, maxPerCreator int) error {
	tx, err := d.session.Begin()
	if err != nil {
		return fmt.Errorf("incomingwebhook: begin tx: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	var gid int
	if _, err = tx.SelectBySql(
		"SELECT id FROM `group` WHERE group_no=? FOR UPDATE",
		m.GroupNo,
	).Load(&gid); err != nil {
		return fmt.Errorf("incomingwebhook: lock group for update: %w", err)
	}

	var count int
	if _, err = tx.SelectBySql(
		// 软删除（statusDeleted）的行不占配额：删除即释放名额（#254）。
		"SELECT count(*) FROM incoming_webhook WHERE group_no=? AND status != ?",
		m.GroupNo, statusDeleted,
	).Load(&count); err != nil {
		return fmt.Errorf("incomingwebhook: count: %w", err)
	}
	if count >= max {
		return ErrQuotaExceeded
	}

	if maxPerCreator > 0 {
		var creatorCount int
		if _, err = tx.SelectBySql(
			// 与群级配额同口径：只排除软删除（statusDeleted）。【禁用】的 webhook 刻意
			// 仍占个人配额——否则成员可用 disable→create→disable→create 循环无限囤积
			// 可随时启用的 webhook，配额就形同虚设。释放名额只能走删除。这也意味着：
			//   - 创建者退群被懒级联禁用的 webhook 仍占其个人配额，本人重新入群后可
			//     自行删除释放；
			//   - 配额是【创建闸】而非持续约束：调低 max_per_creator 不会回收已超额
			//     成员的存量，只是不允许再建。
			"SELECT count(*) FROM incoming_webhook WHERE group_no=? AND creator_uid=? AND status != ?",
			m.GroupNo, m.CreatorUID, statusDeleted,
		).Load(&creatorCount); err != nil {
			return fmt.Errorf("incomingwebhook: creator count: %w", err)
		}
		if creatorCount >= maxPerCreator {
			return ErrCreatorQuotaExceeded
		}
	}

	m.CreatedAt = db.Time(time.Now())
	if _, err = tx.InsertInto("incoming_webhook").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).Exec(); err != nil {
		return fmt.Errorf("incomingwebhook: insert: %w", err)
	}
	return tx.Commit()
}

// queryByWebhookID 不存在时返回 (nil, nil)；dbr.Load 在无结果时即返回 (0, nil)，
// 调用方按 m == nil 判断未命中，无需特别处理 ErrNotFound（那是 LoadOne 的语义）。
func (d *incomingWebhookDB) queryByWebhookID(webhookID string) (*incomingWebhookModel, error) {
	var m *incomingWebhookModel
	_, err := d.session.Select("*").From("incoming_webhook").
		Where("webhook_id=?", webhookID).Load(&m)
	return m, err
}

// queryByGroupNo 列出群下 webhook 供管理端展示，隐藏软删除（statusDeleted）项（#254）。
func (d *incomingWebhookDB) queryByGroupNo(groupNo string) ([]*incomingWebhookModel, error) {
	var list []*incomingWebhookModel
	_, err := d.session.Select("*").From("incoming_webhook").
		Where("group_no=?", groupNo).
		Where("status != ?", statusDeleted).
		OrderDir("created_at", false).
		Load(&list)
	return list, err
}

// updateFieldsAllowed 限定 updateFields 可写的列，防御未来调用方误传用户输入作 key
// 触发任意列改写。新增可更新列时显式在此追加。
var updateFieldsAllowed = map[string]struct{}{
	"name":       {},
	"avatar":     {},
	"status":     {},
	"token_hash": {},
}

// updateFields 更新单个 webhook 的允许列。带 status != statusDeleted 守卫：对已软删除
// 的行一律不写入——这是并发复活漏洞的根因防线。queryManageable（非事务读）与本次写入
// 之间有 TOCTOU 窗口，若期间被并发 DELETE 软删，无守卫的写会把 status / token_hash
// 写回，令已删除 webhook 复活（重回列表 + 旧 token 可推送）。InnoDB 行锁让该条件 UPDATE
// 与并发 DELETE 的 UPDATE 串行化，保证"一旦删除，任何后续写都落空"。调用方应回读确认
// 行未被软删除（见 api 层 update / regenerate）。
//
// ⚠️ 正确性依赖单语句 autocommit 的当前读（UPDATE ... WHERE 对最新已提交行版本求值）。
// 若未来把 update + 回读包进同一显式 REPEATABLE READ 事务、改用快照 SELECT，这个
// "删除后写必落空"的不变量会被破坏，须改用 SELECT ... FOR UPDATE 重新串行化。
func (d *incomingWebhookDB) updateFields(webhookID string, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	for k := range fields {
		if _, ok := updateFieldsAllowed[k]; !ok {
			return fmt.Errorf("incomingwebhook: updateFields: disallowed column %q", k)
		}
	}
	_, err := d.session.Update("incoming_webhook").
		SetMap(fields).
		Where("webhook_id=?", webhookID).
		Where("status != ?", statusDeleted).Exec()
	return err
}

// deleteByWebhookID 软删除（#254）：把 status 置为 statusDeleted 而非物理 DELETE，
// 保留行供该 webhook 历史消息的发送者名/头像渲染（display datasource 不按 status
// 过滤）。push 闸（status != statusEnabled）随之自动失效，列表/配额按 status !=
// statusDeleted 排除，且 update 不再允许复活已删除行。调用方应先确认目标行存在且
// 未删除（api 层在 query 后判 statusDeleted 返回 not-found）。
// status != statusDeleted 守卫使重复软删除幂等：并发两次 DELETE，第二次落空。
func (d *incomingWebhookDB) deleteByWebhookID(webhookID string) error {
	_, err := d.session.Update("incoming_webhook").
		Set("status", statusDeleted).
		Where("webhook_id=?", webhookID).
		Where("status != ?", statusDeleted).Exec()
	return err
}

// queryMemberRole 一次点读返回 uid 在 groupNo 的成员资格与管理员身份：
//   - isMember：是【内部、正常状态、未删除】成员（与 group.QueryIsGroupManagerOrCreator
//     的 fail-safe 口径一致：is_deleted=0 + is_external=0 + status=Normal）；
//   - isAdmin：在 isMember 基础上 role ∈ {creator, manager}。
//
// 单查询同时服务三处（一次往返拿全两个事实，省掉管理路径的二连击）：
//   - 管理路径的 actor 解析（resolveActor：成员资格 + 管理员判定）；
//   - 管理路径的"创建者仍在群内"闸（requireCreatorInGroup）；
//   - push 路径的创建者在群闸 + 覆盖权限（cachedCreatorMembership 的回源查询，
//     isAdmin 决定 push 的 username/avatar_url 覆盖是否生效）。
//
// 直接点读 group_member 表而非经 group 模块 DB：该表已是 bot_api 等模块的既有
// 跨模块读取面，且这里只需一行，避免给 group.DB 增加仅本模块使用的接口。
func (d *incomingWebhookDB) queryMemberRole(groupNo, uid string) (isMember, isAdmin bool, err error) {
	var roles []int
	_, err = d.session.Select("role").From("group_member").
		Where("group_no=? AND uid=? AND is_deleted=0 AND is_external=0 AND status=?",
			groupNo, uid, int(common.GroupMemberStatusNormal)).
		Limit(1).Load(&roles)
	if err != nil || len(roles) == 0 {
		return false, false, err
	}
	return true, roles[0] == group.MemberRoleCreator || roles[0] == group.MemberRoleManager, nil
}

// disableEnabledByWebhookID 把【仍处于启用态】的单个 webhook 置为禁用，用于 push
// 路径发现创建者已退群时的懒级联禁用。status=statusEnabled 守卫保证：
//   - 幂等（并发懒禁用只有一次生效）；
//   - 绝不触碰软删除行（不会把 deleted 翻成 disabled 而复活到管理列表）。
func (d *incomingWebhookDB) disableEnabledByWebhookID(webhookID string) error {
	_, err := d.session.Update("incoming_webhook").
		Set("status", statusDisabled).
		Where("webhook_id=?", webhookID).
		Where("status = ?", statusEnabled).Exec()
	return err
}

// markUsed 累加调用计数并刷新 last_used_at；非关键路径，调用方应忽略错误（最多记日志）。
// 走 ExecContext：审计在 push 路径的同步兜底分支下跑在请求 goroutine 上，必须受 ctx
// 超时约束，否则 DB 饱和变慢会无限拖住 push 响应。
// status != statusDeleted 守卫：与其它写路径一致，异步审计执行时行若已被并发软删除，
// 不再给已删除 webhook 记账（不影响安全，仅保持纵深防御的一致性）。
func (d *incomingWebhookDB) markUsed(ctx context.Context, webhookID string, now time.Time) error {
	_, err := d.session.UpdateBySql(
		"UPDATE incoming_webhook SET call_count = call_count + 1, last_used_at = ? WHERE webhook_id = ? AND status != ?",
		now, webhookID, statusDeleted,
	).ExecContext(ctx)
	return err
}

// disableByGroupNo 把指定群下所有【未删除】的 webhook 置为禁用，用于群解散等级联场景。
// 必须跳过 statusDeleted 行：否则会把软删除（2）翻成禁用（0），令其重新出现在管理列表
// 并重新占用配额，等同"复活"了已删除的 webhook（#254）。
func (d *incomingWebhookDB) disableByGroupNo(groupNo string) error {
	_, err := d.session.Update("incoming_webhook").
		Set("status", statusDisabled).
		Where("group_no=?", groupNo).
		Where("status != ?", statusDeleted).Exec()
	return err
}

// insertAudit 写一条投递审计（成功或失败）。新增列（status/reason/http_status/adapter）
// 由 util.AttrToUnderscore + Record 反射自动映射，无需改动。走 ExecContext，理由见 markUsed。
func (d *incomingWebhookDB) insertAudit(ctx context.Context, m *auditModel) error {
	_, err := d.session.InsertInto("incoming_webhook_audit").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).ExecContext(ctx)
	return err
}

// queryRecentAudits 倒序取某 webhook 最近 limit 条投递记录，供管理端 deliveries 排障。
// 命中 idx_iwa_webhook_time(webhook_id, created_at) 前缀索引：等值 webhook_id + 按
// created_at 倒序，无需回表全扫。limit 由调用方钳制在合理上限。
//
// 二级排序 id 倒序：created_at 是秒级精度，同秒内多条投递若只按 created_at 排序顺序
// 不确定（「最近」语义会抖动）；id 自增、单调，作 tiebreaker 给出确定的最近优先顺序。
func (d *incomingWebhookDB) queryRecentAudits(webhookID string, limit int) ([]*auditModel, error) {
	var list []*auditModel
	_, err := d.session.Select("*").From("incoming_webhook_audit").
		Where("webhook_id=?", webhookID).
		OrderDir("created_at", false).
		OrderDir("id", false).
		Limit(uint64(limit)).
		Load(&list)
	return list, err
}
