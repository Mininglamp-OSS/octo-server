package resourceshare

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gocraft/dbr/v2"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newStoreHarness(t *testing.T) (*DurableStore, *dbr.Session, time.Time) {
	t.Helper()
	conn, err := dbr.Open("sqlite3", ":memory:", nil)
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	session := conn.NewSession(nil)
	schema := []string{
		"CREATE TABLE resource_share_intent (id INTEGER PRIMARY KEY AUTOINCREMENT, nonce_hash BLOB NOT NULL UNIQUE, fingerprint BLOB NOT NULL, idempotency_hash BLOB NOT NULL, actor_uid TEXT NOT NULL, space_id TEXT NOT NULL, provider_id TEXT NOT NULL, resource_type TEXT NOT NULL, resource_id TEXT NOT NULL, resource_revision TEXT NOT NULL, expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL)",
		"CREATE TABLE resource_share_delivery (id INTEGER PRIMARY KEY AUTOINCREMENT, intent_id INTEGER NOT NULL, delivery_id TEXT NOT NULL UNIQUE, target_kind TEXT NOT NULL, target_ref TEXT NOT NULL, state TEXT NOT NULL, retry_at INTEGER NOT NULL DEFAULT 0, message_id TEXT NOT NULL DEFAULT '', message_seq INTEGER NOT NULL DEFAULT 0, client_msg_no TEXT NOT NULL DEFAULT '', outcome_code TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)",
		"CREATE TABLE resource_share_audit (id INTEGER PRIMARY KEY AUTOINCREMENT, intent_id INTEGER NOT NULL, delivery_id TEXT NOT NULL, actor_uid TEXT NOT NULL, space_id TEXT NOT NULL, provider_id TEXT NOT NULL, resource_type TEXT NOT NULL, resource_id TEXT NOT NULL, resource_revision TEXT NOT NULL, target_kind TEXT NOT NULL, target_ref TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL, created_at INTEGER NOT NULL)",
	}
	for _, stmt := range schema {
		_, err := session.Exec(stmt)
		require.NoError(t, err)
	}
	_, err = session.Exec("CREATE INDEX idx_resource_share_delivery_intent ON resource_share_delivery(intent_id)")
	require.NoError(t, err)

	now := time.Unix(1_800_000_000, 0).UTC()
	store := NewDurableStore(session)
	store.now = func() time.Time { return now }
	return store, session, now
}

func verifiedForStore(now time.Time) VerifiedIntent {
	return VerifiedIntent{
		ProviderID:  "smart-summary",
		Intent:      validIntent(now),
		Fingerprint: IntentFingerprint{1},
	}
}

func TestDurableStore_ClaimIntentDistinguishesFirstUseRetryAndReplay(t *testing.T) {
	store, session, now := newStoreHarness(t)
	verified := verifiedForStore(now)

	first, err := store.ClaimIntent(context.Background(), verified)
	require.NoError(t, err)
	assert.Equal(t, ReplayFirstUse, first.Disposition)
	assert.Positive(t, first.IntentID)

	retry, err := store.ClaimIntent(context.Background(), verified)
	require.NoError(t, err)
	assert.Equal(t, ReplayRetry, retry.Disposition)
	assert.Equal(t, first.IntentID, retry.IntentID)

	conflict := verified
	conflict.Fingerprint = IntentFingerprint{2}
	_, err = store.ClaimIntent(context.Background(), conflict)
	assert.ErrorIs(t, err, ErrIntentReplay)

	var count int
	require.NoError(t, session.SelectBySql("SELECT COUNT(*) FROM resource_share_intent").LoadOne(&count))
	assert.Equal(t, 1, count)
}

func TestDurableStore_ClaimIntentStoresOnlyHashesForOpaqueValues(t *testing.T) {
	store, session, now := newStoreHarness(t)
	verified := verifiedForStore(now)
	result, err := store.ClaimIntent(context.Background(), verified)
	require.NoError(t, err)

	var nonceHash, fingerprint, idempotencyHash []byte
	require.NoError(t, session.QueryRow(
		"SELECT nonce_hash, fingerprint, idempotency_hash FROM resource_share_intent WHERE id=?",
		result.IntentID,
	).Scan(&nonceHash, &fingerprint, &idempotencyHash))
	assert.Len(t, nonceHash, 32)
	assert.Len(t, fingerprint, 32)
	assert.Len(t, idempotencyHash, 32)
	assert.NotContains(t, string(nonceHash), verified.Intent.Nonce)
	assert.NotContains(t, string(idempotencyHash), verified.Intent.IdempotencyKey)
}

