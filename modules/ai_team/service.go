package ai_team

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

const aiTeamMutationRetryAttempts = 3

type Service struct {
	ctx           *config.Context
	threadService thread.IService
	log.Log
}

type lockedAgent struct {
	ID             int64  `db:"id"`
	GroupNo        string `db:"group_no"`
	ContainerState int    `db:"container_state"`
}

func NewService(ctx *config.Context) *Service {
	return &Service{ctx: ctx, threadService: thread.NewService(ctx), Log: log.NewTLog("AITeamService")}
}

func (s *Service) validateAuthority(spaceID, userUID, botID string) (string, error) {
	var botName string
	rows, err := s.ctx.DB().SelectBySql(`
		SELECT u.name
		FROM robot r
		JOIN user u ON u.uid=r.robot_id AND u.status=1 AND u.is_destroy<>2
		JOIN space sp ON sp.space_id=? AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=sp.space_id AND human_sm.uid=? AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=sp.space_id AND bot_sm.uid=r.robot_id AND bot_sm.status=1
		WHERE r.robot_id=? AND r.creator_uid=? AND r.status=1
		LIMIT 1`, spaceID, userUID, botID, userUID).Load(&botName)
	if err != nil {
		return "", err
	}
	if rows == 0 {
		return "", errForbidden
	}
	return botName, nil
}

func validateAuthorityTx(tx *dbr.Tx, spaceID, userUID, botID string) error {
	var count int
	err := tx.SelectBySql(`
		SELECT COUNT(*)
		FROM robot r
		JOIN user u ON u.uid=r.robot_id AND u.status=1 AND u.is_destroy<>2
		JOIN space sp ON sp.space_id=? AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=sp.space_id AND human_sm.uid=? AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=sp.space_id AND bot_sm.uid=r.robot_id AND bot_sm.status=1
		WHERE r.robot_id=? AND r.creator_uid=? AND r.status=1`, spaceID, userUID, botID, userUID).LoadOne(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return errForbidden
	}
	return nil
}

func (s *Service) AddAgent(spaceID, userUID, botID string) (*Agent, error) {
	var agent *Agent
	err := retryAITeamMutation(func() error {
		var attemptErr error
		agent, attemptErr = s.addAgentOnce(spaceID, userUID, botID)
		return attemptErr
	})
	return agent, err
}

// addAgentOnce performs one idempotent activation attempt. Keeping the entire
// orchestration behind the retry boundary matters: a lock conflict can occur in
// the private-container transaction or while projecting the team-group roster.
func (s *Service) addAgentOnce(spaceID, userUID, botID string) (*Agent, error) {
	if _, err := s.ensureAgentContainer(spaceID, userUID, botID); err != nil {
		return nil, err
	}
	if err := s.reconcileActiveAgentContainers(spaceID, userUID); err != nil {
		return nil, err
	}
	if _, err := s.ensureTeamGroup(spaceID, userUID); err != nil {
		return nil, err
	}
	return s.getAgent(spaceID, userUID, botID, false)
}

func retryAITeamMutation(fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < aiTeamMutationRetryAttempts; attempt++ {
		err := fn()
		if err == nil || !isRetryableAITeamMutationError(err) {
			return err
		}
		lastErr = err
		if attempt+1 < aiTeamMutationRetryAttempts {
			time.Sleep(time.Duration(attempt+1) * 3 * time.Millisecond)
		}
	}
	return fmt.Errorf("ai team: mutation retries exhausted: %w", lastErr)
}

func isRetryableAITeamMutationError(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && (mysqlErr.Number == 1213 || mysqlErr.Number == 1205)
}

