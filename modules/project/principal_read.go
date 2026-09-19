// Package project · cross-module Project read for non-user principals.
//
// A Bot reads Projects through the Bot API (modules/bot_api/projects.go), but it
// cannot go through the user read entry points:
//
//   - readProjects / readProject are package-private, and modules/bot_api must
//     not re-implement the read predicates.
//   - Their seat predicate is "the caller's own active seat", and a Bot usually
//     has no Project seat at all: the Bot participates as its owner's agent
//     (robot.creator_uid), and the owner's seats are what make those Projects
//     real for it.
//
// So this file is the boundary, in the same shape as
// ListProjectGroupRelationsByProjectIDs (db_group.go): an exported package-level
// read that builds its own database façade and returns the module's own
// projection. What it does NOT do is weaken anything — the Space gate, the seat
// gate and the not-found collapse are the existing predicates, and the seat set
// only extends WHOSE seat can make a Project readable.
//
// The seat set is ordered: the first uid with an active seat supplies my_role, so
// modules/bot_api passes the Bot itself before its owner and the Bot's own role
// wins wherever it has one.
//
// Every uid in the seat set must ALSO clear the caller's own Space gate right
// now — an active `space_member` seat in spaceID plus a valid account
// (principalSeatsWithLiveSpaceAccessTx). A seat row on its own is not evidence
// of current membership: seats close ASYNCHRONOUSLY when their holder leaves the
// Space, and nothing at all closes them when the holder's account is destroyed.
// Without that check, visibility derived from the owner would outlive the
// owner's own access.
package project

import (
	"fmt"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/gocraft/dbr/v2"
)

// ReadProjectsForPrincipal lists the Projects a non-user principal may read
// inside spaceID, as the same Resp projection the user-facing list returns.
//
// Authorization, in order:
//
//  1. callerUID must be an eligible account with an active `space_member` seat in
//     an active Space — the existing projectReadSpaceAccessTx predicate. It is
//     the caller's OWN Space membership that scopes the request; the seat set
//     does not widen it.
//  2. Every uid in seatUIDs must clear that same predicate (see
//     principalSeatsWithLiveSpaceAccessTx): a seat counts only while its holder
//     is still a live member of spaceID.
//  3. A Project is readable when at least one surviving uid in seatUIDs holds an
//     active seat in it. Seat membership is read live from
//     `octo_project_member` (status active AND removing = 0), which is also what
//     closes a seat when its holder leaves the Project.
//
// page is 1-based; limit <= 0 selects the module default and both are clamped to
// the module caps, so a hostile page/limit cannot reach the query unclamped.
// Caller errors: ErrProjectReadForbidden when the caller has no seat in spaceID,
// ErrProjectReadNotFound for an empty/invalid principal, and wrapped internal
// errors otherwise.
func ReadProjectsForPrincipal(
	ctx *config.Context, spaceID, callerUID, keyword string, seatUIDs []string, page, limit int64,
) ([]*Resp, int64, error) {
	p := principalReadFacade(ctx)
	result, err := p.readProjectsForPrincipal(
		spaceID, callerUID, keyword, normalizePrincipalSeatUIDs(seatUIDs), principalReadPage(page, limit),
	)
	if err != nil {
		return nil, 0, err
	}
	// Space role is roleNonMember for list rows exactly as the user-facing list
	// handler passes it: capabilities derive from the Project seat alone there.
	resps := make([]*Resp, 0, len(result.Rows))
	for _, row := range result.Rows {
		resps = append(resps, p.toResp(&row.Model, row.MyRole, roleNonMember, row.Humans, row.Agents, row.Pinned == 1))
	}
	return resps, result.Total, nil
}

