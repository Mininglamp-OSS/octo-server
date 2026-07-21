package notify

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// swSeedPending seeds n deliverable pending rows (user + active member + ledger)
// in spaceID, with uids disambiguated by offset so repeated top-ups don't collide.
func swSeedPending(t *testing.T, ctx *config.Context, spaceID string, n, offset int) {
	t.Helper()
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("%s_u%04d", spaceID, offset+i)
		swInsertUser(t, ctx, uid, 0)
		swInsertMember(t, ctx, spaceID, uid, 1, time.Now().UTC())
		swInsertLedger(t, ctx, spaceID, uid, swStatusPending, "", nil, 0)
	}
}

// Per-Space welcome config + multi-Space delivery
// (task space-welcome-per-space-admin-crud).

// swSetSpaceConfig upserts a per-Space welcome config row.
func swSetSpaceConfig(t *testing.T, ctx *config.Context, spaceID string, enabled bool, activeFrom time.Time, msg string) {
	t.Helper()
	store := common.NewSpaceWelcomeConfigStore(ctx.DB())
	require.NoError(t, store.Upsert(context.Background(), common.SpaceWelcomeSpaceConfig{
		SpaceID:       spaceID,
		Enabled:       enabled,
		ActiveFromRaw: activeFrom.UTC().Format(time.RFC3339),
		Message:       msg,
		UpdatedBy:     "u_admin",
	}, time.Now().UTC()))
}

// TestPerSpace_EffectiveConfig_RowOverridesGlobal: a present per-Space row wins
// over the global fallback, even when it opts the Space OUT (disabled).
func TestPerSpace_EffectiveConfig_RowOverridesGlobal(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_1", 1)
	// Global fallback names spc_1 and is enabled.
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_1", activeFrom))
	svc := swNewService(ctx, settings)

	// No per-Space row yet -> effective is the global config.
	eff, err := svc.effectiveConfig(bg(), "spc_1")
	require.NoError(t, err)
	assert.True(t, eff.Enabled)

	// A disabled per-Space row overrides the global -> effective is off.
	swSetSpaceConfig(t, ctx, "spc_1", false, activeFrom, "")
	eff, err = svc.effectiveConfig(bg(), "spc_1")
	require.NoError(t, err)
	assert.False(t, eff.Enabled, "present (disabled) row must override the enabled global fallback")

	// An enabled per-Space row supplies its OWN message.
	swSetSpaceConfig(t, ctx, "spc_1", true, activeFrom, "space-local hi")
	eff, err = svc.effectiveConfig(bg(), "spc_1")
	require.NoError(t, err)
	assert.True(t, eff.Enabled)
	assert.Equal(t, "space-local hi", eff.Message)
}

// TestPerSpace_EnabledEffectiveConfigs: the enabled set = enabled per-Space rows
// plus the global-fallback Space iff it has no per-Space row overriding it.
func TestPerSpace_EnabledEffectiveConfigs(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_global", 1)
	swInsertSpace(t, ctx, "spc_a", 1)
	// Global names spc_global.
	swSetConfig(t, ctx, settings, swEnabledConfig("spc_global", activeFrom))
	// Per-Space enabled row for spc_a.
	swSetSpaceConfig(t, ctx, "spc_a", true, activeFrom, "a hi")

	svc := swNewService(ctx, settings)
	configs, err := svc.enabledEffectiveConfigs(bg())
	require.NoError(t, err)
	set := map[string]bool{}
	for _, c := range configs {
		set[c.SpaceID] = true
	}
	assert.True(t, set["spc_a"], "enabled per-space row included")
	assert.True(t, set["spc_global"], "global fallback included (no overriding row)")

	// Add a disabled per-Space row for spc_global -> global fallback suppressed.
	swSetSpaceConfig(t, ctx, "spc_global", false, activeFrom, "")
	configs, err = svc.enabledEffectiveConfigs(bg())
	require.NoError(t, err)
	set = map[string]bool{}
	for _, c := range configs {
		set[c.SpaceID] = true
	}
	assert.True(t, set["spc_a"])
	assert.False(t, set["spc_global"], "a present (disabled) row overrides the global fallback")
}

