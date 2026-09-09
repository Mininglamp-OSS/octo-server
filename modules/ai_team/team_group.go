package ai_team

import (
	"fmt"
	"strings"
	"time"

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
	if len(missing) == 0 {
		converged, teamGroup, checkErr := s.teamProjectionConverged(spaceID, userUID)
		if checkErr != nil {
			return nil, checkErr
		}
		if converged {
			return teamGroup, nil
		}
	}
	justBackfilled := make(map[string]struct{}, len(missing))
	for _, botID := range missing {
		if _, err = s.ensureAgentContainer(spaceID, userUID, botID, false); err != nil {
			return nil, err
		}
		justBackfilled[botID] = struct{}{}
	}
	if err = s.reconcileActiveAgentContainersExcept(spaceID, userUID, justBackfilled); err != nil {
		return nil, err
	}
	return s.ensureTeamGroup(spaceID, userUID, false)
}

// teamProjectionConverged is the steady-state read guard. It verifies both the
// private containers and the aggregate roster without opening a write
// transaction, so an ordinary AI Team page load does not take FOR UPDATE locks
// or touch WuKongIM when persisted state is already complete.
func (s *Service) teamProjectionConverged(spaceID, userUID string) (bool, *TeamGroup, error) {
	var brokenContainers int
	err := s.ctx.DB().SelectBySql(`SELECT COUNT(*)
		FROM ai_team_agent a
		JOIN robot r ON r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci
			AND r.status=1 AND r.creator_uid=a.user_uid COLLATE utf8mb4_0900_ai_ci
		JOIN user u ON u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND u.status=1 AND u.is_destroy<>2
		JOIN space sp ON sp.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci
			AND human_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci
			AND bot_sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.status=1
		LEFT JOIN `+"`group`"+` g ON g.group_no=a.group_no COLLATE utf8mb4_0900_ai_ci
		WHERE a.space_id=? AND a.user_uid=? AND a.is_added=1 AND (
			a.group_no IS NULL OR a.group_no='' OR a.container_state<>? OR
			g.group_no IS NULL OR g.space_id<>a.space_id COLLATE utf8mb4_0900_ai_ci OR
			g.creator<>a.user_uid COLLATE utf8mb4_0900_ai_ci OR g.purpose<>? OR g.status<>? OR
			(SELECT COUNT(*) FROM group_member gm
				WHERE gm.group_no=a.group_no COLLATE utf8mb4_0900_ai_ci
					AND gm.is_deleted=0 AND gm.status=1)<>2 OR
			NOT EXISTS (SELECT 1 FROM group_member owner_gm
				WHERE owner_gm.group_no=a.group_no COLLATE utf8mb4_0900_ai_ci
					AND owner_gm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci
					AND owner_gm.is_deleted=0 AND owner_gm.status=1
					AND owner_gm.role=? AND owner_gm.robot=0) OR
			NOT EXISTS (SELECT 1 FROM group_member bot_gm
				WHERE bot_gm.group_no=a.group_no COLLATE utf8mb4_0900_ai_ci
					AND bot_gm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci
					AND bot_gm.is_deleted=0 AND bot_gm.status=1
					AND bot_gm.role=? AND bot_gm.robot=1)
		)`, spaceID, userUID, containerReady, aiteampkg.GroupPurpose,
		group.GroupStatusNormal, group.MemberRoleCreator, group.MemberRoleCommon).LoadOne(&brokenContainers)
	if err != nil {
		return false, nil, err
	}
	if brokenContainers != 0 {
		return false, nil, nil
	}

	desiredBots, err := s.desiredTeamBotIDs(spaceID, userUID)
	if err != nil {
		return false, nil, err
	}
	type teamRow struct {
		GroupNo     string `db:"group_no"`
		State       int    `db:"state"`
		SpaceID     string `db:"group_space_id"`
		Creator     string `db:"creator"`
		Purpose     string `db:"purpose"`
		GroupStatus int    `db:"group_status"`
		ProjectID   string `db:"project_id"`
	}
	var row *teamRow
	_, err = s.ctx.DB().SelectBySql(`SELECT IFNULL(atg.group_no,'') AS group_no,atg.state,
		IFNULL(g.space_id,'') AS group_space_id,IFNULL(g.creator,'') AS creator,
		IFNULL(g.purpose,'') AS purpose,IFNULL(g.status,0) AS group_status,
		IFNULL(g.project_id,'') AS project_id
		FROM ai_team_group atg
		LEFT JOIN `+"`group`"+` g ON g.group_no=atg.group_no COLLATE utf8mb4_0900_ai_ci
		WHERE atg.space_id=? AND atg.user_uid=?`, spaceID, userUID).Load(&row)
	if err != nil {
		return false, nil, err
	}
	if row == nil {
		return len(desiredBots) == 0, nil, nil
	}
	if row.GroupNo == "" || row.State != containerReady || row.SpaceID != spaceID ||
		row.Creator != userUID || row.Purpose != aiteampkg.TeamGroupPurpose ||
		row.GroupStatus != group.GroupStatusNormal || row.ProjectID != "" {
		return false, nil, nil
	}

	var members []*struct {
		UID   string `db:"uid"`
		Role  int    `db:"role"`
		Robot int    `db:"robot"`
	}
	_, err = s.ctx.DB().Select("uid", "role", "robot").From("group_member").
		Where("group_no=? AND is_deleted=0 AND status=1", row.GroupNo).Load(&members)
	if err != nil {
		return false, nil, err
	}
	if len(members) != len(desiredBots)+1 {
		return false, nil, nil
	}
	wanted := make(map[string]int, len(desiredBots)+1)
	wanted[userUID] = group.MemberRoleCreator
	for _, botID := range desiredBots {
		wanted[botID] = group.MemberRoleCommon
	}
	for _, member := range members {
		role, ok := wanted[member.UID]
		if !ok || member.Role != role || member.Robot != boolInt(member.UID != userUID) {
			return false, nil, nil
		}
	}
	return true, &TeamGroup{GroupNo: row.GroupNo, Name: aiteampkg.TeamGroupName, State: row.State}, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
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
		LEFT JOIN ai_team_agent a ON a.space_id=sp.space_id COLLATE utf8mb4_0900_ai_ci
			AND a.user_uid=r.creator_uid COLLATE utf8mb4_0900_ai_ci
			AND a.bot_id=r.robot_id COLLATE utf8mb4_0900_ai_ci
		WHERE r.creator_uid=? AND r.status=1 AND a.id IS NULL
		ORDER BY r.robot_id`, spaceID, userUID, userUID).Load(&botIDs)
	return botIDs, err
}

func (s *Service) reconcileActiveAgentContainers(spaceID, userUID string) error {
	return s.reconcileActiveAgentContainersExcept(spaceID, userUID, nil)
}

func (s *Service) reconcileActiveAgentContainersExcept(spaceID, userUID string, skip map[string]struct{}) error {
	botIDs := make([]string, 0)
	_, err := s.eligibleAgentsQuery(spaceID, userUID, "a.bot_id").OrderAsc("a.bot_id").Load(&botIDs)
	if err != nil {
		return err
	}
	for _, botID := range botIDs {
		if _, ok := skip[botID]; ok {
			continue
		}
		if _, err = s.ensureAgentContainer(spaceID, userUID, botID, false); err != nil {
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
		ORDER BY a.bot_id FOR UPDATE OF a`, spaceID, userUID, containerReady).Load(&botIDs)
	return botIDs, err
}

