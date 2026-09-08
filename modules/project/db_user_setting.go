package project

import (
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// Per-user project settings (P2). One preference so far: pinning.
//
// # Why this is not a column on octo_project_member
//
// Pinning is not a membership fact — a Space admin sees a space_listed project
// without joining it and may reasonably pin it — and every write to
// octo_project_member sits on the member_epoch path, which fleet and drive read
// to decide whether membership changed and which TestMemberEpochOnlyEverIncrements
// pins to +1 steps. A pin is not a membership change and must never move it. The
// migration header argues both at length; repeated here because the next person to
// add a preference will be looking at this file, not at the SQL.

// upsertProjectUserSetting sets or clears the pin for one (project, uid).
//
// INSERT ... ON DUPLICATE KEY UPDATE against uk_octo_project_user_setting, so the
// operation is idempotent by construction: pinning twice rewrites one row rather
// than growing a second. That is the same technique P0 used for member
// re-admission, and it is why the endpoint can be a plain PUT.
//
// pinned_at is written on pin and CLEARED on unpin, because it is the sort key for
// pinned rows and a stale timestamp on an unpinned row would order a future re-pin
// by when it was pinned the FIRST time. Clearing it makes re-pinning move the
// project to the front, which is what a user who just pinned something expects.
//
// One clock, in Go, in UTC — no CURRENT_TIMESTAMP anywhere. MySQL evaluates it in
// the session timezone, and this repo has already shipped a metric that read
// -28799 seconds under TZ=Asia/Shanghai because of exactly that.
func (d *DB) upsertProjectUserSetting(projectID, uid string, pinned bool) error {
	return d.upsertProjectUserSettingTx(nil, projectID, uid, pinned)
}

// upsertProjectUserSettingTx is the same write inside a caller-supplied
// transaction. tx may be nil, in which case it runs on the session — the quota
// path needs the count and the write to share one read view, the routes that
// only clear a pin do not.
func (d *DB) upsertProjectUserSettingTx(tx *dbr.Tx, projectID, uid string, pinned bool) error {
	if projectID == "" || uid == "" {
		return fmt.Errorf("project: upsert user setting: empty project_id or uid")
	}
	now := time.Now().UTC()
	var pinnedAt interface{}
	flag := 0
	if pinned {
		flag = 1
		pinnedAt = now
	}
	runner := dbr.SessionRunner(d.session)
	if tx != nil {
		runner = tx
	}
	_, err := runner.InsertBySql(
		"INSERT INTO octo_project_user_setting "+
			"(project_id, uid, pinned, pinned_at, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE pinned = VALUES(pinned), "+
			"  pinned_at = VALUES(pinned_at), updated_at = VALUES(updated_at)",
		projectID, uid, flag, pinnedAt, now, now,
	).Exec()
	if err != nil {
		return fmt.Errorf("project: upsert user setting: %w", err)
	}
	return nil
}

// queryProjectPinned answers whether uid has pinned projectID.
//
// A point query rather than a join, for the routes that already hold one project
// row and need the caller-specific half of the response: the detail route, and the
// update route — which must report the CURRENT pin state rather than defaulting to
// false, or a client refreshing its card after a rename would watch the project
// silently un-pin itself until the next list fetch.
//
// The create route does not call this: a project id that was generated inside the
// transaction that just committed cannot have a setting row yet, so false there is
// provable rather than assumed.
func (d *DB) queryProjectPinned(projectID, uid string) (bool, error) {
	if projectID == "" || uid == "" {
		return false, nil
	}
	var pinned []int
	_, err := d.session.SelectBySql(
		"SELECT pinned FROM octo_project_user_setting WHERE project_id = ? AND uid = ?",
		projectID, uid,
	).Load(&pinned)
	if err != nil {
		return false, fmt.Errorf("project: query pinned: %w", err)
	}
	return len(pinned) > 0 && pinned[0] == 1, nil
}

// sqlCountPinnedInSpace is the statement countPinnedInSpaceTx runs.
//
// Named for the same reason as sqlListMyProjectGroups: the plan guard EXPLAINs the
// string production executes, not a copy that can drift into passing forever.
//
// The LEFT JOIN carries the membership half of the visibility rule, and removing = 0
// for the same reason listVisibleInSpace carries it — a seat that is closing counts
// as gone everywhere else. pm.uid IS NOT NULL is the clause that admits an unlisted
// project the caller is actually in.
const sqlCountPinnedInSpace = "SELECT COUNT(*) FROM octo_project_user_setting s " +
	"INNER JOIN octo_project p ON p.project_id = s.project_id " +
	"LEFT JOIN octo_project_member pm " +
	"  ON pm.project_id = p.project_id AND pm.uid = s.uid " +
	"     AND pm.status = ? AND pm.removing = 0 " +
	"WHERE s.uid = ? AND s.pinned = 1 AND p.space_id = ? AND p.status = ? " +
	"  AND (p.discoverability = ? OR pm.uid IS NOT NULL)"

// countPinnedInSpaceTx counts how many projects uid currently has pinned inside one
// Space, for the quota check.
//
// # Why the Space is resolved by joining rather than stored on the row
//
// octo_project_user_setting deliberately carries no space_id. The (uid, pinned)
// index narrows to this user's pinned rows across every Space — a single-digit set
// by construction, since the quota is what bounds it — and the join then reads
// space_id from octo_project by primary key. A redundant space_id column would save
// that probe and cost a second copy of a fact, which is the trade the removal
// outbox made for a different reason (a worker touching every row of a large fan-out
// per job) that does not apply to one probe per pin.
//
// # It counts exactly the pins the caller's own list shows, and that is the contract
//
// Not "every pinned row". The quota predicate MIRRORS listVisibleInSpace, because a
// quota that counts rows the list does not show produces a refusal the user cannot
// act on: "you have pinned 6" while their screen shows 5, with no sixth to remove.
//
// Two states reach that, and PR #861s review found the second after the first was
// already handled:
//
//   - A DISBANDED project. Its name is released and every read path treats it as
//     nonexistent. Covered by p.status.
//   - An UNLISTED project the caller is not a member of. projectMiddleware answers
//     not_found for exactly that caller, and PUT /:project_id/setting is the only
//     unpin path — so the pin becomes unreachable through every API surface while
//     still consuming a slot. An owner flipping discoverability is an ordinary
//     action, and it permanently burned one of the victims six slots. The
//     membership half is the same trap by the other door: a member who pins an
//     unlisted project and is then removed from it lands in the identical state.
//
// So the visibility half of listVisibleInSpace comes along:
// (space_listed OR an active member row). A caller whose project becomes visible
// again while they are over the cap converges through unpin, which is never
// refused — the same situation the code already accepts for a cap lowered by
// configuration.
//
// Note what this does NOT do: a Space admin can point-read an unlisted project they
// never joined (projectMiddleware allows it) but that project is absent from their
// LIST too, so a pin on it is invisible to both this count and their screen. They
// can therefore hold more pinned rows than the cap. That is the safe direction of
// the error — an undercount can never trap anyone — and keeping the count equal to
// what the list shows is worth more than making the cap exact for one role.
//
// All three tables are octo_project*, i.e. all general_ci, so no COLLATE: an
// explicit one has coercibility 0 and would cost octo_project its primary key on
// this join.
func (d *DB) countPinnedInSpaceTx(tx *dbr.Tx, spaceID, uid string) (int, error) {
	if spaceID == "" || uid == "" {
		return 0, nil
	}
	var n int
	runner := dbr.SessionRunner(d.session)
	if tx != nil {
		runner = tx
	}
	err := runner.SelectBySql(sqlCountPinnedInSpace,
		MemberStatusActive, uid, spaceID, StatusNormal, DiscoverabilitySpaceListed,
	).LoadOne(&n)
	if err != nil {
		return 0, fmt.Errorf("project: count pinned in space: %w", err)
	}
	return n, nil
}
