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
func (c *threadAuthChecker) AuthorizeThreadFollow(uid, _spaceID, groupNo, shortID string) error {
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
