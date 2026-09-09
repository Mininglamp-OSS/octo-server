package ai_team

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

func normalizeTeamRequest(req *CreateTeamRequest) ([]string, error) {
	if req == nil {
		return nil, errInvalid
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 50 || len(req.BotIDs) == 0 ||
		utf8.RuneCountInString(req.AvatarText) > 4 ||
		(req.AvatarColor != nil && (*req.AvatarColor < 0 || *req.AvatarColor >= avatarrender.PaletteSize())) {
		return nil, errInvalid
	}
	botIDs := make([]string, 0, len(req.BotIDs))
	seen := make(map[string]struct{}, len(req.BotIDs))
	for _, raw := range req.BotIDs {
		botID := strings.TrimSpace(raw)
		if botID == "" {
			return nil, errInvalid
		}
		if _, ok := seen[botID]; ok {
			return nil, errInvalid
		}
		seen[botID] = struct{}{}
		botIDs = append(botIDs, botID)
	}
	sort.Strings(botIDs)
	return botIDs, nil
}

func (s *Service) CreateTeam(spaceID, userUID string, req *CreateTeamRequest) (*Team, error) {
	botIDs, err := normalizeTeamRequest(req)
	if err != nil {
		return nil, err
	}
	groupService := group.NewService(s.ctx)
	created, err := groupService.GetSameDayCreatedCountWithUID(userUID, util.Toyyyy_MM_dd(time.Now()))
	if err != nil {
		return nil, fmt.Errorf("query daily group creation count: %w", err)
	}
	if s.ctx.GetConfig().Group.SameDayCreateMaxCount <= int(created) {
		return nil, errTeamCreateLimit
	}

	groupNo := util.GenerUUID()
	version, err := s.ctx.GenSeq(common.GroupSeqKey)
	if err != nil {
		return nil, fmt.Errorf("generate custom AI team group version: %w", err)
	}
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	if active, checkErr := group.IsAITeamOwnerActiveTx(tx, spaceID, userUID); checkErr != nil {
		return nil, fmt.Errorf("validate custom AI team owner: %w", checkErr)
	} else if !active {
		return nil, errForbidden
	}
	agents, err := resolveEligibleTeamAgentsTx(tx, spaceID, userUID, botIDs)
	if err != nil {
		return nil, fmt.Errorf("resolve custom AI team agents: %w", err)
	}
	if len(agents) != len(botIDs) {
		return nil, errForbidden
	}
	if _, err = tx.InsertBySql(`INSERT INTO `+"`group`"+`
		(group_no,name,creator,status,version,allow_view_history_msg,space_id,allow_external,allow_no_mention,purpose,is_named,avatar_text,avatar_color)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, groupNo, req.Name, userUID, group.GroupStatusNormal,
		version, 1, spaceID, 0, 0, aiteampkg.CustomTeamPurpose, 0, req.AvatarText, req.AvatarColor).Exec(); err != nil {
		return nil, fmt.Errorf("insert custom AI team group: %w", err)
	}
	if _, err = group.SyncAITeamGroupMembersTx(s.ctx, tx, groupNo, spaceID, userUID, botIDs); err != nil {
		return nil, fmt.Errorf("insert custom AI team members: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit custom AI team creation: %w", err)
	}
	if err = s.syncCustomTeam(groupNo, spaceID, userUID); err != nil {
		return nil, fmt.Errorf("sync custom AI team: %w", err)
	}
	return s.GetTeam(spaceID, userUID, groupNo)
}

type teamAgentRef struct {
	ID    int64  `db:"id"`
	BotID string `db:"bot_id"`
}

func resolveEligibleTeamAgentsTx(tx *dbr.Tx, spaceID, userUID string, botIDs []string) ([]*teamAgentRef, error) {
	if len(botIDs) == 0 {
		return []*teamAgentRef{}, nil
	}
	var agents []*teamAgentRef
	_, err := tx.SelectBySql(`SELECT a.id,a.bot_id FROM ai_team_agent a
		JOIN robot r ON r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci
			AND r.creator_uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND r.status=1
		JOIN user u ON u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND u.status=1 AND u.is_destroy<>2
		JOIN space_member sm ON sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci
			AND sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND sm.status=1
		WHERE a.space_id=? AND a.user_uid=? AND a.is_added=1 AND a.container_state=? AND a.bot_id IN ?
		ORDER BY a.bot_id FOR UPDATE OF a`, spaceID, userUID, containerReady, botIDs).Load(&agents)
	return agents, err
}

func (s *Service) ListTeams(spaceID, userUID, before string, limit int) (*TeamPage, error) {
	if limit <= 0 || limit > maxPageSize {
		limit = defaultPageSize
	}
	cursorOrder, cursorGroupNo, err := parseTeamCursor(before)
	if err != nil {
		return nil, err
	}
	if _, err := s.reconcileTeam(spaceID, userUID); err != nil {
		return nil, err
	}
	query := `SELECT teams.* FROM (
		SELECT 0 AS sort_order,g.group_no,g.space_id,g.creator AS user_uid,g.name,? AS state,
			g.avatar_text,g.avatar_color,g.is_upload_avatar,g.created_at,g.updated_at,
			(SELECT COUNT(*) FROM group_member gm WHERE gm.group_no=g.group_no
				AND gm.is_deleted=0 AND gm.status=1 AND gm.robot=1) AS member_count,
			'all_agents' AS team_type
		FROM ` + "`group`" + ` g
		WHERE g.purpose=? AND g.space_id=? AND g.creator=? AND g.status=?
		UNION ALL
		SELECT 1 AS sort_order,g.group_no,g.space_id,g.creator AS user_uid,g.name,? AS state,
			g.avatar_text,g.avatar_color,g.is_upload_avatar,g.created_at,g.updated_at,
			(SELECT COUNT(*) FROM group_member gm WHERE gm.group_no=g.group_no
				AND gm.is_deleted=0 AND gm.status=1 AND gm.robot=1) AS member_count,
			'custom' AS team_type
		FROM ` + "`group`" + ` g
		WHERE g.purpose=? AND g.space_id=? AND g.creator=? AND g.status=?
	) teams WHERE 1=1`
	args := []interface{}{
		containerReady, aiteampkg.TeamGroupPurpose, spaceID, userUID, group.GroupStatusNormal,
		containerReady, aiteampkg.CustomTeamPurpose, spaceID, userUID, group.GroupStatusNormal,
	}
	if cursorGroupNo != "" {
		query += ` AND (teams.sort_order>? OR (teams.sort_order=? AND teams.group_no<?))`
		args = append(args, cursorOrder, cursorOrder, cursorGroupNo)
	}
	query += ` ORDER BY teams.sort_order ASC,teams.group_no DESC LIMIT ?`
	args = append(args, limit+1)
	items := make([]*Team, 0)
	if _, err := s.ctx.DB().SelectBySql(query, args...).Load(&items); err != nil {
		return nil, err
	}
	setTeamCapabilities(items)
	page := &TeamPage{Items: items}
	if len(items) > limit {
		page.NextCursor = fmt.Sprintf("%d:%s", items[limit-1].SortOrder, items[limit-1].GroupNo)
		page.Items = items[:limit]
	}
	return page, nil
}

func parseTeamCursor(cursor string) (int, string, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, "", nil
	}
	parts := strings.SplitN(cursor, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", errInvalid
	}
	order, err := strconv.Atoi(parts[0])
	if err != nil || (order != 0 && order != 1) {
		return 0, "", errInvalid
	}
	return order, parts[1], nil
}

func (s *Service) GetTeam(spaceID, userUID, groupNo string) (*Team, error) {
	var team *Team
	_, err := s.ctx.DB().SelectBySql(`SELECT teams.* FROM (
		SELECT g.group_no,g.space_id,g.creator AS user_uid,g.name,? AS state,
			g.avatar_text,g.avatar_color,g.is_upload_avatar,g.created_at,g.updated_at,
			(SELECT COUNT(*) FROM group_member gm WHERE gm.group_no=g.group_no
				AND gm.is_deleted=0 AND gm.status=1 AND gm.robot=1) AS member_count,
			'all_agents' AS team_type
		FROM `+"`group`"+` g
		WHERE g.group_no=? AND g.purpose=? AND g.space_id=? AND g.creator=? AND g.status=?
		UNION ALL
		SELECT g.group_no,g.space_id,g.creator AS user_uid,g.name,? AS state,
			g.avatar_text,g.avatar_color,g.is_upload_avatar,g.created_at,g.updated_at,
			(SELECT COUNT(*) FROM group_member gm WHERE gm.group_no=g.group_no
				AND gm.is_deleted=0 AND gm.status=1 AND gm.robot=1) AS member_count,
			'custom' AS team_type
		FROM `+"`group`"+` g
		WHERE g.group_no=? AND g.purpose=? AND g.space_id=? AND g.creator=? AND g.status=?
	) teams LIMIT 1`,
		containerReady, groupNo, aiteampkg.TeamGroupPurpose, spaceID, userUID, group.GroupStatusNormal,
		containerReady, groupNo, aiteampkg.CustomTeamPurpose, spaceID, userUID, group.GroupStatusNormal).Load(&team)
	if err != nil {
		return nil, err
	}
	if team == nil {
		return nil, errNotFound
	}
	setTeamCapabilities([]*Team{team})
	team.Agents = make([]*Agent, 0)
	var botIDs []string
	if team.Type == TeamTypeAllAgents {
		botIDs, err = s.desiredTeamBotIDs(spaceID, userUID)
	} else {
		_, err = s.ctx.DB().Select("uid").From("group_member").
			Where("group_no=? AND is_deleted=0 AND status=? AND robot=1", groupNo, common.GroupMemberStatusNormal).
			OrderAsc("uid").Load(&botIDs)
	}
	if err != nil {
		return nil, fmt.Errorf("query custom AI team agents: %w", err)
	}
	for _, botID := range botIDs {
		agent, getErr := s.getAgent(spaceID, userUID, botID, true)
		if getErr == nil {
			team.Agents = append(team.Agents, agent)
		}
	}
	return team, nil
}

func setTeamCapabilities(teams []*Team) {
	for _, team := range teams {
		if team == nil {
			continue
		}
		team.Editable = team.Type == TeamTypeCustom
		team.MembersEditable = team.Type == TeamTypeCustom
	}
}

func (s *Service) UpdateTeam(spaceID, userUID, groupNo string, req *UpdateTeamRequest) (*Team, error) {
	if req == nil || (req.Name == nil && req.AvatarText == nil && req.AvatarColor == nil) {
		return nil, errInvalid
	}
	sets := map[string]interface{}{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || utf8.RuneCountInString(name) > 50 {
			return nil, errInvalid
		}
		sets["name"] = name
	}
	if req.AvatarText != nil {
		if utf8.RuneCountInString(*req.AvatarText) > 4 {
			return nil, errInvalid
		}
		sets["avatar_text"] = *req.AvatarText
		sets["is_upload_avatar"] = 0
	}
	if req.AvatarColor != nil {
		if *req.AvatarColor < 0 || *req.AvatarColor >= avatarrender.PaletteSize() {
			return nil, errInvalid
		}
		sets["avatar_color"] = *req.AvatarColor
	}
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	if err = lockOwnedCustomTeamTx(tx, spaceID, userUID, groupNo, true); err != nil {
		return nil, err
	}
	version, err := s.ctx.GenSeq(common.GroupSeqKey)
	if err != nil {
		return nil, err
	}
	stmt := tx.Update("group").Set("version", version).Where("group_no=?", groupNo)
	for key, value := range sets {
		stmt = stmt.Set(key, value)
	}
	if _, err = stmt.Exec(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.ctx.SendChannelUpdateToGroup(groupNo)
	return s.GetTeam(spaceID, userUID, groupNo)
}

func (s *Service) AddTeamMembers(spaceID, userUID, groupNo string, botIDs []string) error {
	return s.mutateCustomTeamMembers(spaceID, userUID, groupNo, botIDs, true)
}

func (s *Service) RemoveTeamMembers(spaceID, userUID, groupNo string, botIDs []string) error {
	return s.mutateCustomTeamMembers(spaceID, userUID, groupNo, botIDs, false)
}

func normalizeBotIDs(botIDs []string) ([]string, error) {
	if len(botIDs) == 0 {
		return nil, errInvalid
	}
	seen := make(map[string]struct{}, len(botIDs))
	out := make([]string, 0, len(botIDs))
	for _, raw := range botIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, errInvalid
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Service) mutateCustomTeamMembers(spaceID, userUID, groupNo string, rawBotIDs []string, add bool) error {
	botIDs, err := normalizeBotIDs(rawBotIDs)
	if err != nil {
		return err
	}
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()
	if err = lockOwnedCustomTeamTx(tx, spaceID, userUID, groupNo, true); err != nil {
		return err
	}
	current, err := customTeamBotIDsTx(tx, groupNo)
	if err != nil {
		return err
	}
	selected := make(map[string]struct{}, len(current)+len(botIDs))
	for _, botID := range current {
		selected[botID] = struct{}{}
	}
	if add {
		agents, resolveErr := resolveEligibleTeamAgentsTx(tx, spaceID, userUID, botIDs)
		if resolveErr != nil {
			return resolveErr
		}
		if len(agents) != len(botIDs) {
			return errForbidden
		}
		for _, botID := range botIDs {
			selected[botID] = struct{}{}
		}
	} else {
		for _, botID := range botIDs {
			if _, exists := selected[botID]; !exists {
				return errForbidden
			}
			delete(selected, botID)
		}
	}
	desired := make([]string, 0, len(selected))
	for botID := range selected {
		desired = append(desired, botID)
	}
	sort.Strings(desired)
	if _, err = group.SyncAITeamGroupMembersTx(s.ctx, tx, groupNo, spaceID, userUID, desired); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.syncCustomTeam(groupNo, spaceID, userUID)
}

func (s *Service) removeAgentFromCustomTeams(spaceID, userUID, botID string) error {
	var groupNos []string
	_, err := s.ctx.DB().Select("g.group_no").From(dbr.I("group").As("g")).
		Join(dbr.I("group_member").As("gm"), "gm.group_no=g.group_no").
		Where("g.purpose=? AND g.space_id=? AND g.creator=? AND g.status=?", aiteampkg.CustomTeamPurpose, spaceID, userUID, group.GroupStatusNormal).
		Where("gm.uid=? AND gm.is_deleted=0 AND gm.status=?", botID, common.GroupMemberStatusNormal).
		OrderAsc("g.group_no").Load(&groupNos)
	if err != nil {
		return err
	}
	for _, groupNo := range groupNos {
		if err = s.removeCustomTeamMember(spaceID, userUID, groupNo, botID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) removeCustomTeamMember(spaceID, userUID, groupNo, botID string) error {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()
	if err = lockOwnedCustomTeamTx(tx, spaceID, userUID, groupNo, true); err != nil {
		return err
	}
	current, err := customTeamBotIDsTx(tx, groupNo)
	if err != nil {
		return err
	}
	desired := make([]string, 0, len(current))
	for _, currentBotID := range current {
		if currentBotID != botID {
			desired = append(desired, currentBotID)
		}
	}
	if _, err = group.SyncAITeamGroupMembersTx(s.ctx, tx, groupNo, spaceID, userUID, desired); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.syncCustomTeam(groupNo, spaceID, userUID)
}

func lockOwnedCustomTeamTx(tx *dbr.Tx, spaceID, userUID, groupNo string, activeOnly bool) error {
	query := `SELECT group_no FROM ` + "`group`" + `
		WHERE group_no=? AND purpose=? AND space_id=? AND creator=?`
	args := []interface{}{groupNo, aiteampkg.CustomTeamPurpose, spaceID, userUID}
	if activeOnly {
		query += ` AND status=?`
		args = append(args, group.GroupStatusNormal)
	}
	query += ` LIMIT 1 FOR UPDATE`
	var locked string
	count, err := tx.SelectBySql(query, args...).Load(&locked)
	if err != nil {
		return err
	}
	if count != 1 {
		return errNotFound
	}
	return nil
}

func customTeamBotIDsTx(tx *dbr.Tx, groupNo string) ([]string, error) {
	botIDs := make([]string, 0)
	_, err := tx.Select("uid").From("group_member").
		Where("group_no=? AND is_deleted=0 AND status=? AND robot=1", groupNo, common.GroupMemberStatusNormal).
		OrderAsc("uid").Load(&botIDs)
	return botIDs, err
}

func customTeamRosterVersionTx(tx *dbr.Tx, groupNo string) (uint64, error) {
	var version uint64
	_, err := tx.Select("COALESCE(MAX(version),0)").From("group_member").Where("group_no=?", groupNo).Load(&version)
	return version, err
}

func (s *Service) syncCustomTeam(groupNo, spaceID, userUID string) error {
	for attempt := 0; attempt < teamProjectionMaxSnapshots; attempt++ {
		snapshot, err := s.prepareCustomTeamProjectionSnapshot(groupNo, spaceID, userUID)
		if err != nil {
			return err
		}
		if err = group.SyncAITeamGroupSubscribers(
			s.ctx, groupNo, spaceID, snapshot.shortIDs, snapshot.desired, snapshot.excluded,
		); err != nil {
			return fmt.Errorf("%w: sync custom AI team subscribers: %v", errIMUnavailable, err)
		}
		current, err := s.customTeamProjectionCurrent(groupNo, spaceID, userUID, snapshot)
		if err != nil {
			return err
		}
		if current {
			s.ctx.SendChannelUpdateToGroup(groupNo)
			if err := s.ctx.SendCMD(config.MsgCMDReq{
				ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(),
				CMD: common.CMDGroupMemberUpdate, Param: map[string]interface{}{"group_no": groupNo},
			}); err != nil {
				s.Warn("send custom AI team member update command", zap.Error(err), zap.String("group_no", groupNo))
			}
			return nil
		}
	}
	return fmt.Errorf("%w: custom AI team roster did not stabilize", errIMUnavailable)
}

func (s *Service) prepareCustomTeamProjectionSnapshot(groupNo, spaceID, userUID string) (*teamProjectionSnapshot, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	if err = lockOwnedCustomTeamTx(tx, spaceID, userUID, groupNo, true); err != nil {
		return nil, err
	}
	version, err := customTeamRosterVersionTx(tx, groupNo)
	if err != nil {
		return nil, err
	}
	botIDs, err := customTeamBotIDsTx(tx, groupNo)
	if err != nil {
		return nil, err
	}
	desired := append([]string{userUID}, botIDs...)
	var historical []string
	if _, err = tx.Select("uid").From("group_member").Where("group_no=?", groupNo).Load(&historical); err != nil {
		return nil, err
	}
	wanted := make(map[string]struct{}, len(desired))
	for _, uid := range desired {
		wanted[uid] = struct{}{}
	}
	excluded := make([]string, 0)
	for _, uid := range historical {
		if _, ok := wanted[uid]; !ok {
			excluded = append(excluded, uid)
		}
	}
	shortIDs, err := group.PrepareAITeamGroupSubscriberProjectionTx(tx, groupNo, excluded)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &teamProjectionSnapshot{
		desired: desired, excluded: excluded, shortIDs: shortIDs, rosterVersion: version,
	}, nil
}

func (s *Service) customTeamProjectionCurrent(
	groupNo, spaceID, userUID string,
	snapshot *teamProjectionSnapshot,
) (bool, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return false, err
	}
	defer tx.RollbackUnlessCommitted()
	if err = lockOwnedCustomTeamTx(tx, spaceID, userUID, groupNo, true); err != nil {
		return false, err
	}
	version, err := customTeamRosterVersionTx(tx, groupNo)
	if err != nil {
		return false, err
	}
	botIDs, err := customTeamBotIDsTx(tx, groupNo)
	if err != nil {
		return false, err
	}
	desired := append([]string{userUID}, botIDs...)
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return version == snapshot.rosterVersion && sameStringSet(desired, snapshot.desired), nil
}

func (s *Service) DeleteTeam(spaceID, userUID, groupNo string) error {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()
	if err = lockOwnedCustomTeamTx(tx, spaceID, userUID, groupNo, false); err != nil {
		return err
	}
	version, err := s.ctx.GenSeq(common.GroupSeqKey)
	if err != nil {
		return err
	}
	if _, err = tx.Update("group").Set("status", group.GroupStatusDisband).Set("version", version).
		Where("group_no=?", groupNo).Exec(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	var shortIDs []string
	if _, err = s.ctx.DB().Select("short_id").From("thread").Where("group_no=? AND status<>3", groupNo).Load(&shortIDs); err != nil {
		return err
	}
	channels := append([]string{groupNo}, shortIDs...)
	for index, channelID := range channels {
		channelType := common.ChannelTypeGroup.Uint8()
		if index > 0 {
			channelID = groupNo + "____" + channelID
			channelType = common.ChannelTypeCommunityTopic.Uint8()
		}
		if err = s.ctx.IMCreateOrUpdateChannelInfo(&config.ChannelInfoCreateReq{
			ChannelID: channelID, ChannelType: channelType, Disband: 1,
		}); err != nil {
			return fmt.Errorf("%w: disband custom AI team: %v", errIMUnavailable, err)
		}
	}
	s.ctx.SendChannelUpdateToGroup(groupNo)
	return nil
}
