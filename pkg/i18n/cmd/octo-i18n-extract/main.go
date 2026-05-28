// Command octo-i18n-extract is the source-of-truth AST extractor for the
// i18n message marker pipeline (D18 in `i18n 最终方案.md` v7.1).
//
// What it does:
//
//  1. Walk one or more Go source roots (default: pkg/i18n/codes pkg/errcode)
//     and parse every `Register(Code{...})` / `register(codes.Code{...})`
//     composite literal to recover (ID, DefaultMessage) pairs.
//  2. Route markers by ID prefix:
//     - err.shared.* → <shared-dir>/active.en-US.toml
//     - err.server.* / msg.* → <server-dir>/active.en-US.toml
//  3. Cross-check against the registered set obtained by importing the codes
//     and errcode packages. If `len(AST markers) != len(codes.All())` or the
//     ID sets differ, exit non-zero — this is the 100% recall guarantee that
//     Phase 2 module PRs depend on (TODOS §0.8).
//  4. With `-check`, write nothing; instead report whether any marker file
//     on disk diverges from what would be written (CI use).
//
// Exit codes: 0 (clean), 1 (extraction error), 2 (recall mismatch),
// 3 (-check found diff).
//
// Typical invocations:
//
//	go run ./pkg/i18n/cmd/octo-i18n-extract            # rewrite markers
//	go run ./pkg/i18n/cmd/octo-i18n-extract -check     # CI verification
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	// Side-effect imports: these packages register their Codes in init().
	// Listing them explicitly is what makes codes.All() complete; do NOT
	// rely on transitive imports — extractor is a tool binary and must
	// pin its own truth set.
	_ "github.com/Mininglamp-OSS/octo-server/pkg/errcode"
)

const (
	exitOK             = 0
	exitExtractError   = 1
	exitRecallMismatch = 2
	exitCheckDiff      = 3
)

func main() {
	var (
		sharedDir string
		serverDir string
		roots     stringsFlag
		check     bool
	)
	flag.StringVar(&sharedDir, "shared-dir", "tools/i18nmarkers/shared", "output directory for err.shared.* markers")
	flag.StringVar(&serverDir, "server-dir", "tools/i18nmarkers/server", "output directory for err.server.* / msg.* markers")
	flag.Var(&roots, "root", "Go source root to scan (repeatable; default: pkg/i18n/codes, pkg/errcode)")
	flag.BoolVar(&check, "check", false, "verify on-disk markers match expected; exit 3 on diff, write nothing")
	flag.Parse()

	if len(roots) == 0 {
		roots = stringsFlag{"pkg/i18n/codes", "pkg/errcode"}
	}

	markers, err := collectMarkers(roots)
	if err != nil {
		fmt.Fprintf(os.Stderr, "extract: %v\n", err)
		os.Exit(exitExtractError)
	}
	registered := make([]string, 0, len(codes.All()))
	for _, c := range codes.All() {
		registered = append(registered, c.ID)
	}
	if err := verifyRecall(markers, registered); err != nil {
		fmt.Fprintf(os.Stderr, "recall: %v\n", err)
		os.Exit(exitRecallMismatch)
	}

	groups, err := GroupByPrefix(markers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "group: %v\n", err)
		os.Exit(exitExtractError)
	}

	// Iterate in a fixed order so log output (and `-check` diff hints) is
	// deterministic across runs — Go map iteration would otherwise swap the
	// "wrote/unchanged ... → ..." lines, hurting log diffing.
	targetOrder := []string{"shared", "server"}
	targets := map[string]string{
		"shared": joinPath(sharedDir, "active.en-US.toml"),
		"server": joinPath(serverDir, "active.en-US.toml"),
	}

	exit := exitOK
	for _, group := range targetOrder {
		path := targets[group]
		ms := groups[group]
		if check {
			diff, err := checkOnDisk(path, ms)
			if err != nil {
				fmt.Fprintf(os.Stderr, "check %s: %v\n", path, err)
				os.Exit(exitExtractError)
			}
			if diff {
				fmt.Fprintf(os.Stderr, "diff: %s is stale (re-run `make i18n-extract`)\n", path)
				exit = exitCheckDiff
			}
			continue
		}
		changed, err := WriteMarkerFile(path, ms)
		if err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
			os.Exit(exitExtractError)
		}
		if changed {
			fmt.Printf("wrote %d markers → %s\n", len(ms), path)
		} else {
			fmt.Printf("unchanged %d markers → %s\n", len(ms), path)
		}
	}
	os.Exit(exit)
}

// collectMarkers walks each root in turn and merges markers, checking for
// duplicate IDs across roots (a Code registered in two packages is always a
// bug — `codes.Register` would panic at runtime, but during static extraction
// the panic never fires).
func collectMarkers(roots []string) ([]Marker, error) {
	var all []Marker
	seen := make(map[string]string)
	for _, r := range roots {
		ms, err := ExtractFromDir(r)
		if err != nil {
			return nil, fmt.Errorf("root %s: %w", r, err)
		}
		for _, m := range ms {
			if prev, ok := seen[m.ID]; ok {
				return nil, fmt.Errorf("duplicate Code.ID %q across roots: %s and %s", m.ID, prev, m.Pos)
			}
			seen[m.ID] = m.Pos
			all = append(all, m)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, nil
}

// verifyRecall is the 100% guarantee: the AST-extracted set must match the
// runtime-registered set exactly. Diffs surface as a sorted list of missing
// and extra IDs to make CI failures actionable.
//
// `registered` is passed in as a slice (rather than calling codes.All()
// directly) so this function is unit-testable with synthetic inputs — the
// recall guarantee is the load-bearing safety property of this binary and
// deserves direct coverage independent of the live registry's state.
func verifyRecall(markers []Marker, registered []string) error {
	astIDs := make(map[string]struct{}, len(markers))
	for _, m := range markers {
		astIDs[m.ID] = struct{}{}
	}
	regIDs := make(map[string]struct{}, len(registered))
	for _, id := range registered {
		regIDs[id] = struct{}{}
	}

	var missing, extra []string
	for id := range regIDs {
		if _, ok := astIDs[id]; !ok {
			missing = append(missing, id)
		}
	}
	for id := range astIDs {
		if _, ok := regIDs[id]; !ok {
			extra = append(extra, id)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Errorf(
		"AST vs codes.All() mismatch: ast=%d registered=%d\n  missing-from-AST:   %v\n  extra-only-in-AST:  %v",
		len(astIDs), len(regIDs), missing, extra,
	)
}

// checkOnDisk returns true when the file at path differs from the rendering
// of ms (or does not exist). It never writes.
func checkOnDisk(path string, ms []Marker) (bool, error) {
	want := RenderTOML(ms)
	got, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return string(got) != want, nil
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// stringsFlag is a tiny -repeatable flag implementation; standard library has
// no built-in for `flag.Var` slice-of-string accumulation.
type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }
