package workspace

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

const (
	maxWorkspaceNameChars        = 30
	maxWorkspaceDescriptionChars = 500
	maxWorkspaceKeywordChars     = 30
	maxMemberBatch               = 100
)

// Service owns Workspace metadata and membership. It deliberately has no
// dependency on group or internal packages: group consumes the exported,
// transaction-aware LockAccessesTx seam below.
type Service struct {
	ctx     *config.Context
	db      *DB
	session *dbr.Session
}

func NewService(ctx *config.Context) *Service {
	db := NewDB(ctx)
	var session *dbr.Session
	if ctx != nil {
		session = ctx.DB()
	}
	return &Service{ctx: ctx, db: db, session: session}
}

func (s *Service) usable() error {
	if s == nil || s.db == nil || s.session == nil {
		return fmt.Errorf("workspace: service unavailable: %w", ErrDependencyUnavailable)
	}
	return nil
}

// ResolveSpace returns the authoritative active Space for a Workspace. It is
// used by derived-context adapters; archived and unknown Workspaces are both
// intentionally reported as ErrNotFound.
func (s *Service) ResolveSpace(workspaceID string) (string, error) {
	if err := s.usable(); err != nil {
		return "", err
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return "", ErrNotFound
	}
	row, err := s.db.queryActiveWorkspace(workspaceID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", ErrNotFound
	}
	return row.SpaceID, nil
}

func (s *Service) List(scope Scope, spaceID, keyword string, page Page) (*Pagination[Workspace], error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	if scope.UID == "" {
		return nil, ErrForbidden
	}
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		return nil, ErrSpaceRequired
	}
	eligible, err := s.db.queryUserEligible(scope.UID)
	if err != nil {
		return nil, err
	}
	if !eligible {
		return nil, ErrForbidden
	}
	keyword = strings.TrimSpace(keyword)
	if utf8.RuneCountInString(keyword) > maxWorkspaceKeywordChars {
		return nil, ErrRequestInvalid
	}
	if scope.ExpectedSpaceID != "" && scope.ExpectedSpaceID != spaceID {
		return nil, ErrSpaceRequired
	}
	active, err := s.db.querySpaceActive(spaceID)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrNotFound
	}
	member, err := s.db.querySpaceMemberActive(scope.UID, spaceID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, ErrNotFound
	}
	return s.db.listWorkspaces(spaceID, scope.UID, keyword, page)
}

func (s *Service) Get(scope Scope, workspaceID string) (*Workspace, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.readWorkspace(scope, workspaceID)
}

