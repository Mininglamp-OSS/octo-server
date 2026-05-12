// Command migrate-rename rebuilds migration filenames as a single global
// timestamp-prefixed sequence so sql-migrate's numeric-prefix branch sorts
// them strictly by execution order rather than alphabetically.
//
// Run from the repo root:
//
//	go run ./tools/migrate-rename            # dry-run, writes mapping.json
//	go run ./tools/migrate-rename --apply    # also git mv the files
//
// The tool:
//  1. Walks modules/<m>/sql/*.sql and parses each file's DDL/DML targets.
//  2. Builds a table-ownership map: the first migration that CREATEs a
//     table owns it; later ALTER/UPDATE/INSERT against that table from a
//     different module become cross-module dependencies.
//  3. Topologically sorts migrations (DFS, cycle-detecting).
//  4. Sanity-checks that the resulting order is non-decreasing in the
//     migration's original YYYYMMDD field. A regression means someone
//     wrote an ALTER against a table that was created *later* in wall
//     time — we surface that and exit non-zero rather than silently
//     re-time it.
//  5. Assigns each file a 14-digit timestamp YYYYMMDD<NNNNNN> where the
//     date part is its original YYYYMMDD and NNNNNN is its 1-based
//     position within that calendar day in the final order.
//  6. Emits mapping.json (old → new) and, with --apply, runs git mv.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Types

type migration struct {
	Path     string   // absolute path
	Rel      string   // modules/user/sql/user-20191106-01.sql
	Filename string   // user-20191106-01.sql
	Module   string   // user
	Date     string   // 20191106 (8-digit YYYYMMDD)
	NN       string   // 01 (or "" for the one bot_api file that lacks a -NN)
	Creates  []string // tables this migration CREATEs
	Touches  []string // tables this migration ALTER/UPDATE/INSERTs (not CREATE)
	// AlreadyTimestamped is true when the source filename already matches
	// the post-rename convention. The planner preserves the filename for
	// these so reruns are byte-stable.
	AlreadyTimestamped bool
	// Filled in by the planner:
	NewName string
}

// ---------------------------------------------------------------------------
// Regexes

var (
	// Filename → module/date/nn. Two historical conventions exist:
	//   <module>-<YYYYMMDD>-<NN>.sql           (most modules)
	//   <module>_<YYYYMMDD>-<NN>.sql           (group_*.sql)
	//   <module>-<YYYYMMDD>.sql                (bot_api-20260505.sql)
	// We treat the separator after the module name flexibly.
	// Historical conventions (encountered in practice):
	//   <module>-<YYYYMMDD>-<NN>.sql            (most modules)
	//   <module>_<YYYYMMDD>-<NN>.sql            (group_*.sql)
	//   <module>-<YYYYMMDD>.sql                 (bot_api-20260505.sql)
	//   <module>-<YYYYMMDDHHMM>-<NN>.sql        (report-20201222... 12-digit)
	// We accept 8- or 12-digit date prefixes and use only the YYYYMMDD slice
	// for the new timestamp; the HHMM tail (if any) just contributes to the
	// stable secondary sort via NN.
	reFilename = regexp.MustCompile(`^(?P<module>[a-z][a-z_]*?)[-_](?P<date>\d{8}(?:\d{4})?)(?:-(?P<nn>\d{2}))?\.sql$`)

	// Post-rename convention used after this tool has been applied once:
	//   <YYYYMMDD><NNNNNN>_<module>(_legacy<NN>)?(_<desc>)?.sql
	// Re-parsing such a filename yields the same (date, nn, module) it had
	// before the rename, so the tool's output is deterministic across reruns.
	// Without this branch, re-running on the post-rename tree would fail at
	// collection ("filename does not match …") and the tool wouldn't be
	// idempotent.
	reFilenameNew = regexp.MustCompile(`^(?P<date>\d{8})(?P<seq>\d{6})_(?P<module>[a-z][a-z_0-9]*?)(?:_legacy(?P<nn>\d{2}))?(?:_[a-z0-9_]+)?\.sql$`)

	// DDL/DML targets. We strip backticks and trailing punctuation in code.
	// Match CREATE TABLE [IF NOT EXISTS] `t` or t (no schema qualifier in this repo).
	reCreate = regexp.MustCompile(`(?i)create\s+table(?:\s+if\s+not\s+exists)?\s+` + tableTok)
	reAlter  = regexp.MustCompile(`(?i)alter\s+table\s+` + tableTok)
	reInsert = regexp.MustCompile(`(?i)insert(?:\s+ignore)?\s+into\s+` + tableTok)
	reUpdate = regexp.MustCompile(`(?i)update\s+(?:ignore\s+)?` + tableTok)

	reComment = regexp.MustCompile(`(?m)--.*$|/\*[\s\S]*?\*/`)

	// Quoted string literals: 'anything' or "anything", with simple
	// backslash-escape tolerance. We strip these because COMMENT '...'
	// payloads contain English text like "last update time" that the
	// reUpdate regex would otherwise misread as a table reference.
	reSingleQuoted = regexp.MustCompile(`'(?:\\.|[^'\\])*'`)
	reDoubleQuoted = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)

	// MySQL UPDATE clauses that are not statement-level UPDATEs:
	//   ON DUPLICATE KEY UPDATE col = ...
	//   <col> TIMESTAMP ... ON UPDATE CURRENT_TIMESTAMP
	// We rewrite these to a sentinel so reUpdate cannot match them.
	reOnDupKeyUpdate = regexp.MustCompile(`(?i)on\s+duplicate\s+key\s+update`)
	reOnUpdateClause = regexp.MustCompile(`(?i)on\s+update\s+(?:current_timestamp|now)(?:\s*\(\s*\d*\s*\))?`)
)

