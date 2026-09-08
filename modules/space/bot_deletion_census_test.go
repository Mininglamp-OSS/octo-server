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
					case *ast.KeyValueExpr:
						if stringLit(v.Key) == "status" && intLit(v.Value) == "0" {
							cf.setsStatusZero = true
						}
					case *ast.SelectorExpr:
						cf.idents[v.Sel.Name] = true
					case *ast.Ident:
						cf.idents[v.Name] = true
					case *ast.BasicLit:
						if v.Kind == token.STRING && rawRobotDisable.MatchString(v.Value) {
							cf.rawDisablesRobot = true
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
// The dbr form needs BOTH halves — `.Update("robot")` and a literal `"status": 0` —
// so updateRobotInfo (a caller-supplied field map) and `"status": 0` written to some
// other table both stay out, and `updateRobot` ("status": m.Status) stays out too.
func (c *censusFunc) disablesBotAccount() bool {
	return c.rawDisablesRobot || (c.updatesRobotTable && c.setsStatusZero)
}

func (c *censusFunc) String() string { return c.file + ":" + c.name }

func TestEveryBotDeletionEntryPointRoutesThroughD14(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	funcs := scanRepoFuncs(t, repoRoot, "modules", "pkg", "internal")

	// Step 1: the primitives — everything that turns a bot account off.
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

	// Step 2: the doors — every function that calls a non-exempt primitive.
	var doors []*censusFunc
	for _, f := range funcs {
		if primitives[f.name] != nil {
			continue // a primitive is not a door onto itself
		}
		for name := range primitives {
			if _, exempt := d14ExemptPrimitives[name]; exempt {
				continue
			}
			if f.calls[name] {
				doors = append(doors, f)
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
