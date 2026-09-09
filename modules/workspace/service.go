package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/base/outbox"
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
// transaction-aware authorization seams below.
type Service struct {
	ctx     *config.Context
	db      *DB
	session *dbr.Session
}

func NewService(ctx *config.Context) *Service {
	RegisterEventTargets()
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
	keyword = strings.TrimSpace(keyword)
	if utf8.RuneCountInString(keyword) > maxWorkspaceKeywordChars {
		return nil, ErrRequestInvalid
	}
	if scope.ExpectedSpaceID != "" && scope.ExpectedSpaceID != spaceID {
		return nil, ErrSpaceRequired
	}

	tx, err := s.session.BeginTx(context.Background(), &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: begin list read: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	eligible, err := s.db.queryUserEligibleTx(tx, scope.UID)
	if err != nil {
		return nil, err
	}
	if !eligible {
		return nil, ErrForbidden
	}
	active, err := s.db.querySpaceActiveTx(tx, spaceID)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrNotFound
	}
	member, err := s.db.querySpaceMemberActiveTx(tx, scope.UID, spaceID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, ErrNotFound
	}
	result, err := s.db.listWorkspacesTx(tx, spaceID, scope.UID, keyword, page)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit list read: %w", err)
	}
	return result, nil
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
	seats, err := s.db.lockSpaceSeatsTx(tx, []SpaceSeatKey{{SpaceID: spaceID, UID: scope.UID}})
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
	eventID, err := enqueueWorkspaceEventTx(tx, eventTypeWorkspaceCreated, row.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: enqueue create event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit create: %w", err)
	}
	outbox.DeliverNow(eventID)
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
	accesses, err := s.lockAccessesTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, nil)
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
	var eventID string
	if changed {
		eventID, err = enqueueWorkspaceEventTx(tx, eventTypeWorkspaceUpdated, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("workspace: enqueue update event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit update: %w", err)
	}
	if changed {
		outbox.DeliverNow(eventID)
	}
	return result, nil
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
	workspaceID = strings.TrimSpace(workspaceID)
	tx, err := s.session.BeginTx(context.Background(), &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: begin members read: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	access, err := s.readAccessTx(tx, scope.UID, workspaceID, scope.ExpectedSpaceID)
	if err != nil {
		return nil, err
	}
	if access.Role != WorkspaceRoleOwner && access.Role != WorkspaceRoleAdmin && access.Role != WorkspaceRoleMember {
		return nil, ErrForbidden
	}
	result, err := s.db.listMembersTx(tx, workspaceID, filter, page)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit members read: %w", err)
	}
	return result, nil
}

