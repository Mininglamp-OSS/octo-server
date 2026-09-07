package project

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Source guards for the provisioning slice.
//
// Each one pins a property that no behavioural test can reach, and each is written
// so that it fails for the reason it claims — the failure mode these guards have
// historically had in this module is passing vacuously (a scope that covers one call
// site, or a match that a rename empties out). Two habits address that here: every
// guard asserts a non-zero lower bound on what it examined, and every hand-listed
// name is DERIVED from the source instead of typed out.

// handlerMarker identifies a file that contains a request handler. Matched on the
// gin/wkhttp handler signature rather than on a filename convention, because the
// convention is what a new file gets wrong.
const handlerMarker = "(c *wkhttp.Context)"

// provisioningSourceFiles returns the package's non-test .go files partitioned by
// whether they contain a request handler.
func provisioningSourceFiles(t *testing.T) (handlers, others []string) {
	t.Helper()
	for _, f := range moduleSourceFiles(t) {
		if strings.Contains(readStripped(t, f), handlerMarker) {
			handlers = append(handlers, f)
			continue
		}
		others = append(others, f)
	}
	if len(handlers) == 0 {
		t.Fatal("no file in this package matched the handler marker; the confinement guards below " +
			"would pass vacuously. If the handler signature changed, update handlerMarker.")
	}
	return handlers, others
}

// TestProvisioningClientIsConfinedToTheWorker is the outbound-confinement rule.
//
// A request handler that reaches fleet or drive makes a user request depend on
// another service being up, which is why the brief puts the HTTP client behind a
// package boundary. This asserts the boundary is respected from this side: no file
// carrying a handler may name the outbound package or the client field, and the
// ensure call itself lives in exactly one file.
func TestProvisioningClientIsConfinedToTheWorker(t *testing.T) {
	handlers, _ := provisioningSourceFiles(t)
	for _, f := range handlers {
		cleaned := readStripped(t, f)
		for _, banned := range []string{"projectprovision.NewClient", "provisionClient.", ".Ensure("} {
			if strings.Contains(cleaned, banned) {
				t.Errorf("modules/project/%s carries a request handler and reaches the provisioning "+
					"client (%s). A handler that calls fleet or drive makes a user request depend on "+
					"another service being up; the call belongs in the worker.", f, banned)
			}
		}
	}

	// The dependency itself, not just the call: a handler file must not even IMPORT the
	// outbound package. provisionEnsurer exists so this can be an unconditional rule —
	// the struct field that used to force api.go to import it is now typed on a local
	// interface (see provisioning_worker.go).
	root := repoRootForGuard(t)
	importers := grepPackageForImport(t, filepath.Join(root, "modules/project"), "internal/projectprovision")
	for _, f := range importers {
		for _, h := range handlers {
			if f == h {
				t.Errorf("modules/project/%s carries a request handler and imports internal/projectprovision", f)
			}
		}
	}
	if len(importers) == 0 {
		t.Error("nothing in modules/project imports internal/projectprovision; either the worker moved " +
			"or this guard has lost its subject")
	}

	// modules/user is named explicitly by the brief's acceptance item, because verify is
	// the endpoint every subsystem fronts and would be the natural place for someone to
	// "just check whether the container exists" — the exact shape that makes a user
	// request depend on fleet being up.
	if hits := grepPackageForImport(t, filepath.Join(root, "modules/user"), "internal/projectprovision"); len(hits) > 0 {
		t.Errorf("modules/user imports internal/projectprovision from %v; no request handler may reach "+
			"fleet or drive", hits)
	}

	// And the ensure call exists in exactly one file, so "the worker" is a single
	// place rather than a habit.
	var callers []string
	for _, f := range moduleSourceFiles(t) {
		if strings.Contains(readStripped(t, f), ".Ensure(") {
			callers = append(callers, f)
		}
	}
	sort.Strings(callers)
	if len(callers) != 1 || callers[0] != "provisioning_worker.go" {
		t.Errorf("the subsystem ensure call must be made from provisioning_worker.go only; found %v", callers)
	}
}

