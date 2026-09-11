package project

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gocraft/dbr/v2"
)

// Errors returned by the group-facing Project authorization seam. The group
// module maps these to its own localized wire errors; callers must not infer a
// missing Project from a database failure.
var (
	ErrGroupProjectInvalid       = errors.New("project: group access request invalid")
	ErrGroupProjectNotFound      = errors.New("project: group access project not found")
	ErrGroupProjectForbidden     = errors.New("project: group access forbidden")
	ErrGroupProjectSpaceConflict = errors.New("project: group access space conflict")
	ErrGroupProjectDependency    = errors.New("project: group access dependency unavailable")
)

// PrepareGroupProjectSpaceSeatRefs resolves the requested Space-member
// identities before a write transaction begins. The returned primary keys are
// advisory only: every locking helper revalidates the id/Space/UID/status
// tuple under the transaction lock.
func PrepareGroupProjectSpaceSeatRefs(session *dbr.Session, spaceID string, uids []string) (map[string]int64, error) {
	if session == nil {
		return nil, ErrGroupProjectInvalid
	}
	spaceID = strings.TrimSpace(spaceID)
	if spaceID == "" {
		return nil, ErrGroupProjectInvalid
	}
	refs, err := (&DB{session: session}).resolveSpaceSeatIDs(spaceID, uniqueGroupProjectUIDs(uids))
	if err != nil {
		return nil, fmt.Errorf("%w: prepare Space seats: %w", ErrGroupProjectDependency, err)
	}
	result := make(map[string]int64, len(refs))
	for uid, id := range refs {
		result[uid] = id
	}
	return result, nil
}

// RetryGroupProjectLockConflict reuses the Project module's bounded retry
// policy for group-facing write transactions. The callback must own one whole
// transaction attempt and must not include post-commit side effects.
func RetryGroupProjectLockConflict(fn func() error) error {
	if fn == nil {
		return ErrGroupProjectInvalid
	}
	return retryOnLockConflict(fn)
}

// IsGroupProjectLockConflict reports whether an error is one of the transient
// InnoDB lock conflicts covered by RetryGroupProjectLockConflict. It lets a
// best-effort sub-operation refuse to swallow a deadlock that already rolled
// back its containing transaction.
func IsGroupProjectLockConflict(err error) bool {
	return isRetryableTxErr(err)
}

// GroupProjectAccess is the current, transactionally revalidated Project
// membership view consumed by group relation and Project-backed group create
// paths. MemberUIDs is populated only by LockGroupProjectCreateAccessTx.
// EligibleUIDs contains active user accounts among the create candidates;
// SpaceMemberUIDs contains active seats in the Project's Space. Both lists are
// populated only for Project-backed group creation so the Group service can
// apply native external-member semantics without opening another transaction.
type GroupProjectAccess struct {
	ProjectID       string
	SpaceID         string
	ActorUID        string
	ActorRole       int
	MemberUIDs      []string
	EligibleUIDs    []string
	SpaceMemberUIDs []string
}

// LockGroupProjectAccessesTx locks every candidate Space seat and user row
// before locking any Project row. It then locks each Project and the actor's
// Project seat in deterministic ID order.
//
// prepared is used only by Project-backed group creation. Relation operations
// pass no candidates, so the actor is the sole candidate. seatRefs must have
// been prepared before the caller began its write transaction. The helper
// never starts, commits, or rolls back tx.
func LockGroupProjectAccessesTx(tx *dbr.Tx, actorUID string, projectIDs []string, expectedSpaceID string, seatRefs map[string]int64) (map[string]GroupProjectAccess, error) {
	accesses, _, _, err := lockGroupProjectAccessesWithStateTx(tx, actorUID, projectIDs, expectedSpaceID, nil, seatRefs)
	return accesses, err
}

