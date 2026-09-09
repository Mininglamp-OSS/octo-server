package category

import (
	"fmt"
	"time"

	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/gocraft/dbr/v2"
)

func (d *categoryDB) maxSidebarSectionSort(uid, spaceID string) (int, error) {
	var maxSort int
	_, err := d.session.Select("IFNULL(MAX(sort), -1)").From("octo_sidebar_section").
		Where("uid=? AND space_id=? AND status=1", uid, spaceID).
		Load(&maxSort)
	return maxSort, err
}

// ensureSidebarSection is idempotent through the typed unique key. A concurrent
// first insert may choose the same sort as another new entry; that is harmless
// because the primary key remains the deterministic tie-breaker until the user
// explicitly orders both entries.
func (d *categoryDB) ensureSidebarSection(uid, spaceID string, sectionType int, refID string, sort int) error {
	if uid == "" || spaceID == "" || refID == "" {
		return nil
	}
	now := time.Now().UTC()
	_, err := d.session.InsertBySql(
		"INSERT IGNORE INTO octo_sidebar_section "+
			"(uid, space_id, section_type, ref_id, sort, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, 1, ?, ?)",
		uid, spaceID, sectionType, refID, sort, now, now,
	).Exec()
	if err != nil {
		return fmt.Errorf("category: ensure sidebar section: %w", err)
	}
	return nil
}

func (d *categoryDB) ensureCategorySidebarSections(categories []*CategoryModel) error {
	for _, category := range categories {
		if err := d.ensureSidebarSection(
			category.UID, category.SpaceID, sidebarSectionTypeCategory, category.CategoryID, category.Sort,
		); err != nil {
			return err
		}
	}
	return nil
}

