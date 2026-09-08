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
//   - Indirection is resolved ONE level, and only for the caller-supplied-field-map
//     shape (step 0 below). A primitive assembled across two hops would still be
//     invisible.

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
	// setsStatusZero is a `"status": 0` entry in a composite literal.
	setsStatusZero bool
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
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				cf := &censusFunc{
					name:   fn.Name.Name,
					file:   strings.TrimPrefix(path, repoRoot+string(filepath.Separator)),
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
					case *ast.KeyValueExpr:
						if stringLit(v.Key) == "status" && intLit(v.Value) == "0" {
							cf.setsStatusZero = true
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
			return nil
		})
		require.NoError(t, err, "walking %s", root)
	}
	require.NotEmpty(t, out, "the scanner found no functions at all — it is broken, not the tree")
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
// Three spellings count, because all three end with an account that cannot be used:
//
//   - the dbr disable, needing BOTH halves — `.Update("robot")` and a literal
//     `"status": 0` — so `"status": 0` written to some other table stays out, and
//     `updateRobot` ("status": m.Status) stays out too;
//   - the raw-SQL disable;
//   - the HARD delete, in either spelling. This one was missing until the eleventh
//     review, which proved it by adding a `DeleteFrom("robot")` door and watching the
//     guard pass. The gap was invisible from inside because deleteCreatedBotArtifacts
//     — which uses exactly that spelling at modules/botfather/db.go:239 — was already
//     in the known-primitives list, registering only via the raw UPDATE in its
//     fail-closed fallback. It passed for the wrong reason, so it could not expose the
//     matcher's blind spot.
//
// The fourth shape, a caller-supplied field map, is resolved one hop up in step 0 of
// the test rather than here, because neither half of it is in one function.
func (c *censusFunc) disablesBotAccount() bool {
	return c.rawDisablesRobot || c.deletesRobotRow || (c.updatesRobotTable && c.setsStatusZero)
}

func (c *censusFunc) String() string { return c.file + ":" + c.name }

func TestEveryBotDeletionEntryPointRoutesThroughD14(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	funcs := scanRepoFuncs(t, repoRoot, "modules", "pkg", "internal")

	// Step 0: functions that write the `robot` table with a CALLER-SUPPLIED field map.
	//
	// updateRobotInfo (modules/robot/db.go) is the one that exists: it holds the
	// `.Update("robot")` and no literal, while a caller passing {"status": 0} holds the
	// literal and no `.Update("robot")`. Split that way, NEITHER half is a primitive and
	// the caller is not a door — a bypass the eleventh review found by reading the
	// matcher rather than by finding a live instance of it.
	//
	// Resolving one level of indirection closes it, and is the right shape rather than
	// an exemption: the caller genuinely does disable a bot, it just borrows someone
	// else's UPDATE to do it. Recording updateRobotInfo as EXEMPT would have said the
	// opposite — that its callers may skip D14 — which is false. The resolved caller
	// joins the DOOR set in step 2, not the primitive set; see the note there for why
	// that distinction is what makes the mutation red.
	//
	// No false positive today: robotUpdate builds its map from *req.Status, a pointer
	// deref rather than a literal 0, so it is not flagged. That is correct — it is a
	// reversible disable, not a deletion.
	robotFieldWriters := map[string]bool{}
	for _, f := range funcs {
		if f.updatesRobotTable && !f.setsStatusZero {
			robotFieldWriters[f.name] = true
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
		if !f.setsStatusZero || f.disablesBotAccount() {
			continue
		}
		for writer := range robotFieldWriters {
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
