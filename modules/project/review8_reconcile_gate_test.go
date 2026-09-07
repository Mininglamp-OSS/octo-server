package project

// PR #841 review round 5 (yujiawei P0-1 / Jerry-Xin B-1). startReconcileWorker() was the first
// statement of Route() and nothing consulted a flag, so merging this — with writes disabled, zero
// projects and no traffic — still ran three cross-collation scans on every pod every tick.
//
// Both reviewers verified the failure is at statement RESOLUTION, so empty tables do not save it.
// The consequence is worse than noise: each affected gauge publishes only on a COMPLETE rotation,
// so all three would sit at zero forever and read as "no violations" on the module whose entire
// purpose is to be the invariant safety net.
//
// The gate's SCOPE is the part worth pinning, not just its existence: only the three scans that
// JOIN legacy Space tables are gated. The two that touch this module's own tables run
// unconditionally, because gating them would trade working observability for nothing — and
// scanOwnerlessProjects detects a state P0 cannot repair.

import (
	"regexp"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanRuns reads how many times one scan has been observed, from its duration histogram.
//
// The histogram's SAMPLE COUNT for that scan's label is the honest signal: every scan defers
// exactly one Observe, so the count grows by one per EXECUTED run and not at all for a skipped
// one. Reading the labelled series (rather than the whole family) is what lets this distinguish
// the gated scans from the ungated ones in the same tick.
func scanRuns(t *testing.T, scan string) uint64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "project_reconcile_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "scan" && l.GetValue() == scan {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

func TestReconcileGateCoversExactlyTheCrossSpaceScans(t *testing.T) {
	_, p := setup(t)

	crossSpace := []string{"i1_violations", "abandoned", "orphan"}
	ownTables := []string{"ownerless", "epoch"}

	before := map[string]uint64{}
	for _, s := range append(append([]string{}, crossSpace...), ownTables...) {
		before[s] = scanRuns(t, s)
	}

	// Flag OFF — the merge-time default.
	p.cfg.ReconcileEnabled = false
	resetCursorsForTest()
	p.runReconcile()

	for _, s := range crossSpace {
		assert.Equal(t, before[s], scanRuns(t, s),
			"%s JOINs legacy Space tables and must NOT run with the gate off: on a "+
				"collation-drifted database it fails at statement resolution every tick, and its "+
				"gauge never publishes, so the monitor reads as healthy while never having run", s)
	}
	for _, s := range ownTables {
		assert.Greater(t, scanRuns(t, s), before[s],
			"%s touches only this module's own tables, so gating it would lose working "+
				"observability for nothing — scanOwnerlessProjects in particular detects a state "+
				"P0 cannot repair", s)
	}

	// Flag ON — everything runs.
	mid := map[string]uint64{}
	for _, s := range crossSpace {
		mid[s] = scanRuns(t, s)
	}
	p.cfg.ReconcileEnabled = true
	resetCursorsForTest()
	p.runReconcile()
	for _, s := range crossSpace {
		assert.Greater(t, scanRuns(t, s), mid[s],
			"%s must run once the gate is open, or the flag would be a permanent off switch", s)
	}
}

// TestFailedScanIsCounted pins that a failing scan is observable as more than a log line.
//
// Gauges publish only on a complete rotation, so a scan that errors leaves its gauge untouched —
// "never ran" and "ran, found nothing" are indistinguishable on a dashboard. That is exactly how
// the collation drift would have presented.
func TestFailedScanIsCounted(t *testing.T) {
	_, p := setup(t)
	p.cfg.ReconcileEnabled = true

	before := promtestutil.ToFloat64(reconcileScanFailures.WithLabelValues("i1_violations"))

	orig := p.i1PageFn
	t.Cleanup(func() { p.i1PageFn = orig })
	p.i1PageFn = func(cursorProject, cursorUID string, limit int) ([]*i1Row, error) {
		return nil, assert.AnError
	}
	resetCursorsForTest()
	p.scanI1Violations()

	assert.Equal(t, before+1,
		promtestutil.ToFloat64(reconcileScanFailures.WithLabelValues("i1_violations")),
		"a failed scan must increment its failure counter — without it the only trace is a Warn "+
			"line, and the gauge staying at zero reads as 'no violations'")
}

// TestScanLabelsAgreeAcrossMetrics pins that every scan uses ONE label value across the duration
// histogram, the failure counter, and the capped logger.
//
// Written because the failure counter's first version used "abandoned_leak" while the histogram
// had always used "abandoned". Nothing failed — two metrics simply described the same scan under
// different names, which is the kind of defect that surfaces at 3am on a dashboard that will not
// join.
func TestScanLabelsAgreeAcrossMetrics(t *testing.T) {
	src := readLinesWithoutComments(t, "reconcile.go")

	label := regexp.MustCompile(`reconcileDuration\.WithLabelValues\("([a-z_0-9]+)"\)`)
	histogram := map[string]bool{}
	for _, m := range label.FindAllStringSubmatch(src, -1) {
		histogram[m[1]] = true
	}
	require.GreaterOrEqual(t, len(histogram), 5,
		"expected at least five scan labels on the duration histogram, found %d: %v — the parse "+
			"probably broke, which would make this guard vacuous", len(histogram), histogram)

	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`noteScanFailure\("([a-z_0-9]+)"\)`),
		regexp.MustCompile(`logCapped\{p: p, scan: "([a-z_0-9]+)"\}`),
	} {
		found := pattern.FindAllStringSubmatch(src, -1)
		require.NotEmpty(t, found, "no match for %s — guard would be vacuous", pattern)
		for _, m := range found {
			assert.True(t, histogram[m[1]],
				"scan label %q is used by %s but is not one of the duration histogram's labels "+
					"(%v). One scan must have exactly one name across every metric, or a "+
					"dashboard cannot join failure counts to durations.", m[1], pattern, histogram)
		}
	}
}
