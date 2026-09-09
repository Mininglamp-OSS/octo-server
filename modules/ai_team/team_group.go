package ai_team

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/gocraft/dbr/v2"
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
		Name        string `db:"name"`
		SpaceID     string `db:"group_space_id"`
		Creator     string `db:"creator"`
		Purpose     string `db:"purpose"`
		GroupStatus int    `db:"group_status"`
		ProjectID   string `db:"project_id"`
	}
	rows := make([]*teamRow, 0, 2)
	_, err = s.ctx.DB().SelectBySql(`SELECT group_no,name,space_id AS group_space_id,creator,purpose,status AS group_status,project_id
		FROM `+"`group`"+`
		WHERE space_id=? AND creator=? AND purpose=? AND status=?
		ORDER BY group_no LIMIT 2`, spaceID, userUID, aiteampkg.TeamGroupPurpose, group.GroupStatusNormal).Load(&rows)
	if err != nil {
		return false, nil, err
	}
	if len(rows) == 0 {
		return len(desiredBots) == 0, nil, nil
	}
	if len(rows) != 1 {
		return false, nil, nil
	}
	row := rows[0]
	if row.GroupNo == "" || row.SpaceID != spaceID ||
		row.Creator != userUID || row.Purpose != aiteampkg.TeamGroupPurpose ||
		row.GroupStatus != group.GroupStatusNormal || row.ProjectID != "" || row.Name != aiteampkg.TeamGroupName {
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
	return true, &TeamGroup{GroupNo: row.GroupNo, Name: aiteampkg.TeamGroupName, State: containerReady}, nil
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

	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	// The active owner's Space membership is the stable existing row used to
	// serialize first creation. This gives (space, owner) one automatic group
	// without introducing a second group registry table.
	var lockedOwner string
	count, err := tx.SelectBySql(`SELECT sm.uid FROM space_member sm
		JOIN space sp ON sp.space_id=sm.space_id AND sp.status=1
		JOIN user u ON u.uid=sm.uid AND u.status=1 AND u.is_destroy<>2
		WHERE sm.space_id=? AND sm.uid=? AND sm.status=1
		LIMIT 1 FOR UPDATE OF sm`, spaceID, userUID).Load(&lockedOwner)
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, errForbidden
	}

	var groups []*struct {
		GroupNo string `db:"group_no"`
		Name    string `db:"name"`
	}
	_, err = tx.SelectBySql(`SELECT group_no,name FROM `+"`group`"+`
		WHERE space_id=? AND creator=? AND purpose=? AND status=?
		ORDER BY group_no LIMIT 2 FOR UPDATE`, spaceID, userUID, aiteampkg.TeamGroupPurpose, group.GroupStatusNormal).Load(&groups)
	if err != nil {
		return nil, err
	}
	if len(groups) > 1 {
		return nil, fmt.Errorf("multiple automatic AI-team groups for space %s owner %s", spaceID, userUID)
	}
	if len(botIDs) == 0 && len(groups) == 0 {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}

	// Re-read and lock the authoritative Agent rows after taking the owner and
	// group locks. A concurrent activation/removal cannot publish an older
	// roster after the newer mutation.
	botIDs, err = desiredTeamBotIDsTx(tx, spaceID, userUID)
	if err != nil {
		return nil, err
	}

	created := false
	nameChanged := false
	groupNo := ""
	if len(groups) == 0 {
		groupNo = util.GenerUUID()
		version, genErr := s.ctx.GenSeq(common.GroupSeqKey)
		if genErr != nil {
			return nil, genErr
		}
		if _, err = tx.InsertBySql(`INSERT INTO `+"`group`"+`
			(group_no,name,creator,status,version,allow_view_history_msg,space_id,allow_external,allow_no_mention,purpose)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, groupNo, aiteampkg.TeamGroupName, userUID,
			group.GroupStatusNormal, version, 1, spaceID, 0, 1, aiteampkg.TeamGroupPurpose).Exec(); err != nil {
			return nil, err
		}
		created = true
	} else {
		groupNo = groups[0].GroupNo
		if groups[0].Name != aiteampkg.TeamGroupName {
			version, genErr := s.ctx.GenSeq(common.GroupSeqKey)
			if genErr != nil {
				return nil, genErr
			}
			if _, err = tx.Update("group").Set("name", aiteampkg.TeamGroupName).Set("version", version).
				Where("group_no=? AND purpose=?", groupNo, aiteampkg.TeamGroupPurpose).Exec(); err != nil {
				return nil, err
			}
			nameChanged = true
		}
	}

	changed, err := group.SyncAITeamGroupMembersTx(s.ctx, tx, groupNo, spaceID, userUID, botIDs)
	if err != nil {
		return nil, err
	}
	needsIM := forceIM || created || changed
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if nameChanged {
		s.ctx.SendChannelUpdateToGroup(groupNo)
	}

	if needsIM {
		if err = s.syncTeamGroupProjection(spaceID, userUID, groupNo); err != nil {
			return nil, err
		}
	}
	return &TeamGroup{GroupNo: groupNo, Name: aiteampkg.TeamGroupName, State: containerReady}, nil
}

const teamProjectionMaxSnapshots = 3

type teamProjectionSnapshot struct {
	desired       []string
	excluded      []string
	shortIDs      []string
	rosterVersion uint64
}

// markLifecycleRosterRemovalTx folds account/seat teardown into the Agent
// authority before the protected membership removal commits. group_member is
// the aggregate roster authority, so there is no second team record to update.
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
	case aiteampkg.CustomTeamPurpose:
		// group_member is the custom team's roster authority. The lifecycle
		// removal already mutates that row in this transaction, so there is no
		// second AI-team membership record to maintain.
		return nil
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

// syncTeamGroupProjection never holds a transaction or pooled DB connection
// across WuKongIM HTTP. The snapshot version is derived from group_member. If
// a newer roster commits while HTTP is in flight, the latest snapshot is
// replayed so a stale projection cannot permanently overwrite a later change.
func (s *Service) syncTeamGroupProjection(spaceID, userUID, groupNo string) error {
	for attempt := 0; attempt < teamProjectionMaxSnapshots; attempt++ {
		snapshot, err := s.prepareTeamProjectionSnapshot(spaceID, userUID, groupNo)
		if err != nil {
			return err
		}
		if err = group.SyncAITeamGroupSubscribers(
			s.ctx, groupNo, spaceID, snapshot.shortIDs, snapshot.desired, snapshot.excluded,
		); err != nil {
			return fmt.Errorf("%w: provision AI-team group subscribers: %v", errIMUnavailable, err)
		}
		current, err := s.teamProjectionCurrent(
			spaceID, userUID, groupNo, snapshot.rosterVersion, snapshot.desired,
		)
		if err != nil {
			return err
		}
		if current {
			return nil
		}
	}
	return fmt.Errorf("%w: AI-team group projection roster did not stabilize", errIMUnavailable)
}

func (s *Service) prepareTeamProjectionSnapshot(spaceID, userUID, groupNo string) (*teamProjectionSnapshot, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	var locked string
	count, err := tx.SelectBySql(`SELECT group_no FROM `+"`group`"+`
		WHERE group_no=? AND space_id=? AND creator=? AND purpose=? AND status=?
		LIMIT 1 FOR UPDATE`, groupNo, spaceID, userUID, aiteampkg.TeamGroupPurpose, group.GroupStatusNormal).Load(&locked)
	if err != nil {
		return nil, err
	}
	if count != 1 || locked != groupNo {
		return nil, fmt.Errorf("%w: AI-team group projection target changed", errIMUnavailable)
	}
	version, err := customTeamRosterVersionTx(tx, groupNo)
	if err != nil {
		return nil, err
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
		desired: desired, excluded: excluded, shortIDs: shortIDs, rosterVersion: version,
	}, nil
}

func (s *Service) teamProjectionCurrent(
	spaceID, userUID, groupNo string,
	expectedVersion uint64,
	expectedDesired []string,
) (bool, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return false, err
	}
	defer tx.RollbackUnlessCommitted()
	var locked string
	count, err := tx.SelectBySql(`SELECT group_no FROM `+"`group`"+`
		WHERE group_no=? AND space_id=? AND creator=? AND purpose=? AND status=?
		LIMIT 1 FOR UPDATE`, groupNo, spaceID, userUID, aiteampkg.TeamGroupPurpose, group.GroupStatusNormal).Load(&locked)
	if err != nil {
		return false, err
	}
	if count != 1 || locked != groupNo {
		return false, nil
	}
	version, err := customTeamRosterVersionTx(tx, groupNo)
	if err != nil {
		return false, err
	}
	if version != expectedVersion {
		return false, nil
	}
	currentDesired, err := desiredTeamRosterTx(tx, spaceID, userUID)
	if err != nil {
		return false, err
	}
	if !sameStringSet(currentDesired, expectedDesired) {
		return false, nil
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
	_, err := s.ctx.DB().Select("group_no", "name").From(dbr.I("group")).
		Where("space_id=? AND creator=? AND purpose=? AND status=?", spaceID, userUID, aiteampkg.TeamGroupPurpose, group.GroupStatusNormal).
		OrderAsc("group_no").Limit(1).Load(&row)
	if err != nil || row == nil {
		return row, err
	}
	row.Name = aiteampkg.TeamGroupName
	row.State = containerReady
	return row, nil
}
