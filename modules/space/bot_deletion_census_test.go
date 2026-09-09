package space

// The D14 census, expressed as a test over the SET of bot-deletion entry points
// rather than one test per endpoint.
//
// D14 says: when a bot account is deleted, its Space seats close through the removal
// outbox, so the project-seat cascade and the group detach actually run. PR #855
// shipped that on one door, then a review found a second, then a review found a
// third. Every one of those rounds ended with an endpoint-level test, and every one
// of them left the next door undiscovered, because an endpoint-level test can only
// pin the door it names.
//
// Worse, the census that found door two was run as "who WRITES space_member" — a
// question that structurally cannot find a door whose entire defect is deleting a bot
// WITHOUT writing space_member. The right question is "what deletes a bot", and this
// file asks it of the source tree on every run:
//
//  1. find every function that disables a bot account (writes `robot`.status = 0),
//  2. find every function that calls one,
//  3. require each of those to route through this package's D14 entry point —
//     or to be recorded below as a deliberate exemption with its reason.
//
// A fourth door added tomorrow fails this test the moment it is written. It lives in
// modules/space because D14 is this package's rule: CloseAllSpaceSeats and
// MemberRemoveReasonBotDeleted are both defined here.
//
// Matching is STRUCTURAL (go/ast), never textual: a call is a call expression, a
// constant is an identifier. A comment mentioning CloseAllSpaceSeats cannot satisfy
// anything here — the shape the sixth and seventh reviews caught twice in this PR.
//
// Known limits, stated rather than left to be discovered:
//   - Calls are matched by the selector's NAME, not by resolved type, so two
//     same-named primitives in different packages would collapse into one. The
//     "known primitives must still be found" assertion is what keeps that from
//     going silent for the three that exist today.
//   - A door is the DIRECT caller of a primitive. If someone puts a wrapper between
//     the handler and the primitive, this reports the wrapper — which fails loudly
//     and is the safe direction, rather than passing because the seam is one frame
//     further up.
//   - Indirection is resolved ONE level: a caller of a STATUS-CAPABLE writer is a door
//     candidate (step 0 below). A disable assembled across two hops would still be
//     invisible.
//   - The matched spellings are enumerated by
//     TestCensusMatcherSeesEverySpellingItClaimsTo, which is the list to read and the
//     list to extend. It exists because deleting any one of these branches used to
//     leave the whole suite green.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// d14ExemptPrimitives records bot-disabling primitives that deliberately do NOT go
// through the removal outbox, and why. Anything not listed must route.
var d14ExemptPrimitives = map[string]string{
	"deleteCreatedBotArtifacts": "创建失败的补偿路径：Bot 从来没有活过——它唯一那次 " +
		"space_member 写入正是刚刚失败的那一步，没有席位可关，也没有级联可驱动。" +
		"这里写裸 UPDATE 是 fail-closed 兜底，不是删除一个在用的 Bot。",
}

// d14ExemptDoors records CALL SITES of a non-exempt primitive that deliberately do
// not route, keyed by "<file>:<func>" so a same-named function elsewhere does not
// inherit the exemption.
//
// The distinction from d14ExemptPrimitives is real: deleteRobot is BOTH the deletion
// primitive behind the chat-command door AND the compensation used when bot creation
// half-fails. The primitive cannot be exempt; that one call site can.
var d14ExemptDoors = map[string]string{
	"modules/robot/api_manager.go:updateRobotStatus": "超管**停用/启用** bot（PUT /robot/status/{id}/{status}）。" +
		"停用不是删除：可逆、不释放 username、不轮换 token，账号仍然存在，所以不走 D14。" +
		"第十一轮 review 的第 7 条把它单独拎出来过，并把「停用该不该释放项目席位」定性为**产品决策**——" +
		"在那件事定下来之前，这里登记豁免而不是替产品做主。" +
		"残留说清楚：被停用的 bot 保留 space_member、octo_project_member（仍占 max_members 配额）与全部群成员行，" +
		"而本 PR 之前引入的资格判定把 robot.status != 1 一律视为不可用分身，D13 也不会回收它的席位——" +
		"于是系统自己认为它「不是可用分身」，却没有任何东西对账这件事。" +
		"注意这扇门的 0 是**参数畸形时的默认值**（ParseInt64OrDefault(param, 0)），所以是可以被误触的。",
	"modules/robot/api_manager.go:robotUpdate": "超管编辑 bot（PUT /robots/{id}），status 是可选字段之一，" +
		"经 updateRobotInfo 的调用方字段表落库。与上一条同一件事、同一个理由、同一份残留，" +
		"只是换了一个更宽的端点：这里还能同时改 description。",
	"modules/botfather/command.go:tryCreateBotCore": "创建失败的补偿：AddUser 失败后回滚刚插入的 " +
		"robot 行。此刻 Bot 还没有走到任何 Space 绑定，没有席位可关。与 " +
		"deleteCreatedBotArtifacts 同一类，只是复用了 deleteRobot 这个原语。" +
		"这一条是本次普查自己找出来的第四个调用点——正是它证明「按端点写测试」为什么不够。",
}

