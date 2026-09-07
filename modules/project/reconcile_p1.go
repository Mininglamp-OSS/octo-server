package project

import (
	"time"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// P1 reconcile scans: I2, I3, and the `removing` stall.
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

// i2Threshold values.
const (
	// removingStallAfter is how long a seat may sit at removing = 1 before the
	// stall scan reports it. Generous relative to the worker's 10s poll and its
	// bounded backoff: this alert means the MACHINERY stopped, and firing it on
	// ordinary queue latency would train on-call to ignore it.
	removingStallAfter = 30 * time.Minute
)

// i2Row is one active member of a project group, flagged with whether they are a
// violation and, if exempt, why.
type i2Row struct {
	// ID and UID are the composite cursor. The page cuts on (group id, uid)
	// pairs, not on groups, so both halves have to come back with the row.
	ID        int64  `db:"id"`
	GroupNo   string `db:"group_no"`
	ProjectID string `db:"project_id"`
	UID       string `db:"uid"`
	SpaceID   string `db:"space_id"`
	// Violating is computed in SQL (flag-over-base-page, like P0's member scans)
	// so the page stays bounded regardless of how many rows are violations.
	Violating bool `db:"violating"`
}

// scanI2Violations reports active group_member rows in a project group whose uid
// is not an active member of that project.
//
// # Driven group-first, deliberately
//
// The scan pages `group` by id using the new (space_id, project_id) index to
// find project groups, then reads their members by group_no. The other
// direction — start from group_member — cannot be bounded: no index on
// group_member leads with is_deleted, so a member-first scan walks the table.
//
// # Exemptions, and why an alert without them is worse than no alert
//
// Each of these is a state normal operation produces, and an alert that fires
// during normal operation is noise before the feature has a single user. P0's I1
// scan carries exemptions for the same reason.
//
//  1. removing = 1 with a pending cascade job — the seat is closing and the
//     worker is coming; that IS the designed intermediate state (D4).
//  2. a pending Space-removal cleanup job for the same uid — the Space cascade
//     owns that removal and has not run yet.
//  3. members of a BANNED Space (space.status = 2) — CheckMembershipForCleanup
//     deliberately leaves their seats alone, so their group rows are expected.
//  4. whitelisted system bots — exempt from project membership by design, which
//     is the same exemption the admission gate applies.
func (p *Project) scanI2Violations() {
	start := time.Now()
	defer func() { reconcileDuration.WithLabelValues("i2").Observe(time.Since(start).Seconds()) }()

	cursorGroup, cursorUID, total := cursors.i2Resume()
	log := &logCapped{p: p, scan: "i2"}
	completed := false
	for page := 0; page < reconcileMaxPages; page++ {
		rows, err := p.queryI2Page(cursorGroup, cursorUID, p.cfg.ReconcileLimit)
		if err != nil {
			// break, not return: the cursor save below must be reached, or a
			// failing page resets the rotation to the start every tick and the
			// tail of the keyspace is never examined.
			noteScanFailure("i2")
			p.Warn("对账 I2 扫描失败", zap.Error(err))
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
			log.errorf("I2 违约：项目群里存在非项目成员",
				zap.String("groupNo", row.GroupNo),
				zap.String("projectId", row.ProjectID),
				zap.String("uid", row.UID),
				zap.String("spaceId", row.SpaceID))
			total++
		}
		last := rows[len(rows)-1]
		cursorGroup, cursorUID = last.ID, last.UID
		if len(rows) < p.cfg.ReconcileLimit {
			completed = true
			break
		}
	}
	if completed {
		i2Violations.Set(float64(total))
	}
	cursors.i2Save(cursorGroup, cursorUID, total, completed)
}

// queryI2Page returns one bounded page of (project group, active member) pairs,
// each flagged with whether it violates I2.
//
// # The cursor is composite, and it has to be
//
// The LIMIT is on group_member rows, so the page bounds rows EXAMINED rather
// than groups visited — a single very large group costs one page, not an
// unbounded read. But that means a page can cut a group in half, and an earlier
// version advanced the cursor to the page's last GROUP ID with `WHERE g.id > ?`.
// The next page then started strictly after that group, so every member of it
// past the boundary was skipped. The comment called this self-correcting; it is
// not, and that was the defect: the pages fall on the same boundary on every
// rotation, so the same members are skipped forever. A group whose one
// non-member sorts last was invisible to the scan permanently.
//
// `(g.id, gm.uid) > (?, ?)` resumes at the exact pair the last page ended on,
// which is the same shape P0's queryI1ViolationPage uses over
// (project_id, uid). It also removes the second query the old cursor needed —
// which was differently filtered (no bot exclusion), so it could report a
// different last group than the page it was supposed to describe.
//
// # Disbanded groups are excluded
//
// Group disband only flips group.status and leaves every group_member row in
// place, by design, and no endpoint cleans them up. Those rows grant nothing, so
// they are not a leak — and the cascade skips disbanded groups for the same
// reason, so flagging them here would report a state nothing is allowed to fix.
func (p *Project) queryI2Page(cursorGroupID int64, cursorUID string, limit int) ([]*i2Row, error) {
	var rows []*i2Row
	_, err := p.db.session.SelectBySql(
		"SELECT g.id, g.group_no, g.project_id, gm.uid, g.space_id, "+
			// violating = "not an active project member" AND "no exemption
			// applies". Computed in SQL rather than filtered by a WHERE so the
			// page stays a fixed size: with a WHERE, a burst of exempt rows would
			// starve the page and the rotation would crawl.
			"  ((pm.uid IS NULL OR pm.status <> 1 OR pm.removing = 1) "+
			"   AND jobp.id IS NULL "+ // exemption 1: a pending project cascade job
			"   AND jobs.id IS NULL "+ // exemption 2: a pending Space cleanup job
			"   AND (sp.status IS NULL OR sp.status <> 2) "+ // exemption 3: banned Space
			// exemption 4: a disbanded group (see the doc comment above). This
			// belongs in the flag rather than the WHERE for the reason the cost
			// guard states: `group`.status leads no index, so as disbanded groups
			// accumulate a WHERE predicate on it turns LIMIT back into a bound on
			// rows RETURNED. `IS NOT NULL AND` keeps it exactly equivalent to the
			// WHERE clause it replaces — the column is nullable, and `NULL <> 2`
			// is NULL, which the WHERE dropped and a bare AND here would load
			// into a Go bool.
			"   AND (g.status IS NOT NULL AND g.status <> 2)) "+
			"  AS violating "+
			"FROM `group` g "+
			// group ⋈ group_member: both legacy, so no COLLATE — forcing one here
			// would cost the index on group_member.group_no for nothing.
			"INNER JOIN group_member gm "+
			"  ON gm.group_no = g.group_no "+
			" AND gm.is_deleted = 0 AND gm.status = 1 "+
			// These two cross into the pinned schema, so the legacy side is
			// coerced. This is where COLLATE actually belongs.
			"LEFT JOIN `octo_project_member` pm "+
			"  ON pm.project_id = g.project_id COLLATE utf8mb4_general_ci "+
			" AND pm.uid = gm.uid COLLATE utf8mb4_general_ci "+
			"LEFT JOIN `octo_project_member_removal_cleanup` jobp "+
			"  ON jobp.project_id = g.project_id COLLATE utf8mb4_general_ci "+
			" AND jobp.uid = gm.uid COLLATE utf8mb4_general_ci AND jobp.status = 0 "+
			// space_member_removal_cleanup is NOT legacy: modules/space created it
			// with an explicit general_ci, so it sits on the pinned side and its
			// comparisons against `group` / group_member cross the schemas. This
			// one was miscategorised in the first pass at the COLLATE cleanup and
			// the drift test caught it — which is the point of having the test.
			"LEFT JOIN space_member_removal_cleanup jobs "+
			"  ON jobs.space_id = g.space_id COLLATE utf8mb4_general_ci "+
			" AND jobs.uid = gm.uid COLLATE utf8mb4_general_ci AND jobs.status = 0 "+
			// `space` IS legacy, so this one needs nothing.
			"LEFT JOIN `space` sp ON sp.space_id = g.space_id "+
			// project_id STAYS here, and it is the one predicate that must: moving
			// it into the flag would make every Space-direct group's members base
			// rows, i.e. a walk of the whole group_member table every tick. It is
			// also the selective one — the population it keeps out is the product.
			"WHERE (g.id, gm.uid) > (?, ?) AND g.project_id <> '' "+
			"  AND gm.uid NOT IN ? "+
			"ORDER BY g.id ASC, gm.uid ASC LIMIT ?",
		cursorGroupID, cursorUID, systemBotUIDsForScan(), limit,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

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
// A SEPARATE alert from I2, with the OPPOSITE meaning, and conflating them would
// make both useless: I2 says the invariant broke, this says the machinery
// stopped. A stalled removal is not an invariant violation at all — the member
// is still a member of record, so the subset relation holds — it is a member who
// cannot finish leaving.
//
// # What on-call should look at first
//
// The most likely non-bug cause is a cascade that keeps failing on one group,
// and the most likely cause of THAT used to be a group whose creator is the
// departing member. P1 resolves that automatically now (the cascade performs the
// group's normal owner handover, and detaches the group when nobody in it is
// still in the project), so a stall surviving to this alert means the handover
// itself is failing — check the job's last_error, not the group's ownership.
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

// systemBotUIDsForScan is exemption 4: whitelisted system bots are admitted to
// project groups without a project seat, so their rows are expected and must not
// be reported.
//
// Taken from pkg/space rather than declared here, so the scan and the admission
// gate cannot disagree about who is exempt — a second list would drift, and the
// drift would show up as an alert nobody can act on.
func systemBotUIDsForScan() []string {
	uids := spacepkg.SystemBotList()
	if len(uids) == 0 {
		// IN () is a syntax error in MySQL. A sentinel that matches no uid keeps
		// the statement valid when the whitelist is empty.
		return []string{"\x00-no-system-bot"}
	}
	return uids
}
