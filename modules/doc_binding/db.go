package doc_binding

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/gocraft/dbr/v2"
)

type db struct {
	session *dbr.Session
	ctx     *config.Context
}

func newDB(ctx *config.Context) *db {
	return &db{ctx: ctx, session: ctx.DB()}
}

// store 是 handler 依赖的持久化面；提出接口是让单测能塞 in-memory 实现，
// 生产走 *db（MySQL / gocraft-dbr）。方法集刻意小，别把 DB 内部选择漏出去。
type store interface {
	insert(m *Model) error
	queryBySlug(slug string) (*Model, error)
	updateAllowShareCode(slug string, allow int) error
	deleteBySlug(slug string) error
}

// compile-time 声明：*db 必须满足 store。
var _ store = (*db)(nil)

// insert 新建 binding；slug 唯一索引会把并发重复挂载兜住（调用端翻成 409）。
func (d *db) insert(m *Model) error {
	_, err := d.session.InsertInto("doc_binding").
		Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// queryBySlug slug 主查询；未命中返回 (nil, nil)，交给调用端决定 404/hidden-404。
func (d *db) queryBySlug(slug string) (*Model, error) {
	var m Model
	_, err := d.session.Select("*").From("doc_binding").
		Where("slug=?", slug).Load(&m)
	if m.Slug == "" {
		return nil, err
	}
	return &m, err
}

// updateAllowShareCode PUT 目前只允许改 allow_share_code；其他字段（mount/挂载点）不可变，
// 想换挂载点应删了重建，避免历史 slug 权限漂移。
func (d *db) updateAllowShareCode(slug string, allow int) error {
	_, err := d.session.Update("doc_binding").
		Set("allow_share_code", allow).
		Where("slug=?", slug).Exec()
	return err
}

func (d *db) deleteBySlug(slug string) error {
	_, err := d.session.DeleteFrom("doc_binding").Where("slug=?", slug).Exec()
	return err
}