// d14Seam is what "routes through D14" looks like in a call expression: either the
// package function itself or the injectable field every door holds for testability.
var d14Seam = map[string]bool{
	"CloseAllSpaceSeats": true,
	"closeSeatsFn":       true,
}

// rawRobotDisable matches the raw-SQL spelling of "turn this bot's account off".
var rawRobotDisable = regexp.MustCompile(`(?is)update\s+` + "`?robot`?" + `\s+set\s+status\s*=\s*0`)

// rawRobotDelete matches the raw-SQL spelling of "remove the bot's account row".
var rawRobotDelete = regexp.MustCompile(`(?is)delete\s+from\s+` + "`?robot`?" + `\b`)

type censusFunc struct {
	name   string
	file   string
	calls  map[string]bool
	idents map[string]bool
	// updatesRobotTable is `.Update("robot")` — the dbr spelling of a write to the
	// bot account table, matched with its argument so `.Update("user")` cannot count.
	updatesRobotTable bool
	// setsStatusZero is a literal zero written to the status column, in either dbr
	// spelling: a `"status": 0` entry in a SetMap literal, or `.Set("status", 0)`.
	setsStatusZero bool
	// writesStatusColumn is a write to the status column with ANY value, literal or
	// not — `"status": m.Status`, `.Set("status", v)`, or a caller-supplied field map
	// handed to SetMap. Combined with updatesRobotTable this is what makes a function
	// STATUS-CAPABLE: able to put a bot's account into status 0 at runtime.
	writesStatusColumn bool
	// takesFieldMapParam is `SetMap(p)` where p is one of this function's own
	// PARAMETERS — the caller chooses the columns, so the callee can write status
	// without ever naming it.
	//
	// "not a literal" is not enough, and the widened matcher proved it by accusing
	// bot_api's applyAgentReport: that one hands SetMap a map it builds itself, whose
	// keys are all agent_* columns and never status. A locally-built map is visible
	// in the same function, so the columns are knowable; a parameter is not.
	takesFieldMapParam bool
	// rawDisablesRobot is the raw-SQL spelling of the same write.
	rawDisablesRobot bool
	// deletesRobotRow is `DeleteFrom("robot")` or raw `DELETE FROM robot` — a HARD
	// delete of the account row. A bot whose robot row is gone is at least as deleted
	// as one whose status is 0, so this counts too.
	deletesRobotRow bool
}

// scanRepoFuncs parses every non-test Go file under the given roots.
func scanRepoFuncs(t *testing.T, repoRoot string, roots ...string) []*censusFunc {
	t.Helper()
	var out []*censusFunc
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.Walk(filepath.Join(repoRoot, root), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// ParseComments is deliberately NOT set: the AST carries no comment text at
			// all, so no assertion below can be satisfied by prose.
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			out = append(out, censusFuncsIn(f, strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))...)
			return nil
		})
		require.NoError(t, err, "walking %s", root)
	}
	require.NotEmpty(t, out, "the scanner found no functions at all — it is broken, not the tree")
	return out
}

