package sticker

import (
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

func (d *stickerDB) countByUID(uid string) (int, error) {
	var count int
	_, err := d.session.Select("count(*)").From("sticker").
		Where("uid=? and status=1", uid).
		Load(&count)
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
