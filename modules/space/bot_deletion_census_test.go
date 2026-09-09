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
//     The name space is FLAT and repo-wide: `primitives`, `statusCapableWriters`
//     and the `f.calls[name]` lookup are all keyed by bare function name, while
//     d14ExemptDoors is keyed "file:func". That asymmetry is deliberate — an
//     exemption must not be inherited by a same-named function elsewhere — but it
//     means an unrelated `updateRobot` in another package WOULD inherit
//     door-candidate status, and a same-named non-primitive is excluded from the
//     door scan by `primitives[f.name] == nil`. Both fail loudly, not silently.
//   - writesStatusColumn / setsStatusZero are set from ANY `.Set` or key-value in
//     the function body, not only from the statement that targets `robot`. A
//     function that updates `robot` in one statement and writes `"status": 0` to
//     another table in a second is therefore classified as a deletion primitive.
//     Over-match, fail-closed: a false primitive is a false door and a loud
//     failure, never a missed one. Pinned by the last fixture below.
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
		"注意这扇门的 0 是**参数畸形时的默认值**（ParseInt64OrDefault(param, 0)），所以是可以被误触的。" +
		"——而这一句不是这条豁免的一部分，是一个**未修的缺陷**，必须分开读：api_manager.go 里 " +
		"status 从路径段解析后直接写进 robot.Status，没有跟合法状态集校验过，所以 " +
		"PUT /robot/status/{id}/abc 会静默停用一个 bot，/999 会把 999 写进状态列。" +
		"PR #868 第四轮 review 指出：因为本 PR 把 d14ExemptDoors 变成了必须被消费的断言，" +
		"把这扇门登记在这里，等于让普查从此不再报它——一条豁免会替一个输入校验缺陷提供永久掩护。" +
		"所以写明：**产品决策部分是豁免，参数校验部分不是**，后者记在 " +
		"project-p2-all-member-group/context.yaml 的 open_verification 下，" +
		"该修就修，不因为这条豁免存在而算已定。",
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

// rawRobotSetClause captures a raw-SQL UPDATE's SET clause on the robot table, so the
// status column can be looked for THERE and not in the WHERE.
//
// The dbr side has had a non-literal status write since round 2 (`.Set("status", v)`,
// `SetMap(param)`); the raw side only ever matched the literal zero, so
// `UpdateBySql("UPDATE robot SET status=? ...")` was neither a primitive nor
// status-capable. No live miss today — every raw write to this table was enumerated in
// PR #868's review, P2-4 — but "no example today" is the exact condition this file
// exists to stop relying on.
//
// The capture is what keeps it honest: two live statements write bound_agent_ref with
// `status=1` in their WHERE, and a regex that just looked for `status\s*=` anywhere in
// the string would call both of them bot disables.
var rawRobotSetClause = regexp.MustCompile(`(?is)update\s+` + "`?robot`?" + `\s+set\s+(.*?)(?:\swhere\s|$)`)

