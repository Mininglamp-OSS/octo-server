package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRolloutReconcilerCommitsAllowedDecisionWithBoundEvidence(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	fixedNow := time.Unix(100, 0)
	store.now = func() time.Time { return fixedNow }
	registry := NewWriterRegistry(client, store.UIDTokenPrefix())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, registry.Join(ctx, "build-a", "pod-a", string(SessionModeRevoke), time.Now))

	control := &recordingRolloutController{state: RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3,
	}}
	reconciler := NewRolloutReconciler(store, ReconcilerOptions{
		Registry: registry, Control: control, AutoAdvance: true, ScanBatchSize: 10,
	})

	next := reconciler.reconcileOnce(ctx)
	require.Equal(t, fixedNow, next, "a successful transition is immediately re-evaluated")
	require.Equal(t, 1, control.advanceCalls)
	require.Equal(t, SessionModeRevoke, control.current.Floor)
	require.Equal(t, SessionModeBounded, control.record.To)
	require.NotNil(t, control.record.Observation)
	require.True(t, control.record.Observation.Complete)
	require.NotEmpty(t, control.record.RedisID)
	require.NotEmpty(t, control.record.WriterFingerprint)
}

func TestRolloutReconcilerTreatsMySQLCASLossAsBenign(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	fixedNow := time.Unix(100, 0)
	store.now = func() time.Time { return fixedNow }
	registry := NewWriterRegistry(client, store.UIDTokenPrefix())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, registry.Join(ctx, "build-a", "pod-a", string(SessionModeRevoke), time.Now))

	control := &recordingRolloutController{
		state:      RolloutState{Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3},
		advanceErr: ErrRolloutControlChanged,
	}
	reconciler := NewRolloutReconciler(store, ReconcilerOptions{
		Registry: registry, Control: control, AutoAdvance: true, ScanBatchSize: 10,
	})

	next := reconciler.reconcileOnce(ctx)
	require.Equal(t, fixedNow, next)
	require.Equal(t, 1, control.advanceCalls)
}

func TestRolloutReconcilerTerminalAndCanceledLoopsStayIdle(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeEnforce)
	fixedNow := time.Unix(100, 0)
	store.now = func() time.Time { return fixedNow }
	registry := NewWriterRegistry(client, store.UIDTokenPrefix())
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, registry.Join(ctx, "build-a", "pod-a", string(SessionModeEnforce), time.Now))

	control := &recordingRolloutController{state: RolloutState{
		Floor: SessionModeEnforce, MaxPerUID: 20, Version: 5,
	}}
	reconciler := NewRolloutReconciler(store, ReconcilerOptions{
		Registry: registry, Control: control, AutoAdvance: true,
	})
	next := reconciler.reconcileOnce(ctx)
	require.Equal(t, fixedNow.Add(24*time.Hour), next)
	require.Zero(t, control.advanceCalls)

	cancel()
	reconciler.Run(ctx)
}

func TestRolloutReconcilerHelpersKeepOperatorAndMetricContractsStable(t *testing.T) {
	decision := RolloutAdvanceDecision{BlockedBy: []string{
		convergenceUnobservedReason,
		convergenceBlockedPrefix + "0s, need 30s",
		"v1=1 v2=2 (need 0)",
	}}
	undeterminable, remaining := SplitUndeterminableBlockers(decision.BlockedBy)
	require.Len(t, undeterminable, 2)
	require.Equal(t, []string{"v1=1 v2=2 (need 0)"}, remaining)
	require.False(t, decision.BlockedOnlyOnConvergence())
	require.True(t, (RolloutAdvanceDecision{BlockedBy: undeterminable}).BlockedOnlyOnConvergence())
	require.False(t, (RolloutAdvanceDecision{}).BlockedOnlyOnConvergence())

	for reason, label := range map[string]string{
		"no live writers registered":                    "no-writers",
		"2 distinct builds are live":                    "mixed-builds",
		"1 of 2 writers have not applied floor revoke":  "not-converged",
		"expected 3 writers, registry has 2":            "writer-count-mismatch",
		convergenceUnobservedReason:                     "not-converged-long-enough",
		"v1=1 v2=0 (need 0)":                            "legacy-remaining",
		"keyspace scan did not complete":                "scan-incomplete",
		"floor is already enforce":                      "terminal",
		"a future bounded-cardinality operator message": "other",
	} {
		require.Equal(t, label, blockedReasonLabel(reason), reason)
	}
}

func TestForceAdvanceUsesTheSameControllerAndPredicateDecision(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	control := &recordingRolloutController{state: RolloutState{
		Floor: SessionModeRevoke, MaxPerUID: 20, Version: 3,
	}}
	decision := RolloutAdvanceDecision{
		Current: SessionModeRevoke, Target: SessionModeBounded, StateVersion: 3,
		MaxPerUID: 20, Allowed: true,
	}

	require.NoError(t, store.ForceAdvanceRollout(context.Background(), decision, control))
	require.Equal(t, 1, control.advanceCalls)
	require.Equal(t, "operator", control.record.Actor)

	decision.Allowed = false
	require.Error(t, store.ForceAdvanceRollout(context.Background(), decision, control))
	require.Error(t, store.ForceAdvanceRollout(context.Background(), RolloutAdvanceDecision{Allowed: true}, nil))
}

type recordingRolloutController struct {
	state        RolloutState
	loadErr      error
	advanceErr   error
	advanceCalls int
	current      RolloutState
	record       RolloutAdvanceRecord
}

func (c *recordingRolloutController) Load(context.Context) (RolloutState, error) {
	return c.state, c.loadErr
}

func (c *recordingRolloutController) Advance(
	_ context.Context,
	current RolloutState,
	record RolloutAdvanceRecord,
) (RolloutState, error) {
	c.advanceCalls++
	c.current = current
	c.record = record
	if c.advanceErr != nil {
		return RolloutState{}, c.advanceErr
	}
	next := current
	next.Floor = record.To
	next.Version++
	return next, nil
}

var _ RolloutStateController = (*recordingRolloutController)(nil)

func TestRecordingControllerErrorIsNotMistakenForCASLoss(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	control := &recordingRolloutController{advanceErr: errors.New("mysql unavailable")}
	decision := RolloutAdvanceDecision{
		Current: SessionModeRevoke, Target: SessionModeBounded, StateVersion: 3,
		MaxPerUID: 20, Allowed: true,
	}
	reconciler := NewRolloutReconciler(store, ReconcilerOptions{Control: control})
	require.ErrorContains(t, reconciler.advance(context.Background(), decision, "reconciler"), "mysql unavailable")
}