// TestNoHandlerReachesTheProvisioningTable is D12's read-path rule.
//
// `status = ready` records "we called successfully once", not "the container exists
// now" — a container can be deleted or migrated on the subsystem side with nothing
// informing this service. So a read path that gates on this table can lock a feature
// forever with no user-triggerable repair.
//
// The banned identifier set is DERIVED from db_provisioning.go rather than written
// out, so renaming a DAO method cannot silently empty this guard.
func TestNoHandlerReachesTheProvisioningTable(t *testing.T) {
	dao := readStripped(t, "db_provisioning.go")
	methods := regexp.MustCompile(`func \(d \*DB\) (\w+)\(`).FindAllStringSubmatch(dao, -1)
	if len(methods) < 5 {
		t.Fatalf("found only %d *DB methods in db_provisioning.go; the guard needs the real set "+
			"(if the DAO moved, point this test at the new file)", len(methods))
	}
	var names []string
	for _, m := range methods {
		names = append(names, m[1])
	}

	handlers, _ := provisioningSourceFiles(t)
	for _, f := range handlers {
		cleaned := readStripped(t, f)
		if strings.Contains(cleaned, "octo_project_provisioning") {
			t.Errorf("modules/project/%s carries a request handler and names octo_project_provisioning; "+
				"no read path may gate on the provisioning table (D12)", f)
		}
		for _, name := range names {
			if strings.Contains(cleaned, name+"(") {
				t.Errorf("modules/project/%s carries a request handler and calls the provisioning DAO "+
					"method %s; no read path may gate on the provisioning table (D12)", f, name)
			}
		}
	}

	// The table name itself must appear in exactly one non-test file, so the DAO
	// stays the single access point.
	var tableFiles []string
	for _, f := range moduleSourceFiles(t) {
		if strings.Contains(readStripped(t, f), "octo_project_provisioning") {
			tableFiles = append(tableFiles, f)
		}
	}
	sort.Strings(tableFiles)
	if len(tableFiles) != 1 || tableFiles[0] != "db_provisioning.go" {
		t.Errorf("octo_project_provisioning must be reached only from db_provisioning.go; found %v", tableFiles)
	}
}

// TestContainerIDHasOneProducerAndItIgnoresTheProjectID is the source half of D1.
//
// The behavioural half (TestContainerIDIsNotAFunctionOfProjectID) can only prove that
// the ids it observed were not derived. This proves there is exactly one producer and
// that the producer cannot see a project id.
func TestContainerIDHasOneProducerAndItIgnoresTheProjectID(t *testing.T) {
	src := readStripped(t, "provisioning.go")
	start := strings.Index(src, "func newContainerID(")
	if start < 0 {
		t.Fatal("newContainerID not found in provisioning.go; this guard has lost its target")
	}
	// readStripped JOINS the physical lines, so a function body ends at the next
	// `func ` in the flattened text rather than at a newline. Getting this wrong is
	// how the first version of this guard "examined" the whole rest of the file.
	body := src[start+len("func newContainerID("):]
	// Cut at the next TOP-LEVEL declaration of any kind, not just the next `func `.
	// The first version cut on `func ` alone and therefore swallowed the
	// provisioningJob struct that sits between them — whose db tags are literally
	// `project_id` and `space_id`, so the guard reported a leak that was not there.
	// A guard that fires for the wrong reason is as useless as one that never fires.
	for _, boundary := range []string{"func ", "type ", "const ", "var "} {
		if next := strings.Index(body, boundary); next >= 0 {
			body = body[:next]
		}
	}
	if !strings.Contains(body, "containerIDPrefix") {
		t.Fatal("the extracted newContainerID body does not look like the function; " +
			"the flattening or the extraction has drifted and this guard is examining the wrong text")
	}
	for _, banned := range []string{"projectID", "ProjectID", "project_id", "SpaceID", "space_id"} {
		if strings.Contains(body, banned) {
			t.Errorf("newContainerID references %q: a container id derived from tenant identifiers is "+
				"guessable by anyone who can list projects, which is the chain brief P-3 describes", banned)
		}
	}
	if !strings.Contains(body, "rand.Read") {
		t.Error("newContainerID no longer draws from crypto/rand; the id is a capability until R2/R3 land")
	}

	// The two id prefixes are the only literals a container id is built from, and they
	// live with the producer. A prefix appearing elsewhere means a second construction
	// site the guard above cannot see.
	for _, prefix := range []string{`"octows-"`, `"octods-"`} {
		var files []string
		for _, f := range moduleSourceFiles(t) {
			if strings.Contains(readStripped(t, f), prefix) {
				files = append(files, f)
			}
		}
		if len(files) != 1 || files[0] != "provisioning.go" {
			t.Errorf("container id prefix %s appears in %v; it must exist only in provisioning.go, "+
				"next to the single producer", prefix, files)
		}
	}

	// And the producer is called from the enqueue path only, i.e. inside the create
	// transaction, so no code path can mint an id outside a row.
	var callers []string
	for _, f := range moduleSourceFiles(t) {
		if strings.Contains(readStripped(t, f), "newContainerID(") && f != "provisioning.go" {
			callers = append(callers, f)
		}
	}
	sort.Strings(callers)
	if len(callers) != 1 || callers[0] != "db_provisioning.go" {
		t.Errorf("newContainerID must be called only from the enqueue path in db_provisioning.go; found %v", callers)
	}
}