func TestDurableStore_ReclaimPreTransportDeliveryHonorsRetryAndIntentExpiry(t *testing.T) {
	store, _, now := newStoreHarness(t)
	verified := verifiedForStore(now)
	intentClaim, err := store.ClaimIntent(context.Background(), verified)
	require.NoError(t, err)
	delivery, err := store.ClaimDelivery(
		context.Background(), intentClaim.IntentID, verified,
		Target{Kind: TargetGroup, GroupNo: "group-a"}, "request-1",
	)
	require.NoError(t, err)
	require.NoError(t, store.RecordPreTransportOutcome(
		context.Background(), delivery.Record.ID, DeliveryRateLimited,
		now.Add(5*time.Second), "rate_limited", "request-1",
	))

	err = store.ReclaimPreTransport(context.Background(), delivery.Record.ID, "request-2")
	assert.ErrorIs(t, err, ErrDeliveryConflict)

	store.now = func() time.Time { return now.Add(6 * time.Second) }
	require.NoError(t, store.ReclaimPreTransport(context.Background(), delivery.Record.ID, "request-3"))
	record, err := store.LoadDelivery(context.Background(), delivery.Record.ID)
	require.NoError(t, err)
	assert.Equal(t, DeliveryClaimed, record.State)
	assert.Zero(t, record.RetryAt)

	require.NoError(t, store.RecordPreTransportOutcome(
		context.Background(), delivery.Record.ID, DeliveryFailed,
		now.Add(7*time.Second), "authorization_unavailable", "request-3",
	))
	store.now = func() time.Time { return time.Unix(verified.Intent.ExpiresAt+1, 0) }
	err = store.ReclaimPreTransport(context.Background(), delivery.Record.ID, "request-4")
	assert.ErrorIs(t, err, ErrDeliveryConflict)
}

func TestDeliveryIdentityBindsActorProviderResourceRevisionSpaceAndTarget(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	base := validIntent(now)
	target := Target{Kind: TargetGroup, GroupNo: "group-a"}
	first, err := DeliveryIdentity(base, target)
	require.NoError(t, err)
	assert.Len(t, first, 64)

	tests := []struct {
		name   string
		mutate func(*Intent, *Target)
	}{
		{"actor", func(i *Intent, _ *Target) { i.ActorUID = "user-c" }},
		{"space", func(i *Intent, _ *Target) { i.SpaceID = "space-b" }},
		{"provider", func(i *Intent, _ *Target) { i.Provider = "docs" }},
		{"resource id", func(i *Intent, _ *Target) { i.Resource.ID = "summary-2" }},
		{"revision", func(i *Intent, _ *Target) { i.Resource.Revision = "rev-4" }},
		{"target", func(_ *Intent, target *Target) { target.GroupNo = "group-b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intent := base
			changedTarget := target
			tt.mutate(&intent, &changedTarget)
			changed, err := DeliveryIdentity(intent, changedTarget)
			require.NoError(t, err)
			assert.NotEqual(t, first, changed)
		})
	}
}

func TestDurableStore_ClaimDeliveryIsStableAcrossIntentRetries(t *testing.T) {
	store, session, now := newStoreHarness(t)
	verified := verifiedForStore(now)
	intentClaim, err := store.ClaimIntent(context.Background(), verified)
	require.NoError(t, err)
	target := Target{Kind: TargetGroup, GroupNo: "group-a"}

	first, err := store.ClaimDelivery(context.Background(), intentClaim.IntentID, verified, target, "request-1")
	require.NoError(t, err)
	assert.True(t, first.Created)
	assert.Equal(t, DeliveryClaimed, first.Record.State)

	retry, err := store.ClaimDelivery(context.Background(), intentClaim.IntentID, verified, target, "request-2")
	require.NoError(t, err)
	assert.False(t, retry.Created)
	assert.Equal(t, first.Record.ID, retry.Record.ID)
	assert.Equal(t, first.Record.DeliveryID, retry.Record.DeliveryID)

	var deliveryCount, auditCount int
	require.NoError(t, session.SelectBySql("SELECT COUNT(*) FROM resource_share_delivery").LoadOne(&deliveryCount))
	require.NoError(t, session.SelectBySql("SELECT COUNT(*) FROM resource_share_audit").LoadOne(&auditCount))
	assert.Equal(t, 1, deliveryCount)
	assert.Equal(t, 1, auditCount, "idempotent lookup must not append a fake new delivery audit")
}

