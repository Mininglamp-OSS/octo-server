package group

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	common2 "github.com/Mininglamp-OSS/octo-server/modules/common"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	workspace "github.com/Mininglamp-OSS/octo-server/modules/workspace"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

const workspaceCreateMaxAttempts = 3

// workspaceCreateBeforeCurrentReadHook is a test-only synchronization seam.
// It is nil in production and runs after the create transaction begins but
// before the Workspace snapshot seam starts its locking/current-read sequence.
var workspaceCreateBeforeCurrentReadHook func()

type createGroupState struct {
	req            CreateGroupServiceReq
	creatorUser    *user.Model
	groupNo        string
	groupName      string
	version        int64
	skippedMembers []string
	realMemberUIDs []string
	memberVos      []*config.UserBaseVo
}

type createGroupBase struct {
	req         CreateGroupServiceReq
	creatorUser *user.Model
	creator     string
	explicit    []string
	botUID      string
	spaceID     string
	workspaceID string
	groupNo     string
	groupName   string
	version     int64
	appConfig   *common2.AppConfigResp
}

// prepareCreateGroupCandidates returns a stable, de-duplicated candidate order.
// The creator is always first, followed by explicit members and then the bot.
func prepareCreateGroupCandidates(creator string, members []string, botUID string) []string {
	candidates := make([]string, 0, len(members)+2)
	seen := make(map[string]struct{}, len(members)+2)
	appendUID := func(uid string) {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			return
		}
		if _, ok := seen[uid]; ok {
			return
		}
		seen[uid] = struct{}{}
		candidates = append(candidates, uid)
	}
	appendUID(creator)
	for _, uid := range members {
		appendUID(uid)
	}
	appendUID(botUID)
	return candidates
}

func appendCreateGroupCandidates(candidates []string, extra ...string) []string {
	seen := make(map[string]struct{}, len(candidates)+len(extra))
	for _, uid := range candidates {
		seen[uid] = struct{}{}
	}
	for _, uid := range extra {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		candidates = append(candidates, uid)
	}
	return candidates
}

func workspaceCreateCandidateExpanded(prepared, current []string) bool {
	preparedSet := make(map[string]struct{}, len(prepared))
	for _, uid := range prepared {
		uid = strings.TrimSpace(uid)
		if uid != "" {
			preparedSet[uid] = struct{}{}
		}
	}
	for _, uid := range current {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if _, ok := preparedSet[uid]; !ok {
			return true
		}
	}
	return false
}