// TestPerSpace_MultiSpaceDelivery: two Spaces each with their own enabled
// per-Space config independently welcome their first-join human members; the
// reconciler enqueues both and one worker wake dispatches both, with each
// recipient receiving its own Space's message and no cross-Space delivery.
func TestPerSpace_MultiSpaceDelivery(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)

	swInsertSpace(t, ctx, "spc_a", 1)
	swInsertSpace(t, ctx, "spc_b", 1)
	swInsertUser(t, ctx, "ua", 0)
	swInsertUser(t, ctx, "ub", 0)
	swInsertMember(t, ctx, "spc_a", "ua", 1, time.Now().UTC())
	swInsertMember(t, ctx, "spc_b", "ub", 1, time.Now().UTC())

	swSetSpaceConfig(t, ctx, "spc_a", true, activeFrom, "welcome A")
	swSetSpaceConfig(t, ctx, "spc_b", true, activeFrom, "welcome B")

	svc := swNewService(ctx, settings)
	got := map[string]string{} // recipient uid -> message body seen on the wire
	svc.sendFn = func(_ context.Context, req *config.MsgSendReq) (*swSendResult, error) {
		// Personal DM: ChannelID is the recipient uid (NewPersonalMsgSendReq's
		// first arg = row.UID).
		got[req.ChannelID] = string(req.Payload)
		return &swSendResult{messageID: 1, clientMsgNo: "x"}, nil
	}

	// Reconciler catches up both Spaces; worker drains both.
	svc.runReconcileOnce(bg())
	svc.runWorkerWake(bg())

	sentA, _ := svc.store.countByStatus(bg(), "spc_a", swStatusSent)
	sentB, _ := svc.store.countByStatus(bg(), "spc_b", swStatusSent)
	assert.EqualValues(t, 1, sentA, "spc_a member welcomed exactly once")
	assert.EqualValues(t, 1, sentB, "spc_b member welcomed exactly once")
	assert.Contains(t, got["ua"], "welcome A", "spc_a recipient gets spc_a's body")
	assert.Contains(t, got["ub"], "welcome B", "spc_b recipient gets spc_b's body")
	assert.NotContains(t, got["ua"], "welcome B", "no cross-space body leakage")
}

// TestPerSpace_HandleMemberJoin_ScopedToConfiguredSpace: the event path enqueues
// only for a Space whose effective config is enabled.
func TestPerSpace_HandleMemberJoin_ScopedToConfiguredSpace(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	activeFrom := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_a", 1)
	swInsertSpace(t, ctx, "spc_b", 1)
	swInsertUser(t, ctx, "ua", 0)
	swInsertUser(t, ctx, "ub", 0)
	swInsertMember(t, ctx, "spc_a", "ua", 1, time.Now().UTC())
	swInsertMember(t, ctx, "spc_b", "ub", 1, time.Now().UTC())

	// Only spc_a has an enabled config.
	swSetSpaceConfig(t, ctx, "spc_a", true, activeFrom, "hi A")

	svc := swNewService(ctx, settings)
	svc.handleMemberJoin("spc_a", "ua") // enqueued
	svc.handleMemberJoin("spc_b", "ub") // no config -> ignored

	assert.Equal(t, 1, swCountRows(t, ctx, "spc_a"))
	assert.Equal(t, 0, swCountRows(t, ctx, "spc_b"), "a Space with no enabled config enqueues nothing")
}

// TestPerSpace_Worker_FairRotation: a Space that keeps saturating the per-wake
// cap must not starve the others. spc_a is kept over-cap before every wake;
// without the rotating worker cursor it (ordered first) would consume the whole
// cap every wake and spc_b would never be served. With rotation, spc_b's small
// backlog is drained within len(spaces) wakes.
func TestPerSpace_Worker_FairRotation(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	af := time.Now().UTC().Add(-time.Hour)
	swInsertSpace(t, ctx, "spc_a", 1)
	swInsertSpace(t, ctx, "spc_b", 1)
	swSetSpaceConfig(t, ctx, "spc_a", true, af, "A")
	swSetSpaceConfig(t, ctx, "spc_b", true, af, "B")

	svc := swNewService(ctx, settings)
	svc.sendFn = func(_ context.Context, _ *config.MsgSendReq) (*swSendResult, error) {
		return &swSendResult{messageID: 1, clientMsgNo: "x"}, nil
	}

	swSeedPending(t, ctx, "spc_b", 3, 0) // small fixed backlog

	off := 0
	for w := 0; w < 2; w++ {
		swSeedPending(t, ctx, "spc_a", swWorkerWakeCap+5, off) // keep spc_a saturated
		off += swWorkerWakeCap + 5
		svc.runWorkerWake(bg())
	}

	sentB, _ := svc.store.countByStatus(bg(), "spc_b", swStatusSent)
	assert.EqualValues(t, 3, sentB, "rotation serves spc_b despite spc_a saturating the per-wake cap")
}

// TestPerSpace_SweepAll_CrossSpace: lease-expired claimed rows in different
// Spaces are all reclaimed by one cross-space sweep, even for a Space whose
// config is absent/disabled (stale in-flight rows must not be stranded).
func TestPerSpace_SweepAll_CrossSpace(t *testing.T) {
	ctx := swTestServer(t)
	settings := common.EnsureSystemSettings(ctx)
	swInsertSpace(t, ctx, "spc_a", 1)
	swInsertSpace(t, ctx, "spc_b", 1)
	svc := swNewService(ctx, settings)

	expired := time.Now().UTC().Add(-time.Minute)
	idA := swInsertLedger(t, ctx, "spc_a", "ua", swStatusClaimed, "dead-owner", &expired, 0)
	idB := swInsertLedger(t, ctx, "spc_b", "ub", swStatusClaimed, "dead-owner", &expired, 0)

	svc.sweepAll(bg())

	stA, _, _, _, _ := swRowStatus(t, ctx, idA)
	stB, _, _, _, _ := swRowStatus(t, ctx, idB)
	assert.Equal(t, swStatusPending, stA, "lease-expired claimed row in spc_a reclaimed")
	assert.Equal(t, swStatusPending, stB, "lease-expired claimed row in spc_b reclaimed")
}
