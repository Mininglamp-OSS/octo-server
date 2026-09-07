package project

import (
	"fmt"
	"time"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// P2 reconcile scans: the two halves of invariant I4.
//
//	I4 — every active project has exactly one all-member group, and that group's
//	active member set EQUALS the project's active member set (status = 1 AND
//	removing = 0), whitelisted system bots exempted.
//
// I2 (P1) is the ⊆ direction: nobody is in a project group who is not in the
// project. These two scans are the ⊇ direction, and only for the all-member
// group — an ordinary project group is under no obligation to contain everyone.
//
//	scan A — an active project with no usable all-member group.
//	scan B — an active project member missing from their all-member group.
//
// Same discipline as every scan in this module: read-only, cursor-paged, bounded
// on rows EXAMINED, gauges published only on a completed rotation, and REPORT
// ONLY — neither of them repairs anything. Repair for scan A lives on the write
// paths (D4's lease-claimed rebuild); scan B has no automatic repair at all, by
// decision, because repairing it means writing group_member from a background
// worker whose job is to be the invariant's witness.
//
// # COLLATE, and where it goes
//
// Both scans join octo_project* (pinned utf8mb4_general_ci) against `group` and
// `group_member` (2019 legacy tables, utf8mb4_0900_ai_ci in production). Every
// comparison that CROSSES those two schemas carries an explicit COLLATE, and
// every comparison BETWEEN two legacy tables deliberately does not — adding one
// there makes the predicate non-sargable and costs a full scan. That is P1's
// rule verbatim; TestP1ScansSurviveCollationDrift is its evidence, and the P2
// statements are added to the same test rather than assumed to inherit it.
//
// Like P1's scans these run OUTSIDE p.cfg.ReconcileEnabled: the gate exists for
// statements that cannot survive the drift, and these can.

// i4MissingRow is one active project whose all-member group is missing or unusable.
type i4MissingRow struct {
	ID        int64  `db:"id"`
	ProjectID string `db:"project_id"`
	SpaceID   string `db:"space_id"`
	// GroupNo is what the project points at, "" when it points at nothing.
	// Carried so the log line can distinguish "never provisioned" from "points at
	// a group that is gone", which are different operational problems even though
	// they are one gauge.
	GroupNo string `db:"group_no"`
}

// scanMissingAllMemberGroups reports active projects with no usable all-member
// group (I4 scan A).
//
// "Unusable" is three states, deliberately counted as one:
//
//  1. all_member_group_no is the empty sentinel — provisioning never succeeded,
//     or succeeded and was rolled back.
//  2. it names a group that does not exist, or is disbanded.
//  3. it names a group whose project_id is no longer this project — P1's
//     member-removal cascade detaches a group to Space-direct when its creator
//     leaves the project and nobody left can inherit it, and it does not clear
//     the project's pointer, because modules/group must not write octo_project.
//
// One gauge for three states because the operator action is the same in all
// three: this project has no all-member group and the next write path on it
// should rebuild one. The log line names which, so the on-call reader can tell a
// provisioning outage from a cascade side effect.
func (p *Project) scanMissingAllMemberGroups() {
	start := time.Now()
	defer func() {
		reconcileDuration.WithLabelValues("i4_missing").Observe(time.Since(start).Seconds())
	}()

	cursor, total := cursors.idResume(&cursors.i4Missing, &cursors.i4MissingRun)
	log := &logCapped{p: p, scan: "i4_missing"}
	completed := false
	for page := 0; page < reconcileMaxPages; page++ {
		rows, err := p.queryMissingAllMemberGroupPage(cursor, p.cfg.ReconcileLimit)
		if err != nil {
			// break, not return: the cursor save below has to be reached, or a
			// failing page resets the rotation every tick and the tail of the
			// keyspace is never examined. Same shape as every other scan here.
			noteScanFailure("i4_missing")
			p.Warn("对账 I4-A 扫描失败", zap.Error(err))
			break
		}
		if len(rows) == 0 {
			completed = true
			break
		}
		for _, row := range rows {
			reason := "never_provisioned"
			if row.GroupNo != "" {
				reason = "group_gone_or_detached"
			}
			log.errorf("I4-A：活跃项目没有可用的全员群",
				zap.String("projectId", row.ProjectID),
				zap.String("spaceId", row.SpaceID),
				zap.String("groupNo", row.GroupNo),
				zap.String("reason", reason))
			total++
		}
		cursor = rows[len(rows)-1].ID
		if len(rows) < p.cfg.ReconcileLimit {
			completed = true
			break
		}
	}
	if completed {
		allMemberGroupMissing.Set(float64(total))
	}
	cursors.idSave(&cursors.i4Missing, &cursors.i4MissingRun, cursor, total, completed)
}

// queryMissingAllMemberGroupPage returns one bounded page of active projects
// whose all-member group is missing or unusable.
//
// Driven from octo_project by id, so the page bounds rows examined on THIS
// module's own table rather than on `group`. The LEFT JOIN is a point lookup per
// examined row against group_groupNo (UNIQUE on group_no), so the work per row
// is one index dive, not a scan.
//
// The predicate is expressed as "the join found nothing usable" rather than as
// three ORed conditions on the project row, because two of the three states are
// facts about the GROUP, not about the project.
func (p *Project) queryMissingAllMemberGroupPage(cursor int64, limit int) ([]*i4MissingRow, error) {
	var rows []*i4MissingRow
	_, err := p.db.session.SelectBySql(
		"SELECT p.id, p.project_id, p.space_id, p.all_member_group_no AS group_no "+
			"FROM `octo_project` p "+
			// COLLATE on the driving side's value: p.all_member_group_no is
			// general_ci, g.group_no is the legacy collation. Written this way so
			// the lookup can still use group_groupNo.
			//
			// The g.project_id comparison carries it for the same reason — it puts
			// a general_ci literal against a legacy column, which is exactly the
			// cross-schema case, and a comparison in an ON clause is no different
			// from one in a WHERE (P1 learned that the hard way in the SELECT list).
			"LEFT JOIN `group` g "+
			"  ON g.group_no = p.all_member_group_no COLLATE utf8mb4_general_ci "+
			"  AND g.status <> ? "+
			"  AND g.project_id = p.project_id COLLATE utf8mb4_general_ci "+
			"WHERE p.status = ? AND p.id > ? AND g.id IS NULL "+
			"ORDER BY p.id LIMIT ?",
		groupStatusDisband, StatusNormal, cursor, limit,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query I4 missing all-member group page: %w", err)
	}
	return rows, nil
}

// i4GapRow is one active project member who is not in their all-member group.
type i4GapRow struct {
	// ID and UID form the composite cursor: the page is bounded on MEMBER rows,
	// so it can cut a project in half, and resuming at "the project after the
	// last one seen" would skip every member past the boundary on every rotation
	// — the exact defect P1's queryI2Page documents.
	ID        int64  `db:"id"`
	ProjectID string `db:"project_id"`
	UID       string `db:"uid"`
	SpaceID   string `db:"space_id"`
	GroupNo   string `db:"group_no"`
	// Violating is computed in SQL (flag-over-base-page) so the page stays a
	// fixed size regardless of how many rows are exempt. Filtering exemptions in
	// the WHERE would let a burst of exempt rows starve the page and crawl the
	// rotation.
	Violating bool `db:"violating"`
}

// scanAllMemberGroupGaps reports active project members missing from their
// project's all-member group (I4 scan B).
//
// # Exemptions
//
// Each is a state normal operation produces, and an alert that fires during
// normal operation is noise before the feature has a user. Same reasoning as
// P0's I1 scan and P1's I2 scan.
//
//  1. removing = 1 — the seat is closing; the member is on their way out and
//     the group rows are the cascade's to remove.
//  2. whitelisted system bots — exempt from project membership by design, so
//     they are neither required in the group nor counted as members of it.
//  3. a seat written within allMemberGroupAdmitGrace — the admitter runs AFTER
//     the seat transaction commits (D12), so there is a real window in which the
//     seat exists and the group row does not. Without this the scan would report
//     every single add for as long as that window lasts, i.e. it would report
//     the design.
//
// Not exempted, deliberately: a project with no all-member group at all. Those
// rows are scan A's business, and counting them here too would double-report one
// problem in two gauges and make both move together for one cause.
func (p *Project) scanAllMemberGroupGaps() {
	start := time.Now()
	defer func() {
		reconcileDuration.WithLabelValues("i4_gap").Observe(time.Since(start).Seconds())
	}()

	cursorProject, cursorUID, total := cursors.i4GapResume()
	log := &logCapped{p: p, scan: "i4_gap"}
	completed := false
	for page := 0; page < reconcileMaxPages; page++ {
		rows, err := p.queryAllMemberGroupGapPage(cursorProject, cursorUID, p.cfg.ReconcileLimit)
		if err != nil {
			noteScanFailure("i4_gap")
			p.Warn("对账 I4-B 扫描失败", zap.Error(err))
			break
		}
		if len(rows) == 0 {
			completed = true
			break
		}
		for _, row := range rows {
			if !row.Violating {
				continue
			}
			log.errorf("I4-B：项目活跃成员不在全员群里",
				zap.String("projectId", row.ProjectID),
				zap.String("uid", row.UID),
				zap.String("groupNo", row.GroupNo),
				zap.String("spaceId", row.SpaceID))
			total++
		}
		last := rows[len(rows)-1]
		cursorProject, cursorUID = last.ID, last.UID
		if len(rows) < p.cfg.ReconcileLimit {
			completed = true
			break
		}
	}
	if completed {
		allMemberGroupMemberGaps.Set(float64(total))
	}
	cursors.i4GapSave(cursorProject, cursorUID, total, completed)
}

// queryAllMemberGroupGapPage returns one bounded page of (project, active member)
// pairs, each flagged with whether the member is missing from the all-member group.
//
// Driven project-first then member-by-project_id, mirroring P1's group-first I2
// scan for the same reason: the driving side must have an index that leads with
// the filter. Here that is idx_octo_project_all_member_group (status,
// all_member_group_no) for the projects and the PRIMARY KEY (project_id, uid) for
// their members.
//
// Only projects that HAVE an all-member group are examined: a project without one
// has no gap to measure, and scan A already reports it.
func (p *Project) queryAllMemberGroupGapPage(cursorProjectID int64, cursorUID string, limit int) ([]*i4GapRow, error) {
	var rows []*i4GapRow
	graceCutoff := time.Now().UTC().Add(-allMemberGroupAdmitGrace)
	_, err := p.db.session.SelectBySql(
		"SELECT p.id, p.project_id, pm.uid, pm.space_id, p.all_member_group_no AS group_no, "+
			"  (gm.uid IS NULL AND pm.updated_at < ?) AS violating "+
			"FROM `octo_project` p "+
			"INNER JOIN `octo_project_member` pm "+
			// Both sides are octo_project*, i.e. both general_ci. No COLLATE:
			// adding one between two same-collation columns makes the predicate
			// non-sargable and gives up the PRIMARY KEY on octo_project_member.
			"  ON pm.project_id = p.project_id AND pm.status = ? AND pm.removing = 0 "+
			// Crosses into the legacy schema, so it carries COLLATE on the
			// general_ci side's values.
			"LEFT JOIN `group_member` gm "+
			"  ON gm.group_no = p.all_member_group_no COLLATE utf8mb4_general_ci "+
			"  AND gm.uid = pm.uid COLLATE utf8mb4_general_ci "+
			"  AND gm.is_deleted = 0 AND gm.status = 1 "+
			"WHERE p.status = ? AND p.all_member_group_no <> '' "+
			"  AND (p.id, pm.uid) > (?, ?) "+
			// System bots are exempt from project membership entirely, so they are
			// not expected in the group either.
			"  AND pm.uid NOT IN ? "+
			"ORDER BY p.id, pm.uid LIMIT ?",
		graceCutoff, MemberStatusActive, StatusNormal, cursorProjectID, cursorUID,
		systemBotUIDsForScan(), limit,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query I4 all-member group gap page: %w", err)
	}
	return rows, nil
}

// allMemberGroupAdmitGrace is how long a freshly written project seat is exempt
// from scan B.
//
// The admitter runs after the seat transaction commits and makes its own
// transaction plus a blocking IM call (D12), so the seat legitimately exists
// without the group row for as long as that takes. Without a grace period the
// scan would flag every add in flight — reporting the design as a violation, and
// training whoever reads the gauge to ignore it.
//
// Five minutes is generous against a path measured in hundreds of milliseconds,
// and short relative to how long a real gap would persist: nothing retries the
// admission, so a genuine failure stays until an admin re-adds the member.
const allMemberGroupAdmitGrace = 5 * time.Minute

// groupStatusDisband mirrors modules/group.GroupStatusDisband.
//
// Spelled out rather than imported: modules/project must never import
// modules/group (pkg/project/import_guard_test.go pins it at zero), and this
// scan reads the `group` table directly — which it may, exactly as modules/space
// does, because reading a column is not a module dependency.
const groupStatusDisband = 2

// systemBotExemptForScan reports whether uid is exempt from I4 for the same
// reason it is exempt from I2: the platform adds these accounts to groups itself
// and they hold no project seat.
//
// Unused by the SQL above (which filters with systemBotUIDsForScan) and kept for
// the Go-side assertions in the scan tests, so the two cannot disagree about who
// is exempt.
func systemBotExemptForScan(uid string) bool { return spacepkg.IsSystemBot(uid) }