func TestDurableStore_ClaimDeliveryRejectsMismatchedIntentRow(t *testing.T) {
	store, session, now := newStoreHarness(t)
	firstVerified := verifiedForStore(now)
	firstClaim, err := store.ClaimIntent(context.Background(), firstVerified)
	require.NoError(t, err)

	otherVerified := verifiedForStore(now)
	otherVerified.Intent.Nonce = "other-nonce-0123456789"
	otherVerified.Intent.IdempotencyKey = "other-idem-0123456789"
	otherVerified.Intent.Resource.ID = "summary-other"
	otherVerified.Fingerprint = IntentFingerprint{9}
	_, err = store.ClaimIntent(context.Background(), otherVerified)
	require.NoError(t, err)

	_, err = store.ClaimDelivery(
		context.Background(),
		firstClaim.IntentID,
		otherVerified,
		Target{Kind: TargetGroup, GroupNo: "group-a"},
		"request-mismatch",
	)
	assert.ErrorIs(t, err, ErrIntentReplay)

	var deliveryCount int
	require.NoError(t, session.SelectBySql("SELECT COUNT(*) FROM resource_share_delivery").LoadOne(&deliveryCount))
	assert.Zero(t, deliveryCount)
}

func TestDurableStore_DeliveryStateMachineWritesAuditTransactionally(t *testing.T) {
	store, session, now := newStoreHarness(t)
	verified := verifiedForStore(now)
	intentClaim, err := store.ClaimIntent(context.Background(), verified)
	require.NoError(t, err)
	claim, err := store.ClaimDelivery(context.Background(), intentClaim.IntentID, verified, Target{Kind: TargetGroup, GroupNo: "group-a"}, "request-1")
	require.NoError(t, err)

	require.NoError(t, store.BeginDispatch(context.Background(), claim.Record.ID, "request-1"))
	require.NoError(t, store.MarkSent(context.Background(), claim.Record.ID, TransportResult{
		MessageID:   "9007199254740993",
		MessageSeq:  42,
		ClientMsgNo: "server-generated",
	}, "request-1"))

	record, err := store.LoadDelivery(context.Background(), claim.Record.ID)
	require.NoError(t, err)
	assert.Equal(t, DeliverySent, record.State)
	assert.Equal(t, "9007199254740993", record.MessageID)
	assert.Equal(t, uint32(42), record.MessageSeq)

	var outcomes []string
	_, err = session.Select("outcome").From("resource_share_audit").Where("delivery_id=?", record.DeliveryID).OrderAsc("id").Load(&outcomes)
	require.NoError(t, err)
	assert.Equal(t, []string{string(DeliveryClaimed), string(DeliveryDispatching), string(DeliverySent)}, outcomes)

	err = store.BeginDispatch(context.Background(), claim.Record.ID, "request-2")
	assert.ErrorIs(t, err, ErrDeliveryConflict)
}

func TestDurableStore_MarkSentAllowsOptionalMessageSequence(t *testing.T) {
	store, _, now := newStoreHarness(t)
	verified := verifiedForStore(now)
	intentClaim, err := store.ClaimIntent(context.Background(), verified)
	require.NoError(t, err)
	claim, err := store.ClaimDelivery(context.Background(), intentClaim.IntentID, verified, Target{Kind: TargetDM, PeerUID: "user-b"}, "request-1")
	require.NoError(t, err)
	require.NoError(t, store.BeginDispatch(context.Background(), claim.Record.ID, "request-1"))

	require.NoError(t, store.MarkSent(context.Background(), claim.Record.ID, TransportResult{
		MessageID: "9007199254740993", ClientMsgNo: "server-generated",
	}, "request-1"))

	record, err := store.LoadDelivery(context.Background(), claim.Record.ID)
	require.NoError(t, err)
	assert.Equal(t, DeliverySent, record.State)
	assert.Equal(t, "9007199254740993", record.MessageID)
	assert.Zero(t, record.MessageSeq)
}