// TestNoPackageOutsideTheProvisioningSliceCanReadAContainerID is the repo-wide half of
// the disclosure rule, and it is what actually discharges the acceptance item's
// "appconfig and verify responses" clause.
//
// A behavioural assertion can only cover the response shapes a test thought to call.
// This covers every response every module produces, by attacking the prerequisite
// instead: a container id can only reach a response if some code READS one, and the only
// place one is stored is octo_project_provisioning. So if no package outside this slice
// names that table — and no package outside it can mint an id, since the two prefixes
// live with the single producer — then no other module's response can carry one.
//
// Whole-repo walk rather than a listed set of packages: the point is that a NEW consumer
// has to come here and think about it, and a listed set would not notice one.
func TestNoPackageOutsideTheProvisioningSliceCanReadAContainerID(t *testing.T) {
	root := repoRootForGuard(t)
	// The slice's own files, by path suffix. internal/projectprovision never names the
	// table (it takes the id as an argument), but it is listed so the guard does not
	// depend on that staying true.
	allowed := map[string]bool{
		filepath.Join("modules", "project", "db_provisioning.go"): true,
		filepath.Join("modules", "project", "provisioning.go"):    true,
	}
	needles := []string{"octo_project_provisioning", `"octows-`, `"octods-`}
	scanned := 0
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".octospec", ".context", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if allowed[rel] || strings.HasPrefix(rel, filepath.Join("internal", "projectprovision")) {
			return nil
		}
		scanned++
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := stripComments(string(raw))
		for _, needle := range needles {
			if strings.Contains(src, needle) {
				offenders = append(offenders, rel+" ("+needle+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("only %d files scanned; the walk is not reaching the repository and this guard "+
			"would pass vacuously", scanned)
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s reads the provisioning table or mints a container id outside the provisioning "+
			"slice. A container id must not be readable by any code that can put it in a response — "+
			"until the target narrows authorization by Project, knowing the id is close enough to "+
			"holding access to the container.", o)
	}
}

// TestNoLogFieldCarriesTheContainerID is the log-and-error-detail half of the
// disclosure rule.
//
// Knowing a container id is close enough to holding access to the container until
// fleet's R2 and drive's R3 land, and a log line is as readable as a response body to
// anyone with log access. The regex looks at each zap field individually rather than
// at the whole file, so a legitimate mention in a nearby statement does not mask a
// real leak — and does not produce a false one either.
func TestNoLogFieldCarriesTheContainerID(t *testing.T) {
	zapField := regexp.MustCompile(`zap\.[A-Za-z]+\([^()]*[Cc]ontainer`)
	examined := 0
	for _, f := range moduleSourceFiles(t) {
		cleaned := readStripped(t, f)
		if strings.Contains(cleaned, "zap.") {
			examined++
		}
		if m := zapField.FindString(cleaned); m != "" {
			t.Errorf("modules/project/%s logs a container id (%q). Until the target narrows "+
				"authorization by Project, the id is the only thing protecting the container.", f, m)
		}
	}
	if examined == 0 {
		t.Fatal("no file used zap; this guard would pass vacuously")
	}
}

// TestMainWiresProvisioningSecretsIntoValidateNotifyTokenExclusions pins the boot-time
// wiring, which no test in this package can reach at runtime.
//
// TestLoadProvisioningConfig's collision cases build their own argument list, so they
// would still pass with the production arguments deleted. Only the central call in
// main.go sees the DYNAMIC route-scoped notify tokens and callback secrets loaded from
// OCTO_CARD_ACTION_ROUTES; without these arguments a provisioning secret set equal to
// a route's notify token would pass every local check, and one leaked value would
// authorize both provisioning a container and minting that route's card action.
func TestMainWiresProvisioningSecretsIntoValidateNotifyTokenExclusions(t *testing.T) {
	root := repoRootForGuard(t)
	src, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	args, ok := balancedArgs(string(src), "registry.ValidateNotifyTokenExclusions(")
	if !ok {
		t.Fatal("main.go no longer calls registry.ValidateNotifyTokenExclusions; if the call moved, " +
			"move this guard's target too — the invariant still matters")
	}
	for _, want := range []string{"project.ProvisionFleetSecretEnv", "project.ProvisionDriveSecretEnv"} {
		if !strings.Contains(args, want) {
			t.Errorf("main.go: ValidateNotifyTokenExclusions(...) no longer includes %s.\nArgs:\n%s", want, args)
		}
	}
}

// grepPackageForImport returns the non-test file names in dir whose IMPORT BLOCK names
// importPath.
//
// The import block specifically, not the whole file: a comment or a test-helper string
// that mentions the path is not a dependency, and a guard that cannot tell the
// difference gets disabled by the first person it inconveniences.
func grepPackageForImport(t *testing.T, dir, importPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var hits []string
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		src := string(raw)
		open := strings.Index(src, "import (")
		if open < 0 {
			if strings.Contains(src, `import "`) && strings.Contains(src, importPath+`"`) {
				hits = append(hits, name)
			}
			continue
		}
		end := strings.Index(src[open:], "\n)")
		if end < 0 {
			continue
		}
		// Suffix match with the CLOSING quote only. Requiring a leading quote too was the
		// first version's bug: the real import line is
		// "github.com/Mininglamp-OSS/octo-server/internal/projectprovision", so `"`+path+`"`
		// never matched and the guard passed on a file that did import it. The trailing
		// quote is what stops `internal/projectprovisionfoo` from matching.
		if strings.Contains(src[open:open+end], importPath+`"`) {
			hits = append(hits, name)
		}
	}
	if scanned == 0 {
		t.Fatalf("no source files scanned in %s; this guard would pass vacuously", dir)
	}
	sort.Strings(hits)
	return hits
}