func (s *Service) Member(scope Scope, workspaceID, uid string) (*Member, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	workspaceID = strings.TrimSpace(workspaceID)
	tx, err := s.session.BeginTx(context.Background(), &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: begin member read: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	access, err := s.readAccessTx(tx, scope.UID, workspaceID, scope.ExpectedSpaceID)
	if err != nil {
		return nil, err
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil, ErrRequestInvalid
	}
	member, err := s.db.queryMemberTx(tx, workspaceID, uid)
	if err != nil {
		return nil, err
	}
	if member == nil {
		return nil, ErrNotFound
	}
	if member.Role != MemberRoleMember && member.Role != MemberRoleAdmin {
		return nil, fmt.Errorf("workspace: invalid member role: %w", ErrForbidden)
	}
	result, err := s.db.membersPublicFromModelsTx(tx, []*workspaceMemberModel{member}, access.OwnerUID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit member read: %w", err)
	}
	return &result[0], nil
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
	accesses, seats, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, candidateUIDs)
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
	admittedAny := false
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
		changed, err := s.db.admitMemberTx(tx, admitted)
		if err != nil {
			return nil, err
		}
		if changed {
			admittedAny = true
		}
		resultRows = append(resultRows, admitted)
	}
	result, err := s.db.membersPublicFromModelsTx(tx, resultRows, access.OwnerUID)
	if err != nil {
		return nil, err
	}
	eventID := ""
	if admittedAny {
		eventID, err = enqueueWorkspaceEventTx(tx, eventTypeWorkspaceMembersChanged, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("workspace: enqueue add members event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit add members: %w", err)
	}
	if admittedAny {
		outbox.DeliverNow(eventID)
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
	accesses, seats, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, []string{uid})
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
	roleChanged := false
	if projectedRole(access.OwnerUID, uid, target.Role) != role {
		now := time.Now().UTC()
		if err := s.db.updateMemberRoleTx(tx, workspaceID, uid, storageRole(role), now); err != nil {
			return nil, err
		}
		target.Role = storageRole(role)
		target.UpdatedAt = now
		roleChanged = true
	}
	result, err := s.db.membersPublicFromModelsTx(tx, []*workspaceMemberModel{target}, access.OwnerUID)
	if err != nil {
		return nil, err
	}
	eventID := ""
	if roleChanged {
		eventID, err = enqueueWorkspaceEventTx(tx, eventTypeWorkspaceMembersChanged, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("workspace: enqueue member role event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit member role update: %w", err)
	}
	if roleChanged {
		outbox.DeliverNow(eventID)
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
	accesses, _, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, []string{uid})
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
	eventID, err := enqueueWorkspaceEventTx(tx, eventTypeWorkspaceMembersChanged, workspaceID)
	if err != nil {
		return fmt.Errorf("workspace: enqueue remove member event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workspace: commit remove member: %w", err)
	}
	outbox.DeliverNow(eventID)
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
	accesses, seats, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, []string{uid})
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
	eventID, err := enqueueWorkspaceEventTx(tx, eventTypeWorkspaceOwnerTransferred, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: enqueue owner transfer event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workspace: commit owner transfer: %w", err)
	}
	outbox.DeliverNow(eventID)
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
	accesses, _, lockedMembers, err := s.lockAccessesWithStateTx(tx, scope.UID, []string{workspaceID}, scope.ExpectedSpaceID, []string{scope.UID})
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
	eventID, err := enqueueWorkspaceEventTx(tx, eventTypeWorkspaceMembersChanged, workspaceID)
	if err != nil {
		return fmt.Errorf("workspace: enqueue leave event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workspace: commit leave: %w", err)
	}
	outbox.DeliverNow(eventID)
	return nil
}

// LockAccessesTx locks the current authorization facts needed by a caller
// operating on Workspace resources. It never starts or commits a transaction.
// IDs are de-duplicated and sorted before Workspace/member locking. Group code
// must pass every source and target ID in one call, avoiding inverted order.
func (s *Service) LockAccessesTx(tx *dbr.Tx, actorUID string, workspaceIDs []string, expectedSpaceID string) (map[string]Access, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.lockAccessesTx(tx, strings.TrimSpace(actorUID), workspaceIDs, strings.TrimSpace(expectedSpaceID), nil)
}

func (s *Service) lockAccessesTx(tx *dbr.Tx, actorUID string, workspaceIDs []string, expectedSpaceID string, memberUIDs []string) (map[string]Access, error) {
	accesses, _, _, err := s.lockAccessesWithStateTx(tx, actorUID, workspaceIDs, expectedSpaceID, memberUIDs)
	return accesses, err
}

func (s *Service) lockAccessesWithStateTx(tx *dbr.Tx, actorUID string, workspaceIDs []string, expectedSpaceID string, memberUIDs []string) (map[string]Access, map[string]bool, map[string]*workspaceMemberModel, error) {
	if err := s.db.ensureTx(tx); err != nil {
		return nil, nil, nil, err
	}
	actorUID = strings.TrimSpace(actorUID)
	if actorUID == "" {
		return nil, nil, nil, ErrForbidden
	}
	workspaceIDs = uniqueSorted(workspaceIDs)
	if len(workspaceIDs) == 0 {
		return map[string]Access{}, map[string]bool{}, map[string]*workspaceMemberModel{}, nil
	}
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
	seatUIDs := uniqueSorted(append([]string{actorUID}, memberUIDs...))
	seatKeys := make([]SpaceSeatKey, 0, len(spaceIDs)*len(seatUIDs))
	for _, spaceID := range spaceIDs {
		for _, uid := range seatUIDs {
			seatKeys = append(seatKeys, SpaceSeatKey{SpaceID: spaceID, UID: uid})
		}
	}
	seats, err := s.db.lockSpaceSeatsTx(tx, seatKeys)
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
	memberLockUIDs := make([]string, 0, len(memberUIDs)+len(workspaceIDs)+1)
	memberLockUIDs = append(memberLockUIDs, actorUID)
	memberLockUIDs = append(memberLockUIDs, memberUIDs...)
	for _, workspaceID := range workspaceIDs {
		row := lockedWorkspaces[workspaceID]
		if row != nil {
			memberLockUIDs = append(memberLockUIDs, row.OwnerUID)
		}
	}
	memberLockUIDs = uniqueSorted(memberLockUIDs)
	memberKeys := make([]workspaceMemberKey, 0, len(workspaceIDs)*len(memberLockUIDs))
	for _, workspaceID := range workspaceIDs {
		for _, uid := range memberLockUIDs {
			memberKeys = append(memberKeys, workspaceMemberKey{WorkspaceID: workspaceID, UID: uid})
		}
	}
	lockedMembers, err := s.db.lockMemberRowsTx(tx, memberKeys)
	if err != nil {
		return nil, nil, nil, err
	}
	userUIDs := uniqueSorted(append([]string{actorUID}, memberUIDs...))
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
		accesses[workspaceID] = Access{
			WorkspaceID: workspaceID,
			SpaceID:     row.SpaceID,
			OwnerUID:    row.OwnerUID,
			Role:        role,
		}
	}
	return accesses, seats, lockedMembers, nil
}

