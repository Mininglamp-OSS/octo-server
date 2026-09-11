package category

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

func (c *Category) listSidebarSections(ctx *wkhttp.Context) {
	uid := ctx.GetLoginUID()
	spaceID := ctx.Param("space_id")
	if !c.requireSidebarSectionSpaceMember(ctx, uid, spaceID) {
		return
	}

	sections, err := c.sidebarSections(uid, spaceID)
	if err != nil {
		c.Error("查询侧边栏分区失败", zap.Error(err), zap.String("uid", uid), zap.String("spaceID", spaceID))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	ctx.Response(sections)
}

func (c *Category) sortSidebarSections(ctx *wkhttp.Context) {
	uid := ctx.GetLoginUID()
	spaceID := ctx.Param("space_id")
	if !c.requireSidebarSectionSpaceMember(ctx, uid, spaceID) {
		return
	}

	var req sortSidebarSectionsReq
	if err := ctx.BindJSON(&req); err != nil {
		respondCategoryRequestInvalid(ctx, "")
		return
	}
	if len(req.Items) == 0 {
		respondCategoryRequestInvalid(ctx, "items")
		return
	}

	current, err := c.sidebarSectionKeys(uid, spaceID)
	if err != nil {
		c.Error("读取侧边栏分区排序前态失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	if len(req.Items) != len(current) {
		httperr.ResponseErrorL(ctx, errcode.ErrCategorySortListMismatch, nil, nil)
		return
	}

	currentByKey := make(map[string]struct{}, len(current))
	for _, key := range current {
		currentByKey[key] = struct{}{}
	}
	seen := make(map[string]struct{}, len(req.Items))
	for _, item := range req.Items {
		if sidebarSectionTypeFromAPI(item.Type) == 0 || item.ID == "" {
			respondCategoryRequestInvalid(ctx, "items")
			return
		}
		key := sidebarSectionKey(item.Type, item.ID)
		if _, ok := seen[key]; ok {
			httperr.ResponseErrorL(ctx, errcode.ErrCategorySortListDuplicate, nil, nil)
			return
		}
		seen[key] = struct{}{}
		if _, ok := currentByKey[key]; !ok {
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryNotFound, nil, nil)
			return
		}
	}

	tx, err := c.ctx.DB().Begin()
	if err != nil {
		c.Error("开启侧边栏分区排序事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()
	for index, item := range req.Items {
		if err := c.db.updateSidebarSectionSortTx(tx, uid, spaceID, sidebarSectionTypeFromAPI(item.Type), item.ID, index); err != nil {
			c.Error("更新侧边栏分区排序失败", zap.Error(err), zap.String("type", item.Type), zap.String("id", item.ID))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
			return
		}
	}
	if _, err := convext.BumpFollowVersionTx(tx, uid, spaceID); err != nil {
		c.Error("更新 follow_version 失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}
	if err := tx.Commit(); err != nil {
		c.Error("提交侧边栏分区排序事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}
	ctx.ResponseOK()
}

func (c *Category) requireSidebarSectionSpaceMember(ctx *wkhttp.Context, uid, spaceID string) bool {
	isMember, err := spacepkg.CheckMembership(c.db.session, spaceID, uid)
	if err != nil {
		c.Error("检查空间成员失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return false
	}
	if !isMember {
		httperr.ResponseErrorL(ctx, errcode.ErrCategorySpaceMemberRequired, nil, nil)
		return false
	}
	return true
}

func (c *Category) sidebarSections(uid, spaceID string) ([]sidebarSectionResp, error) {
	categories, err := c.listCategoryResponses(uid, spaceID)
	if err != nil {
		return nil, err
	}
	if err := c.db.ensureActiveProjectSidebarSections(uid, spaceID); err != nil {
		return nil, err
	}
	projects, err := c.db.querySidebarProjectSections(uid, spaceID)
	if err != nil {
		return nil, err
	}
	order, err := c.db.querySidebarSectionOrder(uid, spaceID)
	if err != nil {
		return nil, err
	}

	categoryByID := make(map[string]categoryResp, len(categories))
	for _, category := range categories {
		if category.CategoryID != nil {
			categoryByID[*category.CategoryID] = category
		}
	}
	projectByID := make(map[string]*sidebarProjectSectionResp, len(projects))
	projectIDs := make([]string, 0, len(projects))
	for _, section := range projects {
		projectIDs = append(projectIDs, section.ProjectID)
	}
	groupsByProject, err := projectmod.ListProjectGroupRelationsByProjectIDs(c.ctx, spaceID, uid, projectIDs)
	if err != nil {
		return nil, fmt.Errorf("list project groups for sidebar sections: %w", err)
	}
	for _, section := range projects {
		projectByID[section.ProjectID] = &sidebarProjectSectionResp{
			ProjectID:        section.ProjectID,
			ProjectName:      section.Name,
			Logo:             section.Logo,
			AllMemberGroupNo: section.AllMemberGroupNo,
			Groups:           groupsByProject[section.ProjectID],
		}
	}

	result := make([]sidebarSectionResp, 0, len(order))
	for _, row := range order {
		switch row.SectionType {
		case sidebarSectionTypeCategory:
			category, ok := categoryByID[row.RefID]
			if !ok {
				continue
			}
			result = append(result, sidebarSectionResp{
				Type: sidebarSectionAPITypeCategory, ID: row.RefID, Sort: row.Sort, Category: &category,
			})
		case sidebarSectionTypeProject:
			project, ok := projectByID[row.RefID]
			if !ok {
				// A departed, disbanded, or non-member Project is omitted even if
				// an old ordering or pin row remains. The visibility query is the
				// read gate and never grants Project access.
				continue
			}
			result = append(result, sidebarSectionResp{
				Type: sidebarSectionAPITypeProject, ID: row.RefID, Sort: row.Sort, Project: project,
			})
		}
	}
	return result, nil
}

// sidebarSectionKeys returns exactly the visible (type,id) set accepted by the
// sort endpoint without loading category contents or Project groups. It keeps
// the same repair and visibility gates as sidebarSections so stale clients get
// a list-mismatch response instead of sorting hidden or foreign rows.
func (c *Category) sidebarSectionKeys(uid, spaceID string) ([]string, error) {
	if err := EnsureDefaultCategory(c.ctx, uid, spaceID); err != nil {
		c.Warn("确保默认分类失败（降级继续）", zap.Error(err), zap.String("uid", uid), zap.String("spaceID", spaceID))
	}
	rawCategories, err := c.db.queryRawCategoryModels(uid, spaceID)
	if err != nil {
		return nil, err
	}
	if err := c.db.ensureCategorySidebarSections(rawCategories); err != nil {
		return nil, err
	}
	if err := c.db.ensureActiveProjectSidebarSections(uid, spaceID); err != nil {
		return nil, err
	}
	categories, err := c.db.queryCategoryModelsForSidebar(uid, spaceID)
	if err != nil {
		return nil, err
	}
	projects, err := c.db.querySidebarProjectSections(uid, spaceID)
	if err != nil {
		return nil, err
	}
	order, err := c.db.querySidebarSectionOrder(uid, spaceID)
	if err != nil {
		return nil, err
	}

	visible := make(map[string]struct{}, len(categories)+len(projects))
	for _, category := range categories {
		visible[sidebarSectionKey(sidebarSectionAPITypeCategory, category.CategoryID)] = struct{}{}
	}
	for _, project := range projects {
		visible[sidebarSectionKey(sidebarSectionAPITypeProject, project.ProjectID)] = struct{}{}
	}

	keys := make([]string, 0, len(visible))
	for _, row := range order {
		var sectionType string
		switch row.SectionType {
		case sidebarSectionTypeCategory:
			sectionType = sidebarSectionAPITypeCategory
		case sidebarSectionTypeProject:
			sectionType = sidebarSectionAPITypeProject
		default:
			continue
		}
		key := sidebarSectionKey(sectionType, row.RefID)
		if _, ok := visible[key]; ok {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func sidebarSectionTypeFromAPI(sectionType string) int {
	switch sectionType {
	case sidebarSectionAPITypeCategory:
		return sidebarSectionTypeCategory
	case sidebarSectionAPITypeProject:
		return sidebarSectionTypeProject
	default:
		return 0
	}
}

func sidebarSectionKey(sectionType, id string) string {
	return sectionType + "\x00" + id
}