func (s *Service) createGroupBeforeIM(req *CreateGroupServiceReq) (*createGroupState, error) {
	if req == nil {
		return nil, errors.New("request is required")
	}
	creator := strings.TrimSpace(req.Creator)
	if creator == "" {
		return nil, errors.New("creator is required")
	}
	workspaceID := strings.TrimSpace(req.WorkspaceID)

	explicit := make([]string, 0, len(req.Members))
	seen := make(map[string]struct{}, len(req.Members)+1)
	for _, uid := range req.Members {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		explicit = append(explicit, uid)
	}
	botUID := strings.TrimSpace(req.BotUID)
	allUserCandidates := prepareCreateGroupCandidates(creator, explicit, "")

	creatorUser, err := s.userDB.QueryByUID(creator)
	if err != nil {
		s.Error("query creator info failed", zap.Error(err))
		return nil, errors.New("failed to query creator info")
	}
	if creatorUser == nil {
		return nil, errors.New("creator user not found")
	}
	users, err := s.userDB.QueryByUIDs(allUserCandidates)
	if err != nil {
		s.Error("query member info failed", zap.Error(err))
		return nil, errors.New("failed to query member info")
	}
	userByUID := indexCreateGroupUsers(users)
	orderedUsers := orderedCreateGroupUsers(allUserCandidates, userByUID)
	if workspaceID == "" {
		orderedUsers = liveCreateGroupUsers(orderedUsers)
	}
	if len(orderedUsers) == 0 {
		return nil, errors.New("no valid member found")
	}

	groupName := strings.TrimSpace(req.Name)
	if groupName == "" {
		groupName = createGroupName(orderedUsers)
	}
	groupName = truncateCreateGroupName(groupName)

	groupNo := util.GenerUUID()
	version, err := s.ctx.GenSeq(common.GroupSeqKey)
	if err != nil {
		s.Error("generate group version failed", zap.Error(err))
		return nil, errors.New("failed to generate group version")
	}

	base := &createGroupBase{
		req: CreateGroupServiceReq{
			Creator:         creator,
			Members:         explicit,
			Name:            req.Name,
			SpaceID:         strings.TrimSpace(req.SpaceID),
			ProjectID:       strings.TrimSpace(req.ProjectID),
			WorkspaceID:     workspaceID,
			expectedSpaceID: strings.TrimSpace(req.expectedSpaceID),
			BotUID:          botUID,
			CategoryID:      req.CategoryID,
			AvatarText:      req.AvatarText,
			AvatarColor:     req.AvatarColor,
		},
		creatorUser: creatorUser,
		creator:     creator,
		explicit:    explicit,
		botUID:      botUID,
		spaceID:     strings.TrimSpace(req.SpaceID),
		workspaceID: workspaceID,
		groupNo:     groupNo,
		groupName:   groupName,
		version:     version,
	}

	if workspaceID != "" {
		appConfig := req.preparedAppConfig
		if !req.preparedAppConfigLoaded {
			var configErr error
			appConfig, configErr = common2.NewService(s.ctx).GetAppConfig()
			if configErr != nil {
				s.Error("query application settings for workspace group failed", zap.Error(configErr))
				return nil, errors.New("failed to query application settings")
			}
		}
		base.appConfig = appConfig
		return s.createWorkspaceGroupBeforeIM(base)
	}

	return s.createRegularGroupBeforeIM(base, orderedUsers)
}

func (s *Service) createRegularGroupBeforeIM(base *createGroupBase, memberUsers []*user.Model) (*createGroupState, error) {
	externalMap, sourceSpaceMap, err := s.prepareGroupSpaceExternal(base.spaceID, base.creator, base.botUID, base.explicit, true)
	if err != nil {
		return nil, err
	}

	candidateUIDs := make([]string, 0, len(memberUsers)+1)
	for _, memberUser := range memberUsers {
		candidateUIDs = append(candidateUIDs, memberUser.UID)
	}
	candidateUIDs = appendCreateGroupCandidates(candidateUIDs, base.botUID)
	memberVersions, err := s.allocateCreateGroupMemberVersions(candidateUIDs)
	if err != nil {
		return nil, err
	}
	return s.insertCreateGroupTx(base, base.spaceID, "", memberUsers, externalMap, sourceSpaceMap, memberVersions)
}

func (s *Service) createWorkspaceGroupBeforeIM(base *createGroupBase) (*createGroupState, error) {
	ws := workspace.NewService(s.ctx)
	for attempt := range workspaceCreateMaxAttempts {
		spaceID, err := ws.ResolveSpace(base.workspaceID)
		if err != nil {
			return nil, err
		}
		if base.spaceID != "" && base.spaceID != spaceID {
			return nil, errGroupWorkspaceConflict
		}
		preReadMembers, err := ws.ListActiveMemberUIDs(base.workspaceID)
		if err != nil {
			return nil, err
		}
		preparedCandidates := prepareCreateGroupCandidates(base.creator, base.explicit, base.botUID)
		preparedCandidates = appendCreateGroupCandidates(preparedCandidates, preReadMembers...)

		// Target-space membership and source-space mappings are authoritative
		// transaction facts. Do not pre-read or carry those classifications
		// across the transaction boundary.
		externalMap := make(map[string]bool)
		sourceSpaceMap := make(map[string]string)
		memberVersions, err := s.allocateCreateGroupMemberVersions(preparedCandidates)
		if err != nil {
			return nil, err
		}
		seatKeys := make([]workspace.SpaceSeatKey, 0, len(preparedCandidates))
		for _, uid := range preparedCandidates {
			seatKeys = append(seatKeys, workspace.SpaceSeatKey{SpaceID: spaceID, UID: uid})
		}

		state, retry, err := s.insertWorkspaceCreateGroupTx(base, spaceID, preparedCandidates, externalMap, sourceSpaceMap, memberVersions, seatKeys)
		if err != nil {
			return nil, err
		}
		if !retry {
			return state, nil
		}
		s.Warn("workspace group snapshot changed during preparation",
			zap.String("workspace_id", base.workspaceID), zap.Int("attempt", attempt+1))
	}
	return nil, workspace.ErrDependencyUnavailable
}

