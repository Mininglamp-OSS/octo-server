package ai_team

import (
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// ProvisionOwnedBot is the lifecycle entry used after a User Bot has been
// durably bound to a Space. It activates the Agent and converges both managed
// groups before returning.
func ProvisionOwnedBot(ctx *config.Context, spaceID, userUID, botID string) error {
	if !aiteampkg.Enabled() {
		return nil
	}
	_, err := NewService(ctx).AddAgent(spaceID, userUID, botID)
	return err
}

// reconcileTeam backfills only Bot identities that have never had an Agent
// row. An explicit DELETE leaves is_added=0 behind, so ordinary list calls do
// not silently undo a user's removal.
func (s *Service) reconcileTeam(spaceID, userUID string) (*TeamGroup, error) {
	missing, err := s.missingOwnedBotIDs(spaceID, userUID)
	if err != nil {
		return nil, err
	}
	for _, botID := range missing {
		if _, err = s.ensureAgentContainer(spaceID, userUID, botID); err != nil {
			return nil, err
		}
	}
	if err = s.reconcileActiveAgentContainers(spaceID, userUID); err != nil {
		return nil, err
	}
	return s.ensureTeamGroup(spaceID, userUID)
}

func (s *Service) missingOwnedBotIDs(spaceID, userUID string) ([]string, error) {
	botIDs := make([]string, 0)
	_, err := s.ctx.DB().SelectBySql(`
		SELECT r.robot_id
		FROM robot r
		JOIN user u ON u.uid=r.robot_id AND u.status=1 AND u.is_destroy<>2
		JOIN space sp ON sp.space_id=? AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=sp.space_id AND human_sm.uid=? AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=sp.space_id AND bot_sm.uid=r.robot_id AND bot_sm.status=1
		LEFT JOIN ai_team_agent a ON a.space_id=sp.space_id COLLATE utf8mb4_general_ci
			AND a.user_uid=r.creator_uid COLLATE utf8mb4_general_ci
			AND a.bot_id=r.robot_id COLLATE utf8mb4_general_ci
		WHERE r.creator_uid=? AND r.status=1 AND a.id IS NULL
		ORDER BY r.robot_id`, spaceID, userUID, userUID).Load(&botIDs)
	return botIDs, err
}

func (s *Service) reconcileActiveAgentContainers(spaceID, userUID string) error {
	botIDs := make([]string, 0)
	_, err := s.eligibleAgentsQuery(spaceID, userUID, "a.bot_id").OrderAsc("a.bot_id").Load(&botIDs)
	if err != nil {
		return err
	}
	for _, botID := range botIDs {
		if _, err = s.ensureAgentContainer(spaceID, userUID, botID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) desiredTeamBotIDs(spaceID, userUID string) ([]string, error) {
	botIDs := make([]string, 0)
	_, err := s.eligibleAgentsQuery(spaceID, userUID, "a.bot_id").
		Where("a.group_no IS NOT NULL AND a.group_no<>'' AND a.container_state=?", containerReady).
		OrderAsc("a.bot_id").Load(&botIDs)
	return botIDs, err
}

func desiredTeamBotIDsTx(tx *dbr.Tx, spaceID, userUID string) ([]string, error) {
	botIDs := make([]string, 0)
	_, err := tx.SelectBySql(`SELECT a.bot_id
		FROM ai_team_agent a
		JOIN robot r ON r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci
			AND r.status=1 AND r.creator_uid=a.user_uid COLLATE utf8mb4_0900_ai_ci
		JOIN user u ON u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND u.status=1 AND u.is_destroy<>2
		JOIN space sp ON sp.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci
			AND human_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci
			AND bot_sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.status=1
		WHERE a.space_id=? AND a.user_uid=? AND a.is_added=1
			AND a.group_no IS NOT NULL AND a.group_no<>'' AND a.container_state=?
		ORDER BY a.bot_id FOR UPDATE`, spaceID, userUID, containerReady).Load(&botIDs)
	return botIDs, err
}

func (s *Service) ensureTeamGroup(spaceID, userUID string) (*TeamGroup, error) {
	botIDs, err := s.desiredTeamBotIDs(spaceID, userUID)
	if err != nil {
		return nil, err
	}

	var existing int
	if err = s.ctx.DB().Select("COUNT(*)").From("ai_team_group").
		Where("space_id=? AND user_uid=?", spaceID, userUID).LoadOne(&existing); err != nil {
		return nil, err
	}
	if len(botIDs) == 0 && existing == 0 {
		return nil, nil
	}

	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	if len(botIDs) > 0 {
		if _, err = tx.InsertBySql(`INSERT IGNORE INTO ai_team_group
			(space_id,user_uid,state) VALUES (?,?,?)`, spaceID, userUID, containerUnassigned).Exec(); err != nil {
			return nil, err
		}
	}
	var row struct {
		GroupNo string `db:"group_no"`
		State   int    `db:"state"`
	}
	count, err := tx.SelectBySql(`SELECT IFNULL(group_no,'') AS group_no,state
		FROM ai_team_group WHERE space_id=? AND user_uid=? FOR UPDATE`, spaceID, userUID).Load(&row)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	// Re-read and lock the authoritative Agent rows after taking the owner-group
	// row lock. This prevents an older roster snapshot from running after a newer
	// AddAgent/RemoveAgent reconciliation and removing its result.
	botIDs, err = desiredTeamBotIDsTx(tx, spaceID, userUID)
	if err != nil {
		return nil, err
	}

	created := false
	if row.GroupNo == "" {
		row.GroupNo = util.GenerUUID()
		version, genErr := s.ctx.GenSeq(common.GroupSeqKey)
		if genErr != nil {
			return nil, genErr
		}
		if _, err = tx.InsertBySql(`INSERT INTO `+"`group`"+`
			(group_no,name,creator,status,version,allow_view_history_msg,space_id,allow_external,allow_no_mention,purpose)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, row.GroupNo, aiteampkg.TeamGroupName, userUID,
			group.GroupStatusNormal, version, 1, spaceID, 0, 1, aiteampkg.TeamGroupPurpose).Exec(); err != nil {
			return nil, err
		}
		if _, err = tx.Update("ai_team_group").Set("group_no", row.GroupNo).
			Set("state", containerProvisioning).Set("last_error", "").
			Where("space_id=? AND user_uid=?", spaceID, userUID).Exec(); err != nil {
			return nil, err
		}
		row.State = containerProvisioning
		created = true
	}

	changed, err := group.SyncAITeamGroupMembersTx(s.ctx, tx, row.GroupNo, spaceID, userUID, botIDs)
	if err != nil {
		return nil, err
	}
	needsIM := created || changed || row.State != containerReady
	if needsIM && row.State == containerReady {
		if _, err = tx.Update("ai_team_group").Set("state", containerProvisioning).
			Where("space_id=? AND user_uid=?", spaceID, userUID).Exec(); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}

	if needsIM {
		subscribers := append([]string{userUID}, botIDs...)
		if err = s.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
			ChannelID: row.GroupNo, ChannelType: common.ChannelTypeGroup.Uint8(), Subscribers: subscribers,
		}); err != nil {
			s.markTeamGroupFailure(spaceID, userUID, err)
			return nil, fmt.Errorf("%w: provision AI-team group channel: %v", errIMUnavailable, err)
		}
		if _, err = s.ctx.DB().Update("ai_team_group").Set("state", containerReady).Set("last_error", "").
			Where("space_id=? AND user_uid=? AND group_no=?", spaceID, userUID, row.GroupNo).Exec(); err != nil {
			return nil, err
		}
		row.State = containerReady
	}
	return &TeamGroup{GroupNo: row.GroupNo, Name: aiteampkg.TeamGroupName, State: row.State}, nil
}

func (s *Service) markTeamGroupFailure(spaceID, userUID string, cause error) {
	reason := strings.TrimSpace(cause.Error())
	if runes := []rune(reason); len(runes) > 255 {
		reason = string(runes[:255])
	}
	if _, err := s.ctx.DB().Update("ai_team_group").Set("state", containerFailed).Set("last_error", reason).
		Where("space_id=? AND user_uid=?", spaceID, userUID).Exec(); err != nil {
		s.Error("mark AI-team group provisioning failure", zap.Error(err), zap.String("space_id", spaceID), zap.String("uid", userUID))
	}
}