// LockAccessesForGroupTx revalidates and returns the authorization facts a
// group-creation caller needs for one target Workspace. seatKeys is the
// de-duplicated prepared (space_id, uid) key set; no all-Space scanning path
// exists. It never starts or commits a transaction.
func (s *Service) LockAccessesForGroupTx(tx *dbr.Tx, actorUID, workspaceID string, seatKeys []SpaceSeatKey) (GroupAccess, error) {
	var access GroupAccess
	if err := s.usable(); err != nil {
		return access, err
	}
	if err := s.db.ensureTx(tx); err != nil {
		return access, err
	}
	actorUID = strings.TrimSpace(actorUID)
	workspaceID = strings.TrimSpace(workspaceID)
	if actorUID == "" {
		return access, ErrForbidden
	}
	if workspaceID == "" {
		return access, ErrRequestInvalid
	}
	seatKeys = uniqueSpaceSeatKeys(seatKeys)
	if len(seatKeys) == 0 {
		return access, ErrCandidateIneligible
	}
	targetSpaceID := seatKeys[0].SpaceID
	for _, key := range seatKeys[1:] {
		if key.SpaceID != targetSpaceID {
			return access, ErrSpaceRequired
		}
	}

	// Lock in repository order: exact Space seats, Space, Workspace, then
	// exact prepared Workspace member rows.
	seats, err := s.db.lockSpaceSeatsTx(tx, seatKeys)
	if err != nil {
		return access, err
	}
	activeSpaces, err := s.db.lockSpacesTx(tx, []string{targetSpaceID})
	if err != nil {
		return access, err
	}
	if !activeSpaces[targetSpaceID] {
		return access, ErrNotFound
	}
	lockedWorkspaces, err := s.db.lockWorkspaceRowsTx(tx, []string{workspaceID})
	if err != nil {
		return access, err
	}
	row := lockedWorkspaces[workspaceID]
	if row == nil {
		return access, ErrNotFound
	}
	if row.SpaceID != targetSpaceID {
		return access, ErrSpaceRequired
	}
	preparedUIDs := make(map[string]struct{}, len(seatKeys))
	for _, key := range seatKeys {
		preparedUIDs[key.UID] = struct{}{}
	}
	memberKeys := make([]workspaceMemberKey, 0, len(preparedUIDs)+2)
	memberKeys = append(memberKeys, workspaceMemberKey{WorkspaceID: workspaceID, UID: actorUID})
	memberKeys = append(memberKeys, workspaceMemberKey{WorkspaceID: workspaceID, UID: row.OwnerUID})
	for uid := range preparedUIDs {
		memberKeys = append(memberKeys, workspaceMemberKey{WorkspaceID: workspaceID, UID: uid})
	}
	lockedMembers, err := s.db.lockMemberRowsTx(tx, memberKeys)
	if err != nil {
		return access, err
	}
	currentMembers, err := s.db.queryActiveWorkspaceMembersTx(tx, workspaceID)
	if err != nil {
		return access, err
	}
	userUIDs := make([]string, 0, len(currentMembers)+1)
	userUIDs = append(userUIDs, actorUID)
	for _, member := range currentMembers {
		if member != nil {
			userUIDs = append(userUIDs, member.UID)
		}
	}
	eligibleUsers, err := s.db.lockEligibleUsersTx(tx, userUIDs)
	if err != nil {
		return access, err
	}
	if !seats[targetSpaceID+"\x00"+actorUID] {
		return access, ErrForbidden
	}
	actor := lockedMembers[workspaceID+"\x00"+actorUID]
	owner := lockedMembers[workspaceID+"\x00"+row.OwnerUID]
	if actor == nil || actor.Status != MemberStatusActive || owner == nil || owner.Status != MemberStatusActive {
		return access, ErrForbidden
	}
	if !eligibleUsers[actorUID] {
		return access, ErrForbidden
	}
	if (actor.Role != MemberRoleMember && actor.Role != MemberRoleAdmin) ||
		(owner.Role != MemberRoleMember && owner.Role != MemberRoleAdmin) {
		return access, ErrForbidden
	}
	role := projectedRole(row.OwnerUID, actorUID, actor.Role)
	if role != WorkspaceRoleOwner && role != WorkspaceRoleAdmin && role != WorkspaceRoleMember {
		return access, ErrForbidden
	}
	access = GroupAccess{
		WorkspaceID:        workspaceID,
		SpaceID:            row.SpaceID,
		OwnerUID:           row.OwnerUID,
		Role:               role,
		MemberUIDs:         make([]string, 0, len(currentMembers)),
		EligibleMemberUIDs: make([]string, 0, len(currentMembers)),
	}
	for _, member := range currentMembers {
		if member == nil || member.Status != MemberStatusActive {
			continue
		}
		if member.Role != MemberRoleMember && member.Role != MemberRoleAdmin {
			return GroupAccess{}, ErrForbidden
		}
		access.MemberUIDs = append(access.MemberUIDs, member.UID)
		if seats[targetSpaceID+"\x00"+member.UID] && eligibleUsers[member.UID] {
			access.EligibleMemberUIDs = append(access.EligibleMemberUIDs, member.UID)
		}
	}
	sort.Strings(access.MemberUIDs)
	sort.Strings(access.EligibleMemberUIDs)
	return access, nil
}

