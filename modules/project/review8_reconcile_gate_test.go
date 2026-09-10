package project

// Reconcile observability checks: a failed scan increments its failure counter,
// and every metric emitted for a scan uses the same label.

import (
	"regexp"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
