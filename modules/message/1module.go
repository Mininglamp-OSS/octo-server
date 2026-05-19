package message

import (
	"embed"

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
			svc.SetThreadAuthChecker(newThreadAuthChecker(appCtx))
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
	// 1. Membership check: must be a member of the parent group.
	isMember, err := c.groupSvc.ExistMember(groupNo, uid)
	if err != nil {
		return err
	}
	if !isMember {
		return convext.ErrThreadForbidden
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
	// 3. Parent-group must be visible in the requested Space.
	groups, err := c.groupSvc.GetGroups([]string{groupNo})
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		// Group disbanded between membership-check and now; safe to reject.
		return convext.ErrThreadForbidden
	}
	parentSpaceID := groups[0].SpaceID
	if parentSpaceID == "" {
		// Legacy group without space_id is visible everywhere; allow.
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
	return convext.ErrThreadForbidden
}