// balancedArgs extracts a call's argument text up to its balanced closing paren.
// Paren-counting rather than a regex because the argument list contains nested calls
// whose ')' would close a lazy match early.
func balancedArgs(src, needle string) (string, bool) {
	start := strings.Index(src, needle)
	if start < 0 {
		return "", false
	}
	depth := 1
	var b strings.Builder
	for i := start + len(needle); i < len(src) && depth > 0; i++ {
		switch src[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return b.String(), true
			}
		}
		b.WriteByte(src[i])
	}
	return "", false
}

// repoRootForGuard walks up from this file to the module root.
func repoRootForGuard(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine the caller file")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from the test file")
		}
		dir = parent
	}
}

// TestProvisioningMigrationDeclaresTheInvariantsTheCodeRelies On is a schema guard:
// the two unique keys and the app-written time columns are load-bearing and easy to
// lose in a later migration edit.
func TestProvisioningMigrationDeclaresTheInvariants(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean("sql/20260907000001_project_provisioning.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)
	for _, want := range []string{
		// One row per (project, target) — the structural form of "exactly one row per
		// target", and what stops a replay from manufacturing a second container.
		"UNIQUE KEY `uk_octo_project_provisioning_target` (`project_id`, `target`)",
		// A duplicate container id is a generator defect; fail the INSERT rather than
		// silently reuse an id that is also the receiver's idempotency key.
		"UNIQUE KEY `uk_octo_project_provisioning_container` (`container_id`)",
		"COLLATE=utf8mb4_general_ci",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration no longer declares %q", want)
		}
	}
	// Time columns must have no MySQL-side default: claim compares next_attempt_at
	// against a Go UTC clock, and CURRENT_TIMESTAMP would introduce a second clock in
	// the MySQL session timezone. modules/space shipped a metric broken exactly that way.
	for _, banned := range []string{"CURRENT_TIMESTAMP", "ON UPDATE", "NOW()"} {
		if strings.Contains(stripSQLComments(sql), banned) {
			t.Errorf("migration uses %q; every time value must be written by the application in UTC", banned)
		}
	}
}

// stripSQLComments removes `--` comment text so prose describing a banned construct is
// not mistaken for the construct.
func stripSQLComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
