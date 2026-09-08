package project

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The invariant this file guards:
//
//	EVERY writer that can change the answer to "is this uid a member of this
//	project" must move member_epoch in the same transaction.
//
// Rounds 5, 6 and 7 of this PR's review each found a DIFFERENT writer that did
// not, which is the signature of per-call-site discipline rather than a closed
// argument. Two of the three axes are structural and already closed by
// construction:
//
//   - octo_project_member writes all live in modules/project and all run the
//     lock-project / write-seat / bump three-step; TestMemberEpochOnlyEverIncrements
//     pins the write SHAPE.
//   - Space ban and Space disband move nothing, and do not need to: an inactive
//     parent Space folds every one of its projects into the absent answer at READ
//     time (pkg/project.ProjectEpochsInSpace), so the answer and the invalidation
//     channel move together without a write.
//
// What is left is the third axis: a single member's space_member row being CLOSED OR
// REOPENED while the Space stays active. Either transition flips _verify (its Space
// conjunction sees both) without touching any octo_project row, so unless the writer
// moves the epoch, a peer re-reads the same number and its staleness check AGREES.
//
// Both directions matter, and only assuming otherwise is how round 8 found the
// reopening half still open. A consumer caches the DECISION, not only positive
// grants: a stuck cached DENIAL locks a valid returning member out for as long as a
// stuck cached grant leaks access. So "does not close a seat" is NOT a sufficient
// reason to exempt a writer — "does not change the answer in either direction" is.
//
// # This sweep can only express ONE of the four axes
//
// Stated here because a guard that looks complete is worse than one that admits its
// scope. The facts that can change _verify's answer, and what moves the epoch today:
//
//	octo_project_member.status/.removing -> same-tx bump on every project-side write
//	space_member.status (both directions) -> same-tx bump via the removal/rejoin steps,
//	                                        EXCEPT the bot writers (see the baseline)
//	space.status                          -> read-time fold through IsActiveSpace
//	user.status / user.is_destroy         -> read-time conjunction (pkg/user.ActiveAccounts)
//	                                        plus an admission gate; a ban does NOT move
//	                                        the epoch — recorded deviation, see
//	                                        docs/project-member-epoch-rollout.md
//
// Four axes, four different mechanisms, one table this sweep can scan. Rounds 5-8
// each found a different writer for exactly that reason: an instrument cannot fail
// on what it cannot express. A new axis needs its own guard, not a wider regexp
// here.
//
// This sweep is the structural half. It cannot verify that a writer bumps — that
// needs execution — but it CAN make a new writer impossible to add silently, which
// is the failure mode that produced three rounds of the same finding.

// spaceMemberWrite matches any write to space_member: SQL text or dbr builder.
//
// Deliberately over-broad. It also catches `SET status=1` (re-admission, which
// needs no bump) and the disband paths (covered by the read-time fold), because
// the cost of a false positive is one line in the baseline with a reason, and the
// cost of a false negative is a fourth axis of an authorization fact that outlives
// its revocation.
//
// Written as alternatives rather than one clever pattern so a new call style
// (a builder, a raw statement, a heredoc) fails to match only ITS alternative
// instead of quietly emptying the whole guard.
// spaceMemberTable matches the table name and REFUSES a longer one.
//
// The trailing [^0-9A-Za-z_] is not cosmetic: without it the pattern also matched
// space_member_removal_cleanup, i.e. the outbox table, which inflated the
// sanctioned files' counts with writes that have nothing to do with Space seats.
// A baseline padded with unrelated matches is how a count-based guard goes blind —
// a real new writer can then be offset by an unrelated deletion.
const spaceMemberTable = "`?space_member`?[^0-9A-Za-z_]"

var spaceMemberWrite = regexp.MustCompile(
	`(?i)(?:UPDATE\s+` + spaceMemberTable +
		`|DELETE\s+FROM\s+` + spaceMemberTable +
		`|INSERT\s+(?:IGNORE\s+)?INTO\s+` + spaceMemberTable +
		`|\.Update\(\s*"space_member"` +
		`|\.DeleteFrom\(\s*"space_member"` +
		`|\.InsertInto\(\s*"space_member")`)