// censusFuncsIn classifies every function in one parsed file.
//
// Split out of the walk so the matcher can be driven from source STRINGS as well as
// from the tree — see TestCensusMatcherSeesEverySpellingItClaimsTo. Without that the
// detection branches are only ever exercised by whatever the repository happens to
// contain today, which is the failure this whole file exists to prevent, one level up:
// the branches that close a blind spot become blind spots themselves the moment the
// tree stops containing an example. PR #868s review, P2-2.
func censusFuncsIn(f *ast.File, file string) []*censusFunc {
	var out []*censusFunc
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		params := map[string]bool{}
		if fn.Type.Params != nil {
			for _, field := range fn.Type.Params.List {
				for _, id := range field.Names {
					params[id.Name] = true
				}
			}
		}
		cf := &censusFunc{
			name:   fn.Name.Name,
			file:   file,
			calls:  map[string]bool{},
			idents: map[string]bool{},
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				name := calleeName(v.Fun)
				cf.calls[name] = true
				if name == "Update" && len(v.Args) == 1 && stringLit(v.Args[0]) == "robot" {
					cf.updatesRobotTable = true
				}
				if name == "DeleteFrom" && len(v.Args) == 1 && stringLit(v.Args[0]) == "robot" {
					cf.deletesRobotRow = true
				}
				// `.Set("status", …)` — the chained spelling, used 182 times in
				// this tree. Missing it did not merely hide a door: the function
				// landed in the status-capable set's OLD counterpart, i.e. it was
				// filed as a benign helper. PR #868's review, P2-1.
				if name == "Set" && len(v.Args) == 2 && stringLit(v.Args[0]) == "status" {
					cf.writesStatusColumn = true
					if intLit(v.Args[1]) == "0" {
						cf.setsStatusZero = true
					}
				}
				// `SetMap(p)` where p is one of this function's parameters: the
				// COLUMNS are the caller's, so this callee can write status
				// without naming it.
				if name == "SetMap" && len(v.Args) == 1 {
					if id, ok := v.Args[0].(*ast.Ident); ok && params[id.Name] {
						cf.takesFieldMapParam = true
					}
				}
			case *ast.KeyValueExpr:
				if stringLit(v.Key) == "status" {
					cf.writesStatusColumn = true
					if intLit(v.Value) == "0" {
						cf.setsStatusZero = true
					}
				}
			case *ast.SelectorExpr:
				cf.idents[v.Sel.Name] = true
			case *ast.Ident:
				cf.idents[v.Name] = true
			case *ast.BasicLit:
				if v.Kind != token.STRING {
					return true
				}
				if rawRobotDisable.MatchString(v.Value) {
					cf.rawDisablesRobot = true
				}
				if rawRobotDelete.MatchString(v.Value) {
					cf.deletesRobotRow = true
				}
			}
			return true
		})
		out = append(out, cf)
	}
	return out
}

// calleeName reduces a call's function expression to the name a census can match:
// `f()` -> "f", `x.y.f()` -> "f".
func calleeName(fun ast.Expr) string {
	switch v := fun.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.IndexExpr: // generic instantiation
		return calleeName(v.X)
	}
	return ""
}

// stringLit returns the value of a string literal expression, or "".
func stringLit(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return v
}

// intLit returns the text of an integer literal expression, or "".
func intLit(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return ""
	}
	return lit.Value
}

// disablesBotAccount reports whether this function turns a bot's `robot` row off,
// in either spelling used in this repository.
//
// The spellings matched today — an enumeration of what IS handled, not a claim that
// the set is complete; the fixtures in TestCensusMatcherSeesEverySpellingItClaimsTo are
// the authoritative list and each one dies if its branch is removed:
//
//   - the dbr disable, needing BOTH halves — `.Update("robot")` and a literal zero on
//     the status column, in either the `SetMap{"status": 0}` or the `.Set("status", 0)`
//     spelling — so `"status": 0` written to some other table stays out. The chained
//     form was missing until PR #868, and its absence did more than hide a door: the
//     function landed in the benign-helper set instead;
//   - the raw-SQL disable;
//   - the HARD delete, in either spelling. This one was missing until the eleventh
//     review, which proved it by adding a `DeleteFrom("robot")` door and watching the
//     guard pass. The gap was invisible from inside because deleteCreatedBotArtifacts
//     — which uses exactly that spelling at modules/botfather/db.go:239 — was already
//     in the known-primitives list, registering only via the raw UPDATE in its
//     fail-closed fallback. It passed for the wrong reason, so it could not expose the
//     matcher's blind spot.
//
// A write that puts status to a value it does not spell — `updateRobot`
// ("status": m.Status), or `updateRobotInfo` (SetMap of a caller-supplied map) — is not
// a primitive but is STATUS-CAPABLE, and its callers are resolved one hop up in step 0
// of the test rather than here, because neither half is in one function.
func (c *censusFunc) disablesBotAccount() bool {
	return c.rawDisablesRobot || c.deletesRobotRow || (c.updatesRobotTable && c.setsStatusZero)
}

