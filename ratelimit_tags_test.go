package main

// Repo-wide invariant: every StrictIPRateLimitMiddleware call site must use a
// DISTINCT Redis keyspace tag, unless the share is explicitly declared here.
//
// # Why this test reads the repository instead of a list
//
// The tag is the Redis key prefix `ratelimit:strict:<tag>:`, so two routes that
// share a tag share one per-IP quota: traffic to one silently consumes the
// other's budget, and a busy endpoint can lock a security-critical one out.
//
// The first attempt at guarding this (modules/space, deleted with the roster
// endpoint) compared one new tag against a hardcoded slice of "existing" tags.
// That is a tautology dressed as a test — it asserts a fact about two literals
// in the same file, it can only ever fail if someone edits it to fail, and its
// list was ALREADY missing `integration_oidc` and `manager_login` at the time
// it was written. A guard that cannot observe the thing it guards is worse than
// none, because it discharges the reviewer's attention.
//
// This version parses every non-test .go file in the repository, finds each
// StrictIPRateLimitMiddleware call, resolves its tag argument (string literal
// or same-package const), and reports duplicates. Adding a new limiter with a
// copy-pasted tag fails here with no edit to any list.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// intentionalStrictRateLimitTagShares declares tags that are deliberately used
// by more than one call site, with the reason. Anything not listed here must be
// unique. Keep this map tiny and keep the reasons concrete: each entry is a
// merged quota someone has to reason about during an incident.
var intentionalStrictRateLimitTagShares = map[string]string{
	// modules/user/api.go (POST /v1/auth/verify …) and
	// modules/bot_provision/bot_api.go (GET /v1/bot/:uid/token) deliberately
	// share one per-IP keyspace for all credential-touching paths. See the
	// comment above the bot_provision call site.
	"verify": "credential-verification paths share one per-IP keyspace on purpose " +
		"(modules/user + modules/bot_provision; see bot_provision/bot_api.go)",
}

type strictRateLimitTagUse struct {
	tag  string
	file string
	line int
}