// ReadProjectForPrincipal reads one Project for a non-user principal, with the
// Space DERIVED from the Project row (the client never supplies the scope, same
// as the user-facing GET /v1/projects/:project_id).
//
// Every invisible case collapses to ErrProjectReadNotFound — absent, dissolved,
// another Space, no Space seat for callerUID, a seat uid that no longer clears
// the Space gate, no seat among seatUIDs — so the endpoint cannot be used to
// probe Project existence.
func ReadProjectForPrincipal(
	ctx *config.Context, projectID, callerUID string, seatUIDs []string,
) (*Resp, error) {
	p := principalReadFacade(ctx)
	result, err := p.readProjectForPrincipal(projectID, callerUID, normalizePrincipalSeatUIDs(seatUIDs))
	if err != nil {
		return nil, err
	}
	return p.toResp(result.Project, result.Role, result.SpaceRole, result.Humans, result.Agents, result.Pinned), nil
}

// principalReadConfig resolves the module configuration ONCE per process, the
// same posture as New(): loadConfig parses environment variables, provisioning
// targets and the lifecycle secret, none of which belong on a per-request read
// path. (The façade below is still built per call — it is a four-field struct
// around the request's db handle.)
var principalReadConfig = sync.OnceValue(loadConfig)

// principalReadFacade builds the minimal *Project the read paths need.
//
// Deliberately NOT New(): that constructor registers the Space-removal cascade
// step and starts background workers, and a second registration would execute
// them twice. The read paths touch only db (queries), cfg (max-members cap in
// toResp) and Log.
func principalReadFacade(ctx *config.Context) *Project {
	return &Project{
		ctx: ctx,
		Log: log.NewTLog("Project"),
		db:  NewDB(ctx),
		cfg: principalReadConfig(),
	}
}

// normalizePrincipalSeatUIDs trims, drops empties and de-duplicates while
// preserving order — the order IS the role precedence, so it must not be sorted.
func normalizePrincipalSeatUIDs(seatUIDs []string) []string {
	out := make([]string, 0, len(seatUIDs))
	seen := make(map[string]struct{}, len(seatUIDs))
	for _, uid := range seatUIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		out = append(out, uid)
	}
	return out
}

// principalSeatsWithLiveSpaceAccessTx narrows the seat set to the uids that
// CURRENTLY hold an active `space_member` seat in spaceID with a valid account,
// so the visibility a seat grants cannot outlive the membership it was granted
// for.
//
// A seat row alone is not enough to establish that membership: closing seats is
// an ASYNC cascade (space_member_removal.go), and between the Space removal
// committing and that step running — or permanently, once its work order reaches
// a terminal state — an `octo_project_member` row exists with status active and
// no live Space seat behind it. The owner uid has the same problem from a second
// direction: nothing closes a Project seat when the owner's ACCOUNT is destroyed
// (`modules/user` touches neither `robot` nor the seat tables), so a destroyed
// owner's bot would keep reading the owner's Projects forever.
//
// The caller is skipped — the Space gate it already passed proves exactly this
// predicate for it — and every OTHER uid is checked, including when it is the
// only uid in the set. That last case is why there is no length shortcut: this
// seam accepts arbitrary principal sets, so a lone delegated uid would otherwise
// be admitted on its seat row alone, which is the very stale-seat authorization
// (removed member, destroyed account) the check exists to stop.
//
// The check reuses the caller's point-read helper — deliberately not a join,
// because authorization must not depend on these tables sharing a collation with
// `user` / `space_member` in imported deployments. An empty result is
// fail-closed: the list returns nothing, the detail returns not-found.
func (p *Project) principalSeatsWithLiveSpaceAccessTx(
	tx *dbr.Tx, spaceID, callerUID string, seatUIDs []string,
) ([]string, error) {
	admitted := make([]string, 0, len(seatUIDs))
	for _, uid := range seatUIDs {
		if uid == callerUID {
			admitted = append(admitted, uid)
			continue
		}
		_, ok, err := p.projectReadSpaceAccessTx(tx, spaceID, uid)
		if err != nil {
			return nil, err
		}
		if ok {
			admitted = append(admitted, uid)
		}
	}
	return admitted, nil
}

