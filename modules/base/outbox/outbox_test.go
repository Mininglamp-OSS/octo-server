package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const outboxTestDDL = `CREATE TABLE IF NOT EXISTS event_outbox (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  event_id CHAR(36) NOT NULL,
  domain VARCHAR(64) NOT NULL,
  resource_id VARCHAR(64) NOT NULL,
  event_type VARCHAR(128) NOT NULL,
  target_service VARCHAR(64) NOT NULL,
  payload MEDIUMTEXT NOT NULL,
  status TINYINT UNSIGNED NOT NULL DEFAULT 0,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  last_error VARCHAR(255) NULL,
  lease_owner VARCHAR(128) NULL,
  lease_until DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  delivered_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_event_outbox_event_target (event_id, target_service),
  KEY idx_event_outbox_pending (status, next_attempt_at, lease_until),
  KEY idx_event_outbox_delivered (status, delivered_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`

type outboxTestEnvelope struct {
	EventID    string         `json:"event_id"`
	Domain     string         `json:"domain"`
	ResourceID string         `json:"resource_id"`
	EventType  string         `json:"event_type"`
	OccurredAt string         `json:"occurred_at"`
	Data       map[string]any `json:"data"`
}

func setupOutboxTest(t *testing.T) *config.Context {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = "root:demo@tcp(127.0.0.1)/test?charset=utf8mb4&parseTime=true"
	cfg.DB.RedisAddr = "127.0.0.1:6379"
	ctx := testutil.NewTestContext(cfg)
	db := ctx.DB()
	if _, err := db.InsertBySql(outboxTestDDL).Exec(); err != nil {
		t.Fatalf("create event_outbox: %v", err)
	}
	if _, err := db.UpdateBySql("DELETE FROM event_outbox").Exec(); err != nil {
		t.Fatalf("clear event_outbox: %v", err)
	}

	client := octoredis.NewInstrumentedClient(cfg, func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
		o.ReadTimeout = time.Second
		o.WriteTimeout = time.Second
	})
	if err := client.Ping().Err(); err != nil {
		_ = client.Close()
		t.Skipf("Redis unavailable: %v", err)
	}
	if err := db.DB.Ping(); err != nil {
		_ = client.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}

	stateMu.Lock()
	dbSession = db
	redisClient = client
	targetRegistry = nil
	stateMu.Unlock()
	DeliveredRetention = DefaultDeliveredRetention
	ClaimBatchSize = DefaultClaimBatchSize
	LeaseDuration = DefaultLeaseDuration
	t.Cleanup(func() {
		stateMu.Lock()
		if redisClient == client {
			redisClient = nil
		}
		dbSession = nil
		targetRegistry = nil
		stateMu.Unlock()
		_ = client.Del("event_queue:workspace:loop", "event_queue:workspace:driver").Err()
		_ = client.Close()
	})
	return ctx
}

func registerTestTarget(enabled bool) {
	RegisterTargets("workspace", Target{
		Service: "loop",
		Enabled: func() bool { return enabled },
	})
}

func enqueueTestEvent(t *testing.T, event Event) string {
	t.Helper()
	tx, err := dbSession.Begin()
	require.NoError(t, err)
	eventID, err := EnqueueTx(tx, event)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return eventID
}

func countOutbox(t *testing.T, where string, args ...interface{}) int {
	t.Helper()
	var n int
	_, err := dbSession.SelectBySql("SELECT COUNT(*) FROM event_outbox "+where, args...).Load(&n)
	require.NoError(t, err)
	return n
}

func loadOutboxState(t *testing.T, eventID string) (status uint8, attempts uint32, lastError sql.NullString) {
	t.Helper()
	var row struct {
		Status    uint8          `db:"status"`
		Attempts  uint32         `db:"attempts"`
		LastError sql.NullString `db:"last_error"`
	}
	require.NoError(t, dbSession.SelectBySql("SELECT status, attempts, last_error FROM event_outbox WHERE event_id=?", eventID).LoadOne(&row))
	return row.Status, row.Attempts, row.LastError
}

