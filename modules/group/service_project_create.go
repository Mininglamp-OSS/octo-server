package group

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

const projectGroupCreateMaxAttempts = 3

// CreateProjectGroup creates a group whose implicit native-member set is the
// current active Project-member snapshot. Explicit request members are admitted
// through the native active-user/Space policy. The Project relation, group row,
// and all initial group_member rows are committed in one group business transaction;
// IM channel creation remains post-commit and uses the existing compensation.
func (s *Service) CreateProjectGroup(req *CreateGroupServiceReq) (*CreateGroupServiceResp, error) {
	if req == nil || strings.TrimSpace(req.Creator) == "" || strings.TrimSpace(req.ProjectID) == "" {
		return nil, errors.New("creator and project_id are required")
	}
	if strings.TrimSpace(req.CategoryID) != "" {
		return nil, errors.New("project groups cannot be assigned to categories")
	}
	creator := strings.TrimSpace(req.Creator)
	projectID := strings.TrimSpace(req.ProjectID)
	botUID := strings.TrimSpace(req.BotUID)
	preparedMembers, spaceID, err := projectmod.ListActiveProjectMemberUIDs(s.ctx.DB(), projectID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.SpaceID) != "" && strings.TrimSpace(req.SpaceID) != spaceID {
		return nil, projectmod.ErrGroupProjectSpaceConflict
	}
	if !containsProjectGroupUID(preparedMembers, creator) {
		return nil, projectmod.ErrGroupProjectForbidden
	}
	if len(preparedMembers) == 0 {
		return nil, projectmod.ErrGroupProjectForbidden
	}
	requestedMembers := projectGroupUniqueUIDs(req.Members)
	candidates := projectGroupUniqueUIDs(append(append([]string{}, preparedMembers...), requestedMembers...))

	groupNo := util.GenerUUID()
	version, err := s.ctx.GenSeq(common.GroupSeqKey)
	if err != nil {
		return nil, errors.New("failed to generate group version")
	}
	var botMemberVersion, botAdminVersion int64
	if botUID != "" {
		botMemberVersion, err = s.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			return nil, errors.New("failed to generate bot member version")
		}
		botAdminVersion, err = s.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			return nil, errors.New("failed to generate bot admin version")
		}
	}
	for range projectGroupCreateMaxAttempts {
		// Sequence values are prepared before opening the transaction and
		// remain valid after a bounded snapshot retry.
		memberVersions, err := s.allocateProjectGroupMemberVersions(candidates)
		if err != nil {
			return nil, err
		}

		var pendingTx *dbr.Tx
		var state *projectGroupCreateState
		snapshotExpanded := false
		txErr := projectmod.RetryGroupProjectLockConflict(func() error {
			seatRefs, prepErr := projectmod.PrepareGroupProjectSpaceSeatRefs(
				s.ctx.DB(), spaceID, candidates,
			)
			if prepErr != nil {
				return prepErr
			}
			tx, beginErr := s.ctx.DB().Begin()
			if beginErr != nil {
				return fmt.Errorf("failed to begin Project group transaction: %w", beginErr)
			}
			nextState, retrySnapshot, insertErr := s.insertProjectGroupTx(
				tx, req, creator, projectID, spaceID, groupNo, version, candidates,
				seatRefs, memberVersions, botUID, botMemberVersion,
			)
			if insertErr != nil {
				_ = tx.Rollback()
				return insertErr
			}
			if retrySnapshot {
				_ = tx.Rollback()
				snapshotExpanded = true
				return nil
			}
			pendingTx = tx
			state = nextState
			return nil
		})
		if txErr != nil {
			return nil, txErr
		}
		if snapshotExpanded {
			fresh, freshSpace, freshErr := projectmod.ListActiveProjectMemberUIDs(s.ctx.DB(), projectID)
			if freshErr != nil {
				return nil, freshErr
			}
			if freshSpace != spaceID {
				return nil, projectmod.ErrGroupProjectSpaceConflict
			}
			candidates = projectGroupUniqueUIDs(append(append([]string{}, fresh...), requestedMembers...))
			continue
		}
		if pendingTx == nil || state == nil {
			return nil, errors.New("Project group transaction produced no state")
		}
		// Commit is intentionally outside the retry callback. A commit result
		// may be uncertain, so this operation must not rerun the create or its
		// post-commit IM side effect.
		defer pendingTx.RollbackUnlessCommitted()
		if commitErr := pendingTx.Commit(); commitErr != nil {
			return nil, errors.New("failed to commit Project group transaction")
		}
		state.botAdminVersion = botAdminVersion
		return s.finishProjectGroupCreate(state)
	}
	return nil, errors.New("Project member snapshot changed during group creation")
}

