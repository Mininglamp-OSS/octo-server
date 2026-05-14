package incomingwebhook

import (
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

type incomingWebhookDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newDB(ctx *config.Context) *incomingWebhookDB {
	return &incomingWebhookDB{ctx: ctx, session: ctx.DB()}
}

func (d *incomingWebhookDB) insert(m *incomingWebhookModel) error {
	_, err := d.session.InsertInto("incoming_webhook").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).Exec()
	if err != nil {
		return fmt.Errorf("incomingwebhook: insert: %w", err)
	}
	return nil
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

// countByGroupNo 统计某群下所有 webhook 数量，**不**过滤 status——禁用的也占配额。
// 这是有意为之：避免 enable→disable→create 循环绕过 max_per_group 限制。
// 删除（不仅是禁用）才会释放配额。
func (d *incomingWebhookDB) countByGroupNo(groupNo string) (int, error) {
	var n int
	err := d.session.Select("count(*)").From("incoming_webhook").
		Where("group_no=?", groupNo).LoadOne(&n)
	return n, err
}

func (d *incomingWebhookDB) updateFields(webhookID string, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
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
