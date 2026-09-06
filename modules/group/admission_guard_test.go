package group

// Source guard for invariant I2 (D8).
//
// I2 is enforced in ONE place — admitOrRestoreMembersTx / assertAdmissibleTx in
// admission.go — and there is no read-path filter behind it. A new code path
// that writes group_member directly does not produce a subtly wrong result; it
// produces a member who sees a project group they are not in, receives its
// messages and can post in it. So the funnel needs something that FAILS when a
// path bypasses it, because the alternative — a comment asking future authors to
// use the funnel — is exactly what this module already had: 「与
// Service.AddGroupMembers 保持一致」 appears five times, and the invariant still
// drifted across eleven implementations.
//
// # Why a tree walk and not a fixed file list
//
// TestGroupNoLegacyResponseError scans a hard-coded list of files. That shape
// cannot see a NEW file, which is the failure mode that matters here: nobody
// adds a twelfth admission path by editing db.go, they add it by writing
// modules/<something>/whatever.go. This guard walks the whole module instead, so
// a new file is in scope the moment it exists.
//
// The model is internal/msgextraseq/source_guard_test.go — walk from the module
// root, skip vendor/.git/node_modules/.octospec and _test.go, allowlist by path,
// collect ALL offenders and fail with the complete list. Reporting only the
// first offender turns one CI round into N.
//
// # What it catches, and what it does not
//
// It catches a direct write to group_member: the dbr builders and the raw-SQL
// equivalents. It does NOT catch a writer that reaches the table some other way
// — a stored procedure, a generated query, an ORM introduced later. No such
// writer exists today; one added later needs a matching needle here.
//
// Comments and strings are not stripped. A comment containing "UPDATE
// group_member" is worth a review too: it usually means someone is describing a
// write they are about to add.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// groupMemberWriteNeedles are the ways this repository writes group_member.
//
// The dbr builders take BARE table names (Update/InsertInto/DeleteFrom), while
// From/Select need backticks — so the builder needles are unquoted and the raw
// SQL needles are matched case-insensitively with and without backticks.
var groupMemberWriteNeedles = []string{
	`InsertInto("group_member"`,
	`Update("group_member"`,
	`DeleteFrom("group_member"`,
	"insert into group_member",
	"insert into `group_member`",
	"update group_member",
	"update `group_member`",
	"delete from group_member",
	"delete from `group_member`",
}

// groupMemberWriteAllowlist maps a repo-relative path to the needles that file
// is allowed to contain. A file absent from the map may contain none of them.
//
// Deliberately per-file-per-needle rather than per-directory: allowlisting all
// of modules/group would let the next admission path be written into api.go and
// pass, which is the exact regression this guard exists to prevent.
var groupMemberWriteAllowlist = map[string][]string{
	// The DAO primitives. These are the only functions that touch the table's
	// columns directly, and they are unexported so nothing outside this package
	// can reach them.
	"modules/group/db.go": groupMemberWriteNeedles,

	// The single admission entry. Its upsert is raw SQL because ON DUPLICATE KEY
	// UPDATE with conditional assignments is not expressible through dbr's
	// builder, and the conditional assignments are what make insert-vs-restore
	// one race-free statement.
	"modules/group/admission.go": {"insert into group_member"},

	// The compensating rollback after IM channel creation fails: the transaction
	// has already committed, so the rows are deleted through a session rather
	// than a tx. It removes a whole group's rows on a failed CREATE, so it can
	// never admit anyone, and routing it through the removal funnel would emit
	// removal system messages for a group that never existed.
	"modules/group/service.go": {`DeleteFrom("group_member"`},
}

// projectIDWriteNeedles catch an UPDATE that changes a group's project
// attribution. I3 makes attribution immutable in v1: there is no re-parenting
// endpoint, and the only writes permitted are the create path (which INSERTs the
// group with its project_id) and the detach step (which reverts a group to
// Space-direct when its project is disbanded, or when no successor is left to
// own it).
//
// Without this, "re-parent a group into another project" is a one-line change
// that silently moves a group full of members into a project none of them are
// in — I2 violated for every existing member at once, with no admission path
// involved and therefore nothing else to catch it.
var projectIDWriteNeedles = []string{
	`Set("project_id"`,
	"set project_id",
	"set `project_id`",
}

var projectIDWriteAllowlist = map[string][]string{
	// The detach step reverts project groups to Space-direct.
	"modules/group/project_cascade.go": projectIDWriteNeedles,
	// The DAO primitive the detach step calls.
	"modules/group/db.go": projectIDWriteNeedles,
}

func TestNoGroupMemberWritesOutsideTheAdmissionFunnel(t *testing.T) {
	assertNoWritesOutsideAllowlist(t,
		"group_member",
		groupMemberWriteNeedles,
		groupMemberWriteAllowlist,
		"every group_member write must go through modules/group/db.go's primitives, "+
			"which are reachable only from admitOrRestoreMembersTx (admission) or "+
			"RemoveGroupMembers (removal). A direct write bypasses invariant I2, and "+
			"there is no read-path filter behind I2 to catch it.")
}

func TestNoProjectIDRewritesOutsideTheDetachStep(t *testing.T) {
	assertNoWritesOutsideAllowlist(t,
		"group.project_id",
		projectIDWriteNeedles,
		projectIDWriteAllowlist,
		"a group's project attribution is immutable in v1 (I3). The create path sets "+
			"it at INSERT; the disband/no-successor detach step reverts it to the empty "+
			"sentinel. Re-parenting a group would move every existing member into a "+
			"project they are not in, violating I2 wholesale with no admission path "+
			"involved.")
}

// assertNoWritesOutsideAllowlist walks the module root and reports every
// non-test .go file that contains a forbidden needle it is not allowlisted for.
func assertNoWritesOutsideAllowlist(
	t *testing.T,
	subject string,
	needles []string,
	allowlist map[string][]string,
	why string,
) {
	t.Helper()

	// This test file lives at <root>/modules/group/.
	pwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(pwd, "..", ".."))
	if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr != nil {
		// Fail loudly rather than silently walking the wrong tree and passing.
		t.Fatalf("expected go.mod at module root %q: %v", root, statErr)
	}

	type offence struct {
		file   string
		line   int
		needle string
		text   string
	}
	var offenders []offence

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "node_modules", ".octospec", ".context":
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
		rel = filepath.ToSlash(rel)

		allowed := allowlist[rel]
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(body), "\n") {
			lower := strings.ToLower(line)
			for _, needle := range needles {
				hit := strings.Contains(line, needle)
				if !hit && needle == strings.ToLower(needle) {
					hit = strings.Contains(lower, needle)
				}
				if !hit {
					continue
				}
				if containsString(allowed, needle) {
					continue
				}
				offenders = append(offenders, offence{
					file:   rel,
					line:   i + 1,
					needle: needle,
					text:   strings.TrimSpace(line),
				})
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}

	if len(offenders) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString(subject)
	b.WriteString(" is written outside the allowlist:\n\n")
	for _, o := range offenders {
		b.WriteString("  ")
		b.WriteString(o.file)
		b.WriteString(":")
		b.WriteString(itoa(o.line))
		b.WriteString("  [")
		b.WriteString(o.needle)
		b.WriteString("]\n      ")
		b.WriteString(o.text)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(why)
	t.Fatal(b.String())
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