// ListActiveMemberUIDs returns the active member candidates used by a
// group-creation preparation phase. It intentionally performs no locking and
// does not start a caller-owned transaction.
func (s *Service) ListActiveMemberUIDs(workspaceID string) ([]string, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, ErrNotFound
	}
	workspace, err := s.db.queryActiveWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if workspace == nil {
		return nil, ErrNotFound
	}
	rows, err := s.db.queryActiveWorkspaceMembers(workspaceID)
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			uids = append(uids, row.UID)
		}
	}
	return uids, nil
}

// AuthorizeWorkspaceTx revalidates read authorization using the caller-owned
// transaction. It never starts, commits, or rolls back the transaction.
func (s *Service) AuthorizeWorkspaceTx(tx *dbr.Tx, actorUID, workspaceID, expectedSpaceID string) (Access, error) {
	if err := s.usable(); err != nil {
		return Access{}, err
	}
	return s.readAccessTx(tx, strings.TrimSpace(actorUID), strings.TrimSpace(workspaceID), strings.TrimSpace(expectedSpaceID))
}

func (s *Service) readAccessTx(tx *dbr.Tx, actorUID, workspaceID, expectedSpaceID string) (Access, error) {
	var access Access
	if err := s.db.ensureTx(tx); err != nil {
		return access, err
	}
	actorUID = strings.TrimSpace(actorUID)
	workspaceID = strings.TrimSpace(workspaceID)
	expectedSpaceID = strings.TrimSpace(expectedSpaceID)
	if actorUID == "" {
		return access, ErrForbidden
	}
	if workspaceID == "" {
		return access, ErrRequestInvalid
	}
	row, err := s.db.queryWorkspaceTx(tx, workspaceID)
	if err != nil {
		return access, err
	}
	if row == nil || row.Status != WorkspaceStatusActive || row.SpaceID == "" {
		return access, ErrNotFound
	}
	activeSpace, err := s.db.querySpaceActiveTx(tx, row.SpaceID)
	if err != nil {
		return access, err
	}
	if !activeSpace {
		return access, ErrNotFound
	}
	if expectedSpaceID != "" && row.SpaceID != expectedSpaceID {
		return access, ErrSpaceRequired
	}
	activeSeat, err := s.db.querySpaceMemberActiveTx(tx, actorUID, row.SpaceID)
	if err != nil {
		return access, err
	}
	if !activeSeat {
		return access, ErrForbidden
	}
	actor, err := s.db.queryWorkspaceMemberReadTx(tx, workspaceID, actorUID)
	if err != nil {
		return access, err
	}
	owner, err := s.db.queryWorkspaceMemberReadTx(tx, workspaceID, row.OwnerUID)
	if err != nil {
		return access, err
	}
	if actor == nil || actor.Status != MemberStatusActive || owner == nil || owner.Status != MemberStatusActive {
		return access, ErrForbidden
	}
	if owner.Role != MemberRoleMember && owner.Role != MemberRoleAdmin {
		return access, ErrForbidden
	}
	eligible, err := s.db.queryUserEligibleTx(tx, actorUID)
	if err != nil {
		return access, err
	}
	if !eligible {
		return access, ErrForbidden
	}
	if actor.Role != MemberRoleMember && actor.Role != MemberRoleAdmin {
		return access, ErrForbidden
	}
	role := projectedRole(row.OwnerUID, actorUID, actor.Role)
	if role != WorkspaceRoleOwner && role != WorkspaceRoleAdmin && role != WorkspaceRoleMember {
		return access, ErrForbidden
	}
	return Access{
		WorkspaceID: workspaceID,
		SpaceID:     row.SpaceID,
		OwnerUID:    row.OwnerUID,
		Role:        role,
	}, nil
}