const tableTok = "`?(?P<table>[A-Za-z_][A-Za-z0-9_]*)`?"

// Reserved SQL keywords we never want to confuse with a real table name.
// e.g. `UPDATE` inside `ON DUPLICATE KEY UPDATE col=...` would match reUpdate
// with table="col" — we filter that downstream by looking at the regex match
// context. The simplest robust filter: skip tokens that are SQL keywords.
var sqlKeywordBlacklist = map[string]bool{
	"set": true, "where": true, "values": true, "select": true,
	"on": true, "duplicate": true, "key": true,
}

// ---------------------------------------------------------------------------
// Walk + parse

func collectMigrations(root string) ([]*migration, error) {
	var out []*migration
	modulesDir := filepath.Join(root, "modules")
	entries, err := os.ReadDir(modulesDir)
	if err != nil {
		return nil, err
	}
	for _, mod := range entries {
		if !mod.IsDir() {
			continue
		}
		sqlDir := filepath.Join(modulesDir, mod.Name(), "sql")
		st, err := os.Stat(sqlDir)
		if err != nil || !st.IsDir() {
			continue
		}
		files, err := os.ReadDir(sqlDir)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".sql") {
				continue
			}
			m, err := parseMigration(root, sqlDir, f.Name())
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", mod.Name(), f.Name(), err)
			}
			out = append(out, m)
		}
	}
	return out, nil
}