func (s *Service) insertWorkspaceCreateGroupTx(base *createGroupBase, spaceID string, preparedCandidates []string, externalMap map[string]bool, sourceSpaceMap map[string]string, memberVersions map[string]int64, seatKeys []workspace.SpaceSeatKey) (*createGroupState, bool, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, false, errors.New("failed to begin transaction")
	}
	defer tx.RollbackUnlessCommitted()
	if hook := workspaceCreateBeforeCurrentReadHook; hook != nil {
		hook()
	}
	access, err := workspace.NewService(s.ctx).LockAccessesForGroupTx(tx, base.creator, base.workspaceID, seatKeys)
	if err != nil {
		return nil, false, err
	}
	if access.SpaceID == "" {
		return nil, false, workspace.ErrNotFound
	}
	if base.req.expectedSpaceID != "" && access.SpaceID != base.req.expectedSpaceID {
		return nil, false, workspace.ErrSpaceRequired
	}
	if base.spaceID != "" && access.SpaceID != base.spaceID {
		return nil, false, errGroupWorkspaceConflict
	}
	if workspaceCreateCandidateExpanded(preparedCandidates, access.MemberUIDs) {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return nil, false, rollbackErr
		}
		return nil, true, nil
	}

	finalUIDs := prepareCreateGroupCandidates(base.creator, base.explicit, base.botUID)
	finalUIDs = appendCreateGroupCandidates(finalUIDs, access.EligibleMemberUIDs...)
	lockedUsers, err := lockCreateGroupUsersTx(tx, finalUIDs)
	if err != nil {
		return nil, false, errors.New("failed to query member info")
	}
	if err := validateCreateGroupRequiredUsers(lockedUsers, finalUIDs, true); err != nil {
		return nil, false, err
	}
	memberUsers := orderedCreateGroupUsers(finalUIDs, lockedUsers)
	if len(memberUsers) == 0 {
		return nil, false, errors.New("no valid member found")
	}

	membership, err := checkMembershipsTx(tx, access.SpaceID, finalUIDs)
	if err != nil {
		return nil, false, err
	}
	if !membership[base.creator] {
		return nil, false, errors.New("creator is not a member of this space")
	}
	if base.botUID != "" && !membership[base.botUID] {
		return nil, false, errors.New("bot is not a member of this space")
	}

	// Rebuild these maps on every attempt. Prepared classifications are never
	// used as authorization and may be stale by the time the transaction reads.
	externalMap = make(map[string]bool)
	externalUIDs := make([]string, 0, len(finalUIDs))
	for _, uid := range finalUIDs {
		if uid == base.creator || uid == base.botUID {
			continue
		}
		if !membership[uid] {
			externalMap[uid] = true
			externalUIDs = append(externalUIDs, uid)
		}
	}
	freshSources, err := getUserDefaultSpaceIDsTx(tx, externalUIDs)
	if err != nil {
		return nil, false, err
	}
	sourceSpaceMap = freshSources

	if base.appConfig != nil && base.appConfig.InviteSystemAccountJoinGroupOn == 0 {
		fileHelperUID := strings.TrimSpace(s.ctx.GetConfig().Account.FileHelperUID)
		if base.botUID == fileHelperUID {
			return nil, false, errors.New("system account is not allowed in this group")
		}
		for _, memberUser := range memberUsers {
			if memberUser != nil && memberUser.UID == fileHelperUID {
				return nil, false, errors.New("system account is not allowed in this group")
			}
		}
	}

	groupName := base.groupName
	if strings.TrimSpace(base.req.Name) == "" {
		groupName = truncateCreateGroupName(createGroupName(memberUsers))
	}
	state, err := s.insertCreateGroupRowsTx(base, access.SpaceID, base.workspaceID, memberUsers, externalMap, sourceSpaceMap, memberVersions, &groupName, tx)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		s.Error("commit transaction failed", zap.Error(err))
		return nil, false, errors.New("failed to commit transaction")
	}
	return state, false, nil
}