func lockGroupProjectAccessesWithStateTx(
	tx *dbr.Tx,
	actorUID string,
	projectIDs []string,
	expectedSpaceID string,
	prepared []string,
	seatRefs map[string]int64,
) (map[string]GroupProjectAccess, map[string]bool, map[string]bool, error) {
	if tx == nil {
		return nil, nil, nil, fmt.Errorf("%w: nil transaction", ErrGroupProjectInvalid)
	}
	actorUID = strings.TrimSpace(actorUID)
	expectedSpaceID = strings.TrimSpace(expectedSpaceID)
	if actorUID == "" {
		return nil, nil, nil, ErrGroupProjectForbidden
	}
	if expectedSpaceID == "" || seatRefs == nil {
		return nil, nil, nil, ErrGroupProjectInvalid
	}
	ids := uniqueGroupProjectIDs(projectIDs)
	if len(ids) == 0 {
		return map[string]GroupProjectAccess{}, map[string]bool{}, map[string]bool{}, ErrGroupProjectInvalid
	}

	locations := make(map[string]*groupProjectLocation, len(ids))
	spaceSet := make(map[string]struct{})
	for _, projectID := range ids {
		location, err := readGroupProjectLocationTx(tx, projectID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: read Project %s: %w", ErrGroupProjectDependency, projectID, err)
		}
		if location == nil || location.Status != StatusNormal || location.SpaceID == "" {
			return nil, nil, nil, ErrGroupProjectNotFound
		}
		if location.SpaceID != expectedSpaceID {
			return nil, nil, nil, ErrGroupProjectSpaceConflict
		}
		locations[projectID] = location
		spaceSet[location.SpaceID] = struct{}{}
	}

	candidateUIDs := uniqueGroupProjectUIDs(append([]string{actorUID}, prepared...))
	spaceIDs := make([]string, 0, len(spaceSet))
	for spaceID := range spaceSet {
		spaceIDs = append(spaceIDs, spaceID)
	}
	sort.Strings(spaceIDs)
	seatKeys := make([]groupProjectSeatKey, 0, len(spaceIDs)*len(candidateUIDs))
	for _, spaceID := range spaceIDs {
		for _, uid := range candidateUIDs {
			seatKeys = append(seatKeys, groupProjectSeatKey{SpaceID: spaceID, UID: uid})
		}
	}
	seats, err := lockGroupProjectSpaceSeatsTx(tx, seatKeys, seatRefs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: lock Space seats: %w", ErrGroupProjectDependency, err)
	}
	spaces, err := lockGroupProjectSpacesTx(tx, spaceIDs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: lock Spaces: %w", ErrGroupProjectDependency, err)
	}
	eligibleUsers, err := lockGroupProjectUsersTx(tx, candidateUIDs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: lock users: %w", ErrGroupProjectDependency, err)
	}
	if !eligibleUsers[actorUID] {
		return nil, nil, nil, ErrGroupProjectForbidden
	}
	for _, location := range locations {
		if !spaces[location.SpaceID] || !seats[location.SpaceID+"\x00"+actorUID] {
			return nil, nil, nil, ErrGroupProjectForbidden
		}
	}

	result := make(map[string]GroupProjectAccess, len(ids))
	for _, projectID := range ids {
		locked, err := lockGroupProjectLocationTx(tx, projectID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: lock Project %s: %w", ErrGroupProjectDependency, projectID, err)
		}
		if locked == nil || locked.Status != StatusNormal || locked.SpaceID == "" {
			return nil, nil, nil, ErrGroupProjectNotFound
		}
		location := locations[projectID]
		if locked.SpaceID != location.SpaceID || locked.SpaceID != expectedSpaceID {
			return nil, nil, nil, ErrGroupProjectSpaceConflict
		}
		role, active, err := lockGroupProjectMemberTx(tx, projectID, locked.SpaceID, actorUID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: lock Project member: %w", ErrGroupProjectDependency, err)
		}
		if !active {
			return nil, nil, nil, ErrGroupProjectForbidden
		}
		result[projectID] = GroupProjectAccess{
			ProjectID: projectID,
			SpaceID:   locked.SpaceID,
			ActorUID:  actorUID,
			ActorRole: role,
		}
	}
	return result, seats, eligibleUsers, nil
}

// LockGroupProjectAccessTx is the single-target convenience form used by
// callers that already know they have no source Project to lock.
func LockGroupProjectAccessTx(tx *dbr.Tx, actorUID, projectID, expectedSpaceID string, seatRefs map[string]int64) (GroupProjectAccess, error) {
	accesses, err := LockGroupProjectAccessesTx(tx, actorUID, []string{projectID}, expectedSpaceID, seatRefs)
	if err != nil {
		return GroupProjectAccess{}, err
	}
	return accesses[strings.TrimSpace(projectID)], nil
}

