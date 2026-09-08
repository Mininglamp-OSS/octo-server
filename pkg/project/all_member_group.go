package project

import (
	"errors"
	"github.com/gocraft/dbr/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// 全员群相关的谓词（P2 的 D5 / D6 / D7）。
//
// 与 membership.go 同属一个包，遵守同一条规矩：**只依赖 octo-lib**，不 import 任何
// modules/*。这正是让 modules/group 能在不 import modules/project 的前提下回答
// 「这个群是不是某项目的全员群」的原因——两个模块依赖同一个事实，而不是彼此依赖。
//
// 状态字面量（status = 1 表示活跃、role = 2 表示 owner）都写死在这里，理由与
// ResolveForGroup 里那段一样：import modules/project 的常量会成环。

// projectRoleOwner mirrors modules/project.RoleOwner. Spelled out rather than
// imported, for the reason above.
const projectRoleOwner = 2

// IsAllMemberGroup answers whether groupNo is projectID's all-member group.
//
// # Both halves of the predicate are required
//
// It checks the project's `all_member_group_no` AND that the group still carries
// `project_id = projectID`. The second half is not redundant, and dropping it is
// a real bug: P1's member-removal cascade detaches a group to Space-direct
// (project_id reset to the empty sentinel) when the departing member was its creator and nobody
// in the project can inherit it. The project row still names that group, but the
// group is no longer part of the project — it is an ordinary Space group, and it
// must stop being protected (D7) and stop counting as the project's group for
// rebuild purposes (D4). Asking only the project side would keep protecting a
// group the project no longer owns, and its members could never leave it.
//
// # No cross-schema comparison, and that is a performance requirement
//
// This used to be ONE statement joining `octo_project` to `group`, with an
// explicit COLLATE so the comparison would not raise 1267 against production,
// where `group` is utf8mb4_0900_ai_ci and octo_project is pinned general_ci. The
// COLLATE was written on the octo_project side, with a comment saying that keeps
// the octo_project indexes usable. True, and it hid the cost: an explicit COLLATE
// has coercibility 0, so the COMPARISON is general_ci, and `group.group_no` must
// then be converted per row — group_groupNo, its UNIQUE index, cannot serve it.
//
// Measured on MySQL 8.0.46 with `group` at 0900_ai_ci / 5000 rows:
//
//	joined form:  p const uk_octo_project_project_id 1 row
//	              g ALL   possible_keys=group_groupNo  key=NULL  rows=5000
//	split form:   p const uk_octo_project_project_id 1 row
//	              g const group_groupNo               1 row
//
// This predicate is the WHOLE of the D7 guard, reached from six user-facing
// handlers (group disband / exit / member removal / owner transfer / blacklist,
// and the bot API member removal). A full scan of `group` — a core IM table — on
// every group exit is not a background cost. Raised in PR #855's fifth review.
//
// Two statements rather than moving the COLLATE to the group side, because the
// second option is right only until the collation conversion lands and then
// silently wrong in the same way: naming 0900_ai_ci would, after conversion, force
// the octo_project side to convert instead. A comparison that crosses no schemas
// needs no collation opinion and survives the conversion untouched.
//
// Not atomic against a concurrent detach — and neither was the join: nothing here
// takes a lock, so both forms answer from a snapshot that can be stale by the time
// the caller acts on it. D7's guard tolerates that (its failure mode is a refusal
// that should have been allowed, or the reverse, both of which the I4 scans see).
//
// # Callers MUST short-circuit on an empty projectID
//
// A Space-direct group carries the empty project_id sentinel and this predicate does not apply to
// it. Passing "" is answered false without a query, but the caller is still
// expected not to reach here: this runs on group exit / disband / member removal
// / owner transfer, which are user-facing paths carrying the product's traffic,
// and a query that runs and returns false is still a latency regression on every
// one of them. Same rule as P1's admission gate (C1), for the same reason.
func IsAllMemberGroup(session *dbr.Session, projectID, groupNo string) (bool, error) {
	if projectID == "" || groupNo == "" {
		return false, nil
	}
	// Two single-table reads, not one join. See the "no cross-schema comparison"
	// section above for the measurement.
	var pointers []string
	if _, err := session.SelectBySql(
		sqlAllMemberGroupPointer,
		projectID,
	).Load(&pointers); err != nil {
		return false, err
	}
	if len(pointers) == 0 || pointers[0] != groupNo {
		return false, nil
	}
	// The group side, on its own. `status <> disband` and `project_id = ?` are the
	// second half of the predicate, and the reason they are required is in the doc
	// comment: a group P1 detached to Space-direct is no longer the project's, and
	// keeping it protected would leave its members unable to ever leave it.
	var ok []int
	if _, err := session.SelectBySql(
		sqlAllMemberGroupRow,
		groupNo, GroupStatusDisband, projectID,
	).Load(&ok); err != nil {
		return false, err
	}
	return len(ok) > 0, nil
}

// The two statements IsAllMemberGroup runs, as constants so a plan guard can
// EXPLAIN exactly what production executes instead of a copy that can drift away
// from it — a copy would pin the plan of a statement nobody runs, which is the
// same failure the drift test exists to prevent one level down.
const (
	sqlAllMemberGroupPointer = "SELECT all_member_group_no FROM `octo_project` " +
		"WHERE project_id = ? AND status = 1"
	sqlAllMemberGroupRow = "SELECT 1 FROM `group` " +
		"WHERE group_no = ? AND status <> ? AND project_id = ?"
)

// AllMemberGroupPredicateStatementsForTest returns those two statements in the
// order IsAllMemberGroup runs them, with the placeholder counts they expect
// (1 and 3). Exported for the collation-drift plan guard in modules/project,
// which is where the drifted-database fixture lives; same shape as
// modules/project.AllMemberGroupHooksRegisteredForTest.
func AllMemberGroupPredicateStatementsForTest() []string {
	return []string{sqlAllMemberGroupPointer, sqlAllMemberGroupRow}
}

// ErrAdmittedButNotSubscribed marks the one admission failure whose damage is NOT
// what the failure message would suggest: the group_member row committed, and the
// broker subscription that follows it did not.
//
// It matters because the two states have different witnesses. A failed admission
// transaction leaves an active project seat with no group row, which I4 scan B
// compares and reports. This one leaves the row in place, so scan B is structurally
// blind to it — what is missing is a subscriber entry, and open_verification records
// that nothing in this repository can read a channel's subscribers back from the
// broker. Pointing on-call at scan B for it is worse than saying nothing.
//
// Lives here because modules/project must never import modules/group: the group side
// wraps it, the project side matches it with errors.Is. PR #855s seventh review.
var ErrAdmittedButNotSubscribed = errors.New("project: admitted to the all-member group but not subscribed")

// GroupStatusDisband mirrors modules/group.GroupStatusDisband.
//
// Spelled out rather than imported: pkg/project is a leaf that modules/group
// imports, so importing back would be a cycle. Everything on this side of the
// boundary that has to reason about a disbanded group reads THIS constant rather
// than writing 2 again, so there is exactly one literal to keep in step —
// modules/group's TestGroupStatusDisbandMatchesPkgProject is what keeps it there.
const GroupStatusDisband = 2

// PickActiveOwner returns an active owner of projectID, preferring the
// longest-standing one, or "" when the project has none.
//
// Used by the all-member group's owner sync (D6) to choose who should hold the
// group when its current creator is no longer a project owner.
//
// "Longest-standing" follows P0's precedent for the Space cascade's successor
// choice (the senior remaining member) rather than inventing a second rule.
// Seniority is a stable, explainable answer, and it does not depend on request
// ordering the way "whoever was promoted last" would.
//
// Returns "" rather than an error for an ownerless project. That state is
// reachable — P0's Space cascade can close a sole owner's seat and leaves the
// project active with no owner, deliberately and with a Warn — so a caller must
// treat "no owner" as a normal answer and leave the group's creator where it is.
// Takes a dbr.SessionRunner for the reason MemberRole documents: the owner sync
// asks both questions while holding locks, and both answers have to come from
// the transaction that will act on them.
func PickActiveOwner(session dbr.SessionRunner, projectID string) (string, error) {
	if projectID == "" {
		return "", nil
	}
	var uids []string
	_, err := session.SelectBySql(
		"SELECT uid FROM `octo_project_member` "+
			"WHERE project_id = ? AND role = ? AND status = 1 AND removing = 0 "+
			// created_at then uid: created_at alone is not a total order (two seats
			// written in the same millisecond tie), and a non-deterministic pick
			// would make the sync's outcome depend on the storage engine's row
			// order — untestable, and different on a replica.
			"ORDER BY created_at ASC, uid ASC LIMIT 1",
		projectID, projectRoleOwner,
	).Load(&uids)
	if err != nil {
		return "", err
	}
	if len(uids) == 0 {
		return "", nil
	}
	return uids[0], nil
}

// An exported AllMemberGroupNo used to live here, answering "what does the
// project point at" without verifying the group side. It had zero callers, and
// it kept answering the PRE-FIX version of the question after IsAllMemberGroup
// gained the disbanded-group and project_id checks — so the one exported helper
// on this subject was the one that would give the wrong answer.
//
// Deleted rather than fixed: an exported predicate with no caller is a trap
// whichever contract it has, and modules/project reads the column through its own
// queryAllMemberGroupNo, which does verify the group side. If a caller outside
// that module ever needs it, add it back with that caller and with the same
// checks IsAllMemberGroup makes. PR #855s second review.

// AllMemberGroupGuardFailures counts D7 guard evaluations that could not be
// decided and were therefore let through.
//
// The guards on both sides — modules/group's five handlers and modules/bot_api's
// member removal — fail OPEN when the predicate query errors, and that choice is
// right: fail-closed turns one database hiccup into "nobody can leave any project
// group", which a user cannot route around, while fail-open leaves an I4 gap that
// reconcile scan B reports and an admin can repair.
//
// It is only right if a fail-open is LOUD. The argument in those comments assumes
// a transient error, but the failure shapes that actually matter — a schema
// change, a collation mismatch — fail deterministically on EVERY call, which turns
// D7 off wholesale with nothing behind it but log lines. This counter is what
// separates "one hiccup on Tuesday" from "the guard has been off since Tuesday".
// Raised as Q12 in PR #855's review.
//
// Here rather than in either module because three call sites across two modules
// share it, and a per-module copy would make the dashboard question ("is D7
// deciding?") need to be asked twice.
var AllMemberGroupGuardFailures = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "octo_project",
	Name:      "all_member_group_guard_failures_total",
	Help: "D7 all-member-group guard evaluations that failed and were let through, " +
		"by the action that was allowed.",
}, []string{"action"})