// ensureAgentContainer is the idempotent half of Agent activation that owns the
// private two-member group. It deliberately does not recurse into team-group
// reconciliation, allowing a caller to repair every Agent first and project the
// complete roster exactly once afterwards.
func (s *Service) ensureAgentContainer(spaceID, userUID, botID string) (*Agent, error) {
	botName, err := s.validateAuthority(spaceID, userUID, botID)
	if err != nil {
		return nil, err
	}
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	if err = validateAuthorityTx(tx, spaceID, userUID, botID); err != nil {
		return nil, err
	}
	_, err = tx.InsertBySql(`
		INSERT INTO ai_team_agent (space_id,user_uid,bot_id,is_added)
		VALUES (?,?,?,1)
		ON DUPLICATE KEY UPDATE is_added=1, updated_at=CURRENT_TIMESTAMP`, spaceID, userUID, botID).Exec()
	if err != nil {
		return nil, err
	}
	var agent *lockedAgent
	_, err = tx.SelectBySql(`SELECT id,IFNULL(group_no,'') AS group_no,container_state FROM ai_team_agent
		WHERE space_id=? AND user_uid=? AND bot_id=? FOR UPDATE`, spaceID, userUID, botID).Load(&agent)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, errNotFound
	}

	if agent.GroupNo == "" {
		agent.GroupNo = util.GenerUUID()
		groupName := strings.TrimSpace(botName)
		if groupName == "" {
			groupName = botID
		}
		groupName += " · AI"
		if r := []rune(groupName); len(r) > group.MaxGroupNameLen {
			groupName = string(r[:group.MaxGroupNameLen])
		}
		version, genErr := s.ctx.GenSeq(common.GroupSeqKey)
		if genErr != nil {
			return nil, genErr
		}
		if _, err = tx.InsertBySql("INSERT INTO `group` (group_no,name,creator,status,version,allow_view_history_msg,space_id,allow_external,allow_no_mention,purpose) VALUES (?,?,?,?,?,?,?,?,?,?)",
			agent.GroupNo, groupName, userUID, group.GroupStatusNormal, version, 1, spaceID, 0, 1, aiteampkg.GroupPurpose).Exec(); err != nil {
			return nil, err
		}
		if _, err = tx.Update("ai_team_agent").Set("group_no", agent.GroupNo).Set("container_state", containerProvisioning).Where("id=?", agent.ID).Exec(); err != nil {
			return nil, err
		}
		agent.ContainerState = containerProvisioning
	}
	repaired, repairErr := s.admitContainerMembersTx(tx, agent.GroupNo, spaceID, userUID, botID)
	if repairErr != nil {
		return nil, repairErr
	}
	containerNeedsReconcile := repaired || agent.ContainerState != containerReady
	if containerNeedsReconcile {
		if _, err = tx.Update("ai_team_agent").Set("container_state", containerProvisioning).Where("id=?", agent.ID).Exec(); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if containerNeedsReconcile {
		if err = s.ensureContainerIMReady(agent.ID, agent.GroupNo, userUID, botID); err != nil {
			s.markContainerFailure(agent.ID, err)
			return nil, fmt.Errorf("%w: reconcile IM channels: %v", errIMUnavailable, err)
		}
		readyTx, beginErr := s.ctx.DB().Begin()
		if beginErr != nil {
			return nil, beginErr
		}
		defer readyTx.RollbackUnlessCommitted()
		if err = validateAuthorityTx(readyTx, spaceID, userUID, botID); err != nil {
			s.markContainerFailure(agent.ID, err)
			return nil, err
		}
		if _, err = readyTx.Update("ai_team_agent").Set("container_state", containerReady).Where("id=?", agent.ID).Exec(); err != nil {
			return nil, err
		}
		if err = readyTx.Commit(); err != nil {
			return nil, err
		}
	}
	return s.getAgent(spaceID, userUID, botID, false)
}

func (s *Service) RemoveAgent(spaceID, userUID, botID string) error {
	return retryAITeamMutation(func() error {
		return s.removeAgentOnce(spaceID, userUID, botID)
	})
}

func (s *Service) removeAgentOnce(spaceID, userUID, botID string) error {
	if _, err := s.validateAuthority(spaceID, userUID, botID); err != nil {
		return err
	}
	if _, err := s.getAgent(spaceID, userUID, botID, false); err != nil {
		return err
	}
	_, err := s.ctx.DB().Update("ai_team_agent").Set("is_added", 0).
		Where("space_id=? AND user_uid=? AND bot_id=?", spaceID, userUID, botID).Exec()
	if err != nil {
		return err
	}
	_, err = s.ensureTeamGroup(spaceID, userUID)
	return err
}

