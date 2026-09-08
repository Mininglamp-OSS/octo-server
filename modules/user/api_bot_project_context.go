package user

import "github.com/gocraft/dbr/v2"

// These are identity/membership facts, not delegated permissions. Owner projects
// are not the Bot's seats and are never emitted as top-level projects.
type verifyBotContext struct {
	UID          string `json:"uid"`
	Active       bool   `json:"active"`
	AgentHosting string `json:"agent_hosting"`
	SpaceID      string `json:"space_id"`
	SpaceMember  bool   `json:"space_member"`
}

type verifyBotOwnerContext struct {
	UID         string                `json:"uid"`
	Active      bool                  `json:"active"`
	SpaceID     string                `json:"space_id"`
	SpaceMember bool                  `json:"space_member"`
	Projects    []verifyProjectAnswer `json:"projects"`
}

// Deliberately uncached. No platform/hosting filter belongs in a general identity query.
func (u *User) fillBotProjectContext(resp *authVerifyBotResp, req authVerifyBotReq) error {
	resp.ContextIncluded = true
	if len(req.ProjectIDs) > maxVerifyProjectIDs {
		return errTooManyProjectIDs
	}
	bot := &verifyBotContext{UID: resp.BotUID, SpaceID: req.SpaceID}
	owner := &verifyBotOwnerContext{UID: resp.OwnerUID, SpaceID: req.SpaceID}
	resp.BotContext, resp.OwnerContext = bot, owner
	var facts struct {
		Hosting     string `db:"hosting"`
		BotActive   bool   `db:"bot_active"`
		OwnerActive bool   `db:"owner_active"`
		BotMember   bool   `db:"bot_member"`
		OwnerMember bool   `db:"owner_member"`
	}
	err := u.db.session.SelectBySql(
		"SELECT IFNULL(r.agent_hosting,'') AS hosting, "+
			"(bu.status = 1 AND bu.robot = 1) AS bot_active, (ou.status = 1 AND ou.robot = 0) AS owner_active, "+
			"(s.space_id IS NOT NULL AND bm.uid IS NOT NULL) AS bot_member, "+
			"(s.space_id IS NOT NULL AND om.uid IS NOT NULL) AS owner_member "+
			"FROM robot r JOIN `user` bu ON bu.uid = r.robot_id JOIN `user` ou ON ou.uid = r.creator_uid "+
			"LEFT JOIN space s ON s.space_id = ? AND s.status = 1 "+
			"LEFT JOIN space_member bm ON bm.space_id = s.space_id AND bm.uid = bu.uid AND bm.status = 1 "+
			"LEFT JOIN space_member om ON om.space_id = s.space_id AND om.uid = ou.uid AND om.status = 1 "+
			"WHERE r.robot_id = ? AND r.creator_uid = ? AND r.bot_token = ? AND r.status = 1 LIMIT 1",
		req.SpaceID, resp.BotUID, resp.OwnerUID, req.BotToken,
	).LoadOne(&facts)
	if err != nil && err != dbr.ErrNotFound {
		return err
	}
	bot.Active, bot.AgentHosting = facts.BotActive, facts.Hosting
	owner.Active = facts.OwnerActive
	bot.SpaceMember = bot.Active && facts.BotMember
	owner.SpaceMember = owner.Active && facts.OwnerMember
	if bot.SpaceMember && owner.SpaceMember {
		owner.Projects, err = u.answerProjectMembership(owner.UID, req.SpaceID, req.ProjectIDs)
		return err
	}
	// No role/epoch disclosure when either identity cannot access the named Space.
	owner.Projects = make([]verifyProjectAnswer, 0, len(req.ProjectIDs))
	seen := make(map[string]bool, len(req.ProjectIDs))
	for _, id := range req.ProjectIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			owner.Projects = append(owner.Projects, verifyProjectAnswer{ProjectID: id})
		}
	}
	return nil
}