func (s *Service) Create(scope Scope, req CreateRequest) (*Workspace, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	if scope.UID == "" {
		return nil, ErrForbidden
	}
	spaceID := strings.TrimSpace(req.SpaceID)
	if spaceID == "" {
		return nil, ErrSpaceRequired
	}
	if scope.ExpectedSpaceID != "" && scope.ExpectedSpaceID != spaceID {
		return nil, ErrSpaceRequired
	}
	name, err := validateWorkspaceName(req.Name)
	if err != nil {
		return nil, err
	}
	description, logo, err := validateWorkspaceProfile(req.Description, req.Logo)
	if err != nil {
		return nil, err
	}

	tx, err := s.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("workspace: begin create: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	seats, err := s.db.lockSpaceSeatsTx(tx, []string{spaceID}, []string{scope.UID}, false)
	if err != nil {
		return nil, err
	}
	activeSpaces, err := s.db.lockSpacesTx(tx, []string{spaceID})
	if err != nil {
		return nil, err
	}
	if !activeSpaces[spaceID] || !seats[spaceID+"\x00"+scope.UID] {
		return nil, ErrSpaceRequired
	}
	now := time.Now().UTC()
	eligible, err := s.db.lockEligibleUsersTx(tx, []string{scope.UID})
	if err != nil {
		return nil, err
	}
	if !eligible[scope.UID] {
		return nil, ErrForbidden
	}
	row := &workspaceModel{
		WorkspaceID: util.GenerUUID(),
		SpaceID:     spaceID,
		Name:        name,
		Description: description,
		Logo:        logo,
		OwnerUID:    scope.UID,
		Status:      WorkspaceStatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.db.insertWorkspaceTx(tx, row); err != nil {
		return nil, err
	}
	if err := s.db.insertMemberTx(tx, &workspaceMemberModel{
		WorkspaceID: row.WorkspaceID,
		UID:         scope.UID,
		Role:        MemberRoleAdmin,
		Status:      MemberStatusActive,
		GrantedBy:   "",
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit create: %w", err)
	}
	return workspacePublic(row, WorkspaceRoleOwner, 1), nil
}

func (s *Service) Update(scope Scope, workspaceID string, req UpdateRequest) (*Workspace, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, ErrRequestInvalid
	}
	if req.Name == nil && req.Description == nil && req.Logo == nil {
		return nil, ErrRequestInvalid
	}
	if req.Name != nil {
		name, err := validateWorkspaceName(*req.Name)
		if err != nil {
			return nil, err
		}
		req.Name = &name
	}
	if req.Description != nil && utf8.RuneCountInString(*req.Description) > maxWorkspaceDescriptionChars {
		return nil, ErrRequestInvalid
	}
	if req.Logo != nil {
		logo, err := validateWorkspaceLogo(*req.Logo)
		if err != nil {
			return nil, err
		}
		req.Logo = &logo
	}

	tx, err := s.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("workspace: begin update: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	accesses, err := s.lockAccessesTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, false, nil)
	if err != nil {
		return nil, err
	}
	access, ok := accesses[workspaceID]
	if !ok || (access.Role != WorkspaceRoleOwner && access.Role != WorkspaceRoleAdmin) {
		return nil, ErrForbidden
	}
	current, err := s.db.queryWorkspaceLockedTx(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrNotFound
	}
	changed := false
	if req.Name != nil {
		if *req.Name == current.Name {
			req.Name = nil
		} else {
			changed = true
		}
	}
	if req.Description != nil {
		if *req.Description == current.Description {
			req.Description = nil
		} else {
			changed = true
		}
	}
	if req.Logo != nil {
		if *req.Logo == current.Logo {
			req.Logo = nil
		} else {
			changed = true
		}
	}
	now := time.Now().UTC()
	if changed {
		if err := s.db.updateWorkspaceTx(tx, workspaceID, req, now); err != nil {
			return nil, err
		}
		if req.Name != nil {
			current.Name = *req.Name
		}
		if req.Description != nil {
			current.Description = *req.Description
		}
		if req.Logo != nil {
			current.Logo = *req.Logo
		}
		current.UpdatedAt = now
	}
	count, err := s.db.countActiveWorkspaceMembersTx(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	result := workspacePublic(current, access.Role, count)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit update: %w", err)
	}
	return result, nil
}

func (s *Service) Archive(scope Scope, workspaceID string) error {
	if err := s.usable(); err != nil {
		return err
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return ErrRequestInvalid
	}
	tx, err := s.session.Begin()
	if err != nil {
		return fmt.Errorf("workspace: begin archive: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	accesses, err := s.lockAccessesTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, false, nil)
	if err != nil {
		return err
	}
	if access, ok := accesses[workspaceID]; !ok || access.Role != WorkspaceRoleOwner {
		return ErrForbidden
	}
	if err := s.db.archiveWorkspaceTx(tx, workspaceID, time.Now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workspace: commit archive: %w", err)
	}
	return nil
}

func (s *Service) Members(scope Scope, workspaceID string, filter MemberFilter, page Page) (*Pagination[Member], error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	if filter.Status != MemberStatusInactive && filter.Status != MemberStatusActive {
		return nil, ErrRequestInvalid
	}
	if len(filter.Roles) > 0 {
		for _, role := range filter.Roles {
			if role != WorkspaceRoleOwner && role != WorkspaceRoleAdmin && role != WorkspaceRoleMember {
				return nil, ErrRoleInvalid
			}
		}
	}
	if _, err := s.readWorkspace(scope, workspaceID); err != nil {
		return nil, err
	}
	return s.db.listMembers(strings.TrimSpace(workspaceID), filter, page)
}

func (s *Service) Member(scope Scope, workspaceID, uid string) (*Member, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	row, err := s.readWorkspace(scope, workspaceID)
	if err != nil {
		return nil, err
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, ErrRequestInvalid
	}
	member, err := s.db.queryWorkspaceMember(strings.TrimSpace(workspaceID), uid)
	if err != nil {
		return nil, err
	}
	if member == nil {
		return nil, ErrNotFound
	}
	if member.Role != MemberRoleMember && member.Role != MemberRoleAdmin {
		return nil, fmt.Errorf("workspace: invalid member role: %w", ErrForbidden)
	}
	return s.db.queryMemberPublic(strings.TrimSpace(workspaceID), uid, row.OwnerUID)
}

func (s *Service) AddMembers(scope Scope, workspaceID string, members []MemberInput) ([]Member, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" || len(members) == 0 {
		return nil, ErrRequestInvalid
	}
	if len(members) > maxMemberBatch {
		return nil, ErrCandidateIneligible
	}
	seen := make(map[string]struct{}, len(members))
	for i := range members {
		members[i].UID = strings.TrimSpace(members[i].UID)
		members[i].WorkspaceRole = strings.TrimSpace(members[i].WorkspaceRole)
		if members[i].UID == "" {
			return nil, ErrRequestInvalid
		}
		if members[i].WorkspaceRole != WorkspaceRoleAdmin && members[i].WorkspaceRole != WorkspaceRoleMember {
			return nil, ErrRoleInvalid
		}
		if _, ok := seen[members[i].UID]; ok {
			return nil, ErrRequestInvalid
		}
		seen[members[i].UID] = struct{}{}
	}
	candidateUIDs := make([]string, 0, len(members))
	for _, member := range members {
		candidateUIDs = append(candidateUIDs, member.UID)
	}
	tx, err := s.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("workspace: begin add members: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	accesses, seats, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, false, candidateUIDs)
	if err != nil {
		return nil, err
	}
	access, ok := accesses[workspaceID]
	if !ok || (access.Role != WorkspaceRoleOwner && access.Role != WorkspaceRoleAdmin) {
		return nil, ErrForbidden
	}
	eligible, err := s.db.lockEligibleUsersTx(tx, candidateUIDs)
	if err != nil {
		return nil, err
	}
	for _, uid := range candidateUIDs {
		if !seats[access.SpaceID+"\x00"+uid] || !eligible[uid] {
			return nil, ErrCandidateIneligible
		}
	}
	now := time.Now().UTC()
	resultRows := make([]*workspaceMemberModel, 0, len(members))
	for _, input := range members {
		key := workspaceID + "\x00" + input.UID
		existing := lockedMembers[key]
		if existing != nil && existing.Status == MemberStatusActive {
			currentRole := projectedRole(access.OwnerUID, existing.UID, existing.Role)
			if currentRole == "" {
				return nil, fmt.Errorf("workspace: invalid member role: %w", ErrForbidden)
			}
			if currentRole != input.WorkspaceRole {
				return nil, ErrRoleInvalid
			}
			resultRows = append(resultRows, existing)
			continue
		}
		admitted := &workspaceMemberModel{
			WorkspaceID: workspaceID,
			UID:         input.UID,
			Role:        storageRole(input.WorkspaceRole),
			Status:      MemberStatusActive,
			GrantedBy:   scope.UID,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if existing != nil {
			admitted.CreatedAt = existing.CreatedAt
		}
		if _, err := s.db.admitMemberTx(tx, admitted); err != nil {
			return nil, err
		}
		resultRows = append(resultRows, admitted)
	}
	result, err := s.db.membersPublicFromModels(resultRows, access.OwnerUID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit add members: %w", err)
	}
	return result, nil
}

func (s *Service) UpdateMember(scope Scope, workspaceID, uid, role string) (*Member, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	workspaceID, uid, role = strings.TrimSpace(workspaceID), strings.TrimSpace(uid), strings.TrimSpace(role)
	if workspaceID == "" || uid == "" {
		return nil, ErrRequestInvalid
	}
	if role != WorkspaceRoleAdmin && role != WorkspaceRoleMember {
		return nil, ErrRoleInvalid
	}
	tx, err := s.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("workspace: begin member role update: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	accesses, seats, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, false, []string{uid})
	if err != nil {
		return nil, err
	}
	access, ok := accesses[workspaceID]
	if !ok || (access.Role != WorkspaceRoleOwner && access.Role != WorkspaceRoleAdmin) {
		return nil, ErrForbidden
	}
	target := lockedMembers[workspaceID+"\x00"+uid]
	if target == nil || target.Status != MemberStatusActive {
		return nil, ErrNotFound
	}
	if !seats[access.SpaceID+"\x00"+uid] {
		return nil, ErrCandidateIneligible
	}
	eligible, err := s.db.lockEligibleUsersTx(tx, []string{uid})
	if err != nil {
		return nil, err
	}
	if !eligible[uid] {
		return nil, ErrCandidateIneligible
	}
	if target.Role != MemberRoleMember && target.Role != MemberRoleAdmin {
		return nil, fmt.Errorf("workspace: invalid member role: %w", ErrForbidden)
	}
	if uid == access.OwnerUID {
		return nil, ErrRoleInvalid
	}
	if projectedRole(access.OwnerUID, uid, target.Role) != role {
		now := time.Now().UTC()
		if err := s.db.updateMemberRoleTx(tx, workspaceID, uid, storageRole(role), now); err != nil {
			return nil, err
		}
		target.Role = storageRole(role)
		target.UpdatedAt = now
	}
	result, err := s.db.membersPublicFromModels([]*workspaceMemberModel{target}, access.OwnerUID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit member role update: %w", err)
	}
	return &result[0], nil
}

func (s *Service) RemoveMember(scope Scope, workspaceID, uid string) error {
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	if err := s.usable(); err != nil {
		return err
	}
	workspaceID, uid = strings.TrimSpace(workspaceID), strings.TrimSpace(uid)
	if workspaceID == "" || uid == "" {
		return ErrRequestInvalid
	}
	tx, err := s.session.Begin()
	if err != nil {
		return fmt.Errorf("workspace: begin remove member: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	accesses, _, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, false, []string{uid})
	if err != nil {
		return err
	}
	access, ok := accesses[workspaceID]
	if !ok || (access.Role != WorkspaceRoleOwner && access.Role != WorkspaceRoleAdmin) {
		return ErrForbidden
	}
	target := lockedMembers[workspaceID+"\x00"+uid]
	if target == nil || target.Status != MemberStatusActive {
		return ErrNotFound
	}
	if target.Role != MemberRoleMember && target.Role != MemberRoleAdmin {
		return fmt.Errorf("workspace: invalid member role: %w", ErrForbidden)
	}
	if uid == access.OwnerUID {
		return ErrOwnerProtected
	}
	if err := s.db.deactivateMemberTx(tx, workspaceID, uid, time.Now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workspace: commit remove member: %w", err)
	}
	return nil
}

func (s *Service) TransferOwner(scope Scope, workspaceID, uid string) (*Workspace, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	workspaceID, uid = strings.TrimSpace(workspaceID), strings.TrimSpace(uid)
	if workspaceID == "" || uid == "" {
		return nil, ErrRequestInvalid
	}
	tx, err := s.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("workspace: begin owner transfer: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	accesses, seats, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, false, []string{uid})
	if err != nil {
		return nil, err
	}
	access, ok := accesses[workspaceID]
	if !ok || access.Role != WorkspaceRoleOwner {
		return nil, ErrForbidden
	}
	if uid == access.OwnerUID {
		return nil, ErrOwnerProtected
	}
	target := lockedMembers[workspaceID+"\x00"+uid]
	if target == nil || target.Status != MemberStatusActive {
		return nil, ErrCandidateIneligible
	}
	if !seats[access.SpaceID+"\x00"+uid] {
		return nil, ErrCandidateIneligible
	}
	eligible, err := s.db.lockEligibleUsersTx(tx, []string{uid})
	if err != nil {
		return nil, err
	}
	if !eligible[uid] {
		return nil, ErrCandidateIneligible
	}
	if target.Role != MemberRoleMember && target.Role != MemberRoleAdmin {
		return nil, ErrRoleInvalid
	}
	now := time.Now().UTC()
	if err := s.db.transferOwnerTx(tx, workspaceID, access.OwnerUID, uid, now); err != nil {
		return nil, err
	}
	current, err := s.db.queryWorkspaceLockedTx(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrNotFound
	}
	count, err := s.db.countActiveWorkspaceMembersTx(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	result := workspacePublic(current, WorkspaceRoleAdmin, count)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit owner transfer: %w", err)
	}
	return result, nil
}

func (s *Service) Leave(scope Scope, workspaceID string) error {
	if err := s.usable(); err != nil {
		return err
	}
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return ErrRequestInvalid
	}
	tx, err := s.session.Begin()
	if err != nil {
		return fmt.Errorf("workspace: begin leave: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	accesses, _, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, false, []string{scope.UID})
	if err != nil {
		return err
	}
	access, ok := accesses[workspaceID]
	if !ok {
		return ErrForbidden
	}
	self := lockedMembers[workspaceID+"\x00"+scope.UID]
	if self == nil || self.Status != MemberStatusActive {
		return ErrNotFound
	}
	if access.Role == WorkspaceRoleOwner {
		return ErrOwnerProtected
	}
	if access.Role != WorkspaceRoleAdmin && access.Role != WorkspaceRoleMember {
		return ErrForbidden
	}
	if err := s.db.deactivateMemberTx(tx, workspaceID, scope.UID, time.Now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workspace: commit leave: %w", err)
	}
	return nil
}

// LockAccessesTx locks all current authorization facts needed by a caller
// operating on Workspace resources. It never starts or commits a transaction.
// IDs are de-duplicated and sorted before Workspace/member locking. Group code
// must pass every source and target ID in one call, avoiding inverted order.
func (s *Service) LockAccessesTx(tx *dbr.Tx, actorUID string, workspaceIDs []string, expectedSpaceID string, snapshot bool) (map[string]Access, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.lockAccessesTx(tx, strings.TrimSpace(actorUID), workspaceIDs, strings.TrimSpace(expectedSpaceID), snapshot, nil)
}

func (s *Service) lockAccessesTx(tx *dbr.Tx, actorUID string, workspaceIDs []string, expectedSpaceID string, snapshot bool, memberUIDs []string) (map[string]Access, error) {
	accesses, _, _, err := s.lockAccessesWithStateTx(tx, actorUID, workspaceIDs, expectedSpaceID, snapshot, memberUIDs)
	return accesses, err
}

func (s *Service) lockAccessesWithStateTx(tx *dbr.Tx, actorUID string, workspaceIDs []string, expectedSpaceID string, snapshot bool, memberUIDs []string) (map[string]Access, map[string]bool, map[string]*workspaceMemberModel, error) {
	if tx == nil {
		return nil, nil, nil, fmt.Errorf("workspace: transaction unavailable: %w", ErrDependencyUnavailable)
	}
	actorUID = strings.TrimSpace(actorUID)
	if actorUID == "" {
		return nil, nil, nil, ErrForbidden
	}
	workspaceIDs = uniqueSorted(workspaceIDs)
	if len(workspaceIDs) == 0 {
		return map[string]Access{}, map[string]bool{}, map[string]*workspaceMemberModel{}, nil
	}
	// Resolve only the locations first. This read does not authorize anything;
	// the active rows are re-read and locked below after Space locking.
	locations, err := s.db.queryWorkspaceLocationsTx(tx, workspaceIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(locations) != len(workspaceIDs) {
		return nil, nil, nil, ErrNotFound
	}
	spaceIDs := make([]string, 0, len(locations))
	for _, workspaceID := range workspaceIDs {
		row := locations[workspaceID]
		if row == nil || row.Status != WorkspaceStatusActive || row.SpaceID == "" {
			return nil, nil, nil, ErrNotFound
		}
		spaceIDs = append(spaceIDs, row.SpaceID)
	}
	spaceIDs = uniqueSorted(spaceIDs)
	seatUIDs := []string{actorUID}
	seatUIDs = append(seatUIDs, memberUIDs...)
	seats, err := s.db.lockSpaceSeatsTx(tx, spaceIDs, seatUIDs, snapshot)
	if err != nil {
		return nil, nil, nil, err
	}
	activeSpaces, err := s.db.lockSpacesTx(tx, spaceIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, spaceID := range spaceIDs {
		if !activeSpaces[spaceID] {
			return nil, nil, nil, ErrNotFound
		}
	}
	lockedWorkspaces, err := s.db.lockWorkspaceRowsTx(tx, workspaceIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(lockedWorkspaces) != len(workspaceIDs) {
		return nil, nil, nil, ErrNotFound
	}
	memberLockUIDs := make([]string, 0, len(workspaceIDs)*2+len(memberUIDs)+1)
	memberLockUIDs = append(memberLockUIDs, actorUID)
	memberLockUIDs = append(memberLockUIDs, memberUIDs...)
	for _, workspaceID := range workspaceIDs {
		memberLockUIDs = append(memberLockUIDs, lockedWorkspaces[workspaceID].OwnerUID)
	}
	lockedMembers, err := s.db.lockMemberRowsTx(tx, workspaceIDs, memberLockUIDs, snapshot)
	if err != nil {
		return nil, nil, nil, err
	}
	userUIDs := []string{actorUID}
	userUIDs = append(userUIDs, memberUIDs...)
	if snapshot {
		for _, member := range lockedMembers {
			if member != nil && member.Status == MemberStatusActive {
				userUIDs = append(userUIDs, member.UID)
			}
		}
	}
	eligibleUsers, err := s.db.lockEligibleUsersTx(tx, userUIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	accesses := make(map[string]Access, len(workspaceIDs))
	for _, workspaceID := range workspaceIDs {
		row := lockedWorkspaces[workspaceID]
		if !seats[row.SpaceID+"\x00"+actorUID] {
			return nil, nil, nil, ErrForbidden
		}
		actor := lockedMembers[workspaceID+"\x00"+actorUID]
		owner := lockedMembers[workspaceID+"\x00"+row.OwnerUID]
		if actor == nil || actor.Status != MemberStatusActive || owner == nil || owner.Status != MemberStatusActive {
			return nil, nil, nil, ErrForbidden
		}
		if !eligibleUsers[actorUID] {
			return nil, nil, nil, ErrForbidden
		}
		if owner.Role != MemberRoleMember && owner.Role != MemberRoleAdmin {
			return nil, nil, nil, ErrForbidden
		}
		if expectedSpaceID != "" && row.SpaceID != expectedSpaceID {
			return nil, nil, nil, ErrSpaceRequired
		}
		if actor.Role != MemberRoleMember && actor.Role != MemberRoleAdmin {
			return nil, nil, nil, ErrForbidden
		}
		role := projectedRole(row.OwnerUID, actorUID, actor.Role)
		if role != WorkspaceRoleOwner && role != WorkspaceRoleAdmin && role != WorkspaceRoleMember {
			return nil, nil, nil, ErrForbidden
		}
		access := Access{WorkspaceID: workspaceID, SpaceID: row.SpaceID, OwnerUID: row.OwnerUID, Role: role}
		if snapshot {
			members, err := s.db.queryLockedActiveMembersForWorkspace(lockedMembers, workspaceID)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, member := range members {
				if !seats[row.SpaceID+"\x00"+member.UID] || !eligibleUsers[member.UID] {
					continue
				}
				if member.Role != MemberRoleMember && member.Role != MemberRoleAdmin {
					return nil, nil, nil, ErrForbidden
				}
				access.MemberUIDs = append(access.MemberUIDs, member.UID)
			}
			sort.Strings(access.MemberUIDs)
		} else {
			access.MemberUIDs = nil
		}
		accesses[workspaceID] = access
	}
	return accesses, seats, lockedMembers, nil
}

func (s *Service) readWorkspace(scope Scope, workspaceID string) (*Workspace, error) {
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, ErrRequestInvalid
	}
	if scope.UID == "" {
		return nil, ErrForbidden
	}
	tx, err := s.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("workspace: begin read: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	accesses, err := s.lockAccessesTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, false, nil)
	if err != nil {
		return nil, err
	}
	access, ok := accesses[workspaceID]
	if !ok || (access.Role != WorkspaceRoleOwner && access.Role != WorkspaceRoleAdmin && access.Role != WorkspaceRoleMember) {
		return nil, ErrForbidden
	}
	row, err := s.db.queryWorkspaceLockedTx(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound
	}
	count, err := s.db.countActiveWorkspaceMembersTx(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	result := workspacePublic(row, access.Role, count)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit read: %w", err)
	}
	return result, nil
}

func workspacePublic(row *workspaceModel, role string, memberCount int64) *Workspace {
	if row == nil {
		return nil
	}
	return &Workspace{
		WorkspaceID:   row.WorkspaceID,
		Name:          row.Name,
		Description:   row.Description,
		Logo:          row.Logo,
		OwnerUID:      row.OwnerUID,
		SpaceID:       row.SpaceID,
		MemberCount:   memberCount,
		Status:        row.Status,
		WorkspaceRole: role,
		CreatedAt:     formatWorkspaceTime(row.CreatedAt),
		UpdatedAt:     formatWorkspaceTime(row.UpdatedAt),
	}
}

func validateWorkspaceName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > maxWorkspaceNameChars {
		return "", ErrRequestInvalid
	}
	return value, nil
}

func validateWorkspaceProfile(description, logo string) (string, string, error) {
	if utf8.RuneCountInString(description) > maxWorkspaceDescriptionChars {
		return "", "", ErrRequestInvalid
	}
	logo, err := validateWorkspaceLogo(logo)
	if err != nil {
		return "", "", err
	}
	return description, logo, nil
}

func validateWorkspaceLogo(logo string) (string, error) {
	if logo == "" {
		return logo, nil
	}
	u, err := url.ParseRequestURI(logo)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", ErrRequestInvalid
	}
	return logo, nil
}

func storageRole(role string) int {
	if role == WorkspaceRoleAdmin {
		return MemberRoleAdmin
	}
	return MemberRoleMember
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