// principalReadPage converts a 1-based page and a raw limit into the module's
// bounded page. Clamping happens HERE, before the offset is computed, so an
// oversized page cannot overflow (page-1)*limit into a negative OFFSET — the
// failure mode GET /v1/bot/space/members already documents on its own paging.
func principalReadPage(page, limit int64) projectReadPage {
	if limit <= 0 {
		limit = projectDefaultPageLimit
	}
	if limit > projectMaxPageLimit {
		limit = projectMaxPageLimit
	}
	if page <= 0 {
		page = 1
	} else if page > projectMaxPage {
		page = projectMaxPage
	}
	return projectReadPage{Offset: (page - 1) * limit, Limit: limit}
}

func (p *Project) readProjectsForPrincipal(
	spaceID, callerUID, keyword string, seatUIDs []string, page projectReadPage,
) (*projectReadListResult, error) {
	spaceID = strings.TrimSpace(spaceID)
	callerUID = strings.TrimSpace(callerUID)
	keyword = strings.TrimSpace(keyword)
	if callerUID == "" || len(seatUIDs) == 0 || spaceID == "" {
		return nil, ErrProjectReadForbidden
	}
	if page.Limit <= 0 {
		page.Limit = projectDefaultPageLimit
	}
	if page.Offset < 0 {
		page.Offset = 0
	}
	tx, err := p.beginProjectReadTx()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	if _, ok, err := p.projectReadSpaceAccessTx(tx, spaceID, callerUID); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrProjectReadForbidden
	}
	seatUIDs, err = p.principalSeatsWithLiveSpaceAccessTx(tx, spaceID, callerUID, seatUIDs)
	if err != nil {
		return nil, err
	}
	result, err := p.db.listProjectsReadForPrincipalsTx(tx, spaceID, callerUID, seatUIDs, keyword, page)
	if err != nil {
		return nil, err
	}
	projectIDs := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		projectIDs = append(projectIDs, row.ProjectID)
	}
	counts, err := p.db.projectReadSeatCountsTx(tx, projectIDs)
	if err != nil {
		return nil, err
	}
	for _, row := range result.Rows {
		count := counts[row.ProjectID]
		row.Humans = count.Humans
		row.Agents = count.Agents
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit principal project list read: %w", err)
	}
	return result, nil
}

func (p *Project) readProjectForPrincipal(
	projectID, callerUID string, seatUIDs []string,
) (*projectReadProjectResult, error) {
	projectID = strings.TrimSpace(projectID)
	callerUID = strings.TrimSpace(callerUID)
	if projectID == "" || callerUID == "" || len(seatUIDs) == 0 {
		return nil, ErrProjectReadNotFound
	}
	tx, err := p.beginProjectReadTx()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	// Space comes from the Project row, not from the request: the same posture as
	// the user-facing detail read, whose middleware resolves it the same way.
	project, err := p.db.queryProjectReadTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil || project.Status != StatusNormal {
		return nil, ErrProjectReadNotFound
	}
	spaceID := strings.TrimSpace(project.SpaceID)
	if spaceID == "" {
		return nil, ErrProjectReadNotFound
	}
	spaceRole, ok, err := p.projectReadSpaceAccessTx(tx, spaceID, callerUID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrProjectReadNotFound
	}
	seatUIDs, err = p.principalSeatsWithLiveSpaceAccessTx(tx, spaceID, callerUID, seatUIDs)
	if err != nil {
		return nil, err
	}
	role, member, err := p.db.queryProjectReadMemberRoleAmongTx(tx, projectID, spaceID, seatUIDs)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, ErrProjectReadNotFound
	}
	counts, err := p.db.projectReadSeatCountsTx(tx, []string{projectID})
	if err != nil {
		return nil, err
	}
	pinned, err := p.db.queryProjectPinnedReadTx(tx, projectID, callerUID)
	if err != nil {
		return nil, err
	}
	count := counts[projectID]
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("project: commit principal project detail read: %w", err)
	}
	return &projectReadProjectResult{
		Project:   project,
		Role:      role,
		SpaceRole: spaceRole,
		Humans:    count.Humans,
		Agents:    count.Agents,
		Pinned:    pinned,
	}, nil
}
