package oidc

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// TestInitialSpaceJoinResultLabels_CoverEveryOutcome pins the metric's label set
// against space.InitialSpaceJoinOutcome, which is the real source of truth:
// autoJoinInitialSpace feeds the outcome straight into WithLabelValues, so an
// outcome added over there without a label here would appear in Prometheus as a
// series that materialises out of nowhere on first occurrence — exactly the
// "cannot tell zero from unregistered" problem the init() pre-warm exists to
// prevent, and it would show up only in production.
//
// This is a compile-plus-assert pairing rather than reflection: listing the
// outcomes here means adding one to the space package fails this test until the
// label list is updated too.
func TestInitialSpaceJoinResultLabels_CoverEveryOutcome(t *testing.T) {
	outcomes := []spacemod.InitialSpaceJoinOutcome{
		spacemod.InitialSpaceJoined,
		spacemod.InitialSpaceAlreadyMember,
		spacemod.InitialSpaceFull,
		spacemod.InitialSpaceInactive,
		spacemod.InitialSpaceFailed,
	}

	labels := map[string]bool{}
	for _, l := range initialSpaceJoinResultLabels() {
		labels[l] = true
	}
	assert.Len(t, labels, len(outcomes), "label set and outcome set must be the same size")

	for _, o := range outcomes {
		assert.Truef(t, labels[string(o)],
			"outcome %q has no pre-warmed metric label; add it to initialSpaceJoinResultLabels", o)
		c, err := metricInitialSpaceJoinTotal.GetMetricWithLabelValues(string(o))
		assert.NoErrorf(t, err, "label %s must be valid", o)
		assert.NotNil(t, c)
	}
}

// TestAutoJoinInitialSpace_NoCtxIsSilentNoOp pins that the join hook is inert
// when the module has no *config.Context.
//
// Two reasons this matters beyond the unit tests that construct OIDC this way
// (newTestOIDC injects no ctx): it is the shape every pre-existing callback test
// runs in, so the assertion is what keeps "feature off behaves exactly as before"
// honest; and reading the settings singleton through a nil ctx would panic on a
// live login path, which the recover would swallow into an error counter rather
// than a crash — silent, and only visible as a metric nobody is watching yet.
func TestAutoJoinInitialSpace_NoCtxIsSilentNoOp(t *testing.T) {
	o := &OIDC{Log: log.NewTLog("OIDC-test")}

	before := totalInitialSpaceJoinSamples()
	assert.NotPanics(t, func() { o.autoJoinInitialSpace("u_new") })
	assert.Equal(t, before, totalInitialSpaceJoinSamples(),
		"no ctx must not touch the counter at all — not even the error label")
}

// TestAutoJoinInitialSpace_EmptyUIDIsSilentNoOp pins the other guard on the same
// early return. An empty uid can only be a caller bug, but the callback path
// already treats an empty session uid as "nothing was created", so the hook must
// agree rather than counting an error for a join nobody asked for.
func TestAutoJoinInitialSpace_EmptyUIDIsSilentNoOp(t *testing.T) {
	o := &OIDC{Log: log.NewTLog("OIDC-test")}

	before := totalInitialSpaceJoinSamples()
	assert.NotPanics(t, func() { o.autoJoinInitialSpace("") })
	assert.Equal(t, before, totalInitialSpaceJoinSamples())
}

// TestAutoJoinInitialSpace_RejectsEmptyArgs pins the space-side contract the hook
// relies on: empty arguments are a caller bug and surface as an error rather than
// being silently treated as "feature off". The off switch is the empty config
// value, checked before this function is ever reached.
//
// Runs without infra: both cases return before any query.
func TestAutoJoinInitialSpace_RejectsEmptyArgs(t *testing.T) {
	outcome, err := spacemod.AutoJoinInitialSpace(nil, "u_1", "sp_1")
	assert.Equal(t, spacemod.InitialSpaceFailed, outcome)
	assert.Error(t, err, "nil ctx must be reported, not silently skipped")
}

// totalInitialSpaceJoinSamples sums the counter across every label so a test can
// assert "nothing was counted" without naming the label it expects to stay at
// zero — an assertion on one label would pass if the code counted a different one.
func totalInitialSpaceJoinSamples() float64 {
	var total float64
	for _, l := range initialSpaceJoinResultLabels() {
		total += testutil.ToFloat64(metricInitialSpaceJoinTotal.WithLabelValues(l))
	}
	return total
}