func TestEnqueueTxRollsBackWithBusinessTransaction(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	_, err := dbSession.InsertBySql("CREATE TABLE IF NOT EXISTS outbox_test_business (id INT PRIMARY KEY, value VARCHAR(32) NOT NULL)").Exec()
	require.NoError(t, err)
	_, err = dbSession.UpdateBySql("DELETE FROM outbox_test_business").Exec()
	require.NoError(t, err)

	tx, err := dbSession.Begin()
	require.NoError(t, err)
	_, err = tx.InsertBySql("INSERT INTO outbox_test_business (id,value) VALUES (?,?)", 1, "committed-with-outbox").Exec()
	require.NoError(t, err)
	_, err = EnqueueTx(tx, Event{Domain: "workspace", ResourceID: "ws-rollback", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-rollback"}})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	require.Equal(t, 0, countOutbox(t, "WHERE resource_id=?", "ws-rollback"))
	var n int
	_, err = dbSession.SelectBySql("SELECT COUNT(*) FROM outbox_test_business WHERE id=1").Load(&n)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestEnqueueTxPersistsRegisteredTargetsWhileDisabled(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(false)
	id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-none", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-none"}})
	require.NotEmpty(t, id)
	require.Equal(t, 1, countOutbox(t, "WHERE event_id=? AND target_service=?", id, "loop"))
}

func TestWorkerKeepsDisabledTargetPendingUntilEnabled(t *testing.T) {
	setupOutboxTest(t)
	enabled := false
	RegisterTargets("workspace", Target{Service: "loop", Enabled: func() bool { return enabled }})
	id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-disabled", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-disabled"}})

	require.NoError(t, runWorkerOnce(context.Background(), time.Minute))
	status, attempts, _ := loadOutboxState(t, id)
	require.Equal(t, outboxStatusPending, status)
	require.Zero(t, attempts)
	length, err := redisClient.XLen("event_queue:workspace:loop").Result()
	require.NoError(t, err)
	require.Zero(t, length)

	enabled = true
	_, err = dbSession.UpdateBySql("UPDATE event_outbox SET next_attempt_at=? WHERE event_id=?", time.Now().UTC().Add(-time.Second), id).Exec()
	require.NoError(t, err)
	require.NoError(t, runWorkerOnce(context.Background(), time.Minute))
	status, attempts, _ = loadOutboxState(t, id)
	require.Equal(t, outboxStatusDelivered, status)
	require.Zero(t, attempts)
	length, err = redisClient.XLen("event_queue:workspace:loop").Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, length)
}
func TestEnqueueTxRejectsUnserializablePayload(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	tx, err := dbSession.Begin()
	require.NoError(t, err)
	_, err = EnqueueTx(tx, Event{Domain: "workspace", ResourceID: "ws-bad", EventType: "workspace.updated", Data: map[string]any{"bad": func() {}}})
	require.Error(t, err)
	require.NoError(t, tx.Rollback())
	require.Equal(t, 0, countOutbox(t, "WHERE resource_id=?", "ws-bad"))
}

func TestDeliverNowWritesDecodableEnvelope(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-envelope", EventType: "workspace.members_changed", Data: map[string]any{"workspace_id": "ws-envelope"}})
	DeliverNow(id)

	messages, err := redisClient.XRange("event_queue:workspace:loop", "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, messages, 1)
	payload, ok := messages[0].Values["event_json"].(string)
	require.True(t, ok)
	var envelope outboxTestEnvelope
	require.NoError(t, json.Unmarshal([]byte(payload), &envelope))
	require.Equal(t, id, envelope.EventID)
	require.Equal(t, "workspace", envelope.Domain)
	require.Equal(t, "ws-envelope", envelope.ResourceID)
	require.Equal(t, "workspace.members_changed", envelope.EventType)
	require.Equal(t, map[string]any{"workspace_id": "ws-envelope"}, envelope.Data)
	_, err = time.Parse(time.RFC3339, envelope.OccurredAt)
	require.NoError(t, err)
	status, attempts, _ := loadOutboxState(t, id)
	require.Equal(t, uint8(1), status)
	require.Zero(t, attempts)
}

func TestDeliverNowRedisFailureLeavesPendingAttemptsUnchanged(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-redis-down", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-redis-down"}})
	bad := rd.NewClient(&rd.Options{Addr: "127.0.0.1:1", MaxRetries: 0, DialTimeout: 20 * time.Millisecond, ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond})
	stateMu.Lock()
	previous := redisClient
	redisClient = bad
	stateMu.Unlock()
	DeliverNow(id)
	stateMu.Lock()
	redisClient = previous
	stateMu.Unlock()
	_ = bad.Close()

	status, attempts, lastError := loadOutboxState(t, id)
	require.Equal(t, uint8(0), status)
	require.Zero(t, attempts)
	require.False(t, lastError.Valid)
}

