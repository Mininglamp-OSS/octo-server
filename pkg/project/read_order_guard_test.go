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

// TestProjectEpochsExcludesInactiveProjects pins that the epoch query filters on
// status. This is what makes disband (and any future archived state, which is
// also not status 1) converge without a dedicated event: the project drops out,
// the caller reads 0, and every authorization snapshot taken against the old
// epoch stops matching.
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
		t.Error("ProjectEpochsInSpace must restrict to status = 1, so a disbanded or " +
			"archived project reads as epoch 0")
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