func TestDurableStore_AmbiguousTransportBecomesTerminalUnknown(t *testing.T) {
	store, _, now := newStoreHarness(t)
	verified := verifiedForStore(now)
	intentClaim, err := store.ClaimIntent(context.Background(), verified)
	require.NoError(t, err)
	claim, err := store.ClaimDelivery(context.Background(), intentClaim.IntentID, verified, Target{Kind: TargetDM, PeerUID: "user-b"}, "request-1")
	require.NoError(t, err)
	require.NoError(t, store.BeginDispatch(context.Background(), claim.Record.ID, "request-1"))
	require.NoError(t, store.MarkUnknown(context.Background(), claim.Record.ID, "transport_ambiguous", "request-1"))

	record, err := store.LoadDelivery(context.Background(), claim.Record.ID)
	require.NoError(t, err)
	assert.Equal(t, DeliveryUnknown, record.State)
	assert.Equal(t, "transport_ambiguous", record.OutcomeCode)
	assert.ErrorIs(t, store.BeginDispatch(context.Background(), record.ID, "request-2"), ErrDeliveryConflict)
}

func TestDurableStore_PreTransportOutcomesNeverCrossDispatchBoundary(t *testing.T) {
	tests := []struct {
		state   DeliveryState
		retryAt time.Time
	}{
		{DeliveryDenied, time.Time{}},
		{DeliveryRateLimited, time.Unix(1_800_000_060, 0)},
		{DeliveryFailed, time.Unix(1_800_000_060, 0)},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			store, _, now := newStoreHarness(t)
			verified := verifiedForStore(now)
			intentClaim, err := store.ClaimIntent(context.Background(), verified)
			require.NoError(t, err)
			claim, err := store.ClaimDelivery(context.Background(), intentClaim.IntentID, verified, Target{Kind: TargetGroup, GroupNo: "group-a"}, "request-1")
			require.NoError(t, err)
			require.NoError(t, store.RecordPreTransportOutcome(context.Background(), claim.Record.ID, tt.state, tt.retryAt, "bounded_reason", "request-1"))

			record, err := store.LoadDelivery(context.Background(), claim.Record.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.state, record.State)
			assert.ErrorIs(t, store.BeginDispatch(context.Background(), record.ID, "request-2"), ErrDeliveryConflict)
		})
	}
}

func TestDurableStore_AuditFailureRollsBackDeliveryTransition(t *testing.T) {
	store, session, now := newStoreHarness(t)
	verified := verifiedForStore(now)
	intentClaim, err := store.ClaimIntent(context.Background(), verified)
	require.NoError(t, err)
	claim, err := store.ClaimDelivery(context.Background(), intentClaim.IntentID, verified, Target{Kind: TargetGroup, GroupNo: "group-a"}, "request-1")
	require.NoError(t, err)
	_, err = session.Exec("DROP TABLE resource_share_audit")
	require.NoError(t, err)

	err = store.BeginDispatch(context.Background(), claim.Record.ID, "request-1")
	assert.ErrorIs(t, err, ErrStore)
	record, err := store.LoadDelivery(context.Background(), claim.Record.ID)
	require.NoError(t, err)
	assert.Equal(t, DeliveryClaimed, record.State)
}

func TestDurableStore_FailsClosedOnCancellationAndMissingDB(t *testing.T) {
	store, _, now := newStoreHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.ClaimIntent(ctx, verifiedForStore(now))
	assert.ErrorIs(t, err, context.Canceled)

	var unavailable *DurableStore
	_, err = unavailable.ClaimIntent(context.Background(), verifiedForStore(now))
	assert.ErrorIs(t, err, ErrStore)
}

func TestResourceShareMigrationContainsDurableBoundariesAndNoSecrets(t *testing.T) {
	raw, err := os.ReadFile("../../modules/resource_share/sql/20260714000001_resource_share.sql")
	require.NoError(t, err)
	sql := string(raw)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS octo_resource_share_intent",
		"CREATE TABLE IF NOT EXISTS octo_resource_share_delivery",
		"CREATE TABLE IF NOT EXISTS octo_resource_share_audit",
		"UNIQUE KEY uk_resource_share_nonce",
		"UNIQUE KEY uk_resource_share_delivery",
		"nonce_hash",
		"idempotency_hash",
	} {
		assert.Contains(t, sql, required)
	}
	assert.NotContains(t, sql, "CREATE TABLE IF NOT EXISTS resource_share_",
		"new tables must use the repository's octo_ prefix")
	for _, forbidden := range []string{"raw_intent", "card_json", "share_proof", "signature", "private_key"} {
		assert.NotContains(t, strings.ToLower(sql), forbidden)
	}
}
