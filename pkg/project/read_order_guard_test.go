package project

import (
	"os"
	"strings"
	"testing"
)

// Source guard for the read order inside ProjectMemberships.
//
// The correctness property it protects cannot be observed from a single-threaded
// test: both orderings return identical results unless a membership write lands
// BETWEEN the two queries. So the guarantee holds by construction — epoch first,
// seats second — and this test is what keeps that true.
//
// Why it matters, restated so a future reader does not "tidy" the order away:
// a consumer caches this answer keyed by (uid, project, member_epoch) and
// re-reads the epoch later to decide whether the cache is still valid. That is
// only sound while the returned epoch is never NEWER than the membership data
// beside it.
//
//	epoch first  -> a concurrent change yields OLD epoch + NEW seats. The
//	                consumer's next epoch check differs, so it re-verifies.
//	                One wasted round trip.
//	seats first  -> the same interleaving yields NEW epoch + OLD seats. The
//	                consumer caches a stale positive under the CURRENT epoch and
//	                its epoch checks keep agreeing, so a removed member stays
//	                authorized until some unrelated write bumps the epoch again.
//
// The second shape is a silent authorization bug with no failing test, which is
// exactly the kind this repository writes grep guards for (see
// TestMemberEpochOnlyEverIncrements in modules/project).
func TestProjectMembershipsReadsEpochBeforeSeats(t *testing.T) {
	raw, err := os.ReadFile("membership.go")
	if err != nil {
		t.Fatalf("read membership.go: %v", err)
	}
	src := string(raw)

	start := strings.Index(src, "func ProjectMemberships(")
	if start < 0 {
		t.Fatal("ProjectMemberships not found — if it moved, point this guard at the new location rather than deleting it")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}

	epochAt := strings.Index(body, "ProjectEpochsInSpace(")
	if epochAt < 0 {
		t.Fatal("ProjectMemberships must read the epoch through ProjectEpochsInSpace")
	}
	seatsAt := strings.Index(body, "octo_project_member")
	if seatsAt < 0 {
		t.Fatal("ProjectMemberships no longer queries octo_project_member")
	}
	if epochAt > seatsAt {
		t.Fatal("ProjectMemberships must read member_epoch BEFORE the seat rows. " +
			"Reading seats first lets a concurrent membership change return a NEW epoch " +
			"beside OLD seats, which a consumer caches as a stale positive under the " +
			"current epoch — see this test's comment.")
	}
}

