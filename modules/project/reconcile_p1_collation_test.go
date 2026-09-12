package project

// P1's answer to the question P0's round 5 raised: do the remaining scans need
// the OCTO_PROJECT_RECONCILE_ENABLED gate too?
//
// P0 gates three scans because they JOIN legacy Space tables with no COLLATE and
// therefore fail with MySQL 1267 on the drifted production shape, at statement
// resolution. P1's two remaining scans have different properties: I3 carries
// explicit COLLATE on its cross-schema comparisons, while removal-stall reads
// only project tables. Both therefore run ungated.
//
// This is a claim about SQL, and this file makes it evidence instead of a
// comment by running the I3 query against a deliberately drifted database and
// after the legacy tables are converted back.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const p1CollationProbeDB = "octo_project_p1_collation_probe"

// scanRuns reads the sample count for one scan's duration histogram label. Each
// scan defers exactly one Observe, so this is the execution count used by the
// reconcile-gate behavior test below.
func scanRuns(t *testing.T, scan string) uint64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "project_reconcile_duration_seconds" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "scan" && label.GetValue() == scan {
					return metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

func currentSchemaName(t *testing.T) string {
	t.Helper()
	var name string
	require.NoError(t, testCtx.DB().SelectBySql("SELECT DATABASE()").LoadOne(&name))
	require.NotEmpty(t, name)
	return name
}

// p1ProbeTables is what P1's scans touch, with the collation each has in
// production. `legacy` means "created without COLLATE, so it inherited the server
// default", which the mysqldump import turned into utf8mb4_0900_ai_ci.
var p1ProbeTables = []struct {
	name   string
	legacy bool
}{
	{"octo_project", false},
	{"octo_project_member", false},
	{"group", true},
}

// newP1CollationProbe builds an isolated database in the measured production
// shape and returns a *Project bound to it plus a converge() performing the
// brief's conversion.
func newP1CollationProbe(t *testing.T) (*Project, func()) {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = testCtx.GetConfig().DB.MySQLAddr
	}
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected DSN shape")
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}
	admin, err := dbr.Open("mysql", addr[:slash+1]+opts, nil)
	require.NoError(t, err)
	defer admin.Close()
	adminSess := admin.NewSession(nil)

	exec := func(sess *dbr.Session, stmt string) {
		_, e := sess.UpdateBySql(stmt).Exec()
		require.NoError(t, e, stmt)
	}
	exec(adminSess, "DROP DATABASE IF EXISTS `"+p1CollationProbeDB+"`")
	// Created general_ci, exactly like CI does — so the drift comes from the
	// TABLES, which is how it arises in production.
	exec(adminSess, "CREATE DATABASE `"+p1CollationProbeDB+
		"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")

	conn, err := dbr.Open("mysql", addr[:slash+1]+p1CollationProbeDB+opts, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminSess.UpdateBySql("DROP DATABASE IF EXISTS `" + p1CollationProbeDB + "`").Exec()
		_ = conn.Close()
	})
	sess := conn.NewSession(nil)

	src := currentSchemaName(t)
	for _, tbl := range p1ProbeTables {
		// CREATE TABLE ... LIKE, not hand-written DDL: the structure comes from
		// the schema the migrations actually produced, so this fixture cannot
		// drift away from the real one.
		exec(sess, "CREATE TABLE `"+tbl.name+"` LIKE `"+src+"`.`"+tbl.name+"`")
		if tbl.legacy {
			exec(sess, "ALTER TABLE `"+tbl.name+
				"` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
		}
	}

	p := &Project{db: &DB{session: sess}}
	p.cfg = loadConfig()
	converge := func() {
		for _, tbl := range p1ProbeTables {
			if tbl.legacy {
				exec(sess, "ALTER TABLE `"+tbl.name+
					"` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
			}
		}
	}
	return p, converge
}

// TestP1ScansSurviveCollationDrift is the evidence for running the P1 scans
// outside the reconcile gate.
//
// Both directions are asserted. Working on the DRIFTED database is the claim;
// still working after the conversion is what stops the COLLATE from becoming the
// next thing that breaks when the legacy tables are finally converted.
func TestP1ScansSurviveCollationDrift(t *testing.T) {
	setup(t) // the shared schema is the source CREATE TABLE ... LIKE copies from
	p, converge := newP1CollationProbe(t)

	statements := map[string]func() error{
		"queryI3Page": func() error {
			_, err := p.queryI3Page(0, 10)
			return err
		},
	}

	for name, run := range statements {
		assert.NoError(t, run(),
			"%s must work on a collation-drifted database — that is the whole reason its "+
				"legacy-to-octo_project comparisons carry an explicit COLLATE, and the reason "+
				"the P1 scans run outside OCTO_PROJECT_RECONCILE_ENABLED. If this fails, either "+
				"add the missing COLLATE or move the scan inside the gate; leaving it ungated "+
				"and failing produces a gauge that never publishes and reads as 'no violations'.",
			name)
	}

	converge()
	for name, run := range statements {
		assert.NoError(t, run(),
			"%s must still work once the legacy tables are converted: an explicit COLLATE "+
				"that only works against the drift would break at the conversion", name)
	}
}

// Written in both directions, like P0's gate test: it fails if the P1 scans are
// skipped when the flag is off, and the drift test above is what makes that safe
// rather than optimistic.
func TestP1ScansRunWithTheReconcileGateOff(t *testing.T) {
	_, p := setup(t)

	p1Scans := []string{"i3", "removing_stall"}
	before := map[string]uint64{}
	for _, s := range p1Scans {
		before[s] = scanRuns(t, s)
	}

	p.cfg.ReconcileEnabled = false // the merge-time default
	resetCursorsForTest()
	p.runReconcile()

	for _, s := range p1Scans {
		assert.Greater(t, scanRuns(t, s), before[s],
			"%s must run with the reconcile gate off: I3 carries explicit COLLATE "+
				"(TestP1ScansSurviveCollationDrift), while the removal-stall scan touches "+
				"only Project tables", s)
	}
}

// TestP1ScanLabelsAgreeAcrossMetrics is the reconcile_p1.go half of P0's
// TestScanLabelsAgreeAcrossMetrics, which reads reconcile.go only.
//
// The defect it guards against is a failure counter using a different label
// from the duration histogram. Nothing breaks; the dashboard simply will not
// join the two series.
func TestP1ScanLabelsAgreeAcrossMetrics(t *testing.T) {
	src := readLinesWithoutComments(t, "reconcile_p1.go")

	histogram := map[string]bool{}
	for _, m := range scanLabelPattern.FindAllStringSubmatch(src, -1) {
		histogram[m[1]] = true
	}
	require.Len(t, histogram, 2,
		"expected exactly two scan labels in reconcile_p1.go, found %v — the parse probably "+
			"broke, which would make this guard vacuous", histogram)

	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`noteScanFailure\("([a-z_0-9]+)"\)`),
		regexp.MustCompile(`logCapped\{p: p, scan: "([a-z_0-9]+)"\}`),
	} {
		found := pattern.FindAllStringSubmatch(src, -1)
		require.Len(t, found, 2, "expected two matches for %s — guard would be vacuous", pattern)
		for _, m := range found {
			assert.True(t, histogram[m[1]],
				"scan label %q is used by %s but is not one of reconcile_p1.go's duration "+
					"histogram labels (%v). One scan must have exactly one name across every "+
					"metric, or a dashboard cannot join failure counts to durations.",
				m[1], pattern, histogram)
		}
	}
}

var scanLabelPattern = regexp.MustCompile(`reconcileDuration\.WithLabelValues\("([a-z_0-9]+)"\)`)