// rawStatusAssignment matches a status assignment inside such a SET clause.
var rawStatusAssignment = regexp.MustCompile(`(?is)\bstatus\s*=`)

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
				// Raw UPDATE whose SET clause assigns status any value. Status-capable,
				// not a primitive — same line the dbr side draws.
				for _, m := range rawRobotSetClause.FindAllStringSubmatch(v.Value, -1) {
					if rawStatusAssignment.MatchString(m[1]) {
						cf.updatesRobotTable = true
						cf.writesStatusColumn = true
					}
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

// assembleCensus turns classified functions into the three sets the census asserts on.
//
// Extracted from the test body so it can be driven by fixtures as well as by the tree.
// That split is the whole point: censusFuncsIn (the CLASSIFIER) was already fixtured,
// and the reviews then showed the ASSEMBLY was not — deleting the status-capable door
// loop, or the uncalled-primitive rule, left every test green, because both branches
// only ever fire on what the repository happens to contain. A branch exercised solely
// by the tree stops working the day the tree stops containing an example, which is the
// failure this file exists to prevent. PR #868s second review round.
func assembleCensus(funcs []*censusFunc) (map[string]*censusFunc, map[string]bool, []*censusFunc) {
	// STATUS-CAPABLE writers — functions that write the robot table's status column,
	// whatever value they write.
	//
	// The rule used to key on a literal `"status": 0` in the caller. It drew the line at
	// the wrong place, and the review proved it with the two doors it missed:
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
	// zero is a malformed-parameter DEFAULT, so it is reachable by accident. The old
	// comment justified the non-match by calling those endpoints "a reversible disable,
	// not a deletion"; deleteRobotSoft is exactly as reversible, so that was a semantic
	// story told about a syntactic behaviour.
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

	// The primitives — everything that turns a bot account off or removes it.
	primitives := map[string]*censusFunc{}
	for _, f := range funcs {
		if f.disablesBotAccount() {
			primitives[f.name] = f
		}
	}

	called := map[string]bool{}
	for _, f := range funcs {
		for name := range f.calls {
			called[name] = true
		}
	}

	var doors []*censusFunc
	seen := map[string]bool{}
	addDoor := func(f *censusFunc) {
		if seen[f.String()] {
			return
		}
		seen[f.String()] = true
		doors = append(doors, f)
	}

	// A primitive NOTHING calls is its own door.
	//
	// "A primitive is not a door onto itself" assumes primitives are db-layer helpers
	// with a handler above them — true of all three today. It is false for a handler
	// that does the deletion inline, and the failure is silent in the worst way: the
	// function is classified, excluded from the door set by that rule, and then checked
	// by nothing.
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
		// The split-spelling door: the deletion site is the function holding the
		// intent, because the UPDATE it borrows lives in a generic helper that is not
		// itself a deletion.
		for writer := range statusCapableWriters {
			if f.calls[writer] {
				addDoor(f)
				break
			}
		}
	}
	return primitives, statusCapableWriters, doors
}