// ReadWorkspaceTx returns an authorized Workspace projection using the
// caller-owned transaction and one repeatable-read snapshot. It never starts,
// commits, or rolls back the transaction.
func (s *Service) ReadWorkspaceTx(tx *dbr.Tx, scope Scope, workspaceID string) (*Workspace, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	scope.UID = strings.TrimSpace(scope.UID)
	scope.ExpectedSpaceID = strings.TrimSpace(scope.ExpectedSpaceID)
	workspaceID = strings.TrimSpace(workspaceID)
	access, err := s.readAccessTx(tx, scope.UID, workspaceID, scope.ExpectedSpaceID)
	if err != nil {
		return nil, err
	}
	row, err := s.db.queryWorkspaceTx(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	if row == nil || row.Status != WorkspaceStatusActive {
		return nil, ErrNotFound
	}
	count, err := s.db.countActiveWorkspaceMembersReadTx(tx, workspaceID)
	if err != nil {
		return nil, err
	}
	return workspacePublic(row, access.Role, count), nil
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
	tx, err := s.session.BeginTx(context.Background(), &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: begin read: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	result, err := s.ReadWorkspaceTx(tx, scope, workspaceID)
	if err != nil {
		return nil, err
	}
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

// InternalGet returns an active Workspace in the service-facing projection.
// It performs no caller or Space membership authorization; the internal API
// authenticates the calling platform service instead.
func (s *Service) InternalGet(workspaceID string) (*InternalWorkspace, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, ErrNotFound
	}
	row, err := s.db.queryActiveWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrNotFound
	}
	memberCount, err := s.db.countActiveWorkspaceMembers(workspaceID)
	if err != nil {
		return nil, err
	}
	return internalWorkspacePublic(row, memberCount), nil
}

// InternalList enumerates active Workspaces for a platform service. An empty
// spaceID intentionally means all Spaces; internal callers are trusted to
// perform their own downstream user authorization.
func (s *Service) InternalList(spaceID string, page Page) (*Pagination[InternalWorkspace], error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.db.listInternalWorkspaces(strings.TrimSpace(spaceID), page)
}

// InternalMembers returns service-facing membership data without a caller
// projection. Filter validation mirrors the user-facing Members method.
func (s *Service) InternalMembers(workspaceID string, filter MemberFilter, page Page) (*Pagination[Member], error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	if filter.Status != MemberStatusInactive && filter.Status != MemberStatusActive {
		return nil, ErrRequestInvalid
	}
	for _, role := range filter.Roles {
		if role != WorkspaceRoleOwner && role != WorkspaceRoleAdmin && role != WorkspaceRoleMember {
			return nil, ErrRoleInvalid
		}
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, ErrRequestInvalid
	}
	return s.db.listInternalMembers(workspaceID, filter, page)
}