func (s *Service) getAgent(spaceID, userUID, botID string, requireAdded bool) (*Agent, error) {
	q := s.ctx.DB().Select(
		"a.id", "a.space_id", "a.user_uid", "a.bot_id", "u.name AS bot_name",
		"IFNULL(a.group_no,'') AS group_no", "a.is_added", "a.container_state", "a.created_at", "a.updated_at",
		"(SELECT COUNT(*) FROM ai_team_session ats2 JOIN thread t2 ON t2.short_id=ats2.short_id AND t2.group_no=a.group_no WHERE ats2.agent_id=a.id AND ats2.state=2 AND t2.status<>3) AS session_count",
	).From(dbr.I("ai_team_agent").As("a")).
		Join(dbr.I("robot").As("r"), "r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci AND r.status=1 AND r.creator_uid=a.user_uid COLLATE utf8mb4_0900_ai_ci").
		Join(dbr.I("user").As("u"), "u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND u.status=1 AND u.is_destroy<>2").
		Join(dbr.I("space").As("sp"), "sp.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND sp.status=1").
		Join(dbr.I("space_member").As("human_sm"), "human_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND human_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_sm.status=1").
		Join(dbr.I("space_member").As("bot_sm"), "bot_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.status=1").
		Where("a.space_id=? AND a.user_uid=? AND a.bot_id=?", spaceID, userUID, botID)
	if requireAdded {
		q = q.Where("a.is_added=1")
	}
	var out *Agent
	_, err := q.Load(&out)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errNotFound
	}
	return out, nil
}

func (s *Service) eligibleAgentsQuery(spaceID, userUID string, columns ...string) *dbr.SelectStmt {
	return s.ctx.DB().Select(columns...).
		From(dbr.I("ai_team_agent").As("a")).
		Join(dbr.I("robot").As("r"), "r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci AND r.status=1 AND r.creator_uid=a.user_uid COLLATE utf8mb4_0900_ai_ci").
		Join(dbr.I("user").As("u"), "u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND u.status=1 AND u.is_destroy<>2").
		Join(dbr.I("space").As("sp"), "sp.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND sp.status=1").
		Join(dbr.I("space_member").As("human_sm"), "human_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND human_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_sm.status=1").
		Join(dbr.I("space_member").As("bot_sm"), "bot_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.status=1").
		Where("a.space_id=? AND a.user_uid=? AND a.is_added=1", spaceID, userUID)
}

func (s *Service) ListAgents(spaceID, userUID string, beforeID int64, limit int) (*AgentPage, error) {
	var teamGroup *TeamGroup
	err := retryAITeamMutation(func() error {
		var attemptErr error
		teamGroup, attemptErr = s.reconcileTeam(spaceID, userUID)
		return attemptErr
	})
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxPageSize {
		limit = defaultPageSize
	}
	q := s.eligibleAgentsQuery(spaceID, userUID,
		"a.id", "a.space_id", "a.user_uid", "a.bot_id", "u.name AS bot_name",
		"IFNULL(a.group_no,'') AS group_no", "a.is_added", "a.container_state", "a.created_at", "a.updated_at",
		fmt.Sprintf("%s AS agent_group", agentGroupSQL),
		"(SELECT COUNT(*) FROM ai_team_session ats2 JOIN thread t2 ON t2.short_id=ats2.short_id AND t2.group_no=a.group_no WHERE ats2.agent_id=a.id AND ats2.state=2 AND t2.status<>3) AS session_count",
	).OrderDesc("a.id").Limit(uint64(limit + 1))
	if beforeID > 0 {
		q = q.Where("a.id<?", beforeID)
	}
	rows := make([]*Agent, 0)
	_, err = q.Load(&rows)
	if err != nil {
		return nil, err
	}
	groupCounts := make([]struct {
		Type  AgentGroupType `db:"agent_group"`
		Count int64          `db:"agent_count"`
	}, 0, 2)
	_, err = s.eligibleAgentsQuery(spaceID, userUID,
		fmt.Sprintf("%s AS agent_group", agentGroupSQL),
		"COUNT(*) AS agent_count",
	).GroupBy(agentGroupSQL).Load(&groupCounts)
	if err != nil {
		return nil, err
	}
	counts := map[AgentGroupType]int64{
		AgentGroupTypeCloudClone:        0,
		AgentGroupTypePersonalAssistant: 0,
	}
	for _, groupCount := range groupCounts {
		counts[groupCount.Type] = groupCount.Count
	}
	page := &AgentPage{TeamGroup: teamGroup, Groups: []*AgentGroup{
		{Type: AgentGroupTypeCloudClone, Count: counts[AgentGroupTypeCloudClone], Items: make([]*Agent, 0)},
		{Type: AgentGroupTypePersonalAssistant, Count: counts[AgentGroupTypePersonalAssistant], Items: make([]*Agent, 0)},
		{Type: AgentGroupTypeDigitalEmployee, Count: 0, Items: make([]*Agent, 0)},
	}}
	if len(rows) > limit {
		rows = rows[:limit]
		page.NextCursor = fmt.Sprintf("%d", rows[limit-1].ID)
	}
	for _, agent := range rows {
		if agent.GroupType == AgentGroupTypeCloudClone {
			page.Groups[0].Items = append(page.Groups[0].Items, agent)
			continue
		}
		page.Groups[1].Items = append(page.Groups[1].Items, agent)
	}
	return page, nil
}

