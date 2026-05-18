package category

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
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
	uidLimit := appwkhttp.SharedUIDRateLimiter(c.ctx)
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
		ctx.ResponseError(errors.New("检查空间成员失败"))
		return
	}
	if !isMember {
		ctx.ResponseError(errors.New("你不是该空间成员"))
		return
	}

	var req createCategoryReq
	if err := ctx.BindJSON(&req); err != nil {
		ctx.ResponseError(errors.New("请求参数错误"))
		return
	}
	if req.Name == "" {
		ctx.ResponseError(errors.New("类别名称不能为空"))
		return
	}
	if len([]rune(req.Name)) > 100 {
		ctx.ResponseError(errors.New("类别名称不能超过100个字符"))
		return
	}

	count, err := c.db.countCategoriesByUIDAndSpaceID(loginUID, spaceID)
	if err != nil {
		c.Error("查询类别数量失败", zap.Error(err))
		ctx.ResponseError(errors.New("查询类别数量失败"))
		return
	}
	if count >= 20 {
		ctx.ResponseError(errors.New("每个空间最多创建20个分类"))
		return
	}

	categoryID := util.GenerUUID()
	nextSort, err := c.db.maxSortByUIDAndSpaceID(loginUID, spaceID)
	if err != nil {
		c.Error("查询排序值失败", zap.Error(err))
		ctx.ResponseError(errors.New("创建类别失败"))
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
		ctx.ResponseError(errors.New("创建类别失败"))
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
		ctx.ResponseError(errors.New("检查空间成员失败"))
		return
	}
	if !isMember {
		ctx.ResponseError(errors.New("你不是该空间成员"))
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
		ctx.ResponseError(errors.New("查询类别失败"))
		return
	}

	groups, err := c.db.queryUserGroupsInSpace(loginUID, spaceID)
	if err != nil {
		c.Error("查询群组失败", zap.Error(err))
		ctx.ResponseError(errors.New("查询群组失败"))
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
			ctx.ResponseError(errors.New("查询类别失败"))
			return
		}
		if defaultCat == nil {
			maxSort, err := c.db.maxSortByUIDAndSpaceID(loginUID, spaceID)
			if err != nil {
				c.Error("查询排序值失败", zap.Error(err))
				ctx.ResponseError(errors.New("查询类别失败"))
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
				ctx.ResponseError(errors.New("创建默认类别失败"))
				return
			}
			// INSERT IGNORE 后重查，确保拿到实际行（防并发竞态）
			categories, err = c.db.queryCategoriesByUIDAndSpaceID(loginUID, spaceID)
			if err != nil {
				c.Error("查询类别失败", zap.Error(err))
				ctx.ResponseError(errors.New("查询类别失败"))
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
		ctx.ResponseError(errors.New("查询类别失败"))
		return
	}
	if cat == nil {
		ctx.ResponseError(errors.New("分类不存在"))
		return
	}
	if cat.UID != loginUID {
		ctx.ResponseError(errors.New("无权限修改此分类"))
		return
	}
	if cat.isDefault() {
		ctx.ResponseError(errors.New("默认分类不可修改"))
		return
	}

	var req updateCategoryReq
	if err := ctx.BindJSON(&req); err != nil {
		ctx.ResponseError(errors.New("请求参数错误"))
		return
	}
	if req.Name == "" {
		ctx.ResponseError(errors.New("类别名称不能为空"))
		return
	}
	if len([]rune(req.Name)) > 100 {
		ctx.ResponseError(errors.New("类别名称不能超过100个字符"))
		return
	}

	err = c.db.updateCategoryName(categoryID, req.Name)
	if err != nil {
		c.Error("更新类别失败", zap.Error(err))
		ctx.ResponseError(errors.New("更新类别失败"))
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
		ctx.ResponseError(errors.New("查询类别失败"))
		return
	}
	if cat == nil {
		ctx.ResponseError(errors.New("分类不存在"))
		return
	}
	if cat.UID != loginUID {
		ctx.ResponseError(errors.New("无权限删除此分类"))
		return
	}
	if cat.isDefault() {
		ctx.ResponseError(errors.New("默认分类不可删除"))
		return
	}

	tx, err := c.ctx.DB().Begin()
	if err != nil {
		c.Error("开启事务失败", zap.Error(err))
		ctx.ResponseError(errors.New("删除类别失败"))
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
		ctx.ResponseError(errors.New("删除类别失败"))
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
		ctx.ResponseError(errors.New("删除类别失败"))
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
		ctx.ResponseError(errors.New("删除类别失败"))
		return
	}

	// 4. bump follow_version 必须先于 user_conversation_ext 的写操作，
	// 与 UpdateSort 同序拿 (version → ext) 锁（PR #21 Round-3 blocker #2）。
	if _, err := convext.BumpFollowVersionTx(tx, loginUID, cat.SpaceID); err != nil {
		c.Error("更新 follow_version 失败", zap.Error(err))
		ctx.ResponseError(errors.New("删除类别失败"))
		return
	}

	// 5. 退订语义（前端提示「分组下的所有会话将取消关注」）：
	//    - 群：group_unfollowed=1 + 级联删 thread ext 行
	//    - DM：DELETE dm_category_id=cat 的 ext 行
	if err := convext.UnfollowGroupsTx(tx, loginUID, cat.SpaceID, groupNos); err != nil {
		c.Error("取消关注分组下群失败", zap.Error(err))
		ctx.ResponseError(errors.New("删除类别失败"))
		return
	}
	if err := convext.UnfollowDMsByCategoryTx(tx, loginUID, cat.SpaceID, categoryID); err != nil {
		c.Error("取消关注分组下私聊失败", zap.Error(err))
		ctx.ResponseError(errors.New("删除类别失败"))
		return
	}

	if err = tx.Commit(); err != nil {
		c.Error("提交事务失败", zap.Error(err))
		ctx.ResponseError(errors.New("删除类别失败"))
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
		ctx.ResponseError(errors.New("检查空间成员失败"))
		return
	}
	if !isMember {
		ctx.ResponseError(errors.New("你不是该空间成员"))
		return
	}

	var req sortCategoriesReq
	if err := ctx.BindJSON(&req); err != nil {
		ctx.ResponseError(errors.New("请求参数错误"))
		return
	}
	if len(req.CategoryIDs) == 0 {
		ctx.ResponseError(errors.New("分类列表不能为空"))
		return
	}

	categories, err := c.db.queryCategoriesByUIDAndSpaceID(loginUID, spaceID)
	if err != nil {
		c.Error("查询类别失败", zap.Error(err))
		ctx.ResponseError(errors.New("查询类别失败"))
		return
	}

	if len(req.CategoryIDs) != len(categories) {
		ctx.ResponseError(errors.New("分类列表数量不匹配"))
		return
	}

	catMap := make(map[string]bool, len(categories))
	for _, cat := range categories {
		catMap[cat.CategoryID] = true
	}
	seen := make(map[string]bool, len(req.CategoryIDs))
	for _, id := range req.CategoryIDs {
		if seen[id] {
			ctx.ResponseError(errors.New("分类列表存在重复"))
			return
		}
		seen[id] = true
		if !catMap[id] {
			ctx.ResponseError(errors.New("分类不存在或无权限"))
			return
		}
	}

	tx, err := c.ctx.DB().Begin()
	if err != nil {
		c.Error("开启事务失败", zap.Error(err))
		ctx.ResponseError(errors.New("更新排序失败"))
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
			ctx.ResponseError(errors.New("更新排序失败"))
			return
		}
	}

	// PR review follow-up: category 排序变化会改变 follow tab 的渲染顺序
	// （sortFollowItems 首主键就是 CategorySort），客户端必须重建 follow 列表。
	if _, err := convext.BumpFollowVersionTx(tx, loginUID, spaceID); err != nil {
		c.Error("更新 follow_version 失败", zap.Error(err))
		ctx.ResponseError(errors.New("更新排序失败"))
		return
	}

	if err = tx.Commit(); err != nil {
		c.Error("提交事务失败", zap.Error(err))
		ctx.ResponseError(errors.New("更新排序失败"))
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
		ctx.ResponseError(errors.New("请求参数错误"))
		return
	}

	// 校验群成员身份
	var memberCount int
	_, err := c.db.session.Select("count(*)").From("group_member").
		Where("group_no=? and uid=? and is_deleted=0", groupNo, loginUID).
		Load(&memberCount)
	if err != nil {
		c.Error("查询群成员失败", zap.Error(err))
		ctx.ResponseError(errors.New("查询群成员失败"))
		return
	}
	if memberCount == 0 {
		ctx.ResponseError(errors.New("你不是该群成员"))
		return
	}

	// 查询群所属 Space
	var groupSpaceID string
	_, err = c.db.session.Select("IFNULL(space_id,'')").From("`group`").
		Where("group_no=?", groupNo).
		Load(&groupSpaceID)
	if err != nil {
		c.Error("查询群信息失败", zap.Error(err))
		ctx.ResponseError(errors.New("查询群信息失败"))
		return
	}
	if groupSpaceID == "" {
		ctx.ResponseError(errors.New("该群组不属于任何空间"))
		return
	}

	var categoryIDPtr *string
	if req.CategoryID != "" {
		// 校验 category 所有权
		cat, err := c.db.queryCategoryByID(req.CategoryID)
		if err != nil {
			c.Error("查询类别失败", zap.Error(err))
			ctx.ResponseError(errors.New("查询类别失败"))
			return
		}
		if cat == nil {
			ctx.ResponseError(errors.New("分类不存在"))
			return
		}
		if cat.UID != loginUID {
			ctx.ResponseError(errors.New("无权限使用此分类"))
			return
		}
		if groupSpaceID != cat.SpaceID {
			ctx.ResponseError(errors.New("群组和分类不在同一空间"))
			return
		}
		categoryIDPtr = &req.CategoryID
	}

	// 查询现有 group_setting
	setting, err := c.db.queryGroupSettingForCategory(groupNo, loginUID)
	if err != nil {
		c.Error("查询群设置失败", zap.Error(err))
		ctx.ResponseError(errors.New("查询群设置失败"))
		return
	}

	// PR review follow-up: 把 group_setting 写入和 follow_version +1 打包到同一个 tx。
	// 群进/出分类直接改变 follow tab 的成员集合（buildFollowItems 要求 CategoryID != nil），
	// 客户端必须重建列表，所以必须 bump。
	tx, err := c.ctx.DB().Begin()
	if err != nil {
		c.Error("开启事务失败", zap.Error(err))
		ctx.ResponseError(errors.New("更新群设置失败"))
		return
	}
	defer tx.RollbackUnlessCommitted()

	if setting == nil {
		version, err := c.ctx.GenSeq(common.GroupSettingSeqKey)
		if err != nil {
			c.Error("生成版本号失败", zap.Error(err))
			ctx.ResponseError(errors.New("生成版本号失败"))
			return
		}
		if _, err := tx.InsertBySql(
			"INSERT INTO group_setting (group_no, uid, category_id, category_sort, revoke_remind, screenshot, receipt, version) VALUES (?, ?, ?, ?, 1, 1, 1, ?)",
			groupNo, loginUID, categoryIDPtr, 0, version,
		).Exec(); err != nil {
			c.Error("创建群设置失败", zap.Error(err))
			ctx.ResponseError(errors.New("创建群设置失败"))
			return
		}
	} else {
		if _, err := tx.Update("group_setting").
			Set("category_id", categoryIDPtr).
			Set("category_sort", 0).
			Where("id=?", setting.Id).Exec(); err != nil {
			c.Error("更新群设置失败", zap.Error(err))
			ctx.ResponseError(errors.New("更新群设置失败"))
			return
		}
	}

	if _, err := convext.BumpFollowVersionTx(tx, loginUID, groupSpaceID); err != nil {
		c.Error("更新 follow_version 失败", zap.Error(err))
		ctx.ResponseError(errors.New("更新群设置失败"))
		return
	}

	if err := tx.Commit(); err != nil {
		c.Error("提交事务失败", zap.Error(err))
		ctx.ResponseError(errors.New("更新群设置失败"))
		return
	}

	ctx.ResponseOK()
}
