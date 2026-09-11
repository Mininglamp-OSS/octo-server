package group

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"go.uber.org/zap"
)

const (
	allMemberGroupActionAdd       = "add"
	allMemberGroupActionDisband   = "disband"
	allMemberGroupActionExit      = "exit"
	allMemberGroupActionInvite    = "invite"
	allMemberGroupActionJoin      = "join"
	allMemberGroupActionRemove    = "remove"
	allMemberGroupActionTransfer  = "transfer"
	allMemberGroupActionBlacklist = "blacklist"
)

// refuseIfAllMemberGroup is an HTTP-handler guard. Service-layer mutation
// primitives remain open for Project and Space cascades and owner sync.
// Predicate failures fail closed so a database read outage cannot authorize
// a protected-group mutation.
func (g *Group) refuseIfAllMemberGroup(c *wkhttp.Context, groupModel *Model, action string) bool {
	if groupModel == nil {
		return false
	}
	return g.refuseIfAllMemberGroupFields(c, groupModel.GroupNo, groupModel.ProjectID, action)
}

func (g *Group) refuseIfAllMemberGroupFields(c *wkhttp.Context, groupNo, projectID, action string) bool {
	if groupNo == "" || projectID == "" {
		return false
	}
	isAllMember, err := projectpkg.IsAllMemberGroup(g.ctx.DB(), projectID, groupNo)
	if err != nil {
		g.Error("判定项目全员群失败，拒绝本次操作", zap.Error(err),
			zap.String("group_no", groupNo), zap.String("project_id", projectID),
			zap.String("action", action))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return true
	}
	if !isAllMember {
		return false
	}
	respondAllMemberGroupProtected(c, action)
	return true
}

func respondAllMemberGroupProtected(c *wkhttp.Context, action string) {
	httperr.ResponseErrorL(c, errcode.ErrGroupAllMemberGroupProtected, nil, i18n.Details{
		"action": action,
	})
}
