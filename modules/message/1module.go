package message

import (
	"embed"
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
)

//go:embed sql
var sqlFS embed.FS

//go:embed swagger/api.yaml
var swaggerContent string

//go:embed swagger/conversation.yaml
var conversationSwagger string

//go:embed swagger/sidebar.yaml
var sidebarSwagger string

func init() {

	register.AddModule(func(ctx interface{}) register.Module {

		return register.Module{
			Name: "message",
			SetupAPI: func() register.APIRouter {
				return New(ctx.(*config.Context))
			},
			SQLDir:  register.NewSQLFS(sqlFS),
			Swagger: swaggerContent,
		}
	})

	register.AddModule(func(ctx interface{}) register.Module {

		return register.Module{
			Name: "conversation",
			SetupAPI: func() register.APIRouter {
				return NewConversation(ctx.(*config.Context))
			},
			Swagger: conversationSwagger,
		}
	})
	register.AddModule(func(ctx interface{}) register.Module {

		return register.Module{
			SetupAPI: func() register.APIRouter {
				return NewManager(ctx.(*config.Context))
			},
		}
	})

	// PR review (Round 3) Blocking #3 — wire ThreadAuthChecker.
	// message module is the natural composition point because it already
	// imports group + thread + conversation_ext for the sidebar handler.
	// We register the checker on the conversation_ext singleton so that
	// modules/conversation_ext stays free of group/thread imports (no cycle).
	//
	// （历史 DMCategoryChecker 注入 issue #75 / PR #79 fix 之后已移除——FollowDM
	// 鉴权改为 conversation_ext 自己的事务内 SELECT ... FOR UPDATE，不再需要
	// 从 message 模块注入 checker。）
	register.AddModule(func(ctx interface{}) register.Module {
		appCtx := ctx.(*config.Context)
		convext.InitGlobalConvExtService(appCtx)
		svc := convext.GetGlobalConvExtService()
		if svc != nil {
			checker := newThreadAuthChecker(appCtx)
			svc.SetThreadAuthChecker(checker)
			// 注入 ThreadEnumerator：FollowChannel 物化既有子区时通过它枚举
			// active 子区的 shortID。同样落在 message 模块以避开 conversation_ext
			// 直接 import modules/thread 的循环依赖（见 ThreadAuthChecker 同款逻辑）。
			svc.SetThreadEnumerator(newThreadEnumerator(appCtx))
			// 注入 ChannelAuthChecker：FollowChannel 写 auto_follow=1 + 物化既有
			// 子区前必须校验 caller 是群成员 + 群在 Space 可见。复用同一个 struct
			// 实现，共享 checkChannelAccess 逻辑。
			svc.SetChannelAuthChecker(checker)
		}
		return register.Module{Name: "conversation_ext_thread_auth"}
	})

	// Sidebar swagger lives in its own file so the sidebar/follow surface can
	// evolve independently from the legacy /v1/conversation contract.
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name:    "sidebar",
			Swagger: sidebarSwagger,
		}
	})
}

// threadAuthChecker is the production ThreadAuthChecker implementation.
// It composes group.IService.ExistMember + thread.DB.QueryActiveByGroupShortIDs
// to satisfy the contract documented in convext.ThreadAuthChecker.
type threadAuthChecker struct {
	groupSvc group.IService
	threadDB *thread.DB
	// groupDB 用于查 external-group mapping，仅在 parent.space_id != request spaceID
	// 时才被读取，避免对绝大多数同 space 请求的额外 IO。
	groupDB *group.DB
}

func newThreadAuthChecker(ctx *config.Context) *threadAuthChecker {
	return &threadAuthChecker{
		groupSvc: group.NewService(ctx),
		threadDB: thread.NewDB(ctx),
		groupDB:  group.NewDB(ctx),
	}
}

// AuthorizeThreadFollow implements convext.ThreadAuthChecker.
//
// Returns convext.ErrThreadForbidden when the user cannot follow this thread.
// Infra errors are wrapped and propagated unchanged.
//
// 校验链：
//  1. spaceID 非空（API 已过 SpaceMiddleware，纵深防御）
//  2. 用户是 parent group 的成员
//  3. thread 存在且 status != deleted 且 group_no 一致
//  4. parent group 在请求的 Space 内可见（PR #21 Round-6 P0-2 by Jerry-Xin / yujiawei）：
//     - 内部群: group.space_id == spaceID
//     - 外部群: 用户作为外部成员加入的 sourceSpaceID == spaceID
//     - 旧群 (group.space_id == ""): 所有 Space 可见
//     这条规则与 FilterRawConversationsBySpace 的可见性判定一致，确保 FollowThread
//     不会在 Space A 的群里写入 Space B 的 ext 行。
func (c *threadAuthChecker) AuthorizeThreadFollow(uid, spaceID, groupNo, shortID string) error {
	if spaceID == "" {
		return convext.ErrThreadForbidden
	}
	// 1. Channel-level checks (membership + Space visibility) — shared with FollowChannel.
	if err := c.checkChannelAccess(uid, spaceID, groupNo); err != nil {
		// 已是 ErrChannelForbidden 时，对 thread API 仍翻译为 ErrThreadForbidden，
		// 让 handler 走原有 403 路径，客户端无需感知两套 sentinel。
		if errors.Is(err, convext.ErrChannelForbidden) {
			return convext.ErrThreadForbidden
		}
		return err
	}
	// 2. Thread existence + status + group consistency in one query.
	threadMap, err := c.threadDB.QueryActiveByGroupShortIDs([]thread.ShortRef{
		{GroupNo: groupNo, ShortID: shortID},
	})
	if err != nil {
		return err
	}
	key := groupNo + "____" + shortID
	if _, ok := threadMap[key]; !ok {
		// Either thread does not exist, status==deleted, or group_no mismatch.
		return convext.ErrThreadForbidden
	}
	return nil
}