func sessionRequestHash(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func (s *Service) admitContainerMembersTx(tx *dbr.Tx, groupNo, spaceID, userUID, botID string) (bool, error) {
	return group.AdmitAITeamContainerMembersTx(s.ctx, tx, groupNo, spaceID, userUID, botID)
}

func (s *Service) CreateSession(spaceID, userUID, botID, idempotencyKey, name string) (*Session, error) {
	botName, err := s.validateAuthority(spaceID, userUID, botID)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = aiteampkg.DefaultSessionName
	}
	requestHash := sessionRequestHash(name)

	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	if err := validateAuthorityTx(tx, spaceID, userUID, botID); err != nil {
		return nil, err
	}

	var agent *lockedAgent
	_, err = tx.SelectBySql(`SELECT id,IFNULL(group_no,'') AS group_no,container_state FROM ai_team_agent
		WHERE space_id=? AND user_uid=? AND bot_id=? AND is_added=1 FOR UPDATE`, spaceID, userUID, botID).Load(&agent)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, errNotFound
	}

	groupNo := agent.GroupNo
	if groupNo == "" {
		groupNo = util.GenerUUID()
		groupName := strings.TrimSpace(botName)
		if groupName == "" {
			groupName = botID
		}
		groupName += " · AI"
		if r := []rune(groupName); len(r) > group.MaxGroupNameLen {
			groupName = string(r[:group.MaxGroupNameLen])
		}
		version, genErr := s.ctx.GenSeq(common.GroupSeqKey)
		if genErr != nil {
			return nil, genErr
		}
		_, err = tx.InsertBySql("INSERT INTO `group` (group_no,name,creator,status,version,allow_view_history_msg,space_id,allow_external,allow_no_mention,purpose) VALUES (?,?,?,?,?,?,?,?,?,?)",
			groupNo, groupName, userUID, group.GroupStatusNormal, version, 1, spaceID, 0, 1, aiteampkg.GroupPurpose).Exec()
		if err != nil {
			return nil, err
		}
		_, err = tx.Update("ai_team_agent").Set("group_no", groupNo).Set("container_state", containerProvisioning).Where("id=?", agent.ID).Exec()
		if err != nil {
			return nil, err
		}
		agent.ContainerState = containerProvisioning
	}

	membershipRepaired, err := s.admitContainerMembersTx(tx, groupNo, spaceID, userUID, botID)
	if err != nil {
		return nil, err
	}
	containerNeedsReconcile := agent.ContainerState != containerReady || membershipRepaired
	if membershipRepaired && agent.ContainerState == containerReady {
		if _, err = tx.Update("ai_team_agent").Set("container_state", containerProvisioning).Where("id=?", agent.ID).Exec(); err != nil {
			return nil, err
		}
	}

	var existing *Session
	_, err = tx.SelectBySql(`SELECT ats.id,ats.agent_id,ats.short_id,ats.request_hash,ats.state,
		ats.manual_title,t.name,t.status,t.message_count,t.last_message_content,t.last_message_at,
		t.created_at,t.updated_at,? AS group_no
		FROM ai_team_session ats JOIN thread t ON t.short_id=ats.short_id
		WHERE ats.agent_id=? AND ats.idempotency_key=? LIMIT 1 FOR UPDATE`, groupNo, agent.ID, idempotencyKey).Load(&existing)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.RequestHash != requestHash {
		return nil, errIdempotencyConflict
	}
	// A deleted thread remains part of the durable idempotency ledger. Reusing
	// its key must fail explicitly instead of falling through to GetSession and
	// turning a create replay into a permanent not-found response.
	if existing != nil && existing.Status == thread.ThreadStatusDeleted {
		return nil, errIdempotencyConflict
	}
	if existing == nil {
		shortID := fmt.Sprintf("%d", s.ctx.UserIDGen.Generate().Int64())
		version, genErr := s.ctx.GenSeq(thread.ThreadSeqKey)
		if genErr != nil {
			return nil, genErr
		}
		result, insertErr := tx.InsertBySql("INSERT INTO thread (short_id,group_no,name,creator_uid,status,version) VALUES (?,?,?,?,?,?)",
			shortID, groupNo, name, userUID, thread.ThreadStatusActive, version).Exec()
		if insertErr != nil {
			return nil, insertErr
		}
		threadID, insertErr := result.LastInsertId()
		if insertErr != nil {
			return nil, insertErr
		}
		memberVersion, genErr := s.ctx.GenSeq(thread.ThreadSeqKey)
		if genErr != nil {
			return nil, genErr
		}
		_, err = tx.InsertBySql("INSERT INTO thread_member (thread_id,uid,role,version) VALUES (?,?,?,?)", threadID, userUID, thread.MemberRoleCreator, memberVersion).Exec()
		if err != nil {
			return nil, err
		}
		_, err = tx.InsertBySql("INSERT INTO ai_team_session (agent_id,short_id,idempotency_key,request_hash,state) VALUES (?,?,?,?,?)",
			agent.ID, shortID, idempotencyKey, requestHash, sessionProvisioning).Exec()
		if err != nil {
			return nil, err
		}
		existing = &Session{AgentID: agent.ID, ShortID: shortID, GroupNo: groupNo, Name: name, Status: thread.ThreadStatusActive, State: sessionProvisioning}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}

	if existing.State != sessionReady || containerNeedsReconcile {
		if containerNeedsReconcile {
			err = s.ensureContainerIMReady(agent.ID, groupNo, userUID, botID)
		} else {
			err = s.ensureIMReady(groupNo, existing.ShortID, userUID, botID)
		}
		if err != nil {
			s.markProvisionFailure(agent.ID, existing.ShortID, err)
			return nil, fmt.Errorf("%w: provision IM channels: %v", errIMUnavailable, err)
		}
		readyTx, beginErr := s.ctx.DB().Begin()
		if beginErr != nil {
			return nil, beginErr
		}
		defer readyTx.RollbackUnlessCommitted()
		if err = validateAuthorityTx(readyTx, spaceID, userUID, botID); err != nil {
			return nil, err
		}
		if _, err = readyTx.Update("ai_team_session").Set("state", sessionReady).Set("last_error", "").Where("agent_id=? AND short_id=?", agent.ID, existing.ShortID).Exec(); err != nil {
			return nil, err
		}
		if _, err = readyTx.Update("ai_team_agent").Set("container_state", containerReady).Where("id=?", agent.ID).Exec(); err != nil {
			return nil, err
		}
		if err = readyTx.Commit(); err != nil {
			return nil, err
		}
	}
	return s.GetSession(spaceID, userUID, existing.ShortID)
}

