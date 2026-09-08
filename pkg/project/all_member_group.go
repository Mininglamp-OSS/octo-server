package project

import (
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
	var count int
	err := session.SelectBySql(
		"SELECT COUNT(*) FROM `octo_project` p "+
			// The `group` side of the join carries the COLLATE: `group` is a legacy
			// table with no declared collation (utf8mb4_0900_ai_ci in production)
			// while octo_project is pinned to utf8mb4_general_ci, and an implicit
			// comparison raises MySQL 1267 in production while passing in CI, whose
			// database is created with general_ci. Written on the driving side's
			// values so the octo_project indexes stay usable — the same rule P1's
			// reconcile scans follow.
			"INNER JOIN `group` g ON g.group_no = p.all_member_group_no COLLATE utf8mb4_general_ci "+
			"WHERE p.project_id = ? AND p.status = 1 "+
			"  AND p.all_member_group_no = ? "+
			// A disbanded group is not the project's all-member group any more,
			// and the two other predicates over this same fact —
			// queryAllMemberGroupNo and I4 scan A — both say so. Leaving it out
			// here made three readers of one fact answer with two different
			// answers, which is the shape that already cost a review round
			// (clearStaleAllMemberGroupPointer exists because the read side got
			// stricter than the write side and nobody noticed until the rebuild
			// silently stopped happening).
			//
			"  AND g.status <> ? "+
			"  AND g.project_id = ? COLLATE utf8mb4_general_ci",
		projectID, groupNo, GroupStatusDisband, projectID,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

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
func PickActiveOwner(session *dbr.Session, projectID string) (string, error) {
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