// AuthorizeChannelFollow implements convext.ChannelAuthChecker.
//
// 与 AuthorizeThreadFollow 共享 channel-level access check（成员资格 + Space 可见性）,
// 仅省略掉 thread-existence 这一步 —— FollowChannel 写群级 ext 行，与具体子区无关。
//
// 引入背景（PR #123 round-1 by Jerry-Xin / yujiawei P1）：FollowChannel 现在会
// 物化 thread ext + 挂 OnThreadCreated fanout 订阅，必须先校验 caller 能"看到"
// 这个群，否则同 Space 内私有群的子区元数据会泄露。
func (c *threadAuthChecker) AuthorizeChannelFollow(uid, spaceID, groupNo string) error {
	return c.checkChannelAccess(uid, spaceID, groupNo)
}

// checkChannelAccess 复用 FollowThread 既有逻辑的群级访问校验：
//  1. spaceID 非空
//  2. caller 是 group 成员
//  3. group 在请求 Space 可见（内部群 same-space / 外部群 sourceSpaceID-match / 旧群 wildcard）
//
// 鉴权失败返回 convext.ErrChannelForbidden；基础设施错误 wrap 后上传。
func (c *threadAuthChecker) checkChannelAccess(uid, spaceID, groupNo string) error {
	if spaceID == "" {
		return convext.ErrChannelForbidden
	}
	// 1. Membership check.
	isMember, err := c.groupSvc.ExistMember(groupNo, uid)
	if err != nil {
		return err
	}
	if !isMember {
		return convext.ErrChannelForbidden
	}
	// 2. Space visibility.
	groups, err := c.groupSvc.GetGroups([]string{groupNo})
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		// Group row gone between membership-check and now; reject.
		return convext.ErrChannelForbidden
	}
	// PR #123 round-6 (lml2468)：显式拒绝 Disband 群。解散流程把 group.status
	// 置为 Disband 但不一定清理 group_member（部分清理路径目前是注释掉的），
	// ExistMember 仍可能为 true；同时解散事件已清空 conversation_ext 行，
	// 这里若放行会让 FollowChannel 重新写入 auto_follow_threads=1 + 物化已解散
	// 群下的 active thread ext，导致已解散的群/子区重新出现在 sidebar。
	// 与 modules/group/api.go 既有路径（"if group == nil || group.Status ==
	// GroupStatusDisband"）保持一致。Disabled (=0, 管理员禁用) 当前不拒绝以
	// 保持最小修复面；若日后产品确认 disabled 群也不应允许 follow，可在此追加。
	if groups[0].Status == group.GroupStatusDisband {
		return convext.ErrChannelForbidden
	}
	parentSpaceID := groups[0].SpaceID
	if parentSpaceID == "" {
		// Legacy group without space_id is visible everywhere.
		return nil
	}
	if parentSpaceID == spaceID {
		return nil
	}
	// External-group fallback: user joined as external member with sourceSpaceID == spaceID.
	externalMap, err := c.groupDB.QueryExternalGroupNosForUser(uid)
	if err != nil {
		return err
	}
	if sourceSpace, ok := externalMap[groupNo]; ok {
		if sourceSpace == spaceID {
			return nil
		}
	}
	return convext.ErrChannelForbidden
}

// threadEnumerator implements convext.ThreadEnumerator for production wiring.
// It thin-wraps thread.DB.QueryByGroupNoWithStatus(active-only) and projects to
// shortID-only to keep the conversation_ext side free of thread.Model leakage.
type threadEnumerator struct {
	threadDB *thread.DB
}

func newThreadEnumerator(ctx *config.Context) *threadEnumerator {
	return &threadEnumerator{threadDB: thread.NewDB(ctx)}
}

// EnumerateActiveShortIDs 返回 groupNo 下 active 子区的 shortID 列表，最多 limit 个。
// 排序由 QueryByGroupNoWithStatus 决定：created_at DESC, id DESC —— 最新建的子区
// 排在前面，截断后被丢弃的是最旧的子区，正好与"产品侧自动归档把旧子区清出 active"
// 配合，让 cap 截断不会丢失"热"子区。
func (e *threadEnumerator) EnumerateActiveShortIDs(groupNo string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	models, err := e.threadDB.QueryByGroupNoWithStatus(groupNo, []int{thread.ThreadStatusActive}, 0, int64(limit))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ShortID)
	}
	return ids, nil
}
