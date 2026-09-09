package project

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
)

const maxCollaborationRoleNameChars = 32

var (
	errCollaborationRoleNameInvalid   = errors.New("project: collaboration role name is invalid")
	errCollaborationRoleInvalid       = errors.New("project: collaboration role is invalid")
	errCollaborationRoleTargetInvalid = errors.New("project: collaboration role target is invalid")
	errCollaborationRoleDuplicated    = errors.New("project: collaboration role name already exists")
	errQuotaCollaborationRoles        = errors.New("project: collaboration role quota reached")
	errQuotaMemberCollaborationRoles  = errors.New("project: member collaboration role quota reached")
)

type builtinCollaborationRole struct {
	Key  string
	Name string
}

var builtinCollaborationRoles = []builtinCollaborationRole{
	{Key: "product", Name: "产品"},
	{Key: "frontend", Name: "前端"},
	{Key: "backend", Name: "后端"},
	{Key: "hr", Name: "HR"},
}

func normalizeCollaborationRoleName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func validateCollaborationRoleName(name string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxCollaborationRoleNameChars {
		return "", "", errCollaborationRoleNameInvalid
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", "", errCollaborationRoleNameInvalid
		}
	}
	return name, normalizeCollaborationRoleName(name), nil
}

func validateCollaborationRoleIDs(roleIDs []string, max int) ([]string, error) {
	if roleIDs == nil {
		return nil, errCollaborationRoleInvalid
	}
	seen := make(map[string]struct{}, len(roleIDs))
	out := make([]string, 0, len(roleIDs))
	for _, roleID := range roleIDs {
		roleID = strings.TrimSpace(roleID)
		if roleID == "" || len(roleID) > 40 {
			return nil, errCollaborationRoleInvalid
		}
		if _, exists := seen[roleID]; exists {
			return nil, errCollaborationRoleInvalid
		}
		seen[roleID] = struct{}{}
		out = append(out, roleID)
	}
	if len(out) > max {
		return nil, errQuotaMemberCollaborationRoles
	}
	return out, nil
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}

func collaborationRoleToResp(role CollaborationRoleModel) CollaborationRoleResp {
	return CollaborationRoleResp{
		RoleID: role.RoleID, BuiltinKey: role.BuiltinKey, Name: role.Name, Source: role.Source,
	}
}

func (p *Project) listCollaborationRoles(projectID string) ([]CollaborationRoleResp, int64, error) {
	roles, epoch, err := p.db.collaborationRoleCatalog(projectID)
	if err != nil {
		return nil, 0, err
	}
	result := make([]CollaborationRoleResp, 0, len(roles))
	for _, role := range roles {
		result = append(result, collaborationRoleToResp(role))
	}
	return result, epoch, nil
}

func (p *Project) createCollaborationRole(
	projectID, spaceID, actorUID, rawName string,
) (*CollaborationRoleModel, error) {
	name, normalizedName, err := validateCollaborationRoleName(rawName)
	if err != nil {
		return nil, err
	}
	var role *CollaborationRoleModel
	err = retryOnLockConflict(func() error {
		var attemptErr error
		role, attemptErr = p.createCollaborationRoleOnce(projectID, spaceID, actorUID, name, normalizedName)
		return attemptErr
	})
	return role, err
}

func (p *Project) createCollaborationRoleOnce(
	projectID, spaceID, actorUID, name, normalizedName string,
) (*CollaborationRoleModel, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, fmt.Errorf("project: begin create collaboration role: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID); err != nil {
		return nil, err
	}
	project, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil || project.SpaceID != spaceID {
		return nil, errProjectGone
	}
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return nil, err
	}
	if actorRole != RoleOwner {
		return nil, errPermissionDenied
	}
	count, err := p.db.countCollaborationRolesTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	if count >= p.cfg.CollaborationRoleMaxPerProject {
		return nil, errQuotaCollaborationRoles
	}
	role := &CollaborationRoleModel{
		RoleID: util.GenerUUID(), ProjectID: projectID, Name: name,
		NormalizedName: normalizedName, Source: CollaborationRoleSourceCustom,
		CreatorUID: actorUID, CreatedAt: now, UpdatedAt: now,
	}
	if err := p.db.insertCustomCollaborationRoleTx(tx, role); err != nil {
		if isDuplicateKeyErr(err) {
			return nil, errCollaborationRoleDuplicated
		}
		return nil, err
	}
	if err := p.db.bumpCollaborationRoleEpochTx(tx, projectID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit create collaboration role: %w", err)
	}
	return role, nil
}

func (p *Project) renameCollaborationRole(
	projectID, spaceID, actorUID, roleID, rawName string,
) (*CollaborationRoleModel, bool, error) {
	name, normalizedName, err := validateCollaborationRoleName(rawName)
	if err != nil {
		return nil, false, err
	}
	var role *CollaborationRoleModel
	var changed bool
	err = retryOnLockConflict(func() error {
		var attemptErr error
		role, changed, attemptErr = p.renameCollaborationRoleOnce(
			projectID, spaceID, actorUID, roleID, name, normalizedName)
		return attemptErr
	})
	return role, changed, err
}