func TestEveryBotDeletionEntryPointRoutesThroughD14(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	funcs := scanRepoFuncs(t, repoRoot, "modules", "pkg", "internal")

	primitives, statusCapableWriters, doors := assembleCensus(funcs)

	names := make([]string, 0, len(primitives))
	for n := range primitives {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("bot-disabling primitives found: %v", names)
	writerNames := make([]string, 0, len(statusCapableWriters))
	for n := range statusCapableWriters {
		writerNames = append(writerNames, n)
	}
	sort.Strings(writerNames)
	t.Logf("status-capable robot writers found: %v", writerNames)

	// The scanner must keep finding the primitives we know about. Without this the
	// census degrades silently into a test that asserts nothing the day someone
	// rewrites one of these with a spelling the matcher misses — the exact shape of
	// vacuity this file has already been caught on twice.
	for _, known := range []string{"deleteRobot", "deleteRobotSoft", "deleteCreatedBotArtifacts"} {
		assert.Contains(t, primitives, known,
			"the scanner no longer recognises %s as a bot-disabling primitive; either it was "+
				"renamed (update this list) or it was rewritten in a spelling disablesBotAccount "+
				"does not match (fix the matcher) — until then the census below is blind to it",
			known)
	}
	// Same for the two status-capable writers. They are what make the two live disable
	// endpoints visible at all, and neither endpoint is reachable through a primitive.
	for _, known := range []string{"updateRobot", "updateRobotInfo"} {
		assert.Contains(t, statusCapableWriters, known,
			"the scanner no longer recognises %s as status-capable. Its callers stop being "+
				"door candidates, and since neither of them calls a primitive, they leave the "+
				"census entirely — silently, because their exemption entries are not assertions",
			known)
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

	// Every recorded exemption must correspond to a door that was actually FOUND.
	//
	// This is the assertion that makes the exemptions load-bearing, and without it the
	// whole status-capable mechanism is deletable while the suite stays green: remove
	// the door loop and updateRobotStatus / robotUpdate simply vanish from the door
	// set — nothing asserts their presence, because neither is reachable through a
	// primitive, and the known-door list below is satisfied by the three that are.
	// Both reviewers reproduced exactly that, independently, and both named this as the
	// fix. An exemption nothing checks is indistinguishable from a deleted matcher.
	for key := range d14ExemptDoors {
		assert.Contains(t, doorNames, key,
			"%s is recorded in d14ExemptDoors but the census did not find it as a door. Either "+
				"it was renamed or removed (drop the exemption), or the matcher stopped seeing "+
				"the shape that made it a door — and in that case the exemption is now dead "+
				"config describing a decision nothing enforces", key)
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
// Measured, not asserted: each of the ten detection branches in censusFuncsIn was
// deleted in turn and the table below re-run. No branch is deletable while green —
// that is the property this test exists for, and it is the one that was checked.
//
// Eight of the ten redden exactly one case, the one named after that spelling. Two
// redden more, both for the same reason — they are not spellings but shapes that
// several fixtures are built out of: `Update("robot")` (six) is the table precondition
// under every robot-table case, and the key-value `"status": 0` (two) is shared by the
// SetMap composite literal and the over-match case at the end.
//
// The raw-SQL SET-clause capture is mutated separately, since deleting the branch and
// widening it are different failures: matching `status =` anywhere in the string rather
// than inside the captured SET clause reddens the WHERE-clause case, which is the live
// shape (two statements write bound_agent_ref guarded by status=1).
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
			name: "status-capable: .Set chain with a variable — pins the chain's non-literal half",
			src: `package p
func probe(s S, id string, v int) error {
	_, err := s.Update("robot").Set("status", v).Where("robot_id=?", id).Exec()
	return err
}`,
			wantStatusCapable: true,
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
		{
			name: "status-capable, raw SQL: a bound status in the SET clause",
			src: `package p
func probe(s S, id string, v int) error {
	_, err := s.UpdateBySql("UPDATE robot SET status=? WHERE robot_id=?", v, id).Exec()
	return err
}`,
			wantStatusCapable: true,
		},
		{
			// The live shape this branch must NOT match: two statements write
			// bound_agent_ref with status=1 as a GUARD. A regex looking for
			// `status =` anywhere in the string would call both bot disables.
			name: "NOT a match, raw SQL: status appears only in the WHERE",
			src: `package p
func probe(s S, id string, ref string) error {
	_, err := s.UpdateBySql("UPDATE robot SET bound_agent_ref=? WHERE robot_id=? AND status=1", ref, id).Exec()
	return err
}`,
		},
		{
			// KNOWN OVER-MATCH, pinned rather than fixed. The two halves come from
			// two different statements: `Update("robot")` from the first, the zero
			// status from the second, which targets another table entirely. The
			// classifier holds both flags per FUNCTION, so it says primitive.
			// That is the fail-closed direction — a false primitive makes its
			// callers false doors and fails loudly — and scoping the flags to a
			// statement means tracking dbr builder chains through locals, which is
			// a different tool. Asserted so the day someone narrows it, this case
			// tells them the behaviour changed on purpose. PR #868's review, P2-5.
			name: "known over-match: robot update and a status zero in DIFFERENT statements",
			src: `package p
func probe(s S, id string) error {
	if _, err := s.Update("robot").Set("agent_version", "v2").Where("robot_id=?", id).Exec(); err != nil {
		return err
	}
	_, err := s.Update("robot_menu").SetMap(map[string]interface{}{"status": 0}).Where("robot_id=?", id).Exec()
	return err
}`,
			wantDisables: true,
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

// TestCensusDoorAssemblyIsPinned guards the half the fixtures above do not reach.
//
// TestCensusMatcherSeesEverySpellingItClaimsTo covers the CLASSIFIER — what each
// function is. This covers the ASSEMBLY — which of them become doors. Both reviews
// found that half unguarded, independently and by the same method: delete the
// status-capable door loop and the two live disable endpoints vanish from the door set
// with every test still green; delete the uncalled-primitive rule and nothing changes,
// because all three primitives happen to have callers today.
//
// Both branches only ever fired on what the repository contains. That is the exact
// failure this file exists to prevent, and it had now recurred at three levels: the
// census (round 11), the classifier branches (round 1 of this PR), and the assembly
// (round 2). Fixtures here so there is no fourth.
func TestCensusDoorAssemblyIsPinned(t *testing.T) {
	// One synthetic package per case, classified through the same censusFuncsIn the
	// census uses, then assembled through the same assembleCensus.
	assemble := func(t *testing.T, src string) (map[string]bool, []string) {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "probe.go", src, 0)
		require.NoError(t, err)
		_, writers, doors := assembleCensus(censusFuncsIn(f, "probe.go"))
		names := make([]string, 0, len(doors))
		for _, d := range doors {
			names = append(names, d.name)
		}
		sort.Strings(names)
		return writers, names
	}

	t.Run("a caller of a status-capable writer is a door", func(t *testing.T) {
		writers, doors := assemble(t, `package p
func writeRobotFields(s S, id string, fields map[string]interface{}) error {
	_, err := s.Update("robot").SetMap(fields).Where("robot_id=?", id).Exec()
	return err
}
func handler(s S, id string) error {
	return writeRobotFields(s, id, map[string]interface{}{"status": 0})
}`)
		assert.True(t, writers["writeRobotFields"], "the writer must be recognised as status-capable")
		assert.Equal(t, []string{"handler"}, doors,
			"the caller of a status-capable writer must be a door. Delete that loop and the "+
				"two live disable endpoints leave the census silently — they reach it through "+
				"no primitive, so nothing else would notice")
	})

	t.Run("a primitive nothing calls is its own door", func(t *testing.T) {
		_, doors := assemble(t, `package p
func inlineHandler(s S, id string) error {
	_, err := s.Update("robot").SetMap(map[string]interface{}{"status": 0}).Where("robot_id=?", id).Exec()
	return err
}`)
		assert.Equal(t, []string{"inlineHandler"}, doors,
			"a handler that deletes INLINE is a primitive, and 'a primitive is not a door onto "+
				"itself' would then exclude it from every check. All three real primitives have "+
				"callers today, so this branch fires for nobody in the tree — which is why it "+
				"needs a fixture rather than a comment")
	})

	t.Run("a primitive with a caller makes the CALLER the door, not itself", func(t *testing.T) {
		_, doors := assemble(t, `package p
func deleteRow(s S, id string) error {
	_, err := s.Update("robot").SetMap(map[string]interface{}{"status": 0}).Where("robot_id=?", id).Exec()
	return err
}
func handler(s S, id string) error { return deleteRow(s, id) }`)
		assert.Equal(t, []string{"handler"}, doors,
			"the db-layer helper is not the entry point; its caller is. Getting this backwards "+
				"is what made the split-spelling fix pass its own mutation the first time")
	})

	t.Run("a writer that never touches status leaves its callers alone", func(t *testing.T) {
		writers, doors := assemble(t, `package p
func setDescription(s S, id string, d string) error {
	_, err := s.Update("robot").Set("description", d).Where("robot_id=?", id).Exec()
	return err
}
func handler(s S, id string) error { return setDescription(s, id, "hello") }`)
		assert.Empty(t, writers, "touching another column must not make a writer status-capable")
		assert.Empty(t, doors,
			"and its callers must not be accused of deleting a bot — the failure message would "+
				"tell them to call CloseAllSpaceSeats, which for this function is wrong advice")
	})
}
