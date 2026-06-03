package incomingwebhook

import (
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

// ErrQuotaExceeded 创建时配额已满，由 insertWithQuota 在事务内原子判定。
var ErrQuotaExceeded = errors.New("incomingwebhook: per-group quota exceeded")

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
func (d *incomingWebhookDB) insertWithQuota(m *incomingWebhookModel, max int) error {
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
		"SELECT count(*) FROM incoming_webhook WHERE group_no=?",
		m.GroupNo,
	).Load(&count); err != nil {
		return fmt.Errorf("incomingwebhook: count: %w", err)
	}
	if count >= max {
		return ErrQuotaExceeded
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

func (d *incomingWebhookDB) queryByGroupNo(groupNo string) ([]*incomingWebhookModel, error) {
	var list []*incomingWebhookModel
	_, err := d.session.Select("*").From("incoming_webhook").
		Where("group_no=?", groupNo).
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
		Where("webhook_id=?", webhookID).Exec()
	return err
}

func (d *incomingWebhookDB) deleteByWebhookID(webhookID string) error {
	_, err := d.session.DeleteFrom("incoming_webhook").
		Where("webhook_id=?", webhookID).Exec()
	return err
}

// markUsed 累加调用计数并刷新 last_used_at；非关键路径，调用方应忽略错误（最多记日志）。
func (d *incomingWebhookDB) markUsed(webhookID string, now time.Time) error {
	_, err := d.session.UpdateBySql(
		"UPDATE incoming_webhook SET call_count = call_count + 1, last_used_at = ? WHERE webhook_id = ?",
		now, webhookID,
	).Exec()
	return err
}

// disableByGroupNo 把指定群下所有 webhook 置为禁用，用于群解散等级联场景。
func (d *incomingWebhookDB) disableByGroupNo(groupNo string) error {
	_, err := d.session.Update("incoming_webhook").
		Set("status", 0).
		Where("group_no=?", groupNo).Exec()
	return err
}

func (d *incomingWebhookDB) insertAudit(m *auditModel) error {
	_, err := d.session.InsertInto("incoming_webhook_audit").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).Exec()
	return err
}