func TestWorkerClaimsAndRedeliversPendingRow(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-worker", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-worker"}})
	require.NoError(t, runWorkerOnce(context.Background(), time.Minute))
	status, attempts, _ := loadOutboxState(t, id)
	require.Equal(t, uint8(1), status)
	require.Zero(t, attempts)
	messages, err := redisClient.XRange("event_queue:workspace:loop", "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, messages, 1)
}
func TestWorkerRedisFailureBacksOffAndReleasesLease(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-worker-fail", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-worker-fail"}})
	bad := rd.NewClient(&rd.Options{Addr: "127.0.0.1:1", MaxRetries: 0, DialTimeout: 20 * time.Millisecond, ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond})
	stateMu.Lock()
	previous := redisClient
	redisClient = bad
	stateMu.Unlock()
	start := time.Now().UTC()
	err := runWorkerOnce(context.Background(), time.Minute)
	stateMu.Lock()
	redisClient = previous
	stateMu.Unlock()
	_ = bad.Close()

	require.Error(t, err)
	var row struct {
		Status      uint8          `db:"status"`
		Attempts    uint32         `db:"attempts"`
		LastError   sql.NullString `db:"last_error"`
		NextAttempt time.Time      `db:"next_attempt_at"`
		LeaseOwner  sql.NullString `db:"lease_owner"`
		LeaseUntil  *time.Time     `db:"lease_until"`
	}
	require.NoError(t, dbSession.SelectBySql("SELECT status,attempts,last_error,next_attempt_at,lease_owner,lease_until FROM event_outbox WHERE event_id=?", id).LoadOne(&row))
	require.Equal(t, uint8(0), row.Status)
	require.Equal(t, uint32(1), row.Attempts)
	require.True(t, row.LastError.Valid)
	require.Greater(t, row.NextAttempt.Sub(start), time.Minute-time.Second)
	require.False(t, row.LeaseOwner.Valid)
	require.Nil(t, row.LeaseUntil)
}

func TestWorkerLeaseMutualExclusionAndExpiredRecovery(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-lease", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-lease"}})
	now := time.Now().UTC()
	rows, err := claimBatch(context.Background(), "owner-a", now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, id, rows[0].EventID)
	rows, err = claimBatch(context.Background(), "owner-b", now.Add(time.Second))
	require.NoError(t, err)
	require.Empty(t, rows)

	_, err = dbSession.UpdateBySql("UPDATE event_outbox SET lease_until=? WHERE event_id=?", now.Add(-time.Second), id).Exec()
	require.NoError(t, err)
	rows, err = claimBatch(context.Background(), "owner-b", now.Add(2*time.Second))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, id, rows[0].EventID)
}

func TestWorkerAllowsDuplicateDeliveryWindow(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-duplicate", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-duplicate"}})
	DeliverNow(id)
	payload := `{"event_id":"` + id + `","domain":"workspace","resource_id":"ws-duplicate","event_type":"workspace.updated","occurred_at":"2026-09-09T00:00:00Z","data":{"workspace_id":"ws-duplicate"}}`
	_, err := redisClient.XAdd(&rd.XAddArgs{Stream: "event_queue:workspace:loop", Values: map[string]interface{}{"event_json": payload}}).Result()
	require.NoError(t, err)
	_, err = dbSession.UpdateBySql("UPDATE event_outbox SET status=0, delivered_at=NULL, next_attempt_at=? WHERE event_id=?", time.Now().UTC().Add(-time.Second), id).Exec()
	require.NoError(t, err)
	require.NoError(t, runWorkerOnce(context.Background(), time.Minute))
	messages, err := redisClient.XRange("event_queue:workspace:loop", "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, messages, 3)
}

func TestMarkDeliveredIsOneWay(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-mark", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-mark"}})
	require.NoError(t, markDelivered(context.Background(), mustOutboxID(t, id), "owner-a"))
	_, err := dbSession.UpdateBySql("UPDATE event_outbox SET lease_owner=?, lease_until=? WHERE event_id=?", "late-owner", time.Now().UTC().Add(time.Minute), id).Exec()
	require.NoError(t, err)
	require.NoError(t, markDelivered(context.Background(), mustOutboxID(t, id), "late-owner"))
	status, _, _ := loadOutboxState(t, id)
	require.Equal(t, uint8(1), status)
}

