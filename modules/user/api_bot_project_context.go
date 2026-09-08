package user

import (
	"errors"
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// These are identity/membership facts, not delegated permissions. Owner projects
// are not the Bot's seats and are never emitted as top-level projects.
type verifyBotContext struct {
	UID    string `json:"uid"`
	Active bool   `json:"active"`
	// AgentHosting is self-reported telemetry. Consumers must not use it as an
	// authorization or quota signal.
	AgentHosting           string  `json:"agent_hosting"`
	AgentReportedHostingAt *string `json:"agent_reported_hosting_at"`
	RequestedSpaceID       string  `json:"requested_space_id"`
	SpaceMember            bool    `json:"space_member"`
}

type verifyBotOwnerContext struct {
	UID              string                   `json:"uid"`
	Active           bool                     `json:"active"`
	RequestedSpaceID string                   `json:"requested_space_id"`
	SpaceMember      bool                     `json:"space_member"`
	Projects         []verifyBotProjectAnswer `json:"projects"`
}

// verifyBotProjectAnswer intentionally omits capabilities and member_epoch.
// The Bot credential is not the owner's delegated credential, so this callback
// may report the owner's membership role but must not export an executable
// capability list or lifecycle metadata for that other principal.
type verifyBotProjectAnswer struct {
	ProjectID string `json:"project_id"`
	Member    bool   `json:"member"`
	Role      *int   `json:"role,omitempty"`
}

// Deliberately uncached. No platform/hosting filter belongs in a general identity query.
func (u *User) fillBotProjectContext(resp *authVerifyBotResp, req authVerifyBotReq) error {
	resp.ContextIncluded = true
	if len(req.ProjectIDs) > maxVerifyProjectIDs {
		return errTooManyProjectIDs
	}
	bot := &verifyBotContext{UID: resp.BotUID, RequestedSpaceID: req.SpaceID}
	owner := &verifyBotOwnerContext{
		UID: resp.OwnerUID, RequestedSpaceID: req.SpaceID,
		Projects: make([]verifyBotProjectAnswer, 0),
	}
	resp.BotContext, resp.OwnerContext = bot, owner
	var facts struct {
		Hosting         string       `db:"hosting"`
		HostingReported dbr.NullTime `db:"hosting_reported_at"`
		BotActive       bool         `db:"bot_active"`
		OwnerActive     bool         `db:"owner_active"`
		BotMember       bool         `db:"bot_member"`
		OwnerMember     bool         `db:"owner_member"`
	}
	err := u.db.session.SelectBySql(
		"SELECT IFNULL(r.agent_hosting,'') AS hosting, r.agent_reported_hosting_at AS hosting_reported_at, "+
			"(COALESCE(bu.status, 0) = 1 AND COALESCE(bu.robot, 0) = 1 AND COALESCE(bu.is_destroy, 0) = 0) AS bot_active, "+
			"(COALESCE(ou.status, 0) = 1 AND COALESCE(ou.robot, 0) = 0 AND COALESCE(ou.is_destroy, 0) = 0) AS owner_active, "+
			"(s.space_id IS NOT NULL AND bm.uid IS NOT NULL) AS bot_member, "+
			"(s.space_id IS NOT NULL AND om.uid IS NOT NULL) AS owner_member "+
			"FROM robot r JOIN `user` bu ON bu.uid = r.robot_id JOIN `user` ou ON ou.uid = r.creator_uid "+
			"LEFT JOIN space s ON s.space_id = ? AND s.status = 1 "+
			"LEFT JOIN space_member bm ON bm.space_id = s.space_id AND bm.uid = bu.uid AND bm.status = 1 "+
			"LEFT JOIN space_member om ON om.space_id = s.space_id AND om.uid = ou.uid AND om.status = 1 "+
			"WHERE r.robot_id = ? AND r.creator_uid = ? AND r.bot_token = ? AND r.status = 1 LIMIT 1",
		req.SpaceID, resp.BotUID, resp.OwnerUID, req.BotToken,
	).LoadOne(&facts)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			return fmt.Errorf("user: bot owner context disappeared after credential verification: %w", err)
		}
		return err
	}
	bot.Active, bot.AgentHosting = facts.BotActive, facts.Hosting
	if facts.HostingReported.Valid {
		formatted := facts.HostingReported.Time.Format(time.DateTime)
		bot.AgentReportedHostingAt = &formatted
	}
	owner.Active = facts.OwnerActive
	bot.SpaceMember = facts.BotMember
	owner.SpaceMember = facts.OwnerMember
	if bot.Active && owner.Active && bot.SpaceMember && owner.SpaceMember {
		var answers []verifyProjectAnswer
		answers, err = u.answerProjectMembership(owner.UID, req.SpaceID, req.ProjectIDs)
		owner.Projects = narrowBotProjectAnswers(answers)
		return err
	}
	// No role disclosure when either identity cannot access the named Space.
	owner.Projects = make([]verifyBotProjectAnswer, 0, len(req.ProjectIDs))
	seen := make(map[string]bool, len(req.ProjectIDs))
	for _, id := range req.ProjectIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			owner.Projects = append(owner.Projects, verifyBotProjectAnswer{ProjectID: id})
		}
	}
	return nil
}

func narrowBotProjectAnswers(answers []verifyProjectAnswer) []verifyBotProjectAnswer {
	result := make([]verifyBotProjectAnswer, 0, len(answers))
	for _, answer := range answers {
		var role *int
		if answer.Role != nil {
			value := *answer.Role
			role = &value
		}
		result = append(result, verifyBotProjectAnswer{
			ProjectID: answer.ProjectID,
			Member:    answer.Member,
			Role:      role,
		})
	}
	return result
}
