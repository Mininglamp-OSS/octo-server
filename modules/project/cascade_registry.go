package project

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// Reverse-registered cascade steps.
//
// modules/project must never import modules/group — measured at HEAD, that
// import is 0 in both directions, and keeping the project side free of it is
// what lets pkg/project stay a plain predicate instead of a module. So the group
// work is registered INTO this module, exactly as modules/group and
// modules/project both already register steps into modules/space.
//
// # The step contract
//
// Copied deliberately from modules/space/member_removal.go:56-64, because the
// same worker shape produces the same hazards:
//
//   - A step must be IDEMPOTENT. It will be re-run after a partial failure.
//   - A step decides "nothing to do" itself and returns nil. It must not assume
//     any other step ran, or ran first.
//   - A returned error re-drives the WHOLE job, including steps that already
//     succeeded. So a step that is expensive and already done must detect that
//     cheaply rather than redoing it.
//   - A step must not assume it runs promptly. A job can sit in the queue, and
//     the member may have been re-admitted meanwhile — which is why the worker
//     re-reads the member row under lock before every batch and why a step
//     should re-check anything it is about to destroy.

// MemberRemoval describes one project seat being closed.
type MemberRemoval struct {
	ProjectID   string
	UID         string
	SpaceID     string
	OperatorUID string
	// Reason is one of the removalReason constants: kicked / left /
	// project_disbanded.
	Reason string
}

// MemberRemovalStep detaches one uid from whatever the registering module owns.
type MemberRemovalStep func(ctx *config.Context, removal MemberRemoval) error

// ProjectDisband describes a project being disbanded, whether by its owner or by
// the ownerless-project branch of P0's Space cascade.
type ProjectDisband struct {
	ProjectID string
	SpaceID   string
	// ByCascade is true when a background worker disbanded the project because
	// it had no owner left, rather than a human choosing to.
	//
	// P0's round-2 review made the Space cascade hand ownership to the senior
	// remaining member and disband the project when there is no successor. That
	// is why the detach step must be reachable from the cascade and not only
	// from the disband handler: a project can now end without anyone asking.
	ByCascade bool
}

// DisbandStep reverts whatever the registering module attached to the project.
type DisbandStep func(ctx *config.Context, disband ProjectDisband) error

type namedMemberRemovalStep struct {
	name string
	fn   MemberRemovalStep
}

type namedDisbandStep struct {
	name string
	fn   DisbandStep
}

var (
	cascadeMu           sync.RWMutex
	memberRemovalSteps  []namedMemberRemovalStep
	projectDisbandSteps []namedDisbandStep
)

// RegisterProjectMemberRemovalStep registers work to run when a project seat
// closes. Re-registering the same name replaces (latest wins), which is what
// lets a test install a stand-in.
func RegisterProjectMemberRemovalStep(name string, fn MemberRemovalStep) {
	if name == "" || fn == nil {
		return
	}
	cascadeMu.Lock()
	defer cascadeMu.Unlock()
	for i := range memberRemovalSteps {
		if memberRemovalSteps[i].name == name {
			memberRemovalSteps[i].fn = fn
			return
		}
	}
	memberRemovalSteps = append(memberRemovalSteps, namedMemberRemovalStep{name: name, fn: fn})
}

// RegisterProjectDisbandStep registers work to run when a project is disbanded.
func RegisterProjectDisbandStep(name string, fn DisbandStep) {
	if name == "" || fn == nil {
		return
	}
	cascadeMu.Lock()
	defer cascadeMu.Unlock()
	for i := range projectDisbandSteps {
		if projectDisbandSteps[i].name == name {
			projectDisbandSteps[i].fn = fn
			return
		}
	}
	projectDisbandSteps = append(projectDisbandSteps, namedDisbandStep{name: name, fn: fn})
}

func snapshotMemberRemovalSteps() []namedMemberRemovalStep {
	cascadeMu.RLock()
	defer cascadeMu.RUnlock()
	out := make([]namedMemberRemovalStep, len(memberRemovalSteps))
	copy(out, memberRemovalSteps)
	return out
}

func snapshotDisbandSteps() []namedDisbandStep {
	cascadeMu.RLock()
	defer cascadeMu.RUnlock()
	out := make([]namedDisbandStep, len(projectDisbandSteps))
	copy(out, projectDisbandSteps)
	return out
}
