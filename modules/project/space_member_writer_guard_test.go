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
// Deliberately over-broad, because the cost of a false positive is one line in the
// baseline with a reason, and the cost of a false negative is an authorization fact
// that outlives its revocation.
//
// It catches the disband paths, which need no bump — an inactive parent Space folds
// every one of its projects into the absent answer at READ time
// (pkg/project.ProjectEpochsInSpace), so the answer and the invalidation channel move
// together with no write.
//
// It also catches `SET status=1`, and that one is NOT an exemption. This header used
// to list re-admission as an example of a match needing no bump; that was exactly
// backwards. Reopening a seat makes a SURVIVING project seat reachable again through
// the Space conjunction with no project-side write, so nothing else moves the epoch and
// a consumer's cached DENIAL keeps agreeing with it. It is the reason all four
// reactivation doors run a tx step, and rounds 8 through 12 each found another instance
// of that same class. A written exemption for it, in the header of the load-bearing
// instrument for the class, is how the next round finds door five.
//
// Recorded rather than quietly corrected: the commit that fixed the two baseline
// entries below CLAIMED this correction in its message and never made the edit, and
// three reviewers over two rounds read the claim before one of them checked the bytes.
// A commit message is not a change.
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
	"modules/space/seat_transition.go": {
		writes: 2,
		why: "THE FUNNEL, and the reason the other two modules/space entries shrank. Both " +
			"writes are the per-member status transition — openSeatTx (0 -> 1) and " +
			"closeSeatTx (1 -> 0) — and each one resolves the canonical uid out of " +
			"space_member and runs the registered tx steps before returning. Seven paths " +
			"used to hand-write that sequence; six of the twelve P1s on this branch were " +
			"one of those seven missing a step of it. A NEW ENTRY APPEARING IN THIS " +
			"BASELINE IS THE SIGNAL TO CHECK: a per-member status write outside this file " +
			"is a door that bypassed the funnel. TestSeatTransitionStepsHaveOneCaller pins " +
			"the other half — that nothing else calls runSeatTransitionTxSteps directly.",
	},
	"modules/space/db_manager.go": {
		writes: 4,
		why: "SANCTIONED, and NONE of the four is a per-member status transition — those " +
			"all moved to seat_transition.go. Enumerated from the SQL: (1) the admin " +
			"DISBAND at :231, `SET status=0 WHERE space_id=? AND status=1`, whole-Space and " +
			"needing no bump because an inactive Space folds all of its projects into the " +
			"absent answer at read time (pkg/project.ProjectEpochsInSpace); (2) " +
			"upsertMembersOnce's `INSERT ... ON DUPLICATE KEY UPDATE status=1` at :518 — " +
			"reachable ONLY after openSeatTx has already returned false for this uid, so " +
			"the row is either absent (a create, with no surviving project seat to reopen) " +
			"or already active (a no-op write, which must not churn the epoch). That " +
			"argument depends on the openSeatTx call above it and would stop holding if " +
			"the ODKU were moved or the call removed; (3) and (4) the two ownership-" +
			"transfer writes at :653 and :659, both `SET role=...`, and role is not part " +
			"of the project membership answer.",
	},
	"modules/space/db.go": {
		writes: 8,
		why: "SANCTIONED, and NONE of the eight is a per-member status transition — the " +
			"three that were (reactivateMember, atomicReactivateMemberIfNotFull, " +
			"approveJoinApplyAtomic's reactivation branch) now go through openSeatTx. " +
			"Enumerated from the SQL: the DISBAND at :236 (`SET status=0 WHERE space_id=? " +
			"AND status=1`, whole-Space, covered by the read-time fold); updateMemberRole " +
			"at :470 (`SET role=`, not part of the answer); and five INSERTs of seats that " +
			"did not exist — :173, insertMember/:310, insertMemberNoTx/:315, " +
			"insertMemberIgnore/:787, :835, and approveJoinApplyAtomic's create branch " +
			"at :1152. An INSERT needs no bump because there is no surviving project seat " +
			"for it to make reachable again; a new member reaching a project goes through " +
			"the project-side write, which bumps on its own. NOTE the count is 8 while " +
			"that list names 8 statements including two INSERT helpers — enumerate from " +
			"the SQL when this changes, not from the function names: two of the four " +
			"reactivation writers this branch missed in round 9 had no form of " +
			"\"reactivate\" in their signature.",
	},
	"modules/space/member_removal_all_spaces.go": {
		writes: 0,
		why: "NO WRITER, and this is an assertion rather than a stale entry. #855's " +
			"\"close this uid's seats in EVERY Space\" path (the BotFather bot-deletion " +
			"route) used to write the seat itself; it now calls closeSeatTx. When it " +
			"landed it enqueued the outbox but NOT the tx steps, so a bot deletion moved " +
			"no epoch and a peer's cached grant outlived it — unbounded once the async job " +
			"is abandoned. Routing it through the funnel is what makes that omission " +
			"unwritable rather than merely fixed. A write reappearing in this file fails " +
			"the count check above; TestBotSeatCloseMovesTheEpoch pins the behaviour.",
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
// QUOTE-AWARE, because the alternative was a caveat rather than a property. The previous
// version cut at the first "//" on a line and documented that a string literal containing
// one would be truncated, with the reason "none of the swept SQL has one". That is the
// same shape of claim this branch has been burned by three times, and the failure
// direction is the bad one: a truncated literal drops the SQL after it, so a real writer
// reads as no writer. It costs about ten lines to make it true instead of asserted.
//
// Block comments remain unhandled ON PURPOSE — see above. The two are not symmetric: an
// unpaired "/*" inside a line comment is a real shape in this tree, and handling block
// comments naively is what deleted a live INSERT.
//
// TestStripLineCommentsKeepsStringLiterals pins both halves.
func stripLineComments(src string) string {
	var out strings.Builder
	// quote lives OUTSIDE the line loop: a raw string literal legitimately spans lines,
	// and resetting at every line boundary loses that state — a `//` inside a multi-line
	// backtick literal would then be treated as a comment and everything after it on that
	// line dropped. Under-count is the direction that hides a writer, so this is the
	// direction worth being right about. Interpreted (") and rune (') literals cannot
	// span lines in Go, so those are reset per line below: carrying THEM across would let
	// one unbalanced quote swallow the rest of the file, which is the block-comment
	// failure this stripper exists to avoid.
	var quote byte // 0 = outside a literal, else the opening delimiter
	for _, line := range strings.Split(src, "\n") {
		if quote == '"' || quote == '\'' {
			quote = 0
		}
		escape := false
		cut := -1
		for i := 0; i < len(line); i++ {
			c := line[i]
			if quote != 0 {
				switch {
				case escape:
					escape = false
				case c == '\\' && quote != '`': // no escapes inside a raw string
					escape = true
				case c == quote:
					quote = 0
				}
				continue
			}
			if c == '"' || c == '`' || c == '\'' {
				quote = c
				continue
			}
			if c == '/' && i+1 < len(line) && line[i+1] == '/' {
				cut = i
				break
			}
		}
		if cut >= 0 {
			line = line[:cut]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// TestStripLineCommentsKeepsStringLiterals pins the census stripper's two halves: it
// removes prose about code, and it does NOT remove code that merely looks like prose.
//
// The second half is the one worth a test. A swept file whose SQL contains "//" — a URL
// in a comment on the same line as a statement, say — used to lose everything after it,
// and a census that loses a writer reports zero writers, which reads as green.
func TestStripLineCommentsKeepsStringLiterals(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "a real line comment is removed",
			src:  `x := 1 // UPDATE space_member SET status=0`,
			want: "x := 1 ",
		},
		{
			name: "a double slash INSIDE a literal is kept, and so is the code after it",
			src:  "q := \"https://x\" + \"UPDATE space_member SET status=0\"",
			want: "q := \"https://x\" + \"UPDATE space_member SET status=0\"",
		},
		{
			name: "raw string literals too",
			src:  "q := `https://x UPDATE space_member SET status=0`",
			want: "q := `https://x UPDATE space_member SET status=0`",
		},
		{
			name: "an escaped quote does not end the literal early",
			src:  `q := "he said \"//\" then UPDATE space_member SET status=0"`,
			want: `q := "he said \"//\" then UPDATE space_member SET status=0"`,
		},
		{
			name: "a comment AFTER a literal is still removed",
			src:  `q := "UPDATE space_member SET status=0" // and prose here`,
			want: `q := "UPDATE space_member SET status=0" `,
		},
		{
			// The line-spanning case. Quote state has to survive the newline or the
			// second line reads as code, its `//` reads as a comment, and the writer
			// after it disappears from the census — an under-count, which is the
			// direction that reports a real writer as no writer.
			name: "a raw string literal spanning lines keeps its state",
			src:  "q := `see https://x\nUPDATE space_member SET status=0`",
			want: "q := `see https://x\nUPDATE space_member SET status=0`",
		},
		{
			// And the opposite direction: an interpreted literal cannot span lines in
			// Go, so its state must NOT carry across one, or a single stray quote
			// swallows the rest of the file — the block-comment failure mode again.
			name: "an interpreted literal does not carry its state across lines",
			src:  "q := \"unterminated\nx := 1 // prose",
			want: "q := \"unterminated\nx := 1 ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.TrimSuffix(stripLineComments(tc.src), "\n")
			if got != tc.want {
				t.Errorf("stripLineComments(%q)\n got  %q\n want %q", tc.src, got, tc.want)
			}
		})
	}
}
