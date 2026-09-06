package space

import (
	"context"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

const (
	directoryDBTimeout         = 3 * time.Second
	directoryHostingTimeFormat = "2006-01-02 15:04:05"
)

type directoryResponse struct {
	Data []directoryMemberResp `json:"data"`
}

type directoryMemberResp struct {
	UID             string               `json:"uid"`
	Name            string               `json:"name"`
	Role            int                  `json:"role"`
	AgentCount      int64                `json:"agent_count"`
	AgentsTruncated bool                 `json:"agents_truncated"`
	Agents          []directoryAgentResp `json:"agents"`
}

type directoryAgentResp struct {
	UID               string  `json:"uid"`
	Name              string  `json:"name"`
	Description       string  `json:"description"`
	IsFriend          bool    `json:"is_friend"`
	Hosting           string  `json:"hosting"`
	HostingReportedAt *string `json:"hosting_reported_at"`
}

// listDirectory returns each active human in a verified Space with the
// non-self-hosted user bots they own. The hosting value is self-reported by a
// bot, so this endpoint uses it solely as a display filter, never as an authz
// or tenancy signal.
func (s *Space) listDirectory(c *wkhttp.Context) {
	spaceID := c.Query("space_id")
	if spaceID == "" {
		// SpaceMiddleware intentionally skips absent selectors, so this handler
		// must close that fail-open path before any cross-member query runs.
		respondSpaceRequestInvalid(c, "space_id")
		return
	}
	// The endpoint contract intentionally requires the query selector. Once it
	// is present, consume the value the Space middleware marked as verified
	// instead of relying on the raw request parameter for data access.
	spaceID = spacepkg.GetSpaceID(c)
	if spaceID == "" {
		respondSpaceRequestInvalid(c, "space_id")
		return
	}

	onlyWithAgents := false
	if raw := c.Query("only_with_agents"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			respondSpaceRequestInvalid(c, "only_with_agents")
			return
		}
		onlyWithAgents = parsed
	}

	// Both reads share one request budget. A failure in either one is terminal:
	// returning an owner list without a complete agent query misstates the
	// directory and makes query failures look like zero owned agents.
	ctx, cancel := context.WithTimeout(c.Request.Context(), directoryDBTimeout)
	defer cancel()
	owners, err := s.db.queryDirectoryOwners(ctx, spaceID)
	if err != nil {
		s.Error("查询空间通讯录真人失败", zap.Error(err), zap.String("space_id", spaceID))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	agents, err := s.db.queryDirectoryAgents(ctx, spaceID, c.GetLoginUID())
	if err != nil {
		s.Error("查询空间通讯录分身失败", zap.Error(err), zap.String("space_id", spaceID))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}

	resp := make([]directoryMemberResp, 0, len(owners))
	ownerIndex := make(map[string]int, len(owners))
	for _, owner := range owners {
		member := MemberDetailModel{Name: owner.Name, RealName: owner.RealName}
		member.UID = owner.UID
		ownerIndex[owner.UID] = len(resp)
		resp = append(resp, directoryMemberResp{
			UID:    owner.UID,
			Name:   member.DisplayName(),
			Role:   owner.Role,
			Agents: make([]directoryAgentResp, 0),
		})
	}

	for _, agent := range agents {
		idx, ok := ownerIndex[agent.CreatorUID]
		if !ok {
			// The agent query already requires an eligible owner, but keep the
			// assembly fail-closed if the two non-transactional reads diverge.
			continue
		}
		member := &resp[idx]
		member.AgentCount = agent.AgentCount
		member.AgentsTruncated = agent.AgentCount > directoryAgentsPerOwner
		member.Agents = append(member.Agents, directoryAgentResp{
			UID:               agent.UID,
			Name:              agent.Name,
			Description:       agent.Description,
			IsFriend:          agent.IsFriend == 1,
			Hosting:           agent.Hosting,
			HostingReportedAt: formatDirectoryHostingReportedAt(agent),
		})
	}

	if onlyWithAgents {
		filtered := make([]directoryMemberResp, 0, len(resp))
		for _, member := range resp {
			if member.AgentCount > 0 {
				filtered = append(filtered, member)
			}
		}
		resp = filtered
	}

	c.Response(directoryResponse{Data: resp})
}

func formatDirectoryHostingReportedAt(agent *directoryAgentModel) *string {
	if !agent.HostingReportedAt.Valid {
		return nil
	}
	formatted := agent.HostingReportedAt.Time.Format(directoryHostingTimeFormat)
	return &formatted
}