func (s *Service) ensureIMReady(groupNo, shortID, userUID, botID string) error {
	subscribers := []string{userUID, botID}
	if err := s.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(), Subscribers: subscribers}); err != nil {
		return err
	}
	return s.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{ChannelID: thread.BuildChannelID(groupNo, shortID), ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Subscribers: subscribers})
}

func (s *Service) ensureContainerIMReady(agentID int64, groupNo, userUID, botID string) error {
	subscribers := []string{userUID, botID}
	if err := s.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
		ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(), Subscribers: subscribers,
	}); err != nil {
		return err
	}
	var shortIDs []string
	_, err := s.ctx.DB().SelectBySql(`SELECT ats.short_id FROM ai_team_session ats
		JOIN thread t ON t.short_id=ats.short_id AND t.group_no=? AND t.status<>3
		WHERE ats.agent_id=?`, groupNo, agentID).Load(&shortIDs)
	if err != nil {
		return err
	}
	for _, shortID := range shortIDs {
		if err := s.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
			ChannelID: thread.BuildChannelID(groupNo, shortID), ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Subscribers: subscribers,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) markProvisionFailure(agentID int64, shortID string, cause error) {
	reason := cause.Error()
	if runes := []rune(reason); len(runes) > 255 {
		reason = string(runes[:255])
	}
	if _, err := s.ctx.DB().Update("ai_team_session").Set("state", sessionFailed).Set("last_error", reason).
		Where("agent_id=? AND short_id=? AND state=?", agentID, shortID, sessionProvisioning).Exec(); err != nil {
		s.Error("mark AI session provisioning failure", zap.Error(err), zap.String("short_id", shortID))
	}
	s.markContainerFailure(agentID, cause)
}

