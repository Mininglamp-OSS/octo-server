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

	// Both spellings of the legacy allocation: the constant and the literal key
	// prefix it holds. A caller that inlined the string still counts.
	legacyAlloc := regexp.MustCompile(`GenSeq\([^)]*(RobotEventSeqKey|"robotEventSeq:)`)

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
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(content), "\n") {
			if legacyAlloc.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				violations = append(violations, rel+":"+itoaGuard(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("bot event ids allocated through octo-lib GenSeq:\n  %s\n\n"+
			"GenSeq is a per-process HiLo block allocator, so two replicas can issue the same "+
			"id and can issue ids out of time order. robotEvent:{robotID} is read with an "+
			"exclusive cursor and acked by score, both of which require strict monotonicity. "+
			"Use botevent.NextEventID instead (pkg/botevent/seq.go), and do not add a GenSeq "+
			"fallback — two live id sources on one queue is the defect this replaced (#697).",
			strings.Join(violations, "\n  "))
	}
}

// TestGenSeqGuardWouldCatchAReintroduction proves the guard above can fail. A
// pattern that matches nothing would pass forever while protecting nothing —
// the same vacuity the sibling guard's minKnownQueueWriters floor defends.
func TestGenSeqGuardWouldCatchAReintroduction(t *testing.T) {
	legacyAlloc := regexp.MustCompile(`GenSeq\([^)]*(RobotEventSeqKey|"robotEventSeq:)`)

	shouldMatch := []string{
		`seq, err := ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))`,
		`seq, err := rb.ctx.GenSeq(fmt.Sprintf("%s%s", common.RobotEventSeqKey, robotID))`,
		`v, _ := c.GenSeq("robotEventSeq:" + id)`,
	}
	for _, s := range shouldMatch {
		if !legacyAlloc.MatchString(s) {
			t.Fatalf("guard would not catch a reintroduction: %s", s)
		}
	}

	// Other GenSeq users are none of this guard's business — #700 tracks the
	// reminders and conversation_extra cursors, which are a different fix.
	shouldNotMatch := []string{
		`version, err := m.ctx.GenSeq(common.RemindersKey)`,
		`version, err := co.ctx.GenSeq(common.SyncConversationExtraKey)`,
		`seq, err := botevent.NextEventID(ctx, robotID)`,
	}
	for _, s := range shouldNotMatch {
		if legacyAlloc.MatchString(s) {
			t.Fatalf("guard is too broad, it matched: %s", s)
		}
	}
}