func (s *Service) insertCreateGroupTx(base *createGroupBase, groupSpaceID, workspaceID string, memberUsers []*user.Model, externalMap map[string]bool, sourceSpaceMap map[string]string, memberVersions map[string]int64) (*createGroupState, error) {
	tx, err := s.ctx.DB().Begin()
	if err != nil {
		return nil, errors.New("failed to begin transaction")
	}
	defer tx.RollbackUnlessCommitted()
	state, err := s.insertCreateGroupRowsTx(base, groupSpaceID, workspaceID, memberUsers, externalMap, sourceSpaceMap, memberVersions, nil, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		s.Error("commit transaction failed", zap.Error(err))
		return nil, errors.New("failed to commit transaction")
	}
	return state, nil
}

func (s *Service) insertCreateGroupRowsTx(base *createGroupBase, groupSpaceID, workspaceID string, memberUsers []*user.Model, externalMap map[string]bool, sourceSpaceMap map[string]string, memberVersions map[string]int64, groupNameOverride *string, tx *dbr.Tx) (*createGroupState, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	groupName := base.groupName
	if groupNameOverride != nil {
		groupName = *groupNameOverride
	}
	isExternalGroup := 0
	for _, memberUser := range memberUsers {
		if memberUser == nil || memberUser.UID == base.creator || memberUser.UID == base.botUID {
			continue
		}
		if externalMap[memberUser.UID] && memberUser.Robot == 0 {
			isExternalGroup = 1
			break
		}
	}

	var workspaceIDPtr *string
	var workspaceLinkedByPtr *string
	if workspaceID != "" {
		workspaceIDPtr = &workspaceID
		linkedBy := base.creator
		workspaceLinkedByPtr = &linkedBy
	}
	if err := s.db.InsertTx(&Model{
		GroupNo:             base.groupNo,
		Name:                groupName,
		IsNamed:             0,
		Creator:             base.creator,
		Status:              GroupStatusNormal,
		Version:             base.version,
		AllowViewHistoryMsg: int(common.GroupAllowViewHistoryMsgEnabled),
		SpaceID:             groupSpaceID,
		ProjectID:           base.req.ProjectID,
		WorkspaceID:         workspaceIDPtr,
		WorkspaceLinkedBy:   workspaceLinkedByPtr,
		AllowExternal:       1,
		AllowNoMention:      1,
		IsExternalGroup:     isExternalGroup,
		AvatarText:          base.req.AvatarText,
		AvatarColor:         base.req.AvatarColor,
	}, tx); err != nil {
		s.Error("insert group record failed", zap.Error(err))
		return nil, errors.New("failed to insert group record")
	}

	realMemberUIDs := make([]string, 0, len(memberUsers)+1)
	memberVos := make([]*config.UserBaseVo, 0, len(memberUsers)+1)
	initialAdmissions := make([]MemberAdmission, 0, len(memberUsers))
	for _, memberUser := range memberUsers {
		if memberUser == nil || memberUser.IsDestroy == user.IsDestroyDone {
			continue
		}
		if base.botUID != "" && memberUser.UID == base.botUID {
			continue
		}
		memberVersion, ok := memberVersions[memberUser.UID]
		if !ok {
			return nil, fmt.Errorf("missing prepared member version for %s", memberUser.UID)
		}
		role := MemberRoleCommon
		if memberUser.UID == base.creator {
			role = MemberRoleCreator
		}
		isExt := 0
		srcSpaceID := ""
		if externalMap[memberUser.UID] {
			isExt = 1
			srcSpaceID = sourceSpaceMap[memberUser.UID]
		}
		initialAdmissions = append(initialAdmissions, MemberAdmission{
			UID:           memberUser.UID,
			Version:       memberVersion,
			Role:          role,
			InviteUID:     base.creator,
			Robot:         memberUser.Robot,
			IsExternal:    isExt,
			SourceSpaceID: srcSpaceID,
		})
		realMemberUIDs = append(realMemberUIDs, memberUser.UID)
		memberVos = append(memberVos, &config.UserBaseVo{UID: memberUser.UID, Name: memberUser.Name})
	}
	if len(realMemberUIDs) == 0 {
		return nil, errors.New("no valid member to add")
	}
	if err := s.db.admitOrRestoreMembersTx(tx, base.groupNo, groupSpaceID, base.req.ProjectID,
		initialAdmissions, AdmissionEntryCreateGroup); err != nil {
		s.Error("insert members failed", zap.Error(err), zap.String("groupNo", base.groupNo))
		if errors.Is(err, ErrAdmissionRefused) {
			return nil, err
		}
		return nil, errors.New("failed to insert group member")
	}

	if base.botUID != "" {
		botMemberVersion, ok := memberVersions[base.botUID]
		if !ok {
			return nil, fmt.Errorf("missing prepared bot member version for %s", base.botUID)
		}
		err := s.db.admitOrRestoreMembersTx(tx, base.groupNo, groupSpaceID, base.req.ProjectID, []MemberAdmission{{
			UID:       base.botUID,
			Version:   botMemberVersion,
			Role:      MemberRoleCommon,
			InviteUID: base.creator,
			Robot:     1,
		}}, AdmissionEntryCreateGroupBot)
		if err != nil {
			s.Error("insert bot member failed", zap.Error(err))
			if workspaceID != "" {
				return nil, fmt.Errorf("failed to insert bot member: %w", err)
			}
		} else {
			realMemberUIDs = append(realMemberUIDs, base.botUID)
			memberVos = append(memberVos, &config.UserBaseVo{UID: base.botUID, Name: base.botUID})
		}
	}

	return &createGroupState{
		req:            base.req,
		creatorUser:    base.creatorUser,
		groupNo:        base.groupNo,
		groupName:      groupName,
		version:        base.version,
		skippedMembers: nil,
		realMemberUIDs: realMemberUIDs,
		memberVos:      memberVos,
	}, nil
}