// LockGroupProjectSpaceAccessTx locks an actor's native Space access in the
// same seat -> Space -> user order used by Project-backed writes. It is used
// for an unbound relation, where there is no Project row to lock.
func LockGroupProjectSpaceAccessTx(tx *dbr.Tx, actorUID, expectedSpaceID string, seatRefs map[string]int64) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("%w: nil transaction", ErrGroupProjectInvalid)
	}
	actorUID = strings.TrimSpace(actorUID)
	expectedSpaceID = strings.TrimSpace(expectedSpaceID)
	if actorUID == "" {
		return false, ErrGroupProjectForbidden
	}
	if expectedSpaceID == "" || seatRefs == nil {
		return false, ErrGroupProjectInvalid
	}
	seats, err := lockGroupProjectSpaceSeatsTx(tx, []groupProjectSeatKey{{SpaceID: expectedSpaceID, UID: actorUID}}, seatRefs)
	if err != nil {
		return false, fmt.Errorf("%w: lock Space seat: %w", ErrGroupProjectDependency, err)
	}
	spaces, err := lockGroupProjectSpacesTx(tx, []string{expectedSpaceID})
	if err != nil {
		return false, fmt.Errorf("%w: lock Space: %w", ErrGroupProjectDependency, err)
	}
	users, err := lockGroupProjectUsersTx(tx, []string{actorUID})
	if err != nil {
		return false, fmt.Errorf("%w: lock user: %w", ErrGroupProjectDependency, err)
	}
	return seats[expectedSpaceID+"\x00"+actorUID] && spaces[expectedSpaceID] && users[actorUID], nil
}

// LockGroupProjectCreateAccessTx obtains actor authorization and the current
// effective Project-member snapshot under one transaction. prepared is the
// advisory candidate set from the preparation phase; expanded reports whether
// a new Project member appeared after that phase and requires retrying.
func LockGroupProjectCreateAccessTx(tx *dbr.Tx, actorUID, projectID, expectedSpaceID string, prepared []string, seatRefs map[string]int64) (access GroupProjectAccess, expanded bool, err error) {
	accesses, seats, eligibleUsers, err := lockGroupProjectAccessesWithStateTx(
		tx, actorUID, []string{projectID}, expectedSpaceID, prepared, seatRefs,
	)
	if err != nil {
		return GroupProjectAccess{}, false, err
	}
	access = accesses[strings.TrimSpace(projectID)]
	candidates := uniqueGroupProjectUIDs(append([]string{actorUID}, prepared...))
	access.EligibleUIDs = make([]string, 0, len(candidates))
	access.SpaceMemberUIDs = make([]string, 0, len(candidates))
	for _, uid := range candidates {
		if eligibleUsers[uid] {
			access.EligibleUIDs = append(access.EligibleUIDs, uid)
		}
		if seats[access.SpaceID+"\x00"+uid] {
			access.SpaceMemberUIDs = append(access.SpaceMemberUIDs, uid)
		}
	}
	sort.Strings(access.EligibleUIDs)
	sort.Strings(access.SpaceMemberUIDs)
	members, err := lockGroupProjectMemberUIDsTx(tx, access.ProjectID, access.SpaceID)
	if err != nil {
		return GroupProjectAccess{}, false, fmt.Errorf("%w: snapshot Project members: %w", ErrGroupProjectDependency, err)
	}
	preparedSet := make(map[string]struct{}, len(prepared)+1)
	preparedSet[strings.TrimSpace(actorUID)] = struct{}{}
	for _, uid := range prepared {
		if uid = strings.TrimSpace(uid); uid != "" {
			preparedSet[uid] = struct{}{}
		}
	}
	access.MemberUIDs = make([]string, 0, len(members))
	for _, uid := range members {
		if _, ok := preparedSet[uid]; !ok {
			expanded = true
		}
		if seats[access.SpaceID+"\x00"+uid] && eligibleUsers[uid] {
			access.MemberUIDs = append(access.MemberUIDs, uid)
		}
	}
	sort.Strings(access.MemberUIDs)
	return access, expanded, nil
}

