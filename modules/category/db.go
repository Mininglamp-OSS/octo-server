package category

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

type categoryDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newCategoryDB(ctx *config.Context) *categoryDB {
	return &categoryDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

func (d *categoryDB) insertCategory(m *CategoryModel) error {
	_, err := d.session.InsertInto("group_category").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

func (d *categoryDB) queryCategoriesByUIDAndSpaceID(uid, spaceID string) ([]*CategoryModel, error) {
	return d.queryCategoryModelsForSidebar(uid, spaceID)
}

func (d *categoryDB) queryDefaultCategory(uid, spaceID string) (*CategoryModel, error) {
	var model *CategoryModel
	_, err := d.session.Select("*").From("group_category").
		Where("uid=? and space_id=? and is_default=1 and status=1", uid, spaceID).
		Limit(1).
		Load(&model)
	return model, err
}

func (d *categoryDB) insertDefaultCategory(m *CategoryModel) error {
	m.IsDefault = intPtr(1)
	m.Status = 1
	_, err := d.session.InsertBySql(
		"INSERT IGNORE INTO group_category (category_id, space_id, uid, name, sort, status, is_default) VALUES (?, ?, ?, ?, ?, 1, 1)",
		m.CategoryID, m.SpaceID, m.UID, m.Name, m.Sort,
	).Exec()
	return err
}

func (d *categoryDB) queryCategoryByID(categoryID string) (*CategoryModel, error) {
	var model *CategoryModel
	_, err := d.session.Select("*").From("group_category").
		Where("category_id=? and status=1", categoryID).
		Load(&model)
	return model, err
}

func (d *categoryDB) countCategoriesByUIDAndSpaceID(uid, spaceID string) (int, error) {
	var count int
	_, err := d.session.Select("count(*)").From("group_category").
		Where("uid=? and space_id=? and status=1 and (is_default IS NULL)", uid, spaceID).
		Load(&count)
	return count, err
}

func (d *categoryDB) maxSortByUIDAndSpaceID(uid, spaceID string) (int, error) {
	return d.maxSidebarSectionSort(uid, spaceID)
}

func (d *categoryDB) updateCategoryName(categoryID, name string) error {
	_, err := d.session.Update("group_category").
		Set("name", name).
		Where("category_id=?", categoryID).
		Exec()
	return err
}