func (s *Service) allocateCreateGroupMemberVersions(uids []string) (map[string]int64, error) {
	versions := make(map[string]int64, len(uids))
	for _, uid := range uids {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		version, err := s.ctx.GenSeq(common.GroupMemberSeqKey)
		if err != nil {
			s.Error("generate member version failed", zap.Error(err), zap.String("uid", uid))
			return nil, err
		}
		versions[uid] = version
	}
	return versions, nil
}

func (s *Service) prepareGroupSpaceExternal(spaceID, creator, botUID string, explicit []string, strict bool) (map[string]bool, map[string]string, error) {
	externalMap := make(map[string]bool)
	sourceSpaceMap := make(map[string]string)
	if spaceID == "" {
		return externalMap, sourceSpaceMap, nil
	}
	candidates := prepareCreateGroupCandidates(creator, explicit, botUID)
	active, err := spacepkg.ActiveMembers(s.ctx.DB(), spaceID, candidates)
	if err != nil {
		s.Error("check group space memberships failed", zap.Error(err))
		return nil, nil, errors.New("failed to check space membership")
	}
	if strict {
		if !active[creator] {
			return nil, nil, errors.New("creator is not a member of this space")
		}
		if botUID != "" && !active[botUID] {
			return nil, nil, errors.New("bot is not a member of this space")
		}
	}
	for _, uid := range explicit {
		uid = strings.TrimSpace(uid)
		if uid == "" || active[uid] {
			continue
		}
		externalMap[uid] = true
		sourceSpaceMap[uid] = spacemod.GetUserDefaultSpaceID(s.ctx, uid)
	}
	return externalMap, sourceSpaceMap, nil
}

func indexCreateGroupUsers(users []*user.Model) map[string]*user.Model {
	byUID := make(map[string]*user.Model, len(users))
	for _, model := range users {
		if model != nil {
			byUID[model.UID] = model
		}
	}
	return byUID
}