func parseMigration(root, dir, name string) (*migration, error) {
	matches := reFilename.FindStringSubmatch(name)
	usedNew := false
	if matches == nil {
		// Try the post-rename layout (idempotent rerun).
		matches = reFilenameNew.FindStringSubmatch(name)
		if matches == nil {
			return nil, fmt.Errorf("filename matches neither <module>[-_]<YYYYMMDD>[-<NN>].sql nor <YYYYMMDD><NNNNNN>_<module>...sql")
		}
		usedNew = true
	}
	var mod, rawDate, nn string
	if usedNew {
		mod = matches[reFilenameNew.SubexpIndex("module")]
		rawDate = matches[reFilenameNew.SubexpIndex("date")]
		nn = matches[reFilenameNew.SubexpIndex("nn")]
		if nn == "" {
			// Derive a stable NN-substitute from the per-day sequence so that
			// the (date, nn, module) sort key still partitions cleanly.
			nn = matches[reFilenameNew.SubexpIndex("seq")][4:6]
		}
	} else {
		mod = matches[reFilename.SubexpIndex("module")]
		rawDate = matches[reFilename.SubexpIndex("date")]
		nn = matches[reFilename.SubexpIndex("nn")]
	}
	// Collapse 12-digit YYYYMMDDHHMM to its YYYYMMDD prefix for ordering.
	// The HHMM tail is preserved implicitly by NN (and by ordering within
	// the same date — there are only two 12-digit files and they live in
	// different modules, so collision is impossible).
	date := rawDate[:8]
	if len(rawDate) > 8 && nn == "" {
		// Tie-break key for the rare bot_api-20260505.sql case where NN is
		// absent: use the HHMM remainder so two same-date NN-less files
		// would still get a stable order.
		nn = rawDate[8:]
	}

	path := filepath.Join(dir, name)
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	stripped := preprocess(string(body))

	creates := extractTables(stripped, reCreate)
	touches := unionMinusCreates(
		extractTables(stripped, reAlter),
		extractTables(stripped, reInsert),
		extractTables(stripped, reUpdate),
		creates,
	)

	rel, _ := filepath.Rel(root, path)
	return &migration{
		Path:               path,
		Rel:                rel,
		Filename:           name,
		Module:             mod,
		Date:               date,
		NN:                 nn,
		Creates:            creates,
		Touches:            touches,
		AlreadyTimestamped: usedNew,
	}, nil
}

// preprocess strips noise that the DDL/DML regexes would otherwise misread.
// Order matters: comments and quoted strings first (they can contain SQL
// keywords as English prose), then the two non-statement UPDATE constructs.
func preprocess(body string) string {
	body = reComment.ReplaceAllString(body, " ")
	body = reSingleQuoted.ReplaceAllString(body, "''")
	body = reDoubleQuoted.ReplaceAllString(body, `""`)
	body = reOnDupKeyUpdate.ReplaceAllString(body, " __ON_DUP_KEY__ ")
	body = reOnUpdateClause.ReplaceAllString(body, " __ON_UPDATE_TS__ ")
	return body
}