func TestStrictIPRateLimitTagsAreUniqueRepoWide(t *testing.T) {
	uses, resolvedFrom := collectStrictRateLimitTagUses(t)

	// Self-check: if the walker stops finding call sites (helper renamed, files
	// moved), every assertion below becomes vacuously true. Fail loudly instead.
	require.GreaterOrEqual(t, len(uses), 10,
		"only %d StrictIPRateLimitMiddleware call sites found — the walker is probably "+
			"broken (helper renamed? module layout changed?). A guard that finds nothing "+
			"passes for the wrong reason.", len(uses))
	require.Contains(t, resolvedFrom, "identifier",
		"no tag was resolved from a named constant — const resolution is broken, so any "+
			"limiter whose tag is a const would be invisible to this guard")
	require.Contains(t, resolvedFrom, "literal",
		"no tag was resolved from a string literal — literal extraction is broken")

	byTag := map[string][]strictRateLimitTagUse{}
	for _, u := range uses {
		byTag[u.tag] = append(byTag[u.tag], u)
	}

	tags := make([]string, 0, len(byTag))
	for tag := range byTag {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	for _, tag := range tags {
		sites := byTag[tag]
		if len(sites) == 1 {
			continue
		}
		if reason, ok := intentionalStrictRateLimitTagShares[tag]; ok {
			t.Logf("tag %q shared by %d call sites (declared intentional: %s)",
				tag, len(sites), reason)
			continue
		}
		locations := make([]string, 0, len(sites))
		for _, s := range sites {
			locations = append(locations, s.file+":"+strconv.Itoa(s.line))
		}
		t.Errorf("strict-IP rate-limit tag %q is used by %d call sites:\n  %s\n\n"+
			"The tag is the Redis key prefix `ratelimit:strict:<tag>:`, so these routes "+
			"share one per-IP quota — traffic to one consumes the other's budget. Give "+
			"the new route its own tag, or, if the merged quota is deliberate, declare "+
			"it in intentionalStrictRateLimitTagShares with the reason.",
			tag, len(sites), strings.Join(locations, "\n  "))
	}

	// Sanity: every declared exception must actually be shared, so the map does
	// not accumulate stale entries that quietly excuse a future duplicate.
	for tag := range intentionalStrictRateLimitTagShares {
		require.Greater(t, len(byTag[tag]), 1,
			"intentionalStrictRateLimitTagShares declares %q as a deliberate share, but it "+
				"has %d call site(s). Remove the stale entry — otherwise it silently "+
				"pre-approves a future duplicate.", tag, len(byTag[tag]))
	}
}

// collectStrictRateLimitTagUses parses every non-test .go file in the repo and
// returns each StrictIPRateLimitMiddleware tag argument it can resolve, plus
// the set of resolution strategies that actually fired (used for self-checks).
func collectStrictRateLimitTagUses(t *testing.T) ([]strictRateLimitTagUse, map[string]bool) {
	t.Helper()

	// Package-level string consts, keyed by "<dir>\x00<name>", so an identifier
	// tag can be resolved against its own package.
	consts := map[string]string{}
	var files []string

	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "dashboard", "assets":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file we cannot parse is not a reason to fail this guard, but it
			// IS a reason to say so: an unparsed file is an unchecked file.
			t.Logf("skipping unparsable file %s: %v", path, perr)
			return nil
		}
		parsed[path] = f
		files = append(files, path)
		dir := filepath.Dir(path)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, uerr := strconv.Unquote(lit.Value); uerr == nil {
							consts[dir+"\x00"+name.Name] = v
						}
					}
				}
			}
		}
		return nil
	})
	require.NoError(t, err, "walking the repository")

	resolvedFrom := map[string]bool{}
	var uses []strictRateLimitTagUse

	for _, path := range files {
		dir := filepath.Dir(path)
		ast.Inspect(parsed[path], func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "StrictIPRateLimitMiddleware" {
				return true
			}
			// Signature: (ctx, redisClient, tag, rps, burst) — tag is index 2.
			if len(call.Args) < 3 {
				t.Errorf("%s:%d: StrictIPRateLimitMiddleware called with %d args; the tag "+
					"is expected at index 2. If the helper's signature changed, update "+
					"this guard.", path, fset.Position(call.Pos()).Line, len(call.Args))
				return true
			}
			line := fset.Position(call.Args[2].Pos()).Line
			switch arg := call.Args[2].(type) {
			case *ast.BasicLit:
				if arg.Kind != token.STRING {
					t.Errorf("%s:%d: non-string literal tag argument", path, line)
					return true
				}
				v, uerr := strconv.Unquote(arg.Value)
				if uerr != nil {
					t.Errorf("%s:%d: cannot unquote tag literal %s", path, line, arg.Value)
					return true
				}
				resolvedFrom["literal"] = true
				uses = append(uses, strictRateLimitTagUse{tag: v, file: path, line: line})
			case *ast.Ident:
				v, ok := consts[dir+"\x00"+arg.Name]
				if !ok {
					t.Errorf("%s:%d: tag identifier %q does not resolve to a package-level "+
						"string constant in %s. This guard can only compare tags it can "+
						"read — either make the tag a plain const, or teach the resolver "+
						"about the new shape.", path, line, arg.Name, dir)
					return true
				}
				resolvedFrom["identifier"] = true
				uses = append(uses, strictRateLimitTagUse{tag: v, file: path, line: line})
			default:
				t.Errorf("%s:%d: tag argument is a %T, which this guard cannot resolve. "+
					"Keep rate-limit tags as string literals or package-level consts so "+
					"the uniqueness invariant stays machine-checkable.", path, line, arg)
			}
			return true
		})
	}
	return uses, resolvedFrom
}
