package project

import (
	"regexp"
	"strings"
	"testing"
)

// TestLifecycleVersionBumpsOnEveryLifecycleWrite pins the CALL SITES.
//
// The increment-only guard above says the column can only go up; it says nothing
// about whether anyone bumps it. A lifecycle write that forgot to would leave two
// distinct project states sharing a version, and a consumer would silently keep
// the older one.
func TestLifecycleVersionBumpsOnEveryLifecycleWrite(t *testing.T) {
	src := readStripped(t, "service.go")

	for _, site := range []struct{ fn, why string }{
		{"updateProjectOnce", "a profile change is a lifecycle statement"},
		{"disbandProjectOnce", "disband is a lifecycle statement"},
	} {
		body := funcBody(t, src, "func (p *Project) "+site.fn+"(")
		if !strings.Contains(body, "bumpLifecycleVersionTx") {
			t.Errorf("%s must call bumpLifecycleVersionTx: %s", site.fn, site.why)
		}
	}

	// Creation BUMPS rather than seeding. An earlier draft wrote LifecycleVersion 1
	// into the insert literal; that is an absolute write, the one shape this
	// column's discipline forbids, and member_epoch already had to be redone for
	// exactly it (PR #852 round 1). The bump runs after the insert, where the row
	// exists for the status-guarded UPDATE to match.
	create := funcBody(t, src, "func (p *Project) createProjectOnce(")
	if !strings.Contains(create, "bumpLifecycleVersionTx") {
		t.Error("createProjectOnce must bump the lifecycle version; creation is itself a " +
			"lifecycle statement and a consumer has to order it against what follows")
	}
	insertAt := strings.Index(create, "insertProjectTx")
	bumpAt := strings.Index(create, "bumpLifecycleVersionTx")
	if insertAt < 0 || bumpAt < insertAt {
		t.Error("the lifecycle bump must come AFTER the project insert, or its status guard " +
			"matches no row and silently does nothing")
	}
	if regexp.MustCompile(`LifecycleVersion:\s*\d`).MatchString(create) {
		t.Error("createProjectOnce must not seed LifecycleVersion in the insert literal: an " +
			"absolute write is what lets two writers race the version backwards")
	}
}

// TestLifecycleVersionBumpPrecedesStatusFlipOnDisband pins an ordering that is
// invisible until it breaks.
//
// bumpLifecycleVersionTx is guarded on status = StatusNormal, which is what keeps
// a disbanded project frozen. Disband is the one write that must move the version
// AND change the status, so the bump has to happen first. Flip first and the
// bump silently matches zero rows: no error, no version change, and the disband
// statement a consumer receives carries the same version as the state before it.
func TestLifecycleVersionBumpPrecedesStatusFlipOnDisband(t *testing.T) {
	body := funcBody(t, readStripped(t, "service.go"), "func (p *Project) disbandProjectOnce(")
	bumpAt := strings.Index(body, "bumpLifecycleVersionTx")
	flipAt := strings.Index(body, "disbandProjectTx")
	if bumpAt < 0 || flipAt < 0 {
		t.Fatal("disbandProjectOnce must both bump the lifecycle version and flip the status")
	}
	if bumpAt > flipAt {
		t.Fatal("bumpLifecycleVersionTx must run BEFORE disbandProjectTx: the bump is guarded " +
			"on status = 1, so after the flip it matches no rows and silently does nothing")
	}
}
