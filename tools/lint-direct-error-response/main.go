// Command lint-direct-error-response is the D23 "no new direct error response"
// gate. It AST-counts c.AbortWithStatusJSON(...) and c.AbortWithStatus(...)
// calls per file and compares against a committed baseline, failing CI only
// when a file gains new sites (or a brand-new file introduces any).
//
// Why a baseline ratchet rather than a hard block:
//
//	The Phase 0.1 inventory found ~120 pre-existing direct-error sites across
//	modules that have not yet been migrated to httperr.ResponseErrorL (Phase 2
//	is incremental, by module). A hard "zero AbortWithStatusJSON" gate would
//	red-light main today. Instead we snapshot the current counts in
//	baseline.txt and forbid REGRESSIONS: a PR may not add a direct error
//	response to any file beyond what the baseline already tolerates. As each
//	module migrates, its count drops and the baseline is tightened — a
//	monotonic ratchet toward zero.
//
// Counting is per-file rather than per-line because line numbers churn on
// every edit. The known limitation: removing one site and adding another in
// the same file is net-zero and slips the gate; that is acceptable for a
// migration ratchet (net-zero churn in an actively-migrated file is rare and
// caught in human review) and is documented so reviewers know to watch for it.
//
// Usage:
//
//	go run ./tools/lint-direct-error-response [root...]
//
// Defaults to scanning ./modules and ./pkg. Test files (*_test.go) are
// skipped — tests legitimately construct adversarial responses directly.
//
// Exit codes: 0 (clean, possibly with tighten-advisory), 1 (regression),
// 2 (walk/baseline error).
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// baselineFile lives next to this command so `go run ./tools/...` finds it
// regardless of the working directory the CI step uses.
const baselineRelPath = "tools/lint-direct-error-response/baseline.txt"

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"modules", "pkg"}
	}

	baseline, err := loadBaseline(baselineRelPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load baseline %q: %v\n", baselineRelPath, err)
		os.Exit(2)
	}

	counts, err := countSites(roots)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	var regressions, advisories []string

	// Regressions: current count exceeds the tolerated baseline (or a new
	// file appeared with any direct error responses).
	for file, n := range counts {
		allowed := baseline[file]
		if n > allowed {
			if allowed == 0 {
				regressions = append(regressions,
					fmt.Sprintf("%s: %d direct error response(s) in a file not on the baseline", file, n))
			} else {
				regressions = append(regressions,
					fmt.Sprintf("%s: %d direct error response(s), baseline tolerates %d (+%d new)", file, n, allowed, n-allowed))
			}
		}
	}

	// Advisories: a file dropped below its baseline (a module migrated). Not
	// fatal, but the baseline should be tightened so the ratchet holds.
	for file, allowed := range baseline {
		if counts[file] < allowed {
			advisories = append(advisories,
				fmt.Sprintf("%s: now %d, baseline %d — tighten baseline.txt to lock in the reduction", file, counts[file], allowed))
		}
	}

	sort.Strings(regressions)
	sort.Strings(advisories)

	for _, a := range advisories {
		fmt.Printf("advisory: %s\n", a)
	}

	if len(regressions) > 0 {
		for _, r := range regressions {
			fmt.Println(r)
		}
		fmt.Printf("\nFound %d file(s) with new direct error responses (D23).\n", len(regressions))
		fmt.Println("Use httperr.ResponseErrorL(c, code, params, details) or c.RenderError(spec) instead of")
		fmt.Println("c.AbortWithStatusJSON / c.AbortWithStatus. If a new direct response is genuinely")
		fmt.Println("exempt (health/debug/metrics), raise the file's count in " + baselineRelPath + " with a comment.")
		os.Exit(1)
	}

	fmt.Printf("OK: no new direct error responses (%d baseline file(s) tracked).\n", len(baseline))
}

// countSites walks the roots and returns a map of relative-slash file path →
// number of AbortWithStatusJSON / AbortWithStatus call expressions.
func countSites(roots []string) (map[string]int, error) {
	fset := token.NewFileSet()
	counts := map[string]int{}

	for _, root := range roots {
		walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if name := info.Name(); name == "vendor" || name == "tools" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// go build / go vet is the canonical syntax gate; skip
				// unparseable files rather than failing the lint on them.
				return nil
			}
			n := 0
			ast.Inspect(f, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel == nil {
					return true
				}
				if sel.Sel.Name == "AbortWithStatusJSON" || sel.Sel.Name == "AbortWithStatus" {
					n++
				}
				return true
			})
			if n > 0 {
				counts[filepath.ToSlash(path)] = n
			}
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk %q: %w", root, walkErr)
		}
	}
	return counts, nil
}

// loadBaseline parses baseline.txt. Format: one "<count> <path>" per line;
// blank lines and lines starting with '#' are ignored.
func loadBaseline(path string) (map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	baseline := map[string]int{}
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		// Strip a trailing inline comment so rows may carry annotations, e.g.
		//   4 modules/webhook/github.go  # EXEMPT: external GH webhook
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// Require at least "<count> <path>"; tolerate trailing tokens (e.g. a
		// bare EXEMPT marker) so the format can grow annotations without
		// turning a documentation convention into a CI footgun (PR #193 review).
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed baseline line %d: %q (want '<count> <path>')", lineNo, line)
		}
		n, convErr := strconv.Atoi(fields[0])
		if convErr != nil {
			return nil, fmt.Errorf("malformed count on baseline line %d: %q", lineNo, fields[0])
		}
		baseline[filepath.ToSlash(fields[1])] = n
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return baseline, nil
}
