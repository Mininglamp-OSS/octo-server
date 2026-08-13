package botevent

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoGenSeqForBotEventIDs pins the other half of the #697 fix.
//
// Replacing the allocator only helps if it replaces it *everywhere*. GenSeq hands
// out HiLo blocks from process-local state, so one remaining caller means one
// remaining live id source on the same queue — which is not "most of the fix",
// it is the original defect: two sources issuing into one exclusive-cursor
// sorted set is exactly what produced the 2624 colliding scores measured in
// production.
//
// The sibling guard (TestEveryBotEventQueueWriterRingsTheDoorbell) exists because
// a docstring claimed there was a single chokepoint when there were five writers.
// This one exists for the same reason in the other direction: nothing structural
// stops a sixth writer from reaching for GenSeq, and a code comment would not
// notice.
func TestNoGenSeqForBotEventIDs(t *testing.T) {
	root := repoRootForGuard(t)

	// Assert on the *key*, not on `GenSeq(...key)`.
	//
	// An earlier revision matched `GenSeq\([^)]*(RobotEventSeqKey|"robotEventSeq:)` one
	// line at a time, which a routine refactor walks straight past (review P2-5):
	//
	//	key := common.RobotEventSeqKey + robotID
	//	seq, err := ctx.GenSeq(key)
	//
	// or the same call wrapped across two lines. Nothing outside the allowlisted file
	// has any business naming this key at all — reading the row, formatting it, logging
	// it — because every use of it is either an allocation from the retired source or a
	// reimplementation of the seed's ceiling read. So the key itself is the thing to
	// forbid, and every spelling of the call becomes unreachable rather than
	// individually pattern-matched.
	legacyKey := regexp.MustCompile(`RobotEventSeqKey|"robotEventSeq:|robotEventSeq:%s`)

	// The delegation itself still has to exist; see the assertion below.
	legacyAlloc := regexp.MustCompile(`GenSeq\([^)]*(RobotEventSeqKey|"robotEventSeq:)`)

	// pkg/botevent/seq.go is the single allowlisted GenSeq call site, the same
	// shape internal/msgextraseq uses: the allocator delegates to GenSeq while it
	// is not activated, so a deployment behaves exactly like the old binary until
	// an operator flips the mode. Every OTHER caller is a second live id source.
	const allowlisted = "pkg/botevent/seq.go"

	// cutover_botevent.go is the `app cutover botevent` operator command. It
	// names the key only to sweep the legacy `seq` rows table-wide for cutover
	// floor evidence — the read its standalone predecessor (tools/botevent-seq)
	// did under the tools/ exemption below.
	//
	// The exemption is deliberately narrow: it waives the key-NAME check only,
	// and the allocation check below still applies to the file. Waiving the
	// whole file would let a future edit — "bump the legacy boundary before
	// flipping", say, during a rollback attempt — add a real GenSeq call to the
	// one command an operator runs by hand at the cutover, with the guard
	// staying green while a second live id source appears on the queue.
	allowlistedReaders := map[string]bool{"cutover_botevent.go": true}

	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".octospec", "tools":
				// tools/ is exempt: tools/genseq-repro exists precisely to call the
				// legacy allocator and demonstrate that it collides.
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		relPosix := filepath.ToSlash(rel)
		if relPosix == allowlisted {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// A reader may name the key; nobody outside the allowlisted file may
		// allocate from it.
		pattern := legacyKey
		if allowlistedReaders[relPosix] {
			pattern = legacyAlloc
		}
		for i, line := range strings.Split(string(content), "\n") {
			if pattern.MatchString(line) {
				violations = append(violations, rel+":"+itoaGuard(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// The allowlisted file must actually still contain the delegation. If it stops
	// (say the legacy branch is deleted as "dead code"), pre-activation deployments
	// silently lose the behaviour that makes them safe, and this guard would go
	// green while protecting nothing.
	allowed, readErr := os.ReadFile(filepath.Join(root, allowlisted))
	if readErr != nil {
		t.Fatalf("read allowlisted file: %v", readErr)
	}
	if !legacyAlloc.Match(allowed) {
		t.Fatalf("%s no longer delegates to GenSeq. The pre-activation path is what makes "+
			"deploying this change a no-op until an operator activates it; without it a "+
			"deploy switches allocators while legacy replicas are still running, which is "+
			"the mixed-source loss described in #697.", allowlisted)
	}

	if len(violations) > 0 {
		t.Fatalf("the legacy bot-event seq key is named outside %s:\n  %s\n\n"+
			"GenSeq is a per-process HiLo block allocator, so two replicas can issue the same "+
			"id and can issue ids out of time order. robotEvent:{robotID} is read with an "+
			"exclusive cursor and acked by score, both of which require strict monotonicity. "+
			"Use botevent.NextEventID instead (pkg/botevent/seq.go), and do not add a GenSeq "+
			"fallback — two live id sources on one queue is the defect this replaced (#697). "+
			"If you only need to *read* the legacy ceiling, that already exists as "+
			"legacyCeiling in the allowlisted file.",
			allowlisted, strings.Join(violations, "\n  "))
	}
}

// TestGenSeqGuardWouldCatchAReintroduction proves the guard above can fail. A
// pattern that matches nothing would pass forever while protecting nothing —
// the same vacuity the sibling guard's minKnownQueueWriters floor defends.
func TestGenSeqGuardWouldCatchAReintroduction(t *testing.T) {
	legacyKey := regexp.MustCompile(`RobotEventSeqKey|"robotEventSeq:|robotEventSeq:%s`)

	shouldMatch := []string{
		`seq, err := ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))`,
		`seq, err := rb.ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))`,
		`v, _ := c.GenSeq("robotEventSeq:" + id)`,
		// The two shapes the previous line-anchored pattern walked past (P2-5).
		`	key := common.RobotEventSeqKey + robotID`,
		`		common.RobotEventSeqKey + robotID,`,
	}
	for _, s := range shouldMatch {
		if !legacyKey.MatchString(s) {
			t.Fatalf("guard would not catch a reintroduction: %s", s)
		}
	}

	// Other GenSeq users are none of this guard's business — #700 tracks the
	// reminders and conversation_extra cursors, which are a different fix.
	shouldNotMatch := []string{
		`version, err := m.ctx.GenSeq(common.RemindersKey)`,
		`version, err := co.ctx.GenSeq(common.SyncConversationExtraKey)`,
		`seq, err := botevent.NextEventID(ctx, robotID)`,
		`key := botevent.QueueKey(robotID)`,
	}
	for _, s := range shouldNotMatch {
		if legacyKey.MatchString(s) {
			t.Fatalf("guard is too broad, it matched: %s", s)
		}
	}
}
