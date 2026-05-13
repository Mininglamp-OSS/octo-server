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

//go:embed swagger/conversation_v2.yaml
var conversationV2Swagger string

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

	// PR review (Round 3) Blocking #3 — wire ThreadAuthChecker:
	// message module is the natural composition point because it already
	// imports group + thread + conversation_ext for the sidebar handler.
	// We register the checker on the conversation_ext singleton so that
	// modules/conversation_ext stays free of group/thread imports (no cycle).
	register.AddModule(func(ctx interface{}) register.Module {
		appCtx := ctx.(*config.Context)
		convext.InitGlobalConvExtService(appCtx)
		svc := convext.GetGlobalConvExtService()
		if svc != nil {
			svc.SetThreadAuthChecker(newThreadAuthChecker(appCtx))
		}
		return register.Module{Name: "conversation_ext_thread_auth"}
	})

	// PR review (Round 3) Important #3 — register the v2 sidebar swagger file
	// independently so its `basePath: /v2` is honoured (the legacy file uses /v1).
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name:    "conversation_v2",
			Swagger: conversationV2Swagger,
		}
	})
}

// threadAuthChecker is the production ThreadAuthChecker implementation.
// It composes group.IService.ExistMember + thread.DB.QueryActiveByGroupShortIDs
// to satisfy the contract documented in convext.ThreadAuthChecker.
type threadAuthChecker struct {
	groupSvc group.IService
	threadDB *thread.DB
}

func newThreadAuthChecker(ctx *config.Context) *threadAuthChecker {
	return &threadAuthChecker{
		groupSvc: group.NewService(ctx),
		threadDB: thread.NewDB(ctx),
	}
}

// AuthorizeThreadFollow implements convext.ThreadAuthChecker.
//
// Returns convext.ErrThreadForbidden when the user cannot follow this thread.
// Infra errors are wrapped and propagated unchanged.
//
// PR review follow-up：spaceID 兜底校验。当前在 dmwork 中 group 成员身份隐含
// space 访问权，所以单纯的 membership check 就足够；但显式 reject 空 spaceID 是
// 一道便宜的纵深防御层（API 路径已经经过 SpaceMiddleware，理论上不会传空）。
// 后续需要严格 space membership 校验时在此处接 spacepkg.CheckMembership。
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
	return nil
}
