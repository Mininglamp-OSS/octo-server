package msgextraseq

// Source guard for #627: after the writer cutover (PR-2a/2b/2c), the legacy
// GenSeq(MessageExtraSeqKey…) call site must live ONLY in this package's
// store.go (reserveLegacy). Any *.go file outside this directory that still
// references common.MessageExtraSeqKey is a regression: it means a business
// writer skipped the transactional allocator (or a new caller was added
// without going through ReserveTx), which would re-open the multi-replica
// cursor-skip window this task closes.
//
// The guard walks the module root (three parents up: <root>/internal/msgextraseq/
// → <root>) and greps non-test .go files. Test files are exempted so unit
// tests can still exercise legacy semantics through internal helpers if
// needed. Comments and strings are NOT stripped: a mention in a comment is
// still worth reviewing, since the accepted place to document this key is
// here.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowedDir is the one directory whose non-test *.go files may reference
// common.MessageExtraSeqKey: the allocator package itself (reserveLegacy in
// store.go is the single allowlisted GenSeq call site; other files in this
// package may read the legacy seq by the same key). Any reference OUTSIDE this
// directory means a business writer skipped the transactional allocator (or a
// new caller was added without going through ReserveTx) — the regression this
// guard exists to catch.
//
// The guard is a proxy, not a proof, of the invariant that every
// message_extra.version write goes through ReserveTx (and so takes the state row
// FOR SHARE, which the operator activation drain barrier relies on): it catches a
// bypassed GenSeq(MessageExtraSeqKey), the way that invariant has been broken
// historically, but not a writer that derives a version some other way (e.g.
// MAX(version)+1 or an external batch). No such writer exists today; a new one
// would need a matching guard here.
const allowedDir = "internal/msgextraseq/"

func TestNoLegacyMessageExtraGenSeqOutsideAllocator(t *testing.T) {
	// Locate module root: this test file lives at <root>/internal/msgextraseq/.
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(pwd, "..", ".."))

	// Sanity check: the module root must contain go.mod. Fail loudly rather than
	// silently walking the wrong tree.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected go.mod at module root %q: %v", root, err)
	}

	const needle = "MessageExtraSeqKey"

	var offenders []string
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			// Skip vendor / VCS / build caches — they never contain this repo's writers.
			name := d.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" || name == ".octospec" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		// Use POSIX-style paths in the allow list so it stays stable on Windows CI.
		relPosix := filepath.ToSlash(rel)
		if strings.HasPrefix(relPosix, allowedDir) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), needle) {
			offenders = append(offenders, relPosix)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk module root %q: %v", root, walkErr)
	}

	if len(offenders) > 0 {
		t.Fatalf("#627 source guard: %d non-test file(s) reference common.%s outside internal/msgextraseq/ — every message_extra version allocation must go through msgextraseq.Store.ReserveTx:\n  %s",
			len(offenders), needle, strings.Join(offenders, "\n  "))
	}
}
