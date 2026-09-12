package project

import (
	"time"

	"go.uber.org/zap"
)

// P1 reconcile scans: I3 and the `removing` stall.
//
// All read-only, cursor-paged, bounded on rows EXAMINED, and publishing their
// gauges only on a completed rotation — the same discipline P0's scans carry and
// TestReconcileQueriesAreBounded enforces. A truncated tick has counted part of
// the keyspace, and Set-ing that partial number publishes a value smaller than
// reality, under which an alert threshold never fires.
//
// # Every join that CROSSES the two schemas carries an explicit COLLATE
//
// `group` and `group_member` were created in 2019 with no explicit
// CHARSET/COLLATE and inherit the server default. octo_project* pin
// utf8mb4_general_ci. P0's round-4 verification MEASURED production and found
// the legacy tables at utf8mb4_0900_ai_ci — an artefact of a mysqldump import,
// which omits COLLATE for tables whose collation equalled the source default.
//
// So an implicit join between them is MySQL error 1267 in production while
// passing in CI, because CI creates its database with an explicit
// utf8mb4_general_ci. A scan that 500s in production and is green in CI is worse
// than no scan: it reports zero violations and nobody looks again.
//
// The converse matters just as much, and the first version of this file got it
// wrong in BOTH directions. A join between two LEGACY tables (`group` ⋈
// `group_member`, `group` ⋈ `space`) needs no COLLATE, because both sides
// already agree; adding one there makes the predicate non-sargable — the index
// on the joined column stops being usable — so the "safety" measure costs a full
// scan on the largest tables. But `space_member_removal_cleanup` is NOT legacy
// (modules/space created it with an explicit general_ci), and a comparison in the
// SELECT list crosses the schemas exactly as one in an ON clause does; both were
// missed. The rule is about which SIDE a column is on, not which module wrote the
// query, and it applies wherever two columns meet.
//
// That claim is CI evidence rather than an assertion: TestP1ScansSurviveCollationDrift
// runs these statements against a deliberately drifted database. It is also why
// these scans sit OUTSIDE p.cfg.ReconcileEnabled — the gate exists for statements
// that cannot survive the drift, and these can.

// P1 scan thresholds.
const (
	// removingStallAfter is how long a seat may sit at removing = 1 before the
	// stall scan reports it. Generous relative to the worker's 10s poll and its
	// bounded backoff: this alert means the MACHINERY stopped, and firing it on
	// ordinary queue latency would train on-call to ignore it.
	removingStallAfter = 30 * time.Minute
)

// i3Row is one group whose project attribution is broken.
type i3Row struct {
	ID        int64  `db:"id"`
	GroupNo   string `db:"group_no"`
	ProjectID string `db:"project_id"`
	SpaceID   string `db:"space_id"`
	Violating bool   `db:"violating"`
	Reason    string `db:"reason"`
}

// scanI3Violations reports a group.project_id pointing at a project that is
// disbanded, in a different Space, or absent.
//
// I3 makes attribution immutable in v1, so these states are not reachable by any
// endpoint — which is exactly why they need a scan. The paths that CAN produce
// them are a disband whose detach step failed (the disband commits regardless,
// by design) and a direct database edit.
func (p *Project) scanI3Violations() {
	start := time.Now()
	defer func() { reconcileDuration.WithLabelValues("i3").Observe(time.Since(start).Seconds()) }()

	cursor, total := cursors.idResume(&cursors.i3Group, &cursors.i3Run)
	log := &logCapped{p: p, scan: "i3"}
	completed := false
	for page := 0; page < reconcileMaxPages; page++ {
		rows, err := p.queryI3Page(cursor, p.cfg.ReconcileLimit)
		if err != nil {
			noteScanFailure("i3")
			p.Warn("对账 I3 扫描失败", zap.Error(err))
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
			log.errorf("I3 违约：群的项目归属无效",
				zap.String("groupNo", row.GroupNo),
				zap.String("projectId", row.ProjectID),
				zap.String("spaceId", row.SpaceID),
				zap.String("reason", row.Reason))
			total++
		}
		cursor = rows[len(rows)-1].ID
		if len(rows) < p.cfg.ReconcileLimit {
			completed = true
			break
		}
	}
	if completed {
		i3Violations.Set(float64(total))
	}
	cursors.idSave(&cursors.i3Group, &cursors.i3Run, cursor, total, completed)
}

