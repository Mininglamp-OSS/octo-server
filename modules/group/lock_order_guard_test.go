package group

// The lock-order invariant, pinned instead of asserted.
//
// PR #846's review found that pkg/project.AssertMembersInProjectTx's doc claimed
// the module's table order held on the admission path — "octo_project_member is
// deliberately LAST" — and that the funnel does the opposite: it takes the
// project seat SHARED first and the group_member upsert second. The claim being
// false is what mattered: a three-way cycle (admission holds S(pm) wants X(gm);
// a seat write wants X(pm); the handover holds X(gm) wants S(pm)) is reachable
// and was reproduced on MySQL 8.0.46, and nobody would have looked for it in a
// deadlock log while the comment said it could not happen.
//
// The doc is corrected. This is the part a comment cannot do: the invariant that
// actually holds, enforced.
//
//	No path takes an EXCLUSIVE lock on octo_project_member while holding a
//	group_member lock.
//
// modules/project holds the other half by construction — it takes X(pm) holding
// no group locks at all, because it touches no group table. The half that can be
// broken by an edit is this one: modules/group's handover locks group_member
// rows and then reaches for the project seat, and it is one word away from
// `FOR UPDATE`, which would turn the three-way cycle into a plain two-way ABBA
// between the two hottest paths in the module.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// lockOrderScanDirs are the two places that lock octo_project_member while a
// group_member lock can be held. modules/project is deliberately absent: it
// takes exclusive seat locks and is allowed to, because it holds no group locks.
var lockOrderScanDirs = []string{"modules/group/", "pkg/project/"}

// sqlAliasStopWords keeps the alias regex from reading a SQL keyword as an alias
// when the table is used without one (`FROM ` + "`octo_project_member`" + ` WHERE …`).
var sqlAliasStopWords = map[string]bool{
	"where": true, "set": true, "on": true, "as": true, "order": true,
	"group": true, "limit": true, "for": true, "and": true, "inner": true,
	"left": true, "join": true, "values": true, "select": true, "using": true,
}

var projectMemberAliasRe = regexp.MustCompile("`?octo_project_member`?\\s+([A-Za-z_][A-Za-z0-9_]*)")

// flattenSQL turns Go string concatenation into the statement text it builds, so
// a clause split across source lines reads as one.
func flattenSQL(chunk string) string {
	r := strings.NewReplacer("\"", " ", "+", " ", "\n", " ", "\t", " ")
	return strings.Join(strings.Fields(r.Replace(chunk)), " ")
}

// TestNoExclusiveProjectMemberLockUnderAGroupMemberLock walks every function
// that touches octo_project_member in the two scanned directories and refuses an
// exclusive lock on it.
//
// Two shapes are refused, because both take the exclusive lock:
//
//   - `FOR UPDATE OF <the octo_project_member alias>`, the explicit one;
//   - a bare `FOR UPDATE` with no `OF` clause, which locks EVERY table the
//     statement reads — including the project seat, silently. This is the one
//     that would slip through review, because the statement need not mention the
//     seat anywhere near the locking clause.
func TestNoExclusiveProjectMemberLockUnderAGroupMemberLock(t *testing.T) {
	root := repoRootForLockGuard(t)

	checked := 0
	for _, dir := range lockOrderScanDirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)

			// Comments are stripped first — this very file's package doc, and the
			// corrected doc on AssertMembersInProjectTx, both discuss `FOR UPDATE`
			// on the seat in prose. A guard that fired on its own explanation
			// would be deleted rather than obeyed.
			var stripped strings.Builder
			inBlock := false
			for _, raw := range strings.Split(string(body), "\n") {
				var line string
				line, inBlock = stripGoComments(raw, inBlock)
				stripped.WriteString(line)
				stripped.WriteString("\n")
			}

			for _, chunk := range strings.Split(stripped.String(), "\nfunc ") {
				if !strings.Contains(chunk, "octo_project_member") {
					continue
				}
				checked++
				sql := flattenSQL(chunk)

				for _, m := range projectMemberAliasRe.FindAllStringSubmatch(sql, -1) {
					alias := m[1]
					if sqlAliasStopWords[strings.ToLower(alias)] {
						continue
					}
					if strings.Contains(sql, "FOR UPDATE OF "+alias) {
						t.Errorf("%s: takes FOR UPDATE on octo_project_member (alias %q). "+
							"The seat lock must stay SHARED wherever a group_member lock can be "+
							"held — see pkg/project.AssertMembersInProjectTx's lock-order note: %s",
							rel, alias, firstNChars(sql, 300))
					}
				}

				// A bare FOR UPDATE — one with no OF clause — locks every table in
				// the statement, this one included.
				for _, idx := range indexesOf(sql, "FOR UPDATE") {
					rest := strings.TrimSpace(sql[idx+len("FOR UPDATE"):])
					if strings.HasPrefix(rest, "OF ") {
						continue
					}
					t.Errorf("%s: a statement reading octo_project_member takes a bare "+
						"FOR UPDATE, which locks every table it reads, the project seat "+
						"included. Name the tables with `FOR UPDATE OF <alias>` and keep the "+
						"seat on FOR SHARE: %s", rel, firstNChars(sql, 300))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if checked < 2 {
		t.Fatalf("expected at least the funnel's gate and the handover's successor pick "+
			"to touch octo_project_member, found %d — the scan stopped matching and this "+
			"guard would pass vacuously", checked)
	}
}

func indexesOf(s, sub string) []int {
	var out []int
	for i := 0; ; {
		j := strings.Index(s[i:], sub)
		if j < 0 {
			return out
		}
		out = append(out, i+j)
		i += j + len(sub)
	}
}

func firstNChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func repoRootForLockGuard(t *testing.T) string {
	t.Helper()
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(pwd, "..", ".."))
	if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr != nil {
		t.Fatalf("expected go.mod at module root %q: %v", root, statErr)
	}
	return root
}