// ListActiveProjectMemberUIDs returns a non-locking snapshot for create
// preparation. It is advisory only; the create path must call
// LockGroupProjectCreateAccessTx before writing anything.
func ListActiveProjectMemberUIDs(ctxDB *dbr.Session, projectID string) ([]string, string, error) {
	if ctxDB == nil || strings.TrimSpace(projectID) == "" {
		return nil, "", ErrGroupProjectInvalid
	}
	location, err := readGroupProjectLocationSession(ctxDB, strings.TrimSpace(projectID))
	if err != nil {
		return nil, "", fmt.Errorf("%w: read Project: %w", ErrGroupProjectDependency, err)
	}
	if location == nil || location.Status != StatusNormal || location.SpaceID == "" {
		return nil, "", ErrGroupProjectNotFound
	}
	var uids []string
	_, err = ctxDB.SelectBySql(
		"SELECT uid FROM `octo_project_member` WHERE project_id = ? AND status = ? AND removing = 0 ORDER BY uid",
		projectID, MemberStatusActive,
	).Load(&uids)
	if err != nil {
		return nil, "", fmt.Errorf("%w: list Project members: %w", ErrGroupProjectDependency, err)
	}
	return uids, location.SpaceID, nil
}

// AuthorizeGroupProjectReadTx revalidates an active Project member, active
// account, and the Project's current Space from a caller-owned repeatable-read
// transaction. It intentionally takes no locks: relation reads must not
// contend with relation writes.
func AuthorizeGroupProjectReadTx(tx *dbr.Tx, actorUID, projectID, expectedSpaceID string) (GroupProjectAccess, error) {
	if tx == nil {
		return GroupProjectAccess{}, fmt.Errorf("%w: nil transaction", ErrGroupProjectInvalid)
	}
	actorUID = strings.TrimSpace(actorUID)
	projectID = strings.TrimSpace(projectID)
	expectedSpaceID = strings.TrimSpace(expectedSpaceID)
	if actorUID == "" || projectID == "" {
		return GroupProjectAccess{}, ErrGroupProjectInvalid
	}
	var rows []struct {
		ProjectID string `db:"project_id"`
		SpaceID   string `db:"space_id"`
		Role      int    `db:"role"`
	}
	_, err := tx.SelectBySql(
		"SELECT p.project_id, p.space_id, pm.role FROM `octo_project` p "+
			"INNER JOIN `octo_project_member` pm ON pm.project_id = p.project_id AND pm.space_id = p.space_id "+
			"WHERE p.project_id = ? AND p.status = ? AND pm.uid = ? AND pm.status = ? AND pm.removing = 0",
		projectID, StatusNormal, actorUID, MemberStatusActive,
	).Load(&rows)
	if err != nil {
		return GroupProjectAccess{}, fmt.Errorf("%w: read Project member: %w", ErrGroupProjectDependency, err)
	}
	if len(rows) == 0 {
		return GroupProjectAccess{}, ErrGroupProjectNotFound
	}
	row := rows[0]
	if row.ProjectID == "" || row.SpaceID == "" {
		return GroupProjectAccess{}, ErrGroupProjectNotFound
	}
	if expectedSpaceID != "" && row.SpaceID != expectedSpaceID {
		return GroupProjectAccess{}, ErrGroupProjectSpaceConflict
	}
	activeSeat, err := queryGroupProjectSpaceSeatTx(tx, row.SpaceID, actorUID)
	if err != nil {
		return GroupProjectAccess{}, fmt.Errorf("%w: check Space seat: %w", ErrGroupProjectDependency, err)
	}
	if !activeSeat {
		return GroupProjectAccess{}, ErrGroupProjectForbidden
	}
	activeUser, err := queryGroupProjectUserEligibleTx(tx, actorUID)
	if err != nil {
		return GroupProjectAccess{}, fmt.Errorf("%w: check user: %w", ErrGroupProjectDependency, err)
	}
	if !activeUser {
		return GroupProjectAccess{}, ErrGroupProjectForbidden
	}
	return GroupProjectAccess{ProjectID: row.ProjectID, SpaceID: row.SpaceID, ActorUID: actorUID, ActorRole: row.Role}, nil
}

type groupProjectLocation struct {
	ProjectID string `db:"project_id"`
	SpaceID   string `db:"space_id"`
	Status    int    `db:"status"`
}