func (c *censusFunc) String() string { return c.file + ":" + c.name }

func TestEveryBotDeletionEntryPointRoutesThroughD14(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	funcs := scanRepoFuncs(t, repoRoot, "modules", "pkg", "internal")

	// Step 0: STATUS-CAPABLE writers — functions that write the robot table's status
	// column, whatever value they write.
	//
	// The old rule keyed on a literal `"status": 0` in the caller. It drew the line at
	// the wrong place, and the review proved it with the two doors it misses:
	//
	//	updateRobotStatus  PUT /robot/status/{id}/{status}  robot.Status = int(status)
	//	                                                    -> updateRobot
	//	                                                    -> UPDATE robot SET version=?, status=?
	//	robotUpdate        PUT /robots/{id}                 fields["status"] = *req.Status
	//	                                                    -> updateRobotInfo
	//
	// updateRobot's write is BYTE-IDENTICAL to deleteRobotSoft's, which this file
	// classifies as a deletion primitive. The only difference is that one spells the
	// zero as a literal and the other holds it in a variable — and the first door's
	// zero is a malformed-parameter DEFAULT, so it is reachable by accident. The
	// previous version of this comment justified the non-match by calling those
	// endpoints "a reversible disable, not a deletion"; deleteRobotSoft is exactly as
	// reversible, so that was a semantic story told about a syntactic behaviour.
	//
	// So: a function that can put a bot's status to zero is status-capable, and every
	// caller of one is a door candidate. What happens next is a DECISION recorded in
	// d14ExemptDoors, not an omission in a matcher.
	//
	// This also narrows what used to be flagged: writers that touch other columns
	// (setDescription, updateBotCommands, updateRobotIMTokenCache) are no longer in the
	// set, so their callers are no longer accused of being bot-deletion doors.
	statusCapableWriters := map[string]bool{}
	for _, f := range funcs {
		if !f.updatesRobotTable || f.disablesBotAccount() {
			continue
		}
		if f.writesStatusColumn || f.takesFieldMapParam {
			statusCapableWriters[f.name] = true
		}
	}

	// Step 1: the primitives — everything that turns a bot account off or removes it.
	primitives := map[string]*censusFunc{}
	for _, f := range funcs {
		if f.disablesBotAccount() {
			primitives[f.name] = f
		}
	}
	names := make([]string, 0, len(primitives))
	for n := range primitives {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("bot-disabling primitives found: %v", names)

	// The scanner must keep finding the primitives we know about. Without this the
	// census degrades silently into a test that asserts nothing the day someone
	// rewrites one of these with a spelling the matcher misses — the exact shape of
	// vacuity this PR has already been caught on twice.
	for _, known := range []string{"deleteRobot", "deleteRobotSoft", "deleteCreatedBotArtifacts"} {
		assert.Contains(t, primitives, known,
			"the scanner no longer recognises %s as a bot-disabling primitive; either it was "+
				"renamed (update this list) or it was rewritten in a spelling disablesBotAccount "+
				"does not match (fix the matcher) — until then the census below is blind to it",
			known)
	}

	// Step 2: the doors — every function that calls a non-exempt primitive, plus every
	// function that spells the disable across two hops (step 0).
	//
	// The split-spelling function is a DOOR, not a primitive, and getting that wrong is
	// how the first attempt at this fix passed its own mutation: classified as a
	// primitive it was skipped by the "a primitive is not a door onto itself" rule, and
	// since nothing called it, nothing was ever asserted about it. The classification is
	// not cosmetic — in that shape the deletion site IS the function holding the
	// literal, because the `.Update("robot")` it borrows lives in a generic helper that
	// is not itself a deletion.
	var doors []*censusFunc
	seen := map[string]bool{}
	addDoor := func(f *censusFunc) {
		if seen[f.String()] {
			return
		}
		seen[f.String()] = true
		doors = append(doors, f)
	}
	called := map[string]bool{}
	for _, f := range funcs {
		for name := range primitives {
			if f.name != name && f.calls[name] {
				called[name] = true
			}
		}
	}
	// A primitive NOTHING calls is its own door.
	//
	// "A primitive is not a door onto itself" assumes primitives are db-layer helpers
	// with a handler above them — true of all three today. It is false for a handler
	// that does the deletion inline, and the failure is silent in the worst way: the
	// function is classified, excluded from the door set by that rule, and then checked
	// by nothing. That is the same shape as the split-spelling fix's first attempt, one
	// level over, and it is why this rule is stated rather than assumed.
	for name, f := range primitives {
		if _, exempt := d14ExemptPrimitives[name]; exempt {
			continue
		}
		if !called[name] {
			addDoor(f)
		}
	}
	for _, f := range funcs {
		if primitives[f.name] == nil {
			for name := range primitives {
				if _, exempt := d14ExemptPrimitives[name]; exempt {
					continue
				}
				if f.calls[name] {
					addDoor(f)
					break
				}
			}
		}
		if f.disablesBotAccount() {
			continue
		}
		for writer := range statusCapableWriters {
			if f.calls[writer] {
				addDoor(f)
				break
			}
		}
	}
	require.NotEmpty(t, doors,
		"no bot-deletion entry point found at all — the census cannot be vacuous and pass")
	doorNames := make([]string, 0, len(doors))
	for _, d := range doors {
		doorNames = append(doorNames, d.String())
	}
	sort.Strings(doorNames)
	t.Logf("bot-deletion entry points found: %v", doorNames)

	// The three known doors must still be among them, for the same anti-vacuity
	// reason as the primitive list above.
	for _, known := range []string{"onDeleteConfirm", "deleteUserBot", "robotDelete"} {
		found := false
		for _, d := range doors {
			if d.name == known {
				found = true
			}
		}
		assert.True(t, found,
			"%s is no longer recognised as a bot-deletion entry point — if it was renamed or "+
				"restructured, this list has to follow it, or the census stops covering that door",
			known)
	}

	// Step 3: every door routes through D14, unless it is a recorded exemption.
	for _, d := range doors {
		if reason, exempt := d14ExemptDoors[d.String()]; exempt {
			t.Logf("exempt door %s: %s", d, reason)
			continue
		}
		routed := false
		for seam := range d14Seam {
			if d.calls[seam] {
				routed = true
			}
		}
		assert.True(t, routed,
			"%s deletes a bot without going through CloseAllSpaceSeats. D14 exists because the "+
				"alternatives all leave residue nothing repairs: a bare `UPDATE space_member SET "+
				"status=0` skips the project-seat cascade and the group detach, and closing no "+
				"seat at all leaves an ACTIVE project seat on an account that no longer exists — "+
				"I4 scan B's violating state, and that scan is report-only. If this door is "+
				"deliberately exempt, record the primitive it calls in d14ExemptPrimitives with "+
				"the reason, so the exemption is a decision rather than an omission", d)
		assert.True(t, d.idents["MemberRemoveReasonBotDeleted"],
			"%s must close its seats with reason bot_deleted. The group cascade reads the reason "+
				"to decide whether to post \"X was removed by Y\" in every group the account was "+
				"in, and for an account that ceased to exist that sentence is false; "+
				"force_removed would print it", d)
	}
}

// TestCensusMatcherSeesEverySpellingItClaimsTo guards the guard.
//
// The census above asserts an OUTCOME over the tree: every door routes or is exempt.
// That says nothing about whether the matcher can still SEE each shape — and the
// review proved the difference matters twice over. Round 11 found the census green
// because an exempt primitive matched through an unrelated fallback, so the branch
// meant to catch its real spelling was never exercised. The fix for that then had the
// same property one level up: PR #868's review deleted each newly added branch in turn
// and the suite stayed green, because nothing in the tree spells those shapes today.
//
// A matcher branch with no example is a branch that quietly stops working. So each one
// gets a fixture here, parsed from source rather than from the repository, which is
// also the only way to cover a shape the tree does not currently contain.
//
// Deleting any single detection branch turns exactly one case below red.
func TestCensusMatcherSeesEverySpellingItClaimsTo(t *testing.T) {
	cases := []struct {
		name string
		src  string
		// want is asserted on the function named "probe".
		wantDisables      bool
		wantStatusCapable bool
	}{
		{
			name: "dbr SetMap literal zero — the spelling deleteRobotSoft uses",
			src: `package p
func probe(s S, id string) error {
	_, err := s.Update("robot").SetMap(map[string]interface{}{"status": 0}).Where("robot_id=?", id).Exec()
	return err
}`,
			wantDisables: true,
		},
		{
			name: "dbr Set chain literal zero — 182 uses in this tree, invisible until PR #868",
			src: `package p
func probe(s S, id string) error {
	_, err := s.Update("robot").Set("status", 0).Where("robot_id=?", id).Exec()
	return err
}`,
			wantDisables: true,
		},
		{
			name: "raw SQL disable",
			src: `package p
func probe(s S, id string) error {
	_, err := s.UpdateBySql("UPDATE robot SET status=0 WHERE robot_id=?", id).Exec()
	return err
}`,
			wantDisables: true,
		},
		{
			name: "hard delete, dbr — the spelling deleteCreatedBotArtifacts actually uses",
			src: `package p
func probe(s S, id string) error {
	_, err := s.DeleteFrom("robot").Where("robot_id=?", id).Exec()
	return err
}`,
			wantDisables: true,
		},
		{
			name: "hard delete, raw SQL",
			src: `package p
func probe(s S, id string) error {
	_, err := s.DeleteBySql("DELETE FROM robot WHERE robot_id=?", id).Exec()
	return err
}`,
			wantDisables: true,
		},
		{
			name: "status-capable: writes status from a variable — the updateRobot shape",
			src: `package p
func probe(s S, m *robot) error {
	_, err := s.Update("robot").SetMap(map[string]interface{}{"version": m.Version, "status": m.Status}).Exec()
	return err
}`,
			wantStatusCapable: true,
		},
		{
			name: "status-capable: caller-supplied field map — the updateRobotInfo shape",
			src: `package p
func probe(s S, id string, fields map[string]interface{}) error {
	_, err := s.Update("robot").SetMap(fields).Where("robot_id=?", id).Exec()
	return err
}`,
			wantStatusCapable: true,
		},
		{
			name: "NOT status-capable: a locally built map that never names status",
			src: `package p
func probe(s S, id string, v string) error {
	set := map[string]interface{}{}
	set["agent_version"] = v
	_, err := s.Update("robot").SetMap(set).Where("robot_id=?", id).Exec()
	return err
}`,
		},
		{
			name: "NOT a robot write at all: status zero on another table",
			src: `package p
func probe(s S, id string) error {
	_, err := s.Update("space_member").SetMap(map[string]interface{}{"status": 0}).Where("uid=?", id).Exec()
	return err
}`,
		},
		{
			name: "NOT a match: a table whose name merely starts with robot",
			src: `package p
func probe(s S, id string) error {
	_, err := s.UpdateBySql("DELETE FROM robot_menu WHERE robot_id=?", id).Exec()
	return err
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "probe.go", tc.src, 0)
			require.NoError(t, err)
			var probe *censusFunc
			for _, cf := range censusFuncsIn(f, "probe.go") {
				if cf.name == "probe" {
					probe = cf
				}
			}
			require.NotNil(t, probe, "the fixture must define a function named probe")

			assert.Equal(t, tc.wantDisables, probe.disablesBotAccount(),
				"disablesBotAccount is what promotes a function to a deletion PRIMITIVE, "+
					"and every door of a non-exempt primitive must route through D14. A "+
					"spelling it cannot see is a deletion the census reports nothing about")
			statusCapable := probe.updatesRobotTable &&
				!probe.disablesBotAccount() &&
				(probe.writesStatusColumn || probe.takesFieldMapParam)
			assert.Equal(t, tc.wantStatusCapable, statusCapable,
				"status-capable writers are what make their CALLERS door candidates. Miss "+
					"one and an endpoint that puts a bot into status 0 is invisible; widen "+
					"it too far and innocent callers get told to call CloseAllSpaceSeats")
		})
	}
}