func (s *Service) markContainerFailure(agentID int64, cause error) {
	if _, err := s.ctx.DB().Update("ai_team_agent").Set("container_state", containerFailed).
		Where("id=? AND container_state<>?", agentID, containerReady).Exec(); err != nil {
		s.Error("mark AI container provisioning failure", zap.Error(err), zap.Int64("agent_id", agentID), zap.NamedError("cause", cause))
	}
}

func (s *Service) GetSession(spaceID, userUID, shortID string) (*Session, error) {
	// is_added is intentionally absent: removing an AI only hides it from the
	// picker/list. A caller who already has an authorized session may continue,
	// rename or archive it while the Bot and both Space seats remain active.
	var out *Session
	_, err := s.ctx.DB().SelectBySql(`SELECT ats.id,ats.agent_id,ats.short_id,ats.state,ats.manual_title,
		t.group_no,t.name,t.status,t.message_count,t.last_message_content,t.last_message_at,t.created_at,t.updated_at,
		IFNULL(ts.mute,0) AS mute,EXISTS(SELECT 1 FROM user_pinned_channel upc
			WHERE upc.uid=a.user_uid AND upc.space_id=a.space_id
			AND upc.channel_id=CONCAT(t.group_no,'____',t.short_id) AND upc.channel_type=?) AS is_pinned
		FROM ai_team_session ats
		JOIN ai_team_agent a ON a.id=ats.agent_id
		JOIN thread t ON t.short_id=ats.short_id AND t.group_no=a.group_no
		LEFT JOIN thread_setting ts ON ts.group_no=t.group_no AND ts.short_id=t.short_id AND ts.uid=a.user_uid
		JOIN robot r ON r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci AND r.status=1 AND r.creator_uid=a.user_uid COLLATE utf8mb4_0900_ai_ci
		JOIN user bot_u ON bot_u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_u.status=1 AND bot_u.is_destroy<>2
		JOIN user human_u ON human_u.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_u.status=1 AND human_u.is_destroy<>2
		JOIN space sp ON sp.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND human_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.status=1
		WHERE a.space_id=? AND a.user_uid=? AND ats.short_id=? AND ats.state=2 AND t.status<>3 LIMIT 1`,
		common.ChannelTypeCommunityTopic.Uint8(), spaceID, userUID, shortID).Load(&out)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errNotFound
	}
	out.ChannelID = thread.BuildChannelID(out.GroupNo, out.ShortID)
	return out, nil
}

func (s *Service) ListSessions(spaceID, userUID, botID string, statuses []int, pageIndex, pageSize int) (*SessionPage, error) {
	if _, err := s.getAgent(spaceID, userUID, botID, true); err != nil {
		return nil, err
	}
	if pageIndex < 1 {
		pageIndex = 1
	}
	if pageSize <= 0 || pageSize > maxPageSize {
		pageSize = defaultPageSize
	}
	if len(statuses) == 0 {
		statuses = []int{thread.ThreadStatusActive}
	}
	rows := make([]*Session, 0)
	_, err := s.ctx.DB().Select(
		"ats.id", "ats.agent_id", "ats.short_id", "ats.state", "ats.manual_title",
		"t.group_no", "t.name", "t.status", "t.message_count", "t.last_message_content",
		"t.last_message_at", "t.created_at", "t.updated_at",
		"IFNULL(ts.mute,0) AS mute",
		fmt.Sprintf("EXISTS(SELECT 1 FROM user_pinned_channel upc WHERE upc.uid=a.user_uid AND upc.space_id=a.space_id AND upc.channel_id=CONCAT(t.group_no,'____',t.short_id) AND upc.channel_type=%d) AS is_pinned", common.ChannelTypeCommunityTopic.Uint8()),
	).From(dbr.I("ai_team_session").As("ats")).
		Join(dbr.I("ai_team_agent").As("a"), "a.id=ats.agent_id").
		Join(dbr.I("thread").As("t"), "t.short_id=ats.short_id AND t.group_no=a.group_no").
		LeftJoin(dbr.I("thread_setting").As("ts"), "ts.group_no=t.group_no AND ts.short_id=t.short_id AND ts.uid=a.user_uid").
		Where("a.space_id=? AND a.user_uid=? AND a.bot_id=? AND a.is_added=1", spaceID, userUID, botID).
		Where("ats.state=2 AND t.status IN ?", statuses).
		OrderBy("is_pinned DESC,COALESCE(t.last_message_at,t.created_at) DESC,t.id DESC").
		Limit(uint64(pageSize + 1)).Offset(uint64(pageIndex-1) * uint64(pageSize)).Load(&rows)
	if err != nil {
		return nil, err
	}
	page := &SessionPage{Items: rows, PageIndex: pageIndex, PageSize: pageSize}
	if len(rows) > pageSize {
		page.HasMore = true
		page.Items = rows[:pageSize]
	}
	for _, row := range page.Items {
		row.ChannelID = thread.BuildChannelID(row.GroupNo, row.ShortID)
	}
	return page, nil
}