// spaceMemberWriterBaseline records every file that writes space_member, the
// number of writes in it, and why that is acceptable.
//
// Counts, not line numbers: line numbers churn on every edit, while a count change
// is exactly "someone added or removed a writer here". A new file, or one more
// write in an existing file, fails this test.
var spaceMemberWriterBaseline = map[string]struct {
	writes int
	why    string
}{
	"modules/space/db_manager.go": {
		writes: 6,
		why: "SANCTIONED. The two seat-CLOSING paths — removeMemberLockedOnce and " +
			"removeMembersForceOnce — run runMemberRemovalTxSteps in the same " +
			"transaction, which is where bumpMemberEpochForSpaceMemberTx is registered. " +
			"Of the rest: the admin disband is covered by the read-time Space fold; the " +
			"admin re-admission is an INSERT of a seat that did not exist, so no project " +
			"seat can have survived for it to reopen (a fresh member is admitted to " +
			"projects by a project-side write, which bumps on its own); the two role " +
			"writes change space_member.role, which is not part of the project " +
			"membership answer.",
	},
	"modules/space/db.go": {
		writes: 11,
		why: "SANCTIONED. Single-member removal delegates to removeMemberLocked. BOTH " +
			"reactivation paths (reactivateMember, atomicReactivateMemberIfNotFull) now " +
			"run runMemberReactivationTxSteps in the same transaction — reopening a seat " +
			"makes a SURVIVING project seat reachable again through the Space " +
			"conjunction with no project-side write, so nothing else would move the " +
			"epoch and a consumer's cached denial would keep agreeing with it. The " +
			"disband paths close every member at once and need no bump: an inactive " +
			"Space folds all of its projects into the absent answer at read time " +
			"(pkg/project.ProjectEpochsInSpace). The remaining writes are INSERTs of " +
			"seats that did not exist (no surviving project seat to reopen) and role " +
			"changes (role is not part of the project membership answer).",
	},
	"modules/botfather/api_user.go": {
		writes: 2,
		why: "ONE INSERT (grants a bot a Space seat — admission needs no bump, see " +
			"mint_obo.go) plus ONE KNOWN GAP: `UPDATE space_member SET status=0 WHERE " +
			"uid=?` on bot deletion (api_user.go:529), outside any transaction, so it " +
			"moves no epoch and enqueues no cleanup. A bot CAN hold an " +
			"octo_project_member seat — project admission applies no bot filter — so on " +
			"this axis a peer's cached grant is permanent rather than bounded. Both " +
			"available fixes change user-visible behaviour (refuse bots into projects, " +
			"or route bot deletion through removeMemberLocked, which enqueues group " +
			"cleanup and therefore emits Tip messages bot deletion does not emit today), " +
			"so this needs a product decision. Tracked, not fixed.",
	},
	"modules/botfather/command.go": {
		writes: 1,
		why:    "KNOWN GAP — bot axis, the same statement as api_user.go:529. See above.",
	},
	"modules/botfather/db.go": {
		writes: 3,
		why: "ONE INSERT (admission) plus TWO KNOWN GAPS on the bot axis: " +
			"`DELETE FROM space_member WHERE uid=?` (db.go:231) and its fail-closed " +
			"`UPDATE ... SET status=0` fallback (db.go:270). See api_user.go above.",
	},
	"modules/botfather/mint_obo.go": {
		writes: 1,
		why: "INSERT IGNORE of a bot's Space seat. On its own this needs no bump: an " +
			"INSERT creates a seat that did not exist, so no surviving project seat is " +
			"reopened by it. It becomes reachable ONLY in combination with the bot-" +
			"deletion gap below (deletion leaves the project seat active forever, so a " +
			"re-minted Space seat reopens the answer over it with nothing bumping) — " +
			"i.e. it is a consequence of that gap, closed when that gap is, and tracked " +
			"with it rather than separately.",
	},
}

func TestEverySpaceMemberWriterIsAccountedFor(t *testing.T) {
	root := repoRootForGuard(t)
	found := map[string]int{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "node_modules", ".octospec":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		n := len(spaceMemberWrite.FindAllString(string(data), -1))
		if n == 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		found[filepath.ToSlash(rel)] = n
		return nil
	})
	if err != nil {
		t.Fatalf("sweep space_member writers: %v", err)
	}

	// A guard that matches nothing passes vacuously. This is the mutation check:
	// if a refactor renames the table or changes every call style, the sweep must
	// FAIL rather than report a clean tree.
	if len(found) == 0 {
		t.Fatal("the sweep found no space_member writer at all; the pattern has " +
			"stopped matching this repository and this guard is no longer guarding anything")
	}

	for file, n := range found {
		entry, ok := spaceMemberWriterBaseline[file]
		if !ok {
			t.Errorf("%s writes space_member (%d write(s)) and is not in "+
				"spaceMemberWriterBaseline.\n"+
				"Every writer that can close a member's Space seat must move "+
				"member_epoch in the same transaction, or a peer keeps a grant its "+
				"staleness check cannot detect. Route it through "+
				"modules/space.removeMemberLocked (which runs the registered tx step), "+
				"or add it here with the reason it does not need to.", file, n)
			continue
		}
		if n != entry.writes {
			t.Errorf("%s now has %d space_member write(s), baseline says %d.\n"+
				"Baseline reason: %s\n"+
				"If you added a writer, make sure it moves member_epoch (see "+
				"modules/project.bumpMemberEpochForSpaceMemberTx) and then update the count.",
				file, n, entry.writes, entry.why)
		}
	}

	// The other direction: a baseline entry that no longer matches means the guard
	// has been silently emptied for that file — the exact way a source guard dies.
	for file := range spaceMemberWriterBaseline {
		if _, ok := found[file]; !ok {
			t.Errorf("spaceMemberWriterBaseline lists %s but the sweep found no "+
				"space_member write there; either the writer moved (re-point the baseline) "+
				"or the pattern stopped matching it (fix the pattern)", file)
		}
	}
}
