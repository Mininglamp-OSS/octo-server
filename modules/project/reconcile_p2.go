package project

import (
	"fmt"
	"time"

	"go.uber.org/zap"
)

// P2 reconciliation keeps the initial all-member-group provisioning health scan.
//
// Project creation provisions one native all-member group from a single member
// snapshot. If that post-commit provisioning attempt leaves the Project without
// a usable group, scan A reports the missing initial artifact for operations.
// Project member changes do not synchronize native group_member rows, so there
// is intentionally no member-gap scan here.
//
// The scan is read-only, cursor-paged, bounded on rows EXAMINED, and publishes
// its gauge only after a completed rotation.
//
// # COLLATE, and where it goes
//
// The scan joins octo_project (pinned utf8mb4_general_ci) against `group`
// (a legacy table, utf8mb4_0900_ai_ci in production). The cross-schema
// comparisons carry an explicit COLLATE, while comparisons within the legacy
// schema do not add one.
//
// The scan remains behind p.cfg.ReconcileEnabled because its measured join plan
// can be expensive under the production collation shape. The startup warning
// makes a disabled monitor visible.

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
	// Violating is computed in SQL rather than filtered by a WHERE, so LIMIT
	// bounds rows EXAMINED and not rows RETURNED. See queryMissingAllMemberGroupPage.
	Violating bool `db:"violating"`
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
// One gauge for three states because the operational response is the same:
// repair or re-run provisioning for this project. The log line names which
// state was observed, so the on-call reader can tell a provisioning outage from
// a cascade side effect.
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
			if !row.Violating {
				continue
			}
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

// queryMissingAllMemberGroupPage returns one bounded page of active projects,
// each flagged with whether its all-member group is missing or unusable.
//
// # The flag is computed in SQL, and that is what makes the page bounded
//
// An earlier version put `g.id IS NULL` in the WHERE. LIMIT then bounds rows
// RETURNED, not rows EXAMINED — so in the HEALTHY case, where every project has
// its group, the statement matches nothing and walks the whole of octo_project
// with a `group` lookup per row, every tick, forever. The cost is highest exactly
// when there is nothing to report.
//
// Flag-over-base-page is the shape P0 and P1 use for the same reason. Keep the
// computed flag in the SELECT list so LIMIT bounds the active base rows even
// when every project is healthy; filtering violations in WHERE would scan past
// the page boundary.
// `p.status` stays in the WHERE deliberately: it selects the BASE population
// (active projects are what the invariant is about), and a disbanded project is
// not a violation to be flagged but a row that is out of scope.
//
// It is NOT served by an index, and this file used to say it was. The paging
// predicate is `p.id > ? ORDER BY p.id`, which only the PRIMARY KEY can serve:
// measured on MySQL 8.0.46 (2000 projects), `p type=range key=PRIMARY` under BOTH
// collation shapes, with idx_octo_project_all_member_group in possible_keys and
// unchosen — a secondary index on (status, all_member_group_no) orders its rows by
// all_member_group_no, not by id. That index has been dropped from the migration
// for exactly this reason. PR #855s fifth review, Q1.
//
// # The LEFT JOIN's plan, stated accurately
//
// The explicit COLLATE has coercibility 0, so the comparison is general_ci and
// `group`.group_no — 0900_ai_ci in production — must be converted per row,
// which its UNIQUE index cannot serve. A production plan can therefore show a
// full scan of `group` plus a temporary table, defeating the ORDER BY / LIMIT
// paging this scan depends on. CI uses general_ci on both sides, so it reports
// an eq_ref plan instead.
//
// The scan remains bounded by ReconcileLimit and reconcileMaxPages, and the
// ReconcileEnabled gate keeps the measured production cost explicit until the
// collation conversion is complete.
func (p *Project) queryMissingAllMemberGroupPage(cursor int64, limit int) ([]*i4MissingRow, error) {
	var rows []*i4MissingRow
	_, err := p.db.session.SelectBySql(
		"SELECT p.id, p.project_id, p.space_id, p.all_member_group_no AS group_no, "+
			"  (g.id IS NULL) AS violating "+
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
			"WHERE p.status = ? AND p.id > ? "+
			"ORDER BY p.id LIMIT ?",
		groupStatusDisband, StatusNormal, cursor, limit,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query I4 missing all-member group page: %w", err)
	}
	return rows, nil
}

// groupStatusDisband mirrors modules/group.GroupStatusDisband. Project keeps the
// literal locally because importing modules/group would create a dependency cycle.
const groupStatusDisband = 2