func (s *Service) RenameSession(spaceID, userUID, shortID, name string) error {
	current, err := s.GetSession(spaceID, userUID, shortID)
	if err != nil {
		return err
	}
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.RollbackUnlessCommitted()
	var groupNo string
	count, err := tx.SelectBySql(`SELECT IFNULL(group_no,'') FROM ai_team_agent
		WHERE id=? AND space_id=? AND user_uid=? FOR UPDATE`, current.AgentID, spaceID, userUID).Load(&groupNo)
	if err != nil {
		return err
	}
	if count != 1 || groupNo == "" {
		return errNotFound
	}
	var sessionID int64
	count, err = tx.SelectBySql(`SELECT id FROM ai_team_session
		WHERE id=? AND agent_id=? AND short_id=? AND state=? FOR UPDATE`,
		current.ID, current.AgentID, shortID, sessionReady).Load(&sessionID)
	if err != nil {
		return err
	}
	if count != 1 {
		return errNotFound
	}
	var threadID int64
	count, err = tx.SelectBySql(`SELECT id FROM thread
		WHERE group_no=? AND short_id=? AND status<>3 FOR UPDATE`, groupNo, shortID).Load(&threadID)
	if err != nil {
		return err
	}
	if count != 1 {
		return errNotFound
	}
	if _, err = tx.Update("thread").Set("name", name).Where("id=?", threadID).Exec(); err != nil {
		return err
	}
	if _, err = tx.Update("ai_team_session").Set("manual_title", 1).Where("id=?", sessionID).Exec(); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) ArchiveSession(spaceID, userUID, shortID string, archive bool) error {
	session, err := s.GetSession(spaceID, userUID, shortID)
	if err != nil {
		return err
	}
	if archive {
		return s.threadService.ArchiveThread(session.GroupNo, shortID, userUID)
	}
	return s.threadService.UnarchiveThread(session.GroupNo, shortID, userUID)
}

func (s *Service) UpdateSessionSetting(spaceID, userUID, shortID string, settings map[string]interface{}) error {
	session, err := s.GetSession(spaceID, userUID, shortID)
	if err != nil {
		return err
	}
	return s.threadService.UpdateSetting(session.GroupNo, shortID, userUID, settings)
}

// DeleteSession applies the existing thread soft-delete contract: the thread is
// hidden and its IM channel is banned, while persisted history is not physically
// erased. Ownership is resolved from the AI association before the thread service
// performs its own parent membership and creator checks.
func (s *Service) DeleteSession(spaceID, userUID, shortID string) error {
	session, err := s.GetSession(spaceID, userUID, shortID)
	if err != nil {
		return err
	}
	return s.threadService.DeleteThread(session.GroupNo, shortID, userUID)
}

func normalizeStatuses(value string) ([]int, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "active":
		return []int{thread.ThreadStatusActive}, nil
	case "archived":
		return []int{thread.ThreadStatusArchived}, nil
	case "all":
		return []int{thread.ThreadStatusActive, thread.ThreadStatusArchived}, nil
	default:
		return nil, errors.New("invalid status")
	}
}