func (d *categoryDB) createCategoryAndSidebar(m *CategoryModel) error {
	tx, err := d.session.Begin()
	if err != nil {
		return fmt.Errorf("category: begin create sidebar section: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if _, err := tx.InsertBySql(
		"INSERT INTO group_category (category_id, space_id, uid, name, sort, status, is_default) VALUES (?, ?, ?, ?, ?, ?, ?)",
		m.CategoryID, m.SpaceID, m.UID, m.Name, m.Sort, m.Status, m.IsDefault,
	).Exec(); err != nil {
		return fmt.Errorf("category: insert category: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.InsertBySql(
		"INSERT INTO octo_sidebar_section (uid, space_id, section_type, ref_id, sort, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 1, ?, ?)",
		m.UID, m.SpaceID, sidebarSectionTypeCategory, m.CategoryID, m.Sort, now, now,
	).Exec(); err != nil {
		return fmt.Errorf("category: insert sidebar section: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("category: commit create sidebar section: %w", err)
	}
	return nil
}

func (d *categoryDB) queryCategoryModelsForSidebar(uid, spaceID string) ([]*CategoryModel, error) {
	var models []*CategoryModel
	_, err := d.session.Select(
		"gc.category_id", "gc.space_id", "gc.uid", "gc.name", "ss.sort", "gc.status", "gc.is_default",
	).
		From(dbr.I("group_category").As("gc")).
		Join(dbr.I("octo_sidebar_section").As("ss"),
			"ss.uid=gc.uid AND ss.space_id=gc.space_id AND ss.section_type=1 AND ss.ref_id=gc.category_id AND ss.status=1").
		Where("gc.uid=? AND gc.space_id=? AND gc.status=1", uid, spaceID).
		OrderAsc("ss.sort").
		OrderAsc("ss.id").
		Load(&models)
	return models, err
}

func (d *categoryDB) queryRawCategoryModels(uid, spaceID string) ([]*CategoryModel, error) {
	var models []*CategoryModel
	_, err := d.session.Select("category_id", "space_id", "uid", "name", "sort", "status", "is_default").
		From("group_category").
		Where("uid=? AND space_id=? AND status=1", uid, spaceID).
		OrderAsc("sort").
		OrderAsc("id").
		Load(&models)
	return models, err
}

func (d *categoryDB) querySidebarProjectSections(uid, spaceID string) ([]*sidebarProjectSectionModel, error) {
	var models []*sidebarProjectSectionModel
	_, err := d.session.Select(
		"p.project_id", "p.name", "p.logo", "p.all_member_group_no", "ss.sort",
	).
		From(dbr.I("octo_sidebar_section").As("ss")).
		Join(dbr.I("octo_project").As("p"),
			"p.project_id=ss.ref_id AND p.space_id=ss.space_id AND p.status=1").
		Where("ss.uid=? AND ss.space_id=? AND ss.section_type=? AND ss.status=1 "+
			"AND (EXISTS (SELECT 1 FROM octo_project_member pm WHERE pm.project_id=p.project_id AND pm.uid=? "+
			"AND pm.space_id=p.space_id AND pm.status=1 AND pm.removing=0) OR "+
			"(p.discoverability=? AND EXISTS (SELECT 1 FROM octo_project_user_setting ps "+
			"WHERE ps.project_id=p.project_id AND ps.uid=ss.uid AND ps.pinned=1)))",
			uid, spaceID, sidebarSectionTypeProject, uid, projectmod.DiscoverabilitySpaceListed).
		OrderAsc("ss.sort").
		OrderAsc("ss.id").
		Load(&models)
	return models, err
}

// ensureActiveProjectSidebarSections is the list-path compensator for the
// post-commit project hooks. A hook may be unavailable during a rolling deploy
// or its independent write may fail; active membership and an explicit pin of a
// Space-listed Project are both valid ways into the personal sidebar, so a read
// repairs every missing section before rendering the unified list.
func (d *categoryDB) ensureActiveProjectSidebarSections(uid, spaceID string) error {
	projectIDs, err := d.queryVisibleProjectSidebarRefs(uid, spaceID)
	if err != nil {
		return err
	}
	if len(projectIDs) == 0 {
		return nil
	}

	maxSort, err := d.maxSidebarSectionSort(uid, spaceID)
	if err != nil {
		return err
	}
	for index, projectID := range projectIDs {
		if err := d.ensureSidebarSection(uid, spaceID, sidebarSectionTypeProject, projectID, maxSort+index+1); err != nil {
			return err
		}
	}
	return nil
}

func (d *categoryDB) queryVisibleProjectSidebarRefs(uid, spaceID string) ([]string, error) {
	var projectIDs []string
	_, err := d.session.Select("p.project_id").
		From(dbr.I("octo_project").As("p")).
		Where("p.space_id=? AND p.status=1 AND "+
			"(EXISTS (SELECT 1 FROM octo_project_member pm WHERE pm.project_id=p.project_id AND pm.uid=? "+
			"AND pm.space_id=p.space_id AND pm.status=1 AND pm.removing=0) OR "+
			"(p.discoverability=? AND EXISTS (SELECT 1 FROM octo_project_user_setting ps "+
			"WHERE ps.project_id=p.project_id AND ps.uid=? AND ps.pinned=1)))",
			spaceID, uid, projectmod.DiscoverabilitySpaceListed, uid).
		OrderAsc("p.id").
		Load(&projectIDs)
	return projectIDs, err
}

func (d *categoryDB) querySidebarSectionOrder(uid, spaceID string) ([]*sidebarSectionOrderRow, error) {
	var rows []*sidebarSectionOrderRow
	_, err := d.session.Select("section_type", "ref_id", "sort").From("octo_sidebar_section").
		Where("uid=? AND space_id=? AND status=1", uid, spaceID).
		OrderAsc("sort").
		OrderAsc("id").
		Load(&rows)
	return rows, err
}

func (d *categoryDB) updateSidebarSectionSortTx(tx *dbr.Tx, uid, spaceID string, sectionType int, refID string, sort int) error {
	_, err := tx.Update("octo_sidebar_section").
		Set("sort", sort).
		Set("updated_at", time.Now().UTC()).
		Where("uid=? AND space_id=? AND section_type=? AND ref_id=? AND status=1", uid, spaceID, sectionType, refID).
		Exec()
	return err
}

func (d *categoryDB) hideSidebarSectionTx(tx *dbr.Tx, uid, spaceID string, sectionType int, refID string) error {
	_, err := tx.Update("octo_sidebar_section").
		Set("status", 2).
		Set("updated_at", time.Now().UTC()).
		Where("uid=? AND space_id=? AND section_type=? AND ref_id=? AND status=1", uid, spaceID, sectionType, refID).
		Exec()
	return err
}
