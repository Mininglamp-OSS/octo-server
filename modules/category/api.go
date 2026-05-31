package category

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// Category 群组类别 API
type Category struct {
	ctx *config.Context
	log.Log
	db *categoryDB
}

// New 创建 Category 实例
func New(ctx *config.Context) *Category {
	return &Category{
		ctx: ctx,
		Log: log.NewTLog("Category"),
		db:  newCategoryDB(ctx),
	}
}

// Route 路由配置
func (c *Category) Route(r *wkhttp.WKHttp) {
	uidLimit := appwkhttp.SharedUIDRateLimiter(r, c.ctx)
	spaces := r.Group("/v1/spaces", c.ctx.AuthMiddleware(r), uidLimit)
	{
		spaces.POST("/:space_id/categories", c.create)
		spaces.GET("/:space_id/categories", c.list)
		spaces.PUT("/:space_id/categories/sort", c.sort)
		spaces.PUT("/:space_id/categories/:category_id", c.update)
		spaces.DELETE("/:space_id/categories/:category_id", c.delete)
	}

	groups := r.Group("/v1/groups", c.ctx.AuthMiddleware(r), uidLimit)
	{
		groups.PUT("/:group_no/category", c.moveGroupToCategory)
	}
}

// create 创建类别
func (c *Category) create(ctx *wkhttp.Context) {
	loginUID := ctx.GetLoginUID()
	spaceID := ctx.Param("space_id")

	isMember, err := spacepkg.CheckMembership(c.db.session, spaceID, loginUID)
	if err != nil {
		c.Error("检查空间成员失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(ctx, errcode.ErrCategorySpaceMemberRequired, nil, nil)
		return
	}

	var req createCategoryReq
	if err := ctx.BindJSON(&req); err != nil {
		respondCategoryRequestInvalid(ctx, "")
		return
	}
	if req.Name == "" {
		respondCategoryRequestInvalid(ctx, "name")
		return
	}
	if len([]rune(req.Name)) > 100 {
		respondCategoryNameTooLong(ctx, 100)
		return
	}

	count, err := c.db.countCategoriesByUIDAndSpaceID(loginUID, spaceID)
	if err != nil {
		c.Error("查询类别数量失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	if count >= 20 {
		respondCategoryLimitExceeded(ctx, 20)
		return
	}

	categoryID := util.GenerUUID()
	nextSort, err := c.db.maxSortByUIDAndSpaceID(loginUID, spaceID)
	if err != nil {
		c.Error("查询排序值失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	nextSort++
	err = c.db.insertCategory(&CategoryModel{
		CategoryID: categoryID,
		SpaceID:    spaceID,
		UID:        loginUID,
		Name:       req.Name,
		Sort:       nextSort,
		Status:     1,
	})
	if err != nil {
		c.Error("创建类别失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	ctx.Response(categoryResp{
		CategoryID: &categoryID,
		Name:       req.Name,
		Sort:       nextSort,
		IsDefault:  false,
		Groups:     make([]groupInCategoryResp, 0),
	})
}

// list 获取当前用户的类别列表（含群组树形结构）
func (c *Category) list(ctx *wkhttp.Context) {
	loginUID := ctx.GetLoginUID()
	spaceID := ctx.Param("space_id")

	isMember, err := spacepkg.CheckMembership(c.db.session, spaceID, loginUID)
	if err != nil {
		c.Error("检查空间成员失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(ctx, errcode.ErrCategorySpaceMemberRequired, nil, nil)
		return
	}

	// 兜底：确保 (uid, spaceID) 下默认分类存在（GH octo-server#1228）。
	// 创建 space / 加入 space 路径已在 space 模块预先补一条；此处为老用户 / 异常路径
	// 的防御性补偿。INSERT IGNORE 幂等，失败降级为 warn，不中断列表返回。
	if err := EnsureDefaultCategory(c.ctx, loginUID, spaceID); err != nil {
		c.Warn("确保默认分类失败（降级继续）", zap.Error(err), zap.String("uid", loginUID), zap.String("spaceID", spaceID))
	}

	categories, err := c.db.queryCategoriesByUIDAndSpaceID(loginUID, spaceID)
	if err != nil {
		c.Error("查询类别失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}

	groups, err := c.db.queryUserGroupsInSpace(loginUID, spaceID)
	if err != nil {
		c.Error("查询群组失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}

	// 按 category_id 分组
	categoryGroupMap := make(map[string][]groupInCategoryResp)
	var uncategorized []groupInCategoryResp
	for _, g := range groups {
		gr := groupInCategoryResp{
			GroupNo:      g.GroupNo,
			Name:         g.GroupName,
			CategorySort: g.CategorySort,
		}
		if g.CategoryID == nil || *g.CategoryID == "" {
			uncategorized = append(uncategorized, gr)
		} else {
			categoryGroupMap[*g.CategoryID] = append(categoryGroupMap[*g.CategoryID], gr)
		}
	}

	// 如果有未分类群组，确保默认分类存在
	if len(uncategorized) > 0 {
		defaultCat, err := c.db.queryDefaultCategory(loginUID, spaceID)
		if err != nil {
			c.Error("查询默认类别失败", zap.Error(err))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
			return
		}
		if defaultCat == nil {
			maxSort, err := c.db.maxSortByUIDAndSpaceID(loginUID, spaceID)
			if err != nil {
				c.Error("查询排序值失败", zap.Error(err))
				httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
				return
			}
			newDefault := &CategoryModel{
				CategoryID: util.GenerUUID(),
				SpaceID:    spaceID,
				UID:        loginUID,
				Name:       defaultCategoryNamePlaceholder,
				Sort:       maxSort + 1,
				IsDefault:  intPtr(1),
			}
			if err = c.db.insertDefaultCategory(newDefault); err != nil {
				c.Error("创建默认类别失败", zap.Error(err))
				httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
				return
			}
			// INSERT IGNORE 后重查，确保拿到实际行（防并发竞态）
			categories, err = c.db.queryCategoriesByUIDAndSpaceID(loginUID, spaceID)
			if err != nil {
				c.Error("查询类别失败", zap.Error(err))
				httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
				return
			}
		}
	}

	result := make([]categoryResp, 0, len(categories))
	defaultSeen := false
	for _, cat := range categories {
		catID := cat.CategoryID
		if cat.isDefault() {
			if defaultSeen {
				continue
			}
			defaultSeen = true
			if uncategorized == nil {
				uncategorized = make([]groupInCategoryResp, 0)
			}
			explicit := categoryGroupMap[cat.CategoryID]
			merged := make([]groupInCategoryResp, 0, len(uncategorized)+len(explicit))
			merged = append(merged, uncategorized...)
			merged = append(merged, explicit...)
			displayName := cat.Name
			if displayName == defaultCategoryNamePlaceholder {
				displayName = defaultCategoryName()
			}
			result = append(result, categoryResp{
				CategoryID: &catID,
				Name:       displayName,
				Sort:       cat.Sort,
				IsDefault:  true,
				Groups:     merged,
			})
		} else {
			catGroups := categoryGroupMap[cat.CategoryID]
			if catGroups == nil {
				catGroups = make([]groupInCategoryResp, 0)
			}
			result = append(result, categoryResp{
				CategoryID: &catID,
				Name:       cat.Name,
				Sort:       cat.Sort,
				IsDefault:  false,
				Groups:     catGroups,
			})
		}
	}

	ctx.Response(result)
}

// update 更新类别名称
func (c *Category) update(ctx *wkhttp.Context) {
	loginUID := ctx.GetLoginUID()
	categoryID := ctx.Param("category_id")

	cat, err := c.db.queryCategoryByID(categoryID)
	if err != nil {
		c.Error("查询类别失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	if cat == nil {
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryNotFound, nil, nil)
		return
	}
	if cat.UID != loginUID {
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryPermissionDenied, nil, nil)
		return
	}
	if cat.isDefault() {
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryDefaultImmutable, nil, nil)
		return
	}

	var req updateCategoryReq
	if err := ctx.BindJSON(&req); err != nil {
		respondCategoryRequestInvalid(ctx, "")
		return
	}
	if req.Name == "" {
		respondCategoryRequestInvalid(ctx, "name")
		return
	}
	if len([]rune(req.Name)) > 100 {
		respondCategoryNameTooLong(ctx, 100)
		return
	}

	err = c.db.updateCategoryName(categoryID, req.Name)
	if err != nil {
		c.Error("更新类别失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	ctx.ResponseOK()
}

// delete 删除类别（事务保证原子性）
func (c *Category) delete(ctx *wkhttp.Context) {
	loginUID := ctx.GetLoginUID()
	categoryID := ctx.Param("category_id")

	cat, err := c.db.queryCategoryByID(categoryID)
	if err != nil {
		c.Error("查询类别失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	if cat == nil {
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryNotFound, nil, nil)
		return
	}
	if cat.UID != loginUID {
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryPermissionDenied, nil, nil)
		return
	}
	if cat.isDefault() {
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryDefaultUndeletable, nil, nil)
		return
	}

	tx, err := c.ctx.DB().Begin()
	if err != nil {
		c.Error("开启事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()

	// 1. 把分组标记为已删除。
	// AND uid=? 是防御性过滤——上层 cat.UID != loginUID 检查已能挡住越权，
	// 这里再加一道，避免将来调用方绕过 ownership 检查时仍能误改他人分组。
	if _, err = tx.Update("group_category").
		Set("status", 2).
		Where("category_id=? AND uid=?", categoryID, loginUID).
		Exec(); err != nil {
		c.Error("删除类别失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	// 2. 采集该分组下用户名下的群编号列表（在解绑前先读，否则丢失对应关系）。
	// FOR UPDATE 锁住目标 group_setting 行：本路径走 group_setting → version → ext
	// 的锁序，moveGroupToCategory 也是同向；不加锁的话同用户并发 move/delete 时
	// 会出现 G1 既被移到新分组又被标记 group_unfollowed=1 的不一致状态
	// （PR #74 review by yujiawei P1）。
	var groupNos []string
	if _, err = tx.SelectBySql(
		"SELECT group_no FROM group_setting WHERE category_id=? AND uid=? FOR UPDATE",
		categoryID, loginUID,
	).Load(&groupNos); err != nil {
		c.Error("查询分组下群列表失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	// 3. 清空 group_setting.category_id 必须在 bump 之前。
	// 锁序统一为 group_setting → user_follow_version → user_conversation_ext，
	// 与 moveGroupToCategory（先 UPDATE group_setting 再 bump）一致，
	// 否则两路并发时会形成 AB-BA 死锁。
	// 同时这一步也避免重新关注后 category_id 仍指向已删分组（list 渲染会丢群）。
	if _, err = tx.Update("group_setting").
		Set("category_id", nil).
		Set("category_sort", 0).
		Where("category_id=? and uid=?", categoryID, loginUID).
		Exec(); err != nil {
		c.Error("清理群设置失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	// 4. bump follow_version 必须先于 user_conversation_ext 的写操作，
	// 与 UpdateSort 同序拿 (version → ext) 锁（PR #21 Round-3 blocker #2）。
	if _, err := convext.BumpFollowVersionTx(tx, loginUID, cat.SpaceID); err != nil {
		c.Error("更新 follow_version 失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	// 5. 退订语义（前端提示「分组下的所有会话将取消关注」）：
	//    - 群：group_unfollowed=1 + 级联删 thread ext 行
	//    - DM：DELETE dm_category_id=cat 的 ext 行
	if err := convext.UnfollowGroupsTx(tx, loginUID, cat.SpaceID, groupNos); err != nil {
		c.Error("取消关注分组下群失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}
	if err := convext.UnfollowDMsByCategoryTx(tx, loginUID, cat.SpaceID, categoryID); err != nil {
		c.Error("取消关注分组下私聊失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	if err = tx.Commit(); err != nil {
		c.Error("提交事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	ctx.ResponseOK()
}

// sort 批量调整类别排序（事务保证原子性）
func (c *Category) sort(ctx *wkhttp.Context) {
	loginUID := ctx.GetLoginUID()
	spaceID := ctx.Param("space_id")

	isMember, err := spacepkg.CheckMembership(c.db.session, spaceID, loginUID)
	if err != nil {
		c.Error("检查空间成员失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	if !isMember {
		httperr.ResponseErrorL(ctx, errcode.ErrCategorySpaceMemberRequired, nil, nil)
		return
	}

	var req sortCategoriesReq
	if err := ctx.BindJSON(&req); err != nil {
		respondCategoryRequestInvalid(ctx, "")
		return
	}
	if len(req.CategoryIDs) == 0 {
		respondCategoryRequestInvalid(ctx, "category_ids")
		return
	}

	categories, err := c.db.queryCategoriesByUIDAndSpaceID(loginUID, spaceID)
	if err != nil {
		c.Error("查询类别失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}

	if len(req.CategoryIDs) != len(categories) {
		httperr.ResponseErrorL(ctx, errcode.ErrCategorySortListMismatch, nil, nil)
		return
	}

	catMap := make(map[string]bool, len(categories))
	for _, cat := range categories {
		catMap[cat.CategoryID] = true
	}
	seen := make(map[string]bool, len(req.CategoryIDs))
	for _, id := range req.CategoryIDs {
		if seen[id] {
			httperr.ResponseErrorL(ctx, errcode.ErrCategorySortListDuplicate, nil, nil)
			return
		}
		seen[id] = true
		if !catMap[id] {
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryNotFound, nil, nil)
			return
		}
	}

	tx, err := c.ctx.DB().Begin()
	if err != nil {
		c.Error("开启事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()

	for i, catID := range req.CategoryIDs {
		_, err := tx.Update("group_category").
			Set("sort", i).
			Where("category_id=?", catID).
			Exec()
		if err != nil {
			c.Error("更新排序失败", zap.Error(err), zap.String("categoryID", catID))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
			return
		}
	}

	// PR review follow-up: category 排序变化会改变 follow tab 的渲染顺序
	// （sortFollowItems 首主键就是 CategorySort），客户端必须重建 follow 列表。
	if _, err := convext.BumpFollowVersionTx(tx, loginUID, spaceID); err != nil {
		c.Error("更新 follow_version 失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	if err = tx.Commit(); err != nil {
		c.Error("提交事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	ctx.ResponseOK()
}

// moveGroupToCategory 移动群组到类别
func (c *Category) moveGroupToCategory(ctx *wkhttp.Context) {
	loginUID := ctx.GetLoginUID()
	groupNo := ctx.Param("group_no")

	var req moveGroupToCategoryReq
	if err := ctx.BindJSON(&req); err != nil {
		respondCategoryRequestInvalid(ctx, "")
		return
	}

	// 校验群成员身份，并取出 is_external / source_space_id 用于计算用户视角空间。
	// Issue #191：外部群（群归属 Space 与用户所在 Space 不同）场景下，用户是该群的
	// 外部成员（is_external=1, source_space_id=用户当前 Space），关注/归类是个人维度
	// 操作，应以用户来源 Space 而非群归属 Space 作为空间维度。
	var member struct {
		IsExternal    int    `db:"is_external"`
		SourceSpaceID string `db:"source_space_id"`
	}
	// IFNULL + Limit(1) 都是防御性写法：source_space_id 列是 NOT NULL DEFAULT ''
	// （见 group/sql/20260424000001_group_legacy01.sql），(group_no, uid) 也有唯一约束、
	// LoadOne 至多命中一行——这里并非暗示该列可空或可能多行，仅作 belt-and-suspenders。
	err := c.db.session.Select("is_external", "IFNULL(source_space_id,'') AS source_space_id").
		From("group_member").
		Where("group_no=? and uid=? and is_deleted=0", groupNo, loginUID).
		Limit(1).
		LoadOne(&member)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryGroupMemberRequired, nil, nil)
			return
		}
		c.Error("查询群成员失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}

	// 查询群所属 Space
	var groupSpaceID string
	_, err = c.db.session.Select("IFNULL(space_id,'')").From("`group`").
		Where("group_no=?", groupNo).
		Load(&groupSpaceID)
	if err != nil {
		c.Error("查询群信息失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}
	if groupSpaceID == "" {
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryGroupSpaceMissing, nil, nil)
		return
	}

	// 用户视角的有效空间：外部成员以来源 Space 为准，内部成员等同于群归属 Space。
	// 后续的同空间校验、follow_version bump、auto_follow_threads 同步都以此为空间维度，
	// 保证外部群被归类后能正确出现在用户当前 Space 的关注 tab（侧边栏按当前 Space 查询）。
	//
	// 外部成员但 source_space_id 为空是合法历史状态（如未绑定 Space 的用户/bot，
	// 见 modules/group/service.go 注释）。必须与 sidebar / space_filter 的解析口径
	// 一致回退到用户默认 Space（最早加入的 Space）：space_filter.decideConvKeepInSpace
	// 对外部群的 `eff == "" → defaultSpaceID`、api_sidebar.sidebarMySourceSpaceID
	// 同口径。否则这些 legacy 行仍无法归类，且 follow_version / auto_follow_threads
	// 会写到群归属 Space，与侧边栏读取的默认 Space 不一致——正是 #191 想消灭的漂移。
	//
	// 注意 corner case：source_space_id 为空且用户连默认 Space 都没有（无任何
	// space_member 行，如未绑定 Space 的 bot）时，下面保留 effectiveSpaceID =
	// groupSpaceID（旧行为），而非像 sidebarMySourceSpaceID 那样解析为 ""。这不是
	// 严格对齐——但 "" 不会匹配任何分类 Space、等于无法归类，保留旧行为是更宽松且
	// 不引入回归的选择。读到此处不要误以为这里与 sidebar 解析逐字一致。
	effectiveSpaceID := groupSpaceID
	if member.IsExternal == 1 {
		if member.SourceSpaceID != "" {
			effectiveSpaceID = member.SourceSpaceID
		} else if defaultSpaceID, derr := spacemod.GetUserDefaultSpaceIDE(c.ctx, loginUID); derr != nil {
			// fail-closed：DB 查询失败时不要静默落到群 Space（会复现 #191），直接报错。
			c.Error("查询用户默认空间失败", zap.Error(derr))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
			return
		} else if defaultSpaceID != "" {
			effectiveSpaceID = defaultSpaceID
		}
	}

	var categoryIDPtr *string

	// 查询现有 group_setting
	setting, err := c.db.queryGroupSettingForCategory(groupNo, loginUID)
	if err != nil {
		c.Error("查询群设置失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
		return
	}

	// PR review follow-up: 把 group_setting 写入和 follow_version +1 打包到同一个 tx。
	// 群进/出分类直接改变 follow tab 的成员集合（buildFollowItems 要求 CategoryID != nil），
	// 客户端必须重建列表，所以必须 bump。
	tx, err := c.ctx.DB().Begin()
	if err != nil {
		c.Error("开启事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}
	defer tx.RollbackUnlessCommitted()

	// Issue #75: category 校验必须放在写事务内，并对 group_category 中匹配
	// category_id 的行加 X 锁（通过 uk_category_id 唯一索引等值定位，InnoDB
	// 同时锁住二级索引行和对应的聚簇索引行，与 delete 路径互斥）。否则
	// reader 在 tx 外读到 status=1、deleter 之后提交 status=2、reader 再写
	// group_setting.category_id=X，落库出现指向已删除分类的悬挂引用
	// （正是 #74 想消灭的脏状态）。
	//
	// 锁谓词只用 `WHERE category_id=?` 等值匹配，状态/归属判断放 Go 层完成。
	// 在 REPEATABLE READ 下，UNIQUE 索引等值命中只取 record lock；如果走
	// `WHERE status=1` 这种非唯一索引谓词，命中/未命中都可能取 next-key
	// (gap) lock，扩大锁范围、增大死锁概率。命中已存在 UUID 时 record-only
	// 是更稳的选择。
	if req.CategoryID != "" {
		var locked struct {
			UID     string `db:"uid"`
			SpaceID string `db:"space_id"`
			Status  int    `db:"status"`
		}
		err := tx.SelectBySql(
			"SELECT uid, space_id, status FROM group_category WHERE category_id=? FOR UPDATE",
			req.CategoryID,
		).LoadOne(&locked)
		if err != nil {
			if errors.Is(err, dbr.ErrNotFound) {
				httperr.ResponseErrorL(ctx, errcode.ErrCategoryNotFound, nil, nil)
				return
			}
			c.Error("查询类别失败", zap.Error(err))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryQueryFailed, nil, nil)
			return
		}
		if locked.Status != 1 {
			// status=2（已删除）等价于"分类不存在"——与旧路径
			// queryCategoryByID(status=1 过滤) 后 nil 检查保持文案一致。
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryNotFound, nil, nil)
			return
		}
		if locked.UID != loginUID {
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryPermissionDenied, nil, nil)
			return
		}
		if effectiveSpaceID != locked.SpaceID {
			httperr.ResponseErrorL(ctx, errcode.ErrCategorySpaceMismatch, nil, nil)
			return
		}
		categoryIDPtr = &req.CategoryID
	}

	if setting == nil {
		version, err := c.ctx.GenSeq(common.GroupSettingSeqKey)
		if err != nil {
			c.Error("生成版本号失败", zap.Error(err))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
			return
		}
		if _, err := tx.InsertBySql(
			"INSERT INTO group_setting (group_no, uid, category_id, category_sort, revoke_remind, screenshot, receipt, version) VALUES (?, ?, ?, ?, 1, 1, 1, ?)",
			groupNo, loginUID, categoryIDPtr, 0, version,
		).Exec(); err != nil {
			c.Error("创建群设置失败", zap.Error(err))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
			return
		}
	} else {
		if _, err := tx.Update("group_setting").
			Set("category_id", categoryIDPtr).
			Set("category_sort", 0).
			Where("id=?", setting.Id).Exec(); err != nil {
			c.Error("更新群设置失败", zap.Error(err))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
			return
		}
	}

	// Project-wide lock order is group_setting → user_follow_version →
	// user_conversation_ext (matches UpdateSort, FollowChannel, UnfollowGroupsTx,
	// deleteCategory).  Bump version BEFORE writing ext: otherwise a concurrent
	// /v1/follow/sort holding the version FOR UPDATE while waiting on ext
	// would AB-BA-deadlock against a move-out holding ext while waiting on
	// version (issue #151 review #4 by an9xyz).
	if _, err := convext.BumpFollowVersionTx(tx, loginUID, effectiveSpaceID); err != nil {
		c.Error("更新 follow_version 失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	// Issue #151 review #3/#4 — synchronize user_conversation_ext.auto_follow_threads
	// with the new category membership state.  buildFollowItems decides follow-tab
	// membership by `cs.CategoryID != nil`; selectEligibleForFanoutTx decides
	// OnThreadCreated fan-out by `auto_follow_threads=1 AND group_unfollowed=0`.
	// These two reads share no column, so the writer (this handler) must keep
	// them in lockstep:
	//
	//   - Move-out (categoryIDPtr == nil): clear auto_follow_threads to match
	//     the new "not in follow tab" state.  Without this, fan-out continues
	//     to target a user whose follow tab no longer shows the group.
	//   - Move-in (categoryIDPtr != nil): restore auto_follow_threads=1 if
	//     an ext row already exists with =0 (left over from a prior move-out).
	//     Sidebar materialization (api_sidebar.go) skips its INSERT IGNORE
	//     when the ext row is already present, so without this restore the
	//     group re-appears in the follow tab but fan-out stays disabled.
	//     For first-time categorize (no ext row) the call is a no-op —
	//     sidebar materialization later creates the row with =1.
	if categoryIDPtr == nil {
		if err := convext.ClearAutoFollowThreadsTx(tx, loginUID, effectiveSpaceID, []string{groupNo}); err != nil {
			c.Error("清理 auto_follow_threads 失败", zap.Error(err))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
			return
		}
	} else {
		if err := convext.RestoreAutoFollowThreadsTx(tx, loginUID, effectiveSpaceID, []string{groupNo}); err != nil {
			c.Error("恢复 auto_follow_threads 失败", zap.Error(err))
			httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		c.Error("提交事务失败", zap.Error(err))
		httperr.ResponseErrorL(ctx, errcode.ErrCategoryStoreFailed, nil, nil)
		return
	}

	ctx.ResponseOK()
}