func readGroupProjectLocationTx(tx *dbr.Tx, projectID string) (*groupProjectLocation, error) {
	var rows []*groupProjectLocation
	_, err := tx.SelectBySql(
		"SELECT project_id, space_id, status FROM `octo_project` WHERE project_id = ? LIMIT 1",
		projectID,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func readGroupProjectLocationSession(session *dbr.Session, projectID string) (*groupProjectLocation, error) {
	var rows []*groupProjectLocation
	_, err := session.SelectBySql(
		"SELECT project_id, space_id, status FROM `octo_project` WHERE project_id = ? LIMIT 1",
		projectID,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func lockGroupProjectLocationTx(tx *dbr.Tx, projectID string) (*groupProjectLocation, error) {
	var rows []*groupProjectLocation
	_, err := tx.SelectBySql(
		"SELECT project_id, space_id, status FROM `octo_project` WHERE project_id = ? LIMIT 1 FOR UPDATE",
		projectID,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func lockGroupProjectMemberTx(tx *dbr.Tx, projectID, spaceID, uid string) (int, bool, error) {
	var rows []struct {
		Role     int `db:"role"`
		Status   int `db:"status"`
		Removing int `db:"removing"`
	}
	_, err := tx.SelectBySql(
		"SELECT role, status, removing FROM `octo_project_member` WHERE project_id = ? AND space_id = ? AND uid = ? LIMIT 1 FOR UPDATE",
		projectID, spaceID, uid,
	).Load(&rows)
	if err != nil {
		return 0, false, err
	}
	if len(rows) == 0 {
		return RoleCommon, false, nil
	}
	return rows[0].Role, rows[0].Status == MemberStatusActive && rows[0].Removing == 0, nil
}

func lockGroupProjectMemberUIDsTx(tx *dbr.Tx, projectID, spaceID string) ([]string, error) {
	var uids []string
	_, err := tx.SelectBySql(
		"SELECT uid FROM `octo_project_member` WHERE project_id = ? AND space_id = ? AND status = ? AND removing = 0 ORDER BY uid FOR SHARE",
		projectID, spaceID, MemberStatusActive,
	).Load(&uids)
	return uids, err
}

type groupProjectSeatKey struct {
	SpaceID string
	UID     string
}

func uniqueGroupProjectUIDs(uids []string) []string {
	seen := make(map[string]struct{}, len(uids))
	result := make([]string, 0, len(uids))
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
	sort.Strings(result)
	return result
}

func uniqueGroupProjectSeatKeys(keys []groupProjectSeatKey) []groupProjectSeatKey {
	seen := make(map[string]struct{}, len(keys))
	result := make([]groupProjectSeatKey, 0, len(keys))
	for _, key := range keys {
		key.SpaceID = strings.TrimSpace(key.SpaceID)
		key.UID = strings.TrimSpace(key.UID)
		if key.SpaceID == "" || key.UID == "" {
			continue
		}
		mapKey := key.SpaceID + "\x00" + key.UID
		if _, ok := seen[mapKey]; ok {
			continue
		}
		seen[mapKey] = struct{}{}
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].SpaceID == result[j].SpaceID {
			return result[i].UID < result[j].UID
		}
		return result[i].SpaceID < result[j].SpaceID
	})
	return result
}

func groupProjectPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

type groupProjectSeatTarget struct {
	id      int64
	spaceID string
	uid     string
}

func lockGroupProjectSeatTargets(keys []groupProjectSeatKey, refs map[string]int64) []groupProjectSeatTarget {
	keys = uniqueGroupProjectSeatKeys(keys)
	targets := make([]groupProjectSeatTarget, 0, len(keys))
	seen := make(map[int64]struct{}, len(keys))
	for _, key := range keys {
		id, ok := refs[key.UID]
		if !ok || id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		targets = append(targets, groupProjectSeatTarget{id: id, spaceID: key.SpaceID, uid: key.UID})
	}
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].id < targets[j].id
	})
	return targets
}

// lockGroupProjectSpaceSeatsTx probes the prepared clustered primary keys in
// ascending id order. The UID is used only after the lock as an identity
// recheck; a UID-index lookup must never be used to infer lock order.
func lockGroupProjectSpaceSeatsTx(tx *dbr.Tx, keys []groupProjectSeatKey, refs map[string]int64) (map[string]bool, error) {
	held := make(map[string]bool)
	if refs == nil {
		return nil, fmt.Errorf("%w: nil prepared Space seats", ErrGroupProjectInvalid)
	}
	targets := lockGroupProjectSeatTargets(keys, refs)
	if len(targets) == 0 {
		return held, nil
	}
	args := make([]interface{}, len(targets))
	for i, target := range targets {
		args[i] = target.id
	}
	var rows []struct {
		ID      int64  `db:"id"`
		SpaceID string `db:"space_id"`
		UID     string `db:"uid"`
	}
	_, err := tx.SelectBySql(
		"SELECT sm.id, sm.space_id, sm.uid FROM `space_member` sm FORCE INDEX (PRIMARY) "+
			"WHERE sm.id IN ("+groupProjectPlaceholders(len(targets))+") AND sm.status = 1 "+
			"ORDER BY sm.id ASC FOR SHARE",
		args...,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	expected := make(map[int64]groupProjectSeatKey, len(targets))
	for _, target := range targets {
		expected[target.id] = groupProjectSeatKey{SpaceID: target.spaceID, UID: target.uid}
	}
	for _, row := range rows {
		key, ok := expected[row.ID]
		if ok && key.SpaceID == row.SpaceID && key.UID == row.UID {
			held[row.SpaceID+"\x00"+row.UID] = true
		}
	}
	return held, nil
}

func lockGroupProjectSpacesTx(tx *dbr.Tx, spaceIDs []string) (map[string]bool, error) {
	spaceIDs = uniqueGroupProjectIDs(spaceIDs)
	active := make(map[string]bool, len(spaceIDs))
	if len(spaceIDs) == 0 {
		return active, nil
	}
	args := make([]interface{}, len(spaceIDs))
	for i, spaceID := range spaceIDs {
		args[i] = spaceID
	}
	var rows []struct {
		SpaceID string `db:"space_id"`
	}
	_, err := tx.SelectBySql(
		"SELECT space_id FROM `space` WHERE status = 1 AND space_id IN ("+
			groupProjectPlaceholders(len(spaceIDs))+") ORDER BY space_id FOR SHARE",
		args...,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		active[row.SpaceID] = true
	}
	return active, nil
}

func lockGroupProjectUsersTx(tx *dbr.Tx, uids []string) (map[string]bool, error) {
	uids = uniqueGroupProjectUIDs(uids)
	eligible := make(map[string]bool, len(uids))
	if len(uids) == 0 {
		return eligible, nil
	}
	args := make([]interface{}, len(uids))
	for i, uid := range uids {
		args[i] = uid
	}
	var rows []struct {
		UID       string `db:"uid"`
		Status    int    `db:"status"`
		IsDestroy int    `db:"is_destroy"`
	}
	_, err := tx.SelectBySql(
		"SELECT uid, status, IFNULL(is_destroy, 0) AS is_destroy FROM `user` WHERE uid IN ("+
			groupProjectPlaceholders(len(uids))+") ORDER BY uid FOR SHARE",
		args...,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Status == 1 && row.IsDestroy != 2 {
			eligible[row.UID] = true
		}
	}
	return eligible, nil
}

func queryGroupProjectSpaceSeatTx(tx *dbr.Tx, spaceID, uid string) (bool, error) {
	var rows []int
	_, err := tx.SelectBySql(
		"SELECT 1 FROM `space_member` sm INNER JOIN `space` s ON s.space_id = sm.space_id "+
			"WHERE sm.space_id = ? AND sm.uid = ? AND sm.status = 1 AND s.status = 1 LIMIT 1",
		spaceID, uid,
	).Load(&rows)
	return len(rows) > 0, err
}

func queryGroupProjectUserEligibleTx(tx *dbr.Tx, uid string) (bool, error) {
	var rows []struct {
		Status    int `db:"status"`
		IsDestroy int `db:"is_destroy"`
	}
	_, err := tx.SelectBySql(
		"SELECT status, IFNULL(is_destroy, 0) AS is_destroy FROM `user` WHERE uid = ? LIMIT 1",
		uid,
	).Load(&rows)
	if err != nil {
		return false, err
	}
	return len(rows) > 0 && rows[0].Status == 1 && rows[0].IsDestroy != 2, nil
}

func uniqueGroupProjectIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func groupProjectCandidateExpanded(prepared, current []string) bool {
	preparedSet := make(map[string]struct{}, len(prepared))
	for _, uid := range prepared {
		if uid = strings.TrimSpace(uid); uid != "" {
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
