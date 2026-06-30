package sticker

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

type stickerDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newStickerDB(ctx *config.Context) *stickerDB {
	return &stickerDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

func (d *stickerDB) insert(m *StickerModel) error {
	_, err := d.session.InsertInto("sticker").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// listByUID returns the user's live stickers, newest first.
func (d *stickerDB) listByUID(uid string) ([]*StickerModel, error) {
	var models []*StickerModel
	_, err := d.session.Select("*").From("sticker").
		Where("uid=? and status=1", uid).
		OrderDesc("id").
		Load(&models)
	return models, err
}

// insertTx inserts within an existing transaction. add() wraps the quota
// count and this insert in one tx so the per-user cap is enforced atomically.
func (d *stickerDB) insertTx(tx *dbr.Tx, m *StickerModel) error {
	_, err := tx.InsertInto("sticker").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// lockUserRowTx takes a record lock on the caller's user row inside tx, so
// concurrent adds for the same user serialize on a guaranteed-existing single
// row matched through the UNIQUE(uid) index — a record lock, not a gap lock.
//
// Why not `SELECT count(*) FROM sticker WHERE uid=? AND status=1 FOR UPDATE`:
// that predicate hits the NON-unique idx_uid_status, which under REPEATABLE READ
// takes next-key/gap locks (cf. category/api.go:619 "扩大锁范围、增大死锁概率").
// On a user's FIRST add it matches zero rows → a pure gap lock; gap-X locks are
// mutually compatible, so concurrent first-adds both pass the count check and
// then contend on insert-intention locks → InnoDB deadlock (1213), surfaced as
// an opaque 500. Locking the parent user row (the repo's established quota-guard
// pattern, incomingwebhook.insertWithQuota) sidesteps the gap entirely
// (PR#508 review: Jerry-Xin / yujiawei / OctoBoooot).
//
// The user row is guaranteed to exist for any authenticated caller. If it is
// somehow absent (e.g. a synthetic test context), the SELECT matches no row and
// we degrade to "no extra lock" rather than failing the add — count+insert stays
// correct; only the concurrent-first-add serialization is lost in that case.
func (d *stickerDB) lockUserRowTx(tx *dbr.Tx, uid string) error {
	var locked string
	_, err := tx.SelectBySql("SELECT uid FROM `user` WHERE uid=? FOR UPDATE", uid).Load(&locked)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil
	}
	return err
}

// countByUIDTx counts the user's live stickers within tx. No FOR UPDATE: a
// preceding lockUserRowTx already serialized same-user adds, so a plain count is
// exact and avoids the child-range gap lock.
func (d *stickerDB) countByUIDTx(tx *dbr.Tx, uid string) (int, error) {
	var count int
	_, err := tx.SelectBySql("SELECT count(*) FROM sticker WHERE uid=? AND status=1", uid).Load(&count)
	return count, err
}

func (d *stickerDB) queryByID(stickerID string) (*StickerModel, error) {
	var model *StickerModel
	_, err := d.session.Select("*").From("sticker").
		Where("sticker_id=? and status=1", stickerID).
		Load(&model)
	return model, err
}

// softDelete marks the sticker deleted. The uid predicate is a defensive
// belt-and-suspenders filter on top of the handler's ownership check, so a
// future caller that forgets the ownership guard still cannot delete another
// user's sticker.
func (d *stickerDB) softDelete(stickerID, uid string) error {
	_, err := d.session.Update("sticker").
		Set("status", 2).
		Where("sticker_id=? and uid=?", stickerID, uid).
		Exec()
	return err
}