func TestCleanupDeliveredOnlyRemovesExpiredRows(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	oldRetention := DeliveredRetention
	DeliveredRetention = 24 * time.Hour
	t.Cleanup(func() { DeliveredRetention = oldRetention })
	oldID := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-old", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-old"}})
	newID := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-new", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-new"}})
	pendingID := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-pending", EventType: "workspace.updated", Data: map[string]any{"workspace_id": "ws-pending"}})
	old := time.Now().UTC().Add(-48 * time.Hour)
	_, err := dbSession.UpdateBySql("UPDATE event_outbox SET status=1, delivered_at=? WHERE event_id=?", old, oldID).Exec()
	require.NoError(t, err)
	_, err = dbSession.UpdateBySql("UPDATE event_outbox SET status=1, delivered_at=? WHERE event_id=?", time.Now().UTC(), newID).Exec()
	require.NoError(t, err)
	require.NoError(t, cleanupDelivered(context.Background(), time.Now().UTC().Add(-24*time.Hour)))
	require.Equal(t, 0, countOutbox(t, "WHERE event_id=?", oldID))
	require.Equal(t, 1, countOutbox(t, "WHERE event_id=?", newID))
	require.Equal(t, 1, countOutbox(t, "WHERE event_id=?", pendingID))
}

func TestDeliveryDoesNotEvictOlderStreamEntries(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	const eventCount = 8
	for i := range eventCount {
		id := enqueueTestEvent(t, Event{Domain: "workspace", ResourceID: "ws-retained-" + uuid.New().String(), EventType: "workspace.updated", Data: map[string]any{"n": i}})
		DeliverNow(id)
	}
	length, err := redisClient.XLen("event_queue:workspace:loop").Result()
	require.NoError(t, err)
	require.EqualValues(t, eventCount, length)
}

func TestScanIntervalFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "empty", want: 3 * time.Minute},
		{name: "five", raw: "5", want: 5 * time.Minute},
		{name: "ten", raw: "10", want: 10 * time.Minute},
		{name: "four invalid", raw: "4", want: 3 * time.Minute},
		{name: "text invalid", raw: "nope", want: 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ScanIntervalEnv, tc.raw)
			require.Equal(t, tc.want, ScanIntervalFromEnv())
		})
	}
}

func mustOutboxID(t *testing.T, eventID string) uint64 {
	t.Helper()
	var id uint64
	require.NoError(t, dbSession.SelectBySql("SELECT id FROM event_outbox WHERE event_id=?", eventID).LoadOne(&id))
	return id
}

func TestRunWorkerStopsWhenContextCancelled(t *testing.T) {
	setupOutboxTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		RunWorker(ctx, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunWorker did not stop after context cancellation")
	}
}

func TestRegisterTargetsDoesNotPanicOnEmptyDomain(t *testing.T) {
	setupOutboxTest(t)
	RegisterTargets("", Target{Service: "loop"})
	require.Empty(t, targetRegistry)
}

func TestEnqueueTxNilTransactionReturnsError(t *testing.T) {
	setupOutboxTest(t)
	_, err := EnqueueTx(nil, Event{Domain: "workspace", ResourceID: "ws-nil", EventType: "workspace.updated"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidEvent))
}

func TestRegisterTargetsDeduplicatesServiceNames(t *testing.T) {
	setupOutboxTest(t)
	RegisterTargets("workspace", Target{Service: "loop"}, Target{Service: "loop"})
	require.Len(t, targetRegistry["workspace"], 1)
}

func TestEnqueueTxRejectsMissingRequiredFields(t *testing.T) {
	setupOutboxTest(t)
	registerTestTarget(true)
	for _, event := range []Event{
		{ResourceID: "ws", EventType: "workspace.updated"},
		{Domain: "workspace", EventType: "workspace.updated"},
		{Domain: "workspace", ResourceID: "ws"},
	} {
		tx, err := dbSession.Begin()
		require.NoError(t, err)
		_, err = EnqueueTx(tx, event)
		require.Error(t, err)
		require.NoError(t, tx.Rollback())
	}
}

func TestErrorSummaryIsBounded(t *testing.T) {
	got := errorSummary(errors.New(strings.Repeat("x", 512)))
	require.Len(t, got, 255)
}

func TestScanIntervalDoesNotAcceptWhitespaceGarbage(t *testing.T) {
	t.Setenv(ScanIntervalEnv, " 5 ")
	require.Equal(t, 5*time.Minute, ScanIntervalFromEnv())
	t.Setenv(ScanIntervalEnv, "5m")
	require.Equal(t, 3*time.Minute, ScanIntervalFromEnv())
}