type projectGroupCreateState struct {
	req             CreateGroupServiceReq
	creatorUser     *user.Model
	groupNo         string
	groupName       string
	version         int64
	botAdminVersion int64
	realMemberUIDs  []string
	memberVos       []*config.UserBaseVo
}

func (s *Service) insertProjectGroupTx(
	tx *dbr.Tx,
	req *CreateGroupServiceReq,
	creator, projectID, spaceID, groupNo string,
	version int64,
	prepared []string,
	seatRefs map[string]int64,
	memberVersions map[string]int64,
	botUID string,
	botMemberVersion int64,
) (*projectGroupCreateState, bool, error) {
	access, expanded, err := projectmod.LockGroupProjectCreateAccessTx(
		tx, creator, projectID, spaceID, prepared, seatRefs,
	)
	if err != nil {
		return nil, false, err
	}
	if expanded {
		return nil, true, nil
	}
	members := projectGroupUniqueUIDs(access.MemberUIDs)
	eligibleUIDs := make(map[string]struct{}, len(access.EligibleUIDs))
	for _, uid := range access.EligibleUIDs {
		eligibleUIDs[uid] = struct{}{}
	}
	for _, uid := range projectGroupUniqueUIDs(req.Members) {
		if _, ok := eligibleUIDs[uid]; !ok {
			// Explicit targets use native Group policy: they must be active
			// accounts, but need not already be Project members.
			return nil, false, projectmod.ErrGroupProjectForbidden
		}
		members = append(members, uid)
	}
	members = projectGroupUniqueUIDs(members)
	if len(members) == 0 || !containsProjectGroupUID(members, creator) {
		return nil, false, projectmod.ErrGroupProjectForbidden
	}
	lockedUsers, err := queryProjectGroupUsersTx(tx, members)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read Project group members: %w", err)
	}
	if err := validateProjectGroupUsers(lockedUsers, members); err != nil {
		return nil, false, err
	}
	defaultSpaces, err := queryProjectGroupDefaultSpacesTx(tx, members)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read Project group member Spaces: %w", err)
	}
	spaceMemberUIDs := make(map[string]struct{}, len(access.SpaceMemberUIDs))
	for _, uid := range access.SpaceMemberUIDs {
		spaceMemberUIDs[uid] = struct{}{}
	}
	isExternalGroup := 0
	for _, uid := range members {
		if uid == creator || uid == botUID {
			continue
		}
		if _, ok := spaceMemberUIDs[uid]; !ok && lockedUsers[uid].Robot == 0 {
			isExternalGroup = 1
			break
		}
	}
	orderedUsers := orderedProjectGroupUsers(members, lockedUsers)
	// transaction-locked user rows are authoritative for the actual write.
	groupName := strings.TrimSpace(req.Name)
	if groupName == "" {
		groupName = truncateCreateGroupName(createProjectGroupName(orderedUsers))
	} else {
		groupName = truncateCreateGroupName(groupName)
	}
	creatorUser := lockedUsers[creator]
	if creatorUser == nil {
		return nil, false, errors.New("creator user not found")
	}
	linkedBy := creator
	if err := s.db.InsertTx(&Model{
		GroupNo:             groupNo,
		Name:                groupName,
		IsNamed:             0,
		Creator:             creator,
		Status:              GroupStatusNormal,
		Version:             version,
		AllowViewHistoryMsg: int(common.GroupAllowViewHistoryMsgEnabled),
		SpaceID:             access.SpaceID,
		ProjectID:           projectID,
		ProjectLinkedBy:     &linkedBy,
		IsExternalGroup:     isExternalGroup,
		AllowExternal:       1,
		AllowNoMention:      1,
		AvatarText:          req.AvatarText,
		AvatarColor:         req.AvatarColor,
	}, tx); err != nil {
		return nil, false, fmt.Errorf("failed to insert Project group record: %w", err)
	}

	admissions := make([]MemberAdmission, 0, len(members)+1)
	memberUIDs := make([]string, 0, len(members)+1)
	memberVos := make([]*config.UserBaseVo, 0, len(members)+1)
	for _, uid := range members {
		memberVersion, ok := memberVersions[uid]
		if !ok {
			return nil, false, fmt.Errorf("missing prepared member version for %s", uid)
		}
		member := lockedUsers[uid]
		role := MemberRoleCommon
		if uid == creator {
			role = MemberRoleCreator
		}
		isExternal := 0
		sourceSpaceID := ""
		if _, ok := spaceMemberUIDs[uid]; !ok {
			isExternal = 1
			sourceSpaceID = defaultSpaces[uid]
		}
		admissions = append(admissions, MemberAdmission{
			UID: uid, Version: memberVersion, Role: role, InviteUID: creator,
			Robot: member.Robot, IsExternal: isExternal, SourceSpaceID: sourceSpaceID,
		})
		memberUIDs = append(memberUIDs, uid)
		memberVos = append(memberVos, &config.UserBaseVo{UID: uid, Name: member.Name})
	}
	if err := s.db.admitOrRestoreMembersTx(tx, groupNo, admissions); err != nil {
		return nil, false, err
	}
	if botUID != "" && !containsProjectGroupUID(members, botUID) {
		err := s.db.admitOrRestoreMembersTx(tx, groupNo, []MemberAdmission{{
			UID: botUID, Version: botMemberVersion, Role: MemberRoleCommon,
			InviteUID: creator, Robot: 1,
		}})
		if err != nil {
			if projectmod.IsGroupProjectLockConflict(err) {
				return nil, false, err
			}
			// Preserve CreateGroup's existing best-effort bot policy: a bot
			// admission failure does not discard an otherwise valid group.
			s.Warn("Project group bot admission failed", zap.Error(err), zap.String("botUID", botUID))
		} else {
			memberUIDs = append(memberUIDs, botUID)
			memberVos = append(memberVos, &config.UserBaseVo{UID: botUID, Name: botUID})
		}
	}
	return &projectGroupCreateState{
		req: *req, creatorUser: creatorUser, groupNo: groupNo, groupName: groupName,
		version: version, realMemberUIDs: memberUIDs, memberVos: memberVos,
	}, false, nil
}