func orderedCreateGroupUsers(uids []string, byUID map[string]*user.Model) []*user.Model {
	users := make([]*user.Model, 0, len(uids))
	seen := make(map[string]struct{}, len(uids))
	for _, uid := range uids {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		model := byUID[uid]
		if model == nil {
			continue
		}
		seen[uid] = struct{}{}
		users = append(users, model)
	}
	return users
}

func liveCreateGroupUsers(users []*user.Model) []*user.Model {
	live := make([]*user.Model, 0, len(users))
	for _, model := range users {
		if model != nil && model.IsDestroy != user.IsDestroyDone {
			live = append(live, model)
		}
	}
	return live
}

func validateCreateGroupRequiredUsers(byUID map[string]*user.Model, required []string, requireActive bool) error {
	for _, uid := range required {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		model := byUID[uid]
		if model == nil || model.IsDestroy == user.IsDestroyDone || (requireActive && model.Status != 1) {
			return workspace.ErrCandidateIneligible
		}
	}
	return nil
}

func createGroupName(users []*user.Model) string {
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

func lockCreateGroupUsersTx(tx *dbr.Tx, uids []string) (map[string]*user.Model, error) {
	unique := prepareCreateGroupCandidates("", uids, "")
	locked := make(map[string]*user.Model, len(unique))
	if len(unique) == 0 {
		return locked, nil
	}
	args := make([]interface{}, len(unique))
	for i, uid := range unique {
		args[i] = uid
	}
	var rows []*user.Model
	if _, err := tx.SelectBySql(
		"SELECT * FROM `user` WHERE uid IN ("+strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",")+") ORDER BY uid FOR SHARE",
		args...,
	).Load(&rows); err != nil {
		return nil, fmt.Errorf("lock group users: %w", err)
	}
	for _, row := range rows {
		if row != nil {
			locked[row.UID] = row
		}
	}
	return locked, nil
}

func checkMembershipsTx(tx *dbr.Tx, spaceID string, uids []string) (map[string]bool, error) {
	members := make(map[string]bool)
	unique := prepareCreateGroupCandidates("", uids, "")
	if strings.TrimSpace(spaceID) == "" || len(unique) == 0 {
		return members, nil
	}
	args := make([]interface{}, 0, len(unique)+1)
	args = append(args, spaceID)
	for _, uid := range unique {
		args = append(args, uid)
	}
	var rows []struct {
		UID string `db:"uid"`
	}
	query := "SELECT sm.uid FROM `space_member` sm INNER JOIN `space` s ON s.space_id=sm.space_id WHERE sm.space_id=? AND sm.uid IN (" + strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",") + ") AND sm.status=1 AND s.status=1"
	if _, err := tx.SelectBySql(query, args...).Load(&rows); err != nil {
		return nil, fmt.Errorf("check group space memberships: %w", err)
	}
	for _, row := range rows {
		members[row.UID] = true
	}
	return members, nil
}

func getUserDefaultSpaceIDsTx(tx *dbr.Tx, uids []string) (map[string]string, error) {
	spaces := make(map[string]string)
	unique := prepareCreateGroupCandidates("", uids, "")
	if len(unique) == 0 {
		return spaces, nil
	}
	args := make([]interface{}, len(unique))
	for i, uid := range unique {
		args[i] = uid
	}
	var rows []struct {
		UID     string `db:"uid"`
		SpaceID string `db:"space_id"`
	}
	query := "SELECT uid, space_id FROM (SELECT sm.uid, sm.space_id, ROW_NUMBER() OVER (PARTITION BY sm.uid ORDER BY sm.created_at ASC, sm.id ASC) AS row_num FROM `space_member` sm INNER JOIN `space` s ON s.space_id=sm.space_id AND s.status=1 WHERE sm.uid IN (" + strings.TrimSuffix(strings.Repeat("?,", len(unique)), ",") + ") AND sm.status=1) ranked WHERE row_num=1"
	if _, err := tx.SelectBySql(query, args...).Load(&rows); err != nil {
		return nil, fmt.Errorf("query group user default spaces: %w", err)
	}
	for _, row := range rows {
		spaces[row.UID] = row.SpaceID
	}
	return spaces, nil
}
