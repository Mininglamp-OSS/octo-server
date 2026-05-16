//go:build ignore

// Sidecar: rewrites the pre-seeded gorp_migrations IDs in init-db.sql using
// mapping.json produced by the main rename tool.
//
// As of the docker/octo + docker/tsdd retirement, the canonical
// init-db.sql now lives in `Mininglamp-OSS/octo-deployment` (under
// `docker/scripts/`; exact filename can vary across branches). The
// `--initdb` flag is REQUIRED — no default is supplied because any
// hard-coded sibling path is environment-specific and silently rots
// when the upstream layout changes. Pass it explicitly:
//
//   go run tools/migrate-rename/rewrite_initdb.go \
//     --mapping tools/migrate-rename/mapping.json \
//     --initdb  ../octo-deployment/docker/scripts/init-db.sql
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
)

func main() {
	mappingPath := flag.String("mapping", "tools/migrate-rename/mapping.json", "")
	// `--initdb` is intentionally required (no default). The canonical
	// init-db.sql lives in the sibling `octo-deployment` checkout and
	// its exact path is environment-specific, so we make the caller
	// pass it explicitly rather than fail with a misleading
	// "no such file or directory" against a stale hard-coded path.
	initdbPath := flag.String("initdb", "", "REQUIRED: path to init-db.sql in your octo-deployment checkout")
	flag.Parse()

	if *initdbPath == "" {
		fmt.Fprintln(os.Stderr, "FATAL: --initdb is required (path to init-db.sql in octo-deployment checkout)")
		fmt.Fprintln(os.Stderr, "example:")
		fmt.Fprintln(os.Stderr, "  go run tools/migrate-rename/rewrite_initdb.go \\")
		fmt.Fprintln(os.Stderr, "    --mapping tools/migrate-rename/mapping.json \\")
		fmt.Fprintln(os.Stderr, "    --initdb  ../octo-deployment/docker/scripts/init-db.sql")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*mappingPath)
	must(err)
	var rep struct {
		Mapping map[string]string `json:"mapping"`
	}
	must(json.Unmarshal(raw, &rep))

	body, err := os.ReadFile(*initdbPath)
	must(err)
	src := string(body)

	// Only touch tokens that appear inside an INSERT INTO `gorp_migrations`
	// VALUES('<id>', ...) — never blanket-replace, since the file also
	// contains schema text and CHECK constraints we shouldn't touch.
	line := regexp.MustCompile("(?m)^(INSERT INTO `gorp_migrations` VALUES\\s*\\(\\s*')([^']+)('.*)$")

	rewritten := 0
	unknown := []string{}
	out := line.ReplaceAllStringFunc(src, func(s string) string {
		m := line.FindStringSubmatch(s)
		old := m[2]
		new, ok := rep.Mapping[old]
		if !ok {
			unknown = append(unknown, old)
			return s
		}
		rewritten++
		return m[1] + new + m[3]
	})

	if len(unknown) > 0 {
		fmt.Fprintf(os.Stderr, "ERROR: %d gorp_migrations rows reference unknown SQL files (missing from mapping.json):\n", len(unknown))
		for _, u := range unknown {
			fmt.Fprintf(os.Stderr, "  %s\n", u)
		}
		os.Exit(1)
	}

	must(os.WriteFile(*initdbPath, []byte(out), 0o644))
	fmt.Fprintf(os.Stderr, "rewrote %d gorp_migrations IDs in %s\n", rewritten, *initdbPath)
	fmt.Fprintf(os.Stderr, "sample diff (first 3):\n")
	for i, l := range firstNDiffs(src, out, 3) {
		fmt.Fprintf(os.Stderr, "  %d: %s\n", i+1, l)
	}
	_ = strings.Builder{}
}

func firstNDiffs(a, b string, n int) []string {
	var out []string
	as := strings.Split(a, "\n")
	bs := strings.Split(b, "\n")
	for i := range as {
		if i >= len(bs) || as[i] == bs[i] {
			continue
		}
		out = append(out, fmt.Sprintf("- %s\n     + %s", as[i], bs[i]))
		if len(out) >= n {
			break
		}
	}
	return out
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(1)
	}
}