// TestBatchPredicatesExcludeClosingSeats pins that both new predicates keep the
// package-wide agreement that a seat being closed is not a member. A consumer
// that disagreed with the admission gate about `removing` would authorize access
// to a project whose groups are being torn down.
func TestBatchPredicatesExcludeClosingSeats(t *testing.T) {
	raw, err := os.ReadFile("membership.go")
	if err != nil {
		t.Fatalf("read membership.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func ProjectMemberships(")
	if start < 0 {
		t.Fatal("ProjectMemberships not found")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "removing = 0") {
		t.Error("ProjectMemberships must exclude rows with removing = 1")
	}
	if !strings.Contains(body, "status = 1") {
		t.Error("ProjectMemberships must restrict to active seats")
	}
}

// TestProjectMembershipsConjoinsTheSpaceHalf pins the other half of the answer.
//
// A project seat is not authorization on its own: the Space→project cascade is
// asynchronous, its cleanup step deactivates the seat without the synchronous
// `removing = 1` phase, and it stops retrying after a cap with the row kept but
// never re-claimed. A user removed from the Space therefore keeps a `status = 1
// AND removing = 0` seat for a window that is unbounded in the failure case.
//
// Every other caller of that predicate sits behind a Space gate. This one does
// not — its consumer is a peer asking about a third party it holds no token for,
// and no endpoint here lets it obtain the Space half — so the conjunction is the
// server's job. Deleting it would be invisible to any test whose fixtures keep
// the two tables agreeing, which is why this is a source guard.
func TestProjectMembershipsConjoinsTheSpaceHalf(t *testing.T) {
	body := funcSourceBody(t, "ProjectMemberships")
	if !strings.Contains(body, "space.ActiveMembers(") {
		t.Fatal("ProjectMemberships must conjoin the Space half through space.ActiveMembers — " +
			"a project seat outlives Space removal by an unbounded window, and this " +
			"function's consumer cannot apply the Space check itself. See this test's comment.")
	}

	// AFTER the seats. The Space read can only remove uids from the answer, so it
	// is the one read whose freshest data is the fail-closed direction. Placed
	// before the seats, a Space removal committing in between yields a stale
	// positive — the exact bug the conjunction is here to remove.
	seatsAt := strings.Index(body, "octo_project_member")
	spaceAt := strings.Index(body, "space.ActiveMembers(")
	if spaceAt < seatsAt {
		t.Fatal("the Space check must run AFTER the seat query: it can only narrow the answer, " +
			"so reading it last is the fail-closed order")
	}

	// Through pkg/space, not a hand-rolled copy. A near-copy of
	// `space_member.status = 1 AND space.status = 1` drifts from CheckMembership's,
	// and pkg/space's own comment says so.
	if strings.Contains(body, "space_member") {
		t.Error("ProjectMemberships must not spell the Space predicate out itself; " +
			"space.ActiveMembers exists so the two cannot drift")
	}
}

// funcSourceBody returns the source of one function in membership.go.
func funcSourceBody(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("membership.go")
	if err != nil {
		t.Fatalf("read membership.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func "+name+"(")
	if start < 0 {
		t.Fatalf("%s not found — if it moved, point this guard at the new location rather than deleting it", name)
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	return body
}

// TestProjectEpochsExcludesInactiveProjects pins that the epoch query filters on
// status. This is what makes disband converge without a dedicated event: the
// project drops out, the caller reads 0, and every authorization snapshot taken
// against the old epoch stops matching.
//
// It covers a future ARCHIVED state only if archive is modelled as a status
// value. If it lands as a separate `archived_at` column beside `status = 1`,
// this predicate does NOT fold it in and the statement needs an explicit
// `archived_at IS NULL` — an earlier version of this comment asserted the free
// coverage unconditionally, which would have read as "already handled" to
// whoever adds the column.
func TestProjectEpochsExcludesInactiveProjects(t *testing.T) {
	raw, err := os.ReadFile("membership.go")
	if err != nil {
		t.Fatalf("read membership.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func ProjectEpochsInSpace(")
	if start < 0 {
		t.Fatal("ProjectEpochsInSpace not found")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "status = 1") {
		t.Error("ProjectEpochsInSpace must restrict to status = 1, so a disbanded project " +
			"reads as epoch 0")
	}

	// The PARENT Space too. A ban flips space.status and touches no project row,
	// so without this the epoch keeps saying "nothing changed" while
	// ProjectMemberships — which does see the ban through its Space conjunction —
	// flips its answer. The peer's only invalidation channel is the epoch, so a
	// cached grant survives the ban and a cached denial survives the unban, both
	// unbounded. Disband is worse: nothing disbands the projects of a disbanded
	// Space, so their rows stay status = 1 forever.
	if !strings.Contains(body, "space.IsActiveSpace(") {
		t.Fatal("ProjectEpochsInSpace must fold an inactive parent Space into the absent " +
			"answer, through space.IsActiveSpace. The two predicates in this file must not " +
			"disagree about whether Space status is part of the answer — see this test's comment.")
	}

	// After the project rows, for the same reason ProjectMemberships reads its
	// Space half last: this read can only REMOVE projects, so freshest-last is
	// the fail-closed order.
	rowsAt := strings.Index(body, "octo_project")
	spaceAt := strings.Index(body, "space.IsActiveSpace(")
	if spaceAt < rowsAt {
		t.Error("the Space check must run AFTER the project query: it can only narrow the " +
			"answer, so reading it last means a ban landing in between drops the projects " +
			"rather than serving a live epoch for a Space that is already banned")
	}

	// Not a JOIN. octo_project pins utf8mb4_general_ci and `space` is a 2019
	// table measured at utf8mb4_0900_ai_ci in production, so an implicit
	// cross-schema comparison is error 1267 THERE while green in CI — and on a
	// fail-closed endpoint that denies the peer everything.
	if strings.Contains(body, "JOIN") {
		t.Error("ProjectEpochsInSpace must not JOIN `space`: that comparison crosses the " +
			"pinned and legacy collations and fails only in production. Every project in " +
			"one call shares one space_id, so a single-row lookup answers the same question.")
	}
	if !strings.Contains(body, "space_id = ?") {
		t.Error("ProjectEpochsInSpace must filter by space_id: a project in another " +
			"Space has to be indistinguishable from one that does not exist")
	}
}

// TestDedupeNonEmpty covers the shared input sanitizer both batch queries use.
func TestDedupeNonEmpty(t *testing.T) {
	got := dedupeNonEmpty([]string{"b", "", "a", "b", "a", ""})
	want := []string{"b", "a"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("first-seen order must be preserved: want %v, got %v", want, got)
		}
	}
	if len(dedupeNonEmpty(nil)) != 0 {
		t.Error("nil input must produce an empty slice")
	}
}