func extractTables(body string, re *regexp.Regexp) []string {
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		t := strings.ToLower(m[re.SubexpIndex("table")])
		if t == "" || sqlKeywordBlacklist[t] {
			continue
		}
		seen[t] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func unionMinusCreates(a, b, c []string, creates []string) []string {
	createSet := map[string]bool{}
	for _, t := range creates {
		createSet[t] = true
	}
	seen := map[string]bool{}
	for _, ts := range [][]string{a, b, c} {
		for _, t := range ts {
			if !createSet[t] {
				seen[t] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Ownership + dependency graph

// tableOwners returns a map table → first migration (by date,nn,module) that
// CREATEs it. Migrations that ALTER/UPDATE/INSERT a table they didn't create
// depend on that owner.
func tableOwners(ms []*migration) map[string]*migration {
	owners := map[string]*migration{}
	sorted := append([]*migration(nil), ms...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return lessByDate(sorted[i], sorted[j])
	})
	for _, m := range sorted {
		for _, t := range m.Creates {
			if _, ok := owners[t]; !ok {
				owners[t] = m
			}
		}
	}
	return owners
}

// lessByDate is the stable secondary key for topo's "ready set". Same-date
// migrations group by module first (so multi-revision authoring like
// botfather-20260318-01 / -02 stays contiguous), then by NN, then by name.
func lessByDate(a, b *migration) bool {
	if a.Date != b.Date {
		return a.Date < b.Date
	}
	if a.Module != b.Module {
		return a.Module < b.Module
	}
	if a.NN != b.NN {
		return a.NN < b.NN
	}
	return a.Filename < b.Filename
}

// buildEdges: m → m' means m depends on m'. We add an edge when m touches a
// table owned by a *different* migration (could be same module — e.g.
// botfather-20260417 ALTERing the robot table created in robot-20210926-01).
func buildEdges(ms []*migration, owners map[string]*migration) (deps map[string][]string, orphan map[string][]string) {
	byFilename := map[string]*migration{}
	for _, m := range ms {
		byFilename[m.Filename] = m
	}
	deps = map[string][]string{}
	orphan = map[string][]string{}
	for _, m := range ms {
		seen := map[string]bool{}
		for _, t := range m.Touches {
			owner, ok := owners[t]
			if !ok {
				orphan[m.Filename] = append(orphan[m.Filename], t)
				continue
			}
			if owner.Filename == m.Filename {
				continue
			}
			if seen[owner.Filename] {
				continue
			}
			seen[owner.Filename] = true
			deps[m.Filename] = append(deps[m.Filename], owner.Filename)
		}
	}
	return
}

// ---------------------------------------------------------------------------
// Topological sort

// topoSort returns a stable order honouring deps:
//   - all of m's dependencies appear before m
//   - among migrations with no remaining dep, sort by (date, nn, module)
func topoSort(ms []*migration, deps map[string][]string) ([]*migration, error) {
	byFn := map[string]*migration{}
	for _, m := range ms {
		byFn[m.Filename] = m
	}
	// incoming-edge count (how many deps each migration still has unresolved)
	indeg := map[string]int{}
	// outgoing: dep → migrations depending on it
	revAdj := map[string][]string{}
	for _, m := range ms {
		indeg[m.Filename] = 0
	}
	for child, parents := range deps {
		for _, p := range parents {
			indeg[child]++
			revAdj[p] = append(revAdj[p], child)
		}
	}

	// Min-heap-equivalent: at every step pick the minimum (date, nn, module)
	// among nodes with indeg == 0. We use a slice resorted each step — n is
	// ~124 so simplicity > asymptotics.
	var ready []*migration
	for fn, d := range indeg {
		if d == 0 {
			ready = append(ready, byFn[fn])
		}
	}

	var order []*migration
	for len(ready) > 0 {
		sort.SliceStable(ready, func(i, j int) bool { return lessByDate(ready[i], ready[j]) })
		next := ready[0]
		ready = ready[1:]
		order = append(order, next)
		for _, child := range revAdj[next.Filename] {
			indeg[child]--
			if indeg[child] == 0 {
				ready = append(ready, byFn[child])
			}
		}
	}
	if len(order) != len(ms) {
		// cycle — emit the remaining nodes for the operator
		var remaining []string
		for fn, d := range indeg {
			if d > 0 {
				remaining = append(remaining, fmt.Sprintf("%s (indeg=%d, deps=%v)", fn, d, deps[fn]))
			}
		}
		sort.Strings(remaining)
		return nil, fmt.Errorf("cycle detected among %d migrations:\n  %s", len(remaining), strings.Join(remaining, "\n  "))
	}
	return order, nil
}

// ---------------------------------------------------------------------------
// Timestamp assignment

// assignTimestamps walks the topo-sorted slice, asserts the original date is
// non-decreasing, and writes m.NewName = <YYYYMMDD><NNNNNN>_<module>_<desc>.sql
// where NNNNNN is the 1-based position of this migration within its date.
//
// "desc" is derived from the legacy NN — we keep that to preserve a hint of
// the original filename so reviewers can correlate.
func assignTimestamps(order []*migration) error {
	var prevDate string
	dayCounter := map[string]int{}
	var violations []string
	for _, m := range order {
		if prevDate != "" && m.Date < prevDate {
			violations = append(violations, fmt.Sprintf("  %s (date=%s) follows date=%s in topo order", m.Filename, m.Date, prevDate))
		}
		dayCounter[m.Date]++
		seq := dayCounter[m.Date]
		if m.AlreadyTimestamped {
			// Preserve the existing post-rename filename — the tool is
			// idempotent on a tree it has already been applied to.
			m.NewName = m.Filename
		} else {
			desc := m.Module
			if m.NN != "" {
				desc = fmt.Sprintf("%s_legacy%s", m.Module, m.NN)
			}
			m.NewName = fmt.Sprintf("%s%06d_%s.sql", m.Date, seq, desc)
		}
		_ = seq
		if m.Date > prevDate {
			prevDate = m.Date
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("topological order violates calendar order — manual intervention required:\n%s", strings.Join(violations, "\n"))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Output

type report struct {
	Total       int               `json:"total"`
	Mapping     map[string]string `json:"mapping"` // old filename → new filename
	OrphanTbls  map[string][]string `json:"orphan_tables_per_file,omitempty"` // file → tables it touches but no migration creates
	CrossModule []string          `json:"cross_module_examples,omitempty"`
}

func writeMapping(order []*migration, orphan map[string][]string, owners map[string]*migration, out string) error {
	// Start from any existing mapping.json so reruns are append-only — once
	// a rename has been recorded it stays in the historical map, even after
	// the source tree no longer carries the old filename. The map is the
	// authoritative source for the runtime ID-rewrite shim, so losing an
	// entry would break upgrades of older deployments.
	mapping := map[string]string{}
	if existing, err := readExistingMapping(out); err == nil {
		for k, v := range existing {
			mapping[k] = v
		}
	}
	for _, m := range order {
		if m.Filename == m.NewName {
			// Skip self-pairs from this run — they're either no-op renames
			// or post-rename files we deliberately preserved.
			continue
		}
		mapping[m.Filename] = m.NewName
	}
	// Pick a handful of cross-module dep examples for the report header.
	var examples []string
	count := 0
	for _, m := range order {
		if count >= 20 {
			break
		}
		for _, t := range m.Touches {
			if owner, ok := owners[t]; ok && owner.Module != m.Module {
				examples = append(examples, fmt.Sprintf("%s (mod=%s) → %s (owner=%s, table=%s)", m.Filename, m.Module, owner.Filename, owner.Module, t))
				count++
				if count >= 20 {
					break
				}
			}
		}
	}
	rep := report{
		Total:       len(order),
		Mapping:     mapping,
		OrphanTbls:  orphan,
		CrossModule: examples,
	}
	buf, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, buf, 0o644)
}

func readExistingMapping(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, err
	}
	return rep.Mapping, nil
}

func applyRenames(order []*migration, root string) error {
	for _, m := range order {
		oldRel, _ := filepath.Rel(root, m.Path)
		newPath := filepath.Join(filepath.Dir(m.Path), m.NewName)
		newRel, _ := filepath.Rel(root, newPath)
		if oldRel == newRel {
			continue
		}
		cmd := exec.Command("git", "mv", oldRel, newRel)
		cmd.Dir = root
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git mv %s %s: %w", oldRel, newRel, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// main

func main() {
	var (
		apply  = flag.Bool("apply", false, "execute git mv after dry-run analysis")
		out    = flag.String("out", "tools/migrate-rename/mapping.json", "where to write the dry-run report")
		rootFl = flag.String("root", ".", "repo root (containing modules/)")
	)
	flag.Parse()
	root, err := filepath.Abs(*rootFl)
	if err != nil {
		die(err)
	}

	ms, err := collectMigrations(root)
	if err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "collected %d migrations\n", len(ms))

	owners := tableOwners(ms)
	deps, orphan := buildEdges(ms, owners)

	order, err := topoSort(ms, deps)
	if err != nil {
		die(err)
	}

	if err := assignTimestamps(order); err != nil {
		die(err)
	}

	if err := writeMapping(order, orphan, owners, filepath.Join(root, *out)); err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "dry-run report written to %s\n", *out)

	// Surface the head + tail so a human can sanity-check
	fmt.Fprintln(os.Stderr, "\nFirst 5:")
	for _, m := range order[:min(5, len(order))] {
		fmt.Fprintf(os.Stderr, "  %s  →  %s\n", m.Filename, m.NewName)
	}
	fmt.Fprintln(os.Stderr, "\nLast 5:")
	tail := order[max(0, len(order)-5):]
	for _, m := range tail {
		fmt.Fprintf(os.Stderr, "  %s  →  %s\n", m.Filename, m.NewName)
	}
	if len(orphan) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d migrations touch tables with no CREATE owner (probably external/IM tables; see mapping.json)\n", len(orphan))
	}

	if *apply {
		fmt.Fprintln(os.Stderr, "\n--apply set: running git mv …")
		if err := applyRenames(order, root); err != nil {
			die(err)
		}
		fmt.Fprintln(os.Stderr, "done. Review with `git status` and commit.")
	}
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
	os.Exit(1)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