func (s *Service) finishProjectGroupCreate(state *projectGroupCreateState) (*CreateGroupServiceResp, error) {
	if state == nil {
		return nil, errors.New("Project group create state is nil")
	}
	if err := s.ctx.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
		ChannelID: state.groupNo, ChannelType: common.ChannelTypeGroup.Uint8(), Subscribers: state.realMemberUIDs,
	}); err != nil {
		s.Error("create Project group IM channel failed, performing compensating rollback", zap.Error(err), zap.String("groupNo", state.groupNo))
		if cleanupErr := s.compensateProjectGroupCreate(state.groupNo); cleanupErr != nil {
			return nil, fmt.Errorf("failed to create IM channel; local Project group compensation failed: %w", cleanupErr)
		}
		return nil, errors.New("failed to create IM channel; local Project group was removed")
	}
	if botUID := strings.TrimSpace(state.req.BotUID); botUID != "" {
		if err := s.db.UpdateBotAdmin(state.groupNo, botUID, 1, state.botAdminVersion); err != nil {
			s.Error("set Project group bot_admin failed", zap.Error(err),
				zap.String("groupNo", state.groupNo), zap.String("botUID", botUID))
		}
	}
	s.ctx.SendGroupCreate(&config.MsgGroupCreateReq{
		Creator: state.req.Creator, CreatorName: state.creatorUser.Name, GroupNo: state.groupNo,
		Version: state.version, Members: state.memberVos,
	})
	return &CreateGroupServiceResp{GroupNo: state.groupNo, Name: state.groupName}, nil
}