func (p *Project) renameCollaborationRoleOnce(
	projectID, spaceID, actorUID, roleID, name, normalizedName string,
) (*CollaborationRoleModel, bool, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("project: begin rename collaboration role: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID); err != nil {
		return nil, false, err
	}
	project, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return nil, false, err
	}
	if project == nil || project.SpaceID != spaceID {
		return nil, false, errProjectGone
	}
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return nil, false, err
	}
	if actorRole != RoleOwner {
		return nil, false, errPermissionDenied
	}
	role, err := p.db.lockCollaborationRoleTx(tx, projectID, roleID)
	if err != nil {
		return nil, false, err
	}
	if role == nil || role.Source != CollaborationRoleSourceCustom {
		return nil, false, errCollaborationRoleInvalid
	}
	// The table's display-name collation is intentionally case-insensitive for
	// ordinary reads. Compare the locked values in Go so a display-only rename
	// such as designer -> Designer is not mistaken for a no-op by SQL collation.
	changed := role.Name != name || role.NormalizedName != normalizedName
	if changed {
		if err := p.db.updateCollaborationRoleTx(tx, projectID, roleID, name, normalizedName, now); err != nil {
			if isDuplicateKeyErr(err) {
				return nil, false, errCollaborationRoleDuplicated
			}
			return nil, false, err
		}
		if err := p.db.bumpCollaborationRoleEpochTx(tx, projectID); err != nil {
			return nil, false, err
		}
		role.Name = name
		role.NormalizedName = normalizedName
		role.UpdatedAt = now
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("project: commit rename collaboration role: %w", err)
	}
	return role, changed, nil
}

func (p *Project) deleteCollaborationRole(
	projectID, spaceID, actorUID, roleID string,
) (bool, error) {
	var changed bool
	err := retryOnLockConflict(func() error {
		var attemptErr error
		changed, attemptErr = p.deleteCollaborationRoleOnce(projectID, spaceID, actorUID, roleID)
		return attemptErr
	})
	return changed, err
}

func (p *Project) deleteCollaborationRoleOnce(
	projectID, spaceID, actorUID, roleID string,
) (bool, error) {
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin delete collaboration role: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	if err := p.requireSpaceSeatsTx(tx, spaceID, actorUID); err != nil {
		return false, err
	}
	project, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return false, err
	}
	if project == nil || project.SpaceID != spaceID {
		return false, errProjectGone
	}
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return false, err
	}
	if actorRole != RoleOwner {
		return false, errPermissionDenied
	}
	role, err := p.db.lockCollaborationRoleTx(tx, projectID, roleID)
	if err != nil {
		return false, err
	}
	if role == nil || role.Source != CollaborationRoleSourceCustom {
		return false, errCollaborationRoleInvalid
	}
	changed, err := p.db.deleteCollaborationRoleTx(tx, projectID, roleID)
	if err != nil {
		return false, err
	}
	if changed {
		if err := p.db.bumpCollaborationRoleEpochTx(tx, projectID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit delete collaboration role: %w", err)
	}
	return changed, nil
}

func (p *Project) replaceMemberCollaborationRoles(
	projectID, spaceID, actorUID, targetUID string, rawRoleIDs []string,
) (bool, error) {
	roleIDs, err := validateCollaborationRoleIDs(rawRoleIDs, p.cfg.CollaborationRoleMaxPerMember)
	if err != nil {
		return false, err
	}
	var changed bool
	err = retryOnLockConflict(func() error {
		var attemptErr error
		changed, attemptErr = p.replaceMemberCollaborationRolesOnce(
			projectID, spaceID, actorUID, targetUID, roleIDs)
		return attemptErr
	})
	return changed, err
}

func (p *Project) replaceMemberCollaborationRolesOnce(
	projectID, spaceID, actorUID, targetUID string, roleIDs []string,
) (bool, error) {
	now := time.Now().UTC()
	tx, err := p.db.session.Begin()
	if err != nil {
		return false, fmt.Errorf("project: begin replace member collaboration roles: %w", err)
	}
	defer tx.RollbackUnlessCommitted()

	held, err := p.lockSeatsTx(tx, spaceID, actorUID, nil, []string{targetUID})
	if err != nil {
		return false, err
	}
	project, err := p.db.lockActiveProjectTx(tx, projectID)
	if err != nil {
		return false, err
	}
	if project == nil || project.SpaceID != spaceID {
		return false, errProjectGone
	}
	actorRole, err := p.actorRoleTx(tx, projectID, actorUID)
	if err != nil {
		return false, err
	}
	if !canManageMembers(actorRole) {
		return false, errPermissionDenied
	}
	if targetUID == "" || !held[targetUID] {
		return false, errCollaborationRoleTargetInvalid
	}
	target, err := p.db.queryMemberTx(tx, projectID, targetUID)
	if err != nil {
		return false, err
	}
	if target == nil || target.Status != MemberStatusActive || target.Removing != 0 {
		return false, errCollaborationRoleTargetInvalid
	}
	class, err := p.db.queryAgentClassTx(tx, targetUID)
	if err != nil {
		return false, err
	}
	if class.IsBot {
		return false, errCollaborationRoleTargetInvalid
	}
	validRoles, err := p.db.lockCollaborationRoleIDsTx(tx, projectID, roleIDs)
	if err != nil {
		return false, err
	}
	if len(validRoles) != len(roleIDs) {
		return false, errCollaborationRoleInvalid
	}
	changed, err := p.db.replaceMemberCollaborationRolesTx(tx, projectID, targetUID, roleIDs, now)
	if err != nil {
		return false, err
	}
	if changed {
		if err := p.db.bumpCollaborationRoleEpochTx(tx, projectID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("project: commit replace member collaboration roles: %w", err)
	}
	return changed, nil
}