func (p *Project) queryI3Page(cursor int64, limit int) ([]*i3Row, error) {
	var rows []*i3Row
	_, err := p.db.session.SelectBySql(
		// pr.space_id (pinned) against g.space_id (legacy) crosses the schemas just
		// as the JOIN does, so it carries the same COLLATE. A comparison in the
		// SELECT list is as capable of raising 1267 as one in the ON clause, and
		// this pair was missed until the drift test ran.
		"SELECT g.id, g.group_no, g.project_id, g.space_id, "+
			"  (pr.project_id IS NULL OR pr.status <> 1 "+
			"     OR pr.space_id <> g.space_id COLLATE utf8mb4_general_ci) AS violating, "+
			"  CASE WHEN pr.project_id IS NULL THEN 'missing' "+
			"       WHEN pr.status <> 1 THEN 'disbanded' "+
			"       WHEN pr.space_id <> g.space_id COLLATE utf8mb4_general_ci THEN 'other_space' "+
			"       ELSE '' END AS reason "+
			"FROM `group` g "+
			"LEFT JOIN `octo_project` pr "+
			"  ON pr.project_id = g.project_id COLLATE utf8mb4_general_ci "+
			"WHERE g.id > ? AND g.project_id <> '' "+
			"ORDER BY g.id ASC LIMIT ?",
		cursor, limit,
	).Load(&rows)
	return rows, err
}

// stallRow is one seat stuck mid-removal.
type stallRow struct {
	ProjectID string    `db:"project_id"`
	UID       string    `db:"uid"`
	UpdatedAt time.Time `db:"updated_at"`
}

// scanRemovingStalls reports seats sitting at removing = 1 past the threshold.
//
// This is a machinery-health signal, not a native group-membership invariant:
// the Project seat remains active in storage until the removal worker completes,
// while authorization already excludes it through `removing = 1`.
//
// A stall normally means the removal worker is retrying a registered cleanup
// step or has stopped. Inspect the removal job's last error and lease state;
// the Project seat is intentionally retained until the worker can finish it.
func (p *Project) scanRemovingStalls() {
	start := time.Now()
	defer func() { reconcileDuration.WithLabelValues("removing_stall").Observe(time.Since(start).Seconds()) }()

	before := time.Now().UTC().Add(-removingStallAfter)
	var rows []*stallRow
	_, err := p.db.session.SelectBySql(
		"SELECT project_id, uid, updated_at FROM `octo_project_member` "+
			"WHERE removing = 1 AND updated_at < ? "+
			"ORDER BY updated_at ASC LIMIT ?",
		before, p.cfg.ReconcileLimit,
	).Load(&rows)
	if err != nil {
		noteScanFailure("removing_stall")
		p.Warn("对账 removing 滞留扫描失败", zap.Error(err))
		return
	}
	// Bounded by construction: the (removing, updated_at) index makes this read
	// only the stalled rows, and LIMIT caps it. No cursor is needed because the
	// population this examines is meant to be empty — if it is ever large enough
	// to need paging, the alert has already fired.
	log := &logCapped{p: p, scan: "removing_stall"}
	for _, row := range rows {
		log.errorf("项目席位关闭流程滞留（机器停了，不是不变量被破坏）",
			zap.String("projectId", row.ProjectID),
			zap.String("uid", row.UID),
			zap.Time("since", row.UpdatedAt))
	}
	removingStalls.Set(float64(len(rows)))
}