func (s *Service) compensateProjectGroupCreate(groupNo string) error {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return fmt.Errorf("begin compensation: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	if _, err := tx.DeleteFrom("group_member").Where("group_no=?", groupNo).Exec(); err != nil {
		return fmt.Errorf("delete group members: %w", err)
	}
	if _, err := tx.DeleteFrom("group").Where("group_no=?", groupNo).Exec(); err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit compensation: %w", err)
	}
	return nil
}

func (s *Service) allocateProjectGroupMemberVersions(uids []string) (map[string]int64, error) {
	versions := make(map[string]int64, len(uids))
	for _, uid := range uids {
		version, err := s.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			return nil, err
		}
		versions[uid] = version
	}
	return versions, nil
}

// queryProjectGroupUsersTx reads the user rows after the Project access helper
// has locked every candidate account. It deliberately uses the transaction's
// consistent snapshot rather than taking a second lock after Project rows.
func queryProjectGroupUsersTx(tx *dbr.Tx, uids []string) (map[string]*user.Model, error) {
	uids = projectGroupUniqueUIDs(uids)
	result := make(map[string]*user.Model, len(uids))
	if len(uids) == 0 {
		return result, nil
	}
	args := make([]interface{}, len(uids))
	for i, uid := range uids {
		args[i] = uid
	}
	query := "SELECT * FROM `user` WHERE uid IN (" + strings.TrimSuffix(strings.Repeat("?,", len(uids)), ",") + ") ORDER BY uid"
	var rows []*user.Model
	if _, err := tx.SelectBySql(query, args...).Load(&rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row != nil {
			result[row.UID] = row
		}
	}
	return result, nil
}

func queryProjectGroupDefaultSpacesTx(tx *dbr.Tx, uids []string) (map[string]string, error) {
	uids = projectGroupUniqueUIDs(uids)
	result := make(map[string]string, len(uids))
	if len(uids) == 0 {
		return result, nil
	}
	args := make([]interface{}, len(uids))
	for i, uid := range uids {
		args[i] = uid
	}
	var rows []struct {
		UID     string `db:"uid"`
		SpaceID string `db:"space_id"`
	}
	query := "SELECT uid, space_id FROM `space_member` WHERE status=1 AND uid IN (" +
		strings.TrimSuffix(strings.Repeat("?,", len(uids)), ",") +
		") ORDER BY uid, created_at, space_id"
	if _, err := tx.SelectBySql(query, args...).Load(&rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if _, exists := result[row.UID]; !exists {
			result[row.UID] = row.SpaceID
		}
	}
	return result, nil
}

func validateProjectGroupUsers(users map[string]*user.Model, uids []string) error {
	for _, uid := range uids {
		model := users[uid]
		if model == nil || model.IsDestroy == user.IsDestroyDone || model.Status != 1 {
			return errors.New("Project group member is not an active user")
		}
	}
	return nil
}

func orderedProjectGroupUsers(uids []string, users map[string]*user.Model) []*user.Model {
	result := make([]*user.Model, 0, len(uids))
	for _, uid := range uids {
		if model := users[uid]; model != nil {
			result = append(result, model)
		}
	}
	return result
}

func createProjectGroupName(users []*user.Model) string {
	names := make([]string, 0, len(users))
	for _, model := range users {
		if model != nil {
			names = append(names, model.Name)
		}
	}
	return strings.Join(names, "、")
}

func truncateCreateGroupName(name string) string {
	nameRunes := []rune(name)
	if len(nameRunes) > MaxGroupNameLen {
		return string(nameRunes[:MaxGroupNameLen])
	}
	return name
}

func projectGroupUniqueUIDs(uids []string) []string {
	result := make([]string, 0, len(uids))
	seen := make(map[string]struct{}, len(uids))
	for _, uid := range uids {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		result = append(result, uid)
	}
	return result
}

func containsProjectGroupUID(uids []string, target string) bool {
	for _, uid := range uids {
		if uid == target {
			return true
		}
	}
	return false
}
