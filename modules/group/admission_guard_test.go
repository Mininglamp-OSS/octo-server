package group

// Source guard for native Group membership writes.
//
// Every group_member write must use the shared transaction primitive so that
// insert/restore semantics, versions, and native membership columns cannot
// drift across callers. Project relation authorization is a separate concern.
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
// Go comments ARE stripped before matching, and that is a deliberate reversal of
// the model guard's choice. internal/msgextraseq keeps comments in scope because
// the accepted place to document its key is that one file. Here the opposite is
// true: converging a bypass means DELETING a write and explaining in a comment
// what was deleted and why, and a guard that fires on that explanation pushes
// authors to describe the removed SQL vaguely — which is worse for the next
// reader than the guard is good. The three files that document the raw
// `INSERT INTO group_member (group_no, uid)` this change removed are exactly the
// files that should quote it.
//
// String literals are still in scope: that is where a real write lives.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
	// Project-backed group creation has the same post-commit compensation
	// contract: if WuKongIM channel creation fails, remove its just-committed
	// native member rows without emitting ordinary removal events.
	"modules/group/service_project_create.go": {`DeleteFrom("group_member"`},
}

// projectIDWriteNeedles catch an UPDATE that changes a group's project
// attribution. Relation writes are now allowed only through the dedicated
// atomic Group↔Project DAO primitive; ordinary group paths still cannot
// rewrite project_id.
var projectIDWriteNeedles = []string{
	`Set("project_id"`,
	"set project_id",
	"set `project_id`",
}

var projectIDWriteAllowlist = map[string][]string{
	// The detach step reverts project groups to Space-direct.
	"modules/group/project_cascade.go": projectIDWriteNeedles,
	// The legacy detach primitive.
	"modules/group/db.go": projectIDWriteNeedles,
	// Relation bind/rebind/unbind updates both project_id and project_linked_by
	// atomically after the transaction service has revalidated both sides.
	"modules/group/db_project.go": projectIDWriteNeedles,
}

func TestNoGroupMemberWritesOutsideTheAdmissionFunnel(t *testing.T) {
	assertNoWritesOutsideAllowlist(t,
		"group_member",
		groupMemberWriteNeedles,
		groupMemberWriteAllowlist,
		"which are reachable only from admitOrRestoreMembersTx (admission) or "+
			"RemoveGroupMembers (removal). Direct writes bypass the shared native "+
			"membership semantics.")
}

func TestNoProjectIDRewritesOutsideRelationAndDetachPrimitives(t *testing.T) {
	assertNoWritesOutsideAllowlist(t,
		"group.project_id",
		projectIDWriteNeedles,
		projectIDWriteAllowlist,
		"project_id may change only through the dedicated relation primitive or the "+
			"legacy detach primitive. Every relation update must pair project_id with "+
			"project_linked_by and revalidate Project/group authorization in one transaction.")
}
func assertNoWritesOutsideAllowlist(
	t *testing.T,
	subject string,
	needles []string,
	allowlist map[string][]string,
	why string,
) {
	t.Helper()
	assertNeedlesAbsent(t, "", false, subject, needles, allowlist, why)
}

// assertNoWritesOutsideAllowlistIn restricts the walk to files UNDER prefix.
// Used where a needle is ambiguous outside that subtree — modules/thread has its
// own DAO with identically named methods on a different type, and string
// matching cannot tell the two apart.
func assertNoWritesOutsideAllowlistIn(
	t *testing.T,
	prefix string,
	subject string,
	needles []string,
	allowlist map[string][]string,
	why string,
) {
	t.Helper()
	assertNeedlesAbsent(t, prefix, false, subject, needles, allowlist, why)
}

// assertNoWritesOutsideAllowlistExcluding restricts the walk to files NOT under
// prefix — the cross-module half of the same question.
func assertNoWritesOutsideAllowlistExcluding(
	t *testing.T,
	prefix string,
	subject string,
	needles []string,
	allowlist map[string][]string,
	why string,
) {
	t.Helper()
	assertNeedlesAbsent(t, prefix, true, subject, needles, allowlist, why)
}