func desiredTeamRosterTx(tx *dbr.Tx, spaceID, userUID string) ([]string, error) {
	ownerActive, err := group.IsAITeamOwnerActiveTx(tx, spaceID, userUID)
	if err != nil {
		return nil, err
	}
	if !ownerActive {
		return []string{}, nil
	}
	botIDs, err := desiredTeamBotIDsTx(tx, spaceID, userUID)
	if err != nil {
		return nil, err
	}
	desired := make([]string, 0, len(botIDs)+1)
	desired = append(desired, userUID)
	desired = append(desired, botIDs...)
	return desired, nil
}

func (s *Service) ensureTeamGroup(spaceID, userUID string, forceIM bool) (*TeamGroup, error) {
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
		GroupNo       string     `db:"group_no"`
		State         int        `db:"state"`
		RosterVersion uint64     `db:"roster_version"`
		RetryAfter    *time.Time `db:"retry_after"`
	}
	count, err := tx.SelectBySql(`SELECT IFNULL(group_no,'') AS group_no,state,roster_version,retry_after
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
	cooldownActive := !forceIM && !created && !changed &&
		(row.State == containerProvisioning || row.State == containerFailed) &&
		row.RetryAfter != nil && row.RetryAfter.After(time.Now())
	needsIM := (forceIM || created || changed || row.State != containerReady) && !cooldownActive
	if needsIM {
		if _, err = tx.UpdateBySql(`UPDATE ai_team_group
			SET state=?,last_error='',roster_version=roster_version+1,
				retry_after=DATE_ADD(CURRENT_TIMESTAMP, INTERVAL ? SECOND)
			WHERE space_id=? AND user_uid=?`, containerProvisioning,
			teamProjectionRetryDelaySeconds, spaceID, userUID).Exec(); err != nil {
			return nil, err
		}
		row.State = containerProvisioning
		row.RosterVersion++
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}

	if needsIM {
		if err = s.syncTeamGroupProjection(spaceID, userUID, row.GroupNo, row.RosterVersion); err != nil {
			return nil, err
		}
		row.State = containerReady
	}
	return &TeamGroup{GroupNo: row.GroupNo, Name: aiteampkg.TeamGroupName, State: row.State}, nil
}

const (
	teamProjectionMaxSnapshots      = 3
	teamProjectionRetryDelaySeconds = 30
)

type teamProjectionSnapshot struct {
	desired       []string
	excluded      []string
	shortIDs      []string
	rosterVersion uint64
}

// markLifecycleRosterRemovalTx folds account/seat teardown into the Agent
// authority before the protected membership removal commits. The aggregate
// version is advanced first to preserve the projection lock order
// (ai_team_group -> ai_team_agent -> group_member).
func markLifecycleRosterRemovalTx(
	tx *dbr.Tx,
	purpose, groupNo, spaceID, ownerUID string,
	removedUIDs []string,
) error {
	if tx == nil || len(removedUIDs) == 0 {
		return nil
	}
	type ownerKey struct {
		SpaceID string `db:"space_id"`
		UserUID string `db:"user_uid"`
	}
	var owners []*ownerKey
	switch purpose {
	case aiteampkg.TeamGroupPurpose:
		owners = append(owners, &ownerKey{SpaceID: spaceID, UserUID: ownerUID})
	case aiteampkg.GroupPurpose:
		for _, uid := range removedUIDs {
			if uid == ownerUID {
				owners = append(owners, &ownerKey{SpaceID: spaceID, UserUID: ownerUID})
				break
			}
		}
		if _, err := tx.Select("DISTINCT space_id", "user_uid").From("ai_team_agent").
			Where("group_no=? AND bot_id IN ?", groupNo, removedUIDs).Load(&owners); err != nil {
			return fmt.Errorf("query affected AI-team owners: %w", err)
		}
	default:
		return nil
	}
	seenOwners := make(map[ownerKey]struct{}, len(owners))
	for _, owner := range owners {
		key := ownerKey{SpaceID: owner.SpaceID, UserUID: owner.UserUID}
		if _, seen := seenOwners[key]; seen {
			continue
		}
		seenOwners[key] = struct{}{}
		if _, err := tx.UpdateBySql(`UPDATE ai_team_group
			SET state=?,last_error='',roster_version=roster_version+1,retry_after=CURRENT_TIMESTAMP
			WHERE space_id=? AND user_uid=?`, containerProvisioning, owner.SpaceID, owner.UserUID).Exec(); err != nil {
			return fmt.Errorf("advance AI-team roster version after lifecycle removal: %w", err)
		}
		for _, uid := range removedUIDs {
			if uid != owner.UserUID {
				continue
			}
			if _, err := tx.Update("ai_team_agent").Set("container_state", containerProvisioning).
				Where("space_id=? AND user_uid=? AND is_added=1", owner.SpaceID, owner.UserUID).Exec(); err != nil {
				return fmt.Errorf("mark owner-revoked AI-team containers pending: %w", err)
			}
			break
		}
		if _, err := tx.Update("ai_team_agent").Set("is_added", 0).
			Where("space_id=? AND user_uid=? AND bot_id IN ?", owner.SpaceID, owner.UserUID, removedUIDs).Exec(); err != nil {
			return fmt.Errorf("deactivate lifecycle-removed AI-team agents: %w", err)
		}
	}
	return nil
}

// syncTeamGroupProjection never holds a transaction, pooled DB connection, or
// MySQL named lock across WuKongIM HTTP. The snapshot carries a monotonic roster
// version. If a newer roster commits while HTTP is in flight, the ready CAS
// fails and the current snapshot is replayed, so a stale projection cannot be
// declared converged or permanently overwrite a later removal.
func (s *Service) syncTeamGroupProjection(
	spaceID, userUID, groupNo string,
	requestedVersion uint64,
) error {
	failureVersion := requestedVersion
	for attempt := 0; attempt < teamProjectionMaxSnapshots; attempt++ {
		snapshot, err := s.prepareTeamProjectionSnapshot(spaceID, userUID, groupNo)
		if err != nil {
			s.markTeamGroupFailure(spaceID, userUID, groupNo, failureVersion, err)
			return err
		}
		failureVersion = snapshot.rosterVersion
		if err = group.SyncAITeamGroupSubscribers(
			s.ctx, groupNo, spaceID, snapshot.shortIDs, snapshot.desired, snapshot.excluded,
		); err != nil {
			wrapped := fmt.Errorf("%w: provision AI-team group subscribers: %v", errIMUnavailable, err)
			s.markTeamGroupFailure(spaceID, userUID, groupNo, failureVersion, wrapped)
			return wrapped
		}
		current, err := s.markTeamProjectionReadyIfCurrent(
			spaceID, userUID, groupNo, snapshot.rosterVersion, snapshot.desired,
		)
		if err != nil {
			s.markTeamGroupFailure(spaceID, userUID, groupNo, failureVersion, err)
			return err
		}
		if current {
			return nil
		}
	}
	err := fmt.Errorf("%w: AI-team group projection roster did not stabilize", errIMUnavailable)
	s.markTeamGroupFailure(spaceID, userUID, groupNo, failureVersion, err)
	return err
}

func (s *Service) prepareTeamProjectionSnapshot(spaceID, userUID, groupNo string) (*teamProjectionSnapshot, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	var locked struct {
		GroupNo       string `db:"group_no"`
		RosterVersion uint64 `db:"roster_version"`
	}
	count, err := tx.SelectBySql(`SELECT IFNULL(group_no,'') AS group_no,roster_version
		FROM ai_team_group WHERE space_id=? AND user_uid=? FOR UPDATE`, spaceID, userUID).Load(&locked)
	if err != nil {
		return nil, err
	}
	if count != 1 || locked.GroupNo == "" || locked.GroupNo != groupNo {
		return nil, fmt.Errorf("%w: AI-team group projection target changed", errIMUnavailable)
	}

	desired, err := desiredTeamRosterTx(tx, spaceID, userUID)
	if err != nil {
		return nil, err
	}
	var knownBots []string
	if _, err = tx.SelectBySql(`SELECT bot_id FROM ai_team_agent
		WHERE space_id=? AND user_uid=? ORDER BY bot_id FOR UPDATE`, spaceID, userUID).Load(&knownBots); err != nil {
		return nil, err
	}
	var historicalMembers []string
	if _, err = tx.SelectBySql(`SELECT uid FROM group_member
		WHERE group_no=? ORDER BY uid FOR UPDATE`, groupNo).Load(&historicalMembers); err != nil {
		return nil, err
	}
	knownBots = append(knownBots, userUID)
	knownBots = append(knownBots, historicalMembers...)
	desiredSet := make(map[string]struct{}, len(desired))
	for _, uid := range desired {
		desiredSet[uid] = struct{}{}
	}
	excludedSet := make(map[string]struct{}, len(knownBots))
	excluded := make([]string, 0, len(knownBots))
	for _, uid := range knownBots {
		if uid == "" {
			continue
		}
		if _, wanted := desiredSet[uid]; wanted {
			continue
		}
		if _, seen := excludedSet[uid]; seen {
			continue
		}
		excludedSet[uid] = struct{}{}
		excluded = append(excluded, uid)
	}
	shortIDs, err := group.PrepareAITeamGroupSubscriberProjectionTx(tx, groupNo, excluded)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &teamProjectionSnapshot{
		desired: desired, excluded: excluded, shortIDs: shortIDs, rosterVersion: locked.RosterVersion,
	}, nil
}

func (s *Service) markTeamProjectionReadyIfCurrent(
	spaceID, userUID, groupNo string,
	expectedVersion uint64,
	expectedDesired []string,
) (bool, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return false, err
	}
	defer tx.RollbackUnlessCommitted()
	var row struct {
		GroupNo       string `db:"group_no"`
		State         int    `db:"state"`
		RosterVersion uint64 `db:"roster_version"`
	}
	count, err := tx.SelectBySql(`SELECT IFNULL(group_no,'') AS group_no,state,roster_version
		FROM ai_team_group WHERE space_id=? AND user_uid=? FOR UPDATE`, spaceID, userUID).Load(&row)
	if err != nil {
		return false, err
	}
	if count != 1 || row.GroupNo != groupNo || row.RosterVersion != expectedVersion {
		return false, nil
	}
	currentDesired, err := desiredTeamRosterTx(tx, spaceID, userUID)
	if err != nil {
		return false, err
	}
	if !sameStringSet(currentDesired, expectedDesired) {
		return false, nil
	}
	if row.State != containerReady {
		if _, err = tx.Update("ai_team_group").
			Set("state", containerReady).Set("last_error", "").Set("retry_after", nil).
			Where("space_id=? AND user_uid=? AND group_no=? AND roster_version=?",
				spaceID, userUID, groupNo, expectedVersion).Exec(); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	wanted := make(map[string]struct{}, len(left))
	for _, value := range left {
		wanted[value] = struct{}{}
	}
	if len(wanted) != len(right) {
		return false
	}
	for _, value := range right {
		if _, ok := wanted[value]; !ok {
			return false
		}
		delete(wanted, value)
	}
	return len(wanted) == 0
}

func (s *Service) loadTeamGroup(spaceID, userUID string) (*TeamGroup, error) {
	var row *TeamGroup
	_, err := s.ctx.DB().Select("group_no", "state").From("ai_team_group").
		Where("space_id=? AND user_uid=? AND group_no<>''", spaceID, userUID).Load(&row)
	if err != nil || row == nil {
		return row, err
	}
	row.Name = aiteampkg.TeamGroupName
	return row, nil
}

func (s *Service) markTeamGroupFailure(
	spaceID, userUID, groupNo string,
	rosterVersion uint64,
	cause error,
) {
	reason := strings.TrimSpace(cause.Error())
	if runes := []rune(reason); len(runes) > 255 {
		reason = string(runes[:255])
	}
	if _, err := s.ctx.DB().UpdateBySql(`UPDATE ai_team_group
		SET state=?,last_error=?,retry_after=DATE_ADD(CURRENT_TIMESTAMP, INTERVAL ? SECOND)
		WHERE space_id=? AND user_uid=? AND group_no=? AND roster_version=? AND state=?`,
		containerFailed, reason, teamProjectionRetryDelaySeconds,
		spaceID, userUID, groupNo, rosterVersion, containerProvisioning).Exec(); err != nil {
		s.Error("mark AI-team group provisioning failure", zap.Error(err), zap.String("space_id", spaceID), zap.String("uid", userUID))
	}
}
