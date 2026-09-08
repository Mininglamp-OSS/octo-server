package project

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// listProjectGroupsHandler answers GET /v1/projects/:project_id/groups.
//
// It returns the caller's OWN live groups within the project — see
// listMyProjectGroups for why the list is membership-scoped and why that
// predicate is the access control rather than a filter applied after one.
//
// # No permission gate beyond projectMiddleware, on purpose
//
// listMembersHandler needs canViewMembers because the roster is a fact ABOUT
// OTHER PEOPLE: a space_listed project shows its metadata to any Space member,
// but who is in it is not part of that. This endpoint returns only rows the
// caller is already a member of, so there is nothing to withhold. A Space admin
// who never joined the project gets `[]`, which is the true answer.
//
// Adding a gate here would be worse than redundant: it would answer 403 to a
// caller whose correct answer is an empty list, and make "you are not a project
// member" observable on a route that currently discloses nothing.
//
// # What this endpoint deliberately does not carry
//
//   - Threads. GET /v1/groups/:group_no/threads already exists with its own
//     pagination and its own access gate (thread admission checks membership of
//     the parent group). Nesting them here would be O(groups × threads) with two
//     pagination axes that cannot share one cursor, and would fork thread access
//     control into a second place.
//   - Unread counts and badge state. Those live on the conversation layer
//     (/v1/sidebar/*). A second source of truth for a number that changes on
//     every message, served from a route with a 60-second membership cache, is
//     not a shortcut worth taking.
//   - An "is the all-member group" flag. The project detail already ships
//     all_member_group_no (#855), and a client rendering this list has the
//     project row in hand; comparing two strings does not need a server field
//     that could disagree with the pointer it duplicates.
func (p *Project) listProjectGroupsHandler(c *wkhttp.Context) {
	row := requestProject(c)
	if row == nil {
		p.Error("projectMiddleware 未注入项目行", zap.String("path", c.FullPath()))
		respondQueryFailed(c)
		return
	}
	// No empty-uid check: projectMiddleware already reads GetLoginUID and aborts
	// with respondNotLoggedIn when it is empty, so a second one here would be
	// unreachable AND would answer a different envelope than the middleware does
	// if it ever fired. listMembersHandler omits it for the same reason.
	uid := c.GetLoginUID()

	offset, limit := pageParams(c)
	rows, err := p.db.listMyProjectGroups(row.SpaceID, row.ProjectID, uid, offset, limit)
	if err != nil {
		p.Error("查询项目群列表失败", zap.Error(err),
			zap.String("projectId", row.ProjectID), zap.String("spaceId", row.SpaceID))
		respondQueryFailed(c)
		return
	}

	groupNos := make([]string, 0, len(rows))
	for _, g := range rows {
		groupNos = append(groupNos, g.GroupNo)
	}
	counts, err := p.db.countActiveGroupMembers(groupNos)
	if err != nil {
		p.Error("统计项目群成员数失败", zap.Error(err), zap.String("projectId", row.ProjectID))
		respondQueryFailed(c)
		return
	}

	resps := make([]*GroupResp, 0, len(rows))
	for _, g := range rows {
		resps = append(resps, &GroupResp{
			GroupNo:        g.GroupNo,
			Name:           g.Name,
			IsNamed:        g.IsNamed,
			AvatarText:     g.AvatarText,
			AvatarColor:    g.AvatarColor,
			IsUploadAvatar: g.IsUploadAvatar,
			MemberCount:    counts[g.GroupNo],
		})
	}
	c.Response(resps)
}
