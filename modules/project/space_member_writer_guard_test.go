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
// # A baseline whose REASON is false is worse than a missing entry
//
// Round 9 found the reactivation axis still open on two of its four writers, and
// this file is part of why: the counts were correct, so the guard was green, while
// two entries asserted coverage that the code contradicted ("BOTH reactivation
// paths", "an INSERT of a seat that did not exist"). Nothing mechanical can check a
// prose reason.
//
// So when adding an entry: enumerate from the SQL, not from the function names. Two
// of the four reactivation writers — approveJoinApplyAtomic and upsertMembers — have
// no form of "reactivate" in their signature, and one of them is the DESIGNED rejoin
// funnel. "It looks like an insert" is not a finding; `ON DUPLICATE KEY UPDATE
// status=1` against a unique index is a reactivation.
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
		writes: 7,
		why: "SANCTIONED. The two seat-CLOSING paths — removeMemberLockedOnce and " +
			"removeMembersForceOnce — run runMemberRemovalTxSteps in the same " +
			"transaction, which is where bumpMemberEpochForSpaceMemberTx is registered. " +
			"upsertMembers REOPENS seats (its ON DUPLICATE branch fires on any existing " +
			"removed row) and now runs runMemberReactivationTxSteps for exactly that " +
			"case, gated on a locking read of the row's prior status. The previous " +
			"version of this entry called it \"an INSERT of a seat that did not exist\", " +
			"which its own function name and comment (upsert / add-REACTIVATE) " +
			"contradicted — recorded because a baseline whose REASON is false is worse " +
			"than a missing entry: the counts stay right and the guard reads as green. " +
			"Of the rest: the admin disband is covered by the read-time Space fold, and " +
			"the two role writes change space_member.role, which is not part of the " +
			"project membership answer.",
	},
	"modules/space/db.go": {
		writes: 11,
		why: "SANCTIONED. Single-member removal delegates to removeMemberLocked. ALL " +
			"THREE reactivation writers in this file now run " +
			"runMemberReactivationTxSteps in the same transaction: reactivateMember, " +
			"atomicReactivateMemberIfNotFull, and approveJoinApplyAtomic's " +
			"reactivation branch (memberRows > 0, i.e. an existing row whose status is " +
			"0 — the status==1 case returns approveAlreadyMember earlier under " +
			"FOR UPDATE). Reopening a seat makes a SURVIVING project seat reachable " +
			"again through the Space conjunction with no project-side write, so nothing " +
			"else would move the epoch and a consumer's cached denial would keep " +
			"agreeing with it. The previous version of this entry said \"BOTH " +
			"reactivation paths\" — there were four across this file and db_manager.go, " +
			"and approveJoinApplyAtomic is the DESIGNED rejoin funnel " +
			"(resetApprovedApplyForRejoin exists to route a removed member back through " +
			"it). Enumerate reactivation writers from the SQL, not from the names: two " +
			"of the four have no form of \"reactivate\" in their signature. The disband " +
			"paths close every member at once and need no bump: an inactive Space folds " +
			"all of its projects into the absent answer at read time " +
			"(pkg/project.ProjectEpochsInSpace). The remaining writes are INSERTs of " +
			"seats that did not exist (no surviving project seat to reopen) and role " +
			"changes (role is not part of the project membership answer).",
	},
	"modules/botfather/api_user.go": {
		writes: 1,
		why: "INSERT only now. The bare `UPDATE space_member SET status=0 WHERE uid=?` on " +
			"bot deletion that this entry used to record as a KNOWN GAP was removed by PR " +
			"#855, which routes all three deletion entries (botfather command, botfather " +
			"REST, manager REST) through modules/space.CloseAllSpaceSeats instead. That " +
			"path now runs the removal tx steps, so the epoch moves — see the " +
			"member_removal_all_spaces.go entry. The remaining write grants a bot a Space " +
			"seat; admission needs no bump, see mint_obo.go.",
	},
	"modules/space/member_removal_all_spaces.go": {
		writes: 1,
		why: "SANCTIONED. PR #855's \"close this uid's seats in EVERY Space\" path, which " +
			"replaced botfather's bare cross-Space UPDATE. closeSeatAllSpacesOne runs " +
			"runMemberRemovalTxSteps in the same transaction beside the outbox enqueue. It " +
			"enqueued the outbox but NOT the steps when it landed, so member_epoch did not " +
			"move and a peer's cached grant outlived a bot deletion for as long as the " +
			"async cleanup took — unbounded once that job is abandoned. Same registry and " +
			"same statement as the other two closing paths, because consistency here is " +
			"correctness rather than tidiness: any path that skips it makes epoch " +
			"agreement insufficient for that class of uid. TestBotSeatCloseMovesTheEpoch " +
			"pins it. Down from 3 to 1 when the census started stripping comments: the other " +
			"two matches were the doc comment's quoted SQL and a commented example, i.e. " +
			"prose.",
	},
	"modules/botfather/command.go": {
		writes: 0,
		why: "NO WRITER. #855 replaced the bare `UPDATE space_member SET status=0 WHERE " +
			"uid=?` here with modules/space.CloseAllSpaceSeats, which runs the removal tx " +
			"steps (see the member_removal_all_spaces.go entry), so the bot axis is CLOSED " +
			"rather than a known gap. This entry previously still said \"KNOWN GAP\" while the " +
			"api_user.go entry above correctly recorded the removal — two contradictory claims " +
			"about one fact in one file — and the count keeping the guard green came from the " +
			"comment that QUOTES the deleted statement. The census strips comments now; this " +
			"stays at 0 so a real writer reappearing here fails instead of matching a stale " +
			"baseline.",
	},
	"modules/botfather/db.go": {
		writes: 3,
		why: "ONE INSERT (admission) plus the two writers inside deleteCreatedBotArtifacts — " +
			"`DELETE FROM space_member WHERE uid=?` (:231) and its fail-closed " +
			"`UPDATE ... SET status=0` fallback (:270). Both are the COMPENSATION path for a bot " +
			"creation that FAILED, i.e. a bot that never got far enough to hold a project seat, " +
			"so there is no surviving seat for them to strand. This entry used to call them " +
			"KNOWN GAPs on the bot axis, which overstated them: the real bot-deletion axis is " +
			"closed (see the member_removal_all_spaces.go entry). Left counted rather than " +
			"exempted because the reasoning is about REACHABILITY — a future change that made " +
			"this path run against an established bot would need a fresh look.",
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
		// Comments stripped before matching. Without it the census counted PROSE: a comment
		// quoting the bare `UPDATE space_member SET status=0` that #855 removed was the
		// ENTIRE "write" attributed to modules/botfather/command.go, and the baseline entry
		// then described a statement that no longer exists. The mechanical risk is the
		// reason rather than the tidiness: in a file whose count includes prose matches,
		// deleting a comment while adding one real writer preserves the count and the guard
		// stays green. The epoch guard next door already strips.
		//
		// NOT the package's existing stripSourceComments: its block-comment pass treats an
		// unpaired `/*` as opening a comment that runs to EOF, and `// … /v1/bot/*
		// authentication` in modules/botfather/mint_obo.go is exactly that — it swallowed the
		// rest of the file including a real `INSERT INTO space_member`, taking that writer's
		// count from 1 to 0. A stripper that deletes real code from a census makes the census
		// under-count, which is the direction that hides a writer. Line comments only here.
		n := len(spaceMemberWrite.FindAllString(stripLineComments(string(data)), -1))
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

	// The other direction: a baseline entry that no longer matches means the guard has been
	// silently emptied for that file — the exact way a source guard dies.
	//
	// EXCEPT for an entry declaring `writes: 0`, which is an assertion in its own right:
	// "this file MUST NOT write space_member". Those are files whose writer was removed by
	// an upstream change, kept in the baseline so a reappearance fails the count check
	// above instead of matching nothing and being silently accepted. Treating them as
	// stale would force deleting exactly the entries that make a regression visible.
	for file, entry := range spaceMemberWriterBaseline {
		if _, ok := found[file]; !ok && entry.writes != 0 {
			t.Errorf("spaceMemberWriterBaseline lists %s with writes: %d but the sweep found "+
				"none; either the writer moved (re-point the baseline) or the pattern stopped "+
				"matching it (fix the pattern). If the writer was legitimately REMOVED, set "+
				"writes: 0 with the reason rather than deleting the entry — the entry is what "+
				"makes a reappearance fail.", file, entry.writes)
		}
	}
}

// stripLineComments removes // comments and nothing else, so the census matches CODE
// rather than prose about code.
//
// Line comments only, deliberately. The package's stripSourceComments also handles block
// comments, and its pass treats an unpaired `/*` as running to EOF — which a Go source
// file legitimately contains inside a line comment (`/v1/bot/*`), and which then deletes
// every real statement after it. For a CENSUS that is the dangerous direction: a
// swallowed writer reads as no writer. Over-stripping is safe for a guard asserting
// presence; it is not safe for one counting occurrences.
//
// A string literal containing "//" would be truncated. None of the swept SQL has one, and
// a real lexer is not worth it for a guard whose remaining failure mode is over-counting
// (which shows up as a baseline mismatch, not as silence).
func stripLineComments(src string) string {
	var out strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