func assertNeedlesAbsent(
	t *testing.T,
	prefix string,
	excludePrefix bool,
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
		if prefix != "" {
			under := strings.HasPrefix(rel, prefix)
			if under == excludePrefix {
				return nil
			}
		}

		allowed := allowlist[rel]
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		inBlockComment := false
		for i, raw := range strings.Split(string(body), "\n") {
			var line string
			line, inBlockComment = stripGoComments(raw, inBlockComment)
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
					text:   strings.TrimSpace(raw),
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

// admissionPrimitiveNeedles are the DAO primitives that write an admission.
//
// Guarding the CALLERS is necessary on top of guarding the table, because the
// primitives live in db.go, which the table guard allowlists. A new module doing
//
//	group.NewDB(ctx).InsertMember(&group.MemberModel{...})
//
// writes group_member without tripping the table guard at all. That is not a
// hypothetical: IService.AddMember was exactly that shape — no transaction, no
// Space check, no version, no vercode — exported on the service interface, and
// this change deletes it.
// UpdateMember( is here because a primitive that can put a uid BACK into the
// active member set is an admission primitive, whatever its name says. It writes
// is_deleted from a caller-supplied model, so before PR #844's review round a
// read-then-write caller could resurrect a row that was soft-deleted in between
// — outside the funnel, with no gate, no metric and no test. Its WHERE now
// carries is_deleted = 0 (see DB.UpdateMember); this needle is what stops the
// next caller of it from being added without that being looked at.
var admissionPrimitiveNeedles = []string{
	"InsertMemberTx(",
	"InsertMember(",
	"recoverMemberTx(",
	"UpdateMember(",
}

// admissionPrimitiveAllowlist — inside modules/group, only these files may call
// the primitives.
//
// # Why the primitives stay EXPORTED
//
// The task brief's D3 says InsertMemberTx / InsertMember / recoverMemberTx
// "become unexported and callable only from" the admission entry. Unexporting
// them is not possible without breaking the brief's own non-regression
// acceptance, which requires the suites of group, thread, space, message,
// botfather and bot_api to pass with NO existing test file edited: 41 test files
// in exactly those packages build their fixtures through InsertMember, e.g.
// modules/message/api_message_get_test.go and
// modules/botfather/api_bot_thread_test.go.
//
// The two requirements cannot both hold literally. This guard delivers D3's
// INTENT — the primitives are callable only from the funnel — while leaving the
// test fixtures alone, because "callable only from" is a property a guard can
// assert and the compiler's export rules only approximate.
var admissionPrimitiveAllowlist = map[string][]string{
	"modules/group/db.go":        admissionPrimitiveNeedles, // the definitions
	"modules/group/admission.go": admissionPrimitiveNeedles, // the single entry

	// The remark, mute/unmute and unmute-expiry handlers. They update a member
	// who is already live and never intend to change is_deleted; the predicate
	// in DB.UpdateMember is what makes that true rather than intended. Listed
	// explicitly, and ONLY for this needle, so a fifth caller — or the same call
	// appearing in a new file — has to be argued for rather than merged.
	"modules/group/api.go": {"UpdateMember("},
}

func TestAdmissionPrimitivesAreCalledOnlyFromTheFunnel(t *testing.T) {
	assertNoWritesOutsideAllowlistIn(t, "modules/group/",
		"the group_member admission primitives",
		admissionPrimitiveNeedles,
		admissionPrimitiveAllowlist,
		"InsertMember / InsertMemberTx / recoverMemberTx write a member row without "+
			"the shared admission semantics. All production admissions go through "+
			"admitOrRestoreMembersTx, while test fixtures retain the exported "+
			"helpers for compatibility.")
}

// groupDBHolders — the non-test files outside modules/group that legitimately
// obtain a *group.DB, and why.
//
// group.NewDB is the ONLY constructor for that type (db.go:22), so this list is
// the complete set of places outside the module that could reach a membership
// primitive. Each is checked below for primitive calls, so being on this list
// buys read access, not write access.
var groupDBHolders = map[string]string{
	"modules/incomingwebhook/api.go":           "resolving a webhook's target channel",
	"modules/report/api_manager.go":            "rendering the manager report",
	"modules/base/elastic/service.go":          "group metadata for the index documents",
	"modules/message/1module.go":               "group lookups on the message path",
	"modules/message/api.go":                   "group lookups on the message path",
	"modules/message/api_conversation.go":      "group lookups when building conversations",
	"modules/message/api_sidebar.go":           "group lookups when building the sidebar",
	"modules/message/space_filter.go":          "QueryExternalGroupNosForUser, for cross-Space filtering",
	"modules/messages_search/search_global.go": "QueryExternalGroupNosForUser, for cross-Space filtering",
	"modules/qrcode/api.go":                    "group lookups behind the QR-code join screen",
	"modules/search/api.go":                    "QueryExternalGroupNosForUser, for cross-Space filtering",
}

// TestOnlyDeclaredHoldersReachIntoTheGroupDB closes the cross-module half of the
// primitive guard.
//
// TestAdmissionPrimitivesAreCalledOnlyFromTheFunnel walks modules/group only, so
// a caller elsewhere doing group.NewDB(ctx).InsertMember(m) tripped nothing —
// raised by PR #846's review as P2-4. The needle below is the constructor rather
// than the method names, because the method names collide with other modules'
// own membership tables (modules/thread has its own InsertMember on its own
// MemberModel) and a guard that cries wolf gets deleted.
func TestOnlyDeclaredHoldersReachIntoTheGroupDB(t *testing.T) {
	allowlist := map[string][]string{}
	for file := range groupDBHolders {
		allowlist[file] = []string{"group.NewDB("}
	}
	assertNoWritesOutsideAllowlistExcluding(t, "modules/group/",
		"handles on the group DB",
		[]string{"group.NewDB("},
		allowlist,
		"group.NewDB is the only way to a *group.DB, and that type carries the "+
			"membership primitives. A new holder outside modules/group must be argued "+
			"for here — add it to groupDBHolders with what it reads — because "+
			"TestDeclaredHoldersDoNotWriteMembership then checks it for writes.")
}

// TestDeclaredHoldersDoNotWriteMembership is the other half: being on the
// holder list must not become permission to call a primitive later.
func TestDeclaredHoldersDoNotWriteMembership(t *testing.T) {
	pwd, err := os.Getwd()
	require.NoError(t, err)
	root := filepath.Clean(filepath.Join(pwd, "..", ".."))

	for file, why := range groupDBHolders {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		require.NoError(t, err, "holder %s (%s) is listed but missing — drop the entry", file, why)

		inBlock := false
		for i, line := range strings.Split(string(raw), "\n") {
			var code string
			code, inBlock = stripGoComments(line, inBlock)
			for _, needle := range admissionPrimitiveNeedles {
				require.NotContains(t, code, needle,
					"%s:%d holds a *group.DB to %s and calls %s on it — a membership write "+
						"outside the admission funnel bypasses shared native semantics. "+
						"Route it through the group service, or reverse-register a step "+
						"the way modules/space receives its preset-group admitter: %s",
					file, i+1, why, needle, strings.TrimSpace(line))
			}
		}
	}
}

func TestNoGroupMemberRowsBuiltOutsideModulesGroup(t *testing.T) {
	// The cross-module half. Any non-test file outside modules/group that
	// constructs a group.MemberModel is building a group membership row, which
	// means it is admitting or removing someone without the funnel. Today there
	// are none.
	//
	// This is a proxy rather than a proof — a caller could pass a variable of
	// that type built elsewhere — but it catches the shape every historical
	// bypass actually had, including the one deleted in this change.
	assertNoWritesOutsideAllowlistExcluding(t, "modules/group/",
		"cross-module group membership rows",
		[]string{"group.MemberModel{"},
		map[string][]string{},
		"a group membership row built outside modules/group cannot have gone "+
			"through the admission funnel. Route the operation through the group "+
			"service instead, or reverse-register a step the way modules/space "+
			"receives its preset-group admitter.")
}

// stripGoComments removes // line comments and /* */ block comment content from
// one line, returning the code-only remainder and whether a block comment is
// still open.
//
// Not a Go parser: a "//" inside a string literal is treated as a comment start.
// The consequence is bounded — it can only shorten a line, so the worst case is
// missing a needle that appears after "//" INSIDE a string, which no real write
// looks like. Being a parser here would mean loading go/ast for a grep.
func stripGoComments(line string, inBlock bool) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(line); i++ {
		if inBlock {
			if i+1 < len(line) && line[i] == '*' && line[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if i+1 < len(line) && line[i] == '/' && line[i+1] == '/' {
			break
		}
		if i+1 < len(line) && line[i] == '/' && line[i+1] == '*' {
			inBlock = true
			i++
			continue
		}
		out.WriteByte(line[i])
	}
	return out.String(), inBlock
}
