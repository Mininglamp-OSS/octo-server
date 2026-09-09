package outbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// ScanIntervalEnv controls the interval used by the outbox fallback worker.
	ScanIntervalEnv = "DM_EVENT_OUTBOX_SCAN_MINUTES"

	// DefaultMaxLen is the approximate maximum number of entries retained in a
	// Redis stream for one domain/target pair.
	DefaultMaxLen int64 = 10000
	// DefaultDeliveredRetention is how long delivered rows remain available for
	// audit and troubleshooting.
	DefaultDeliveredRetention time.Duration = 7 * 24 * time.Hour
	// DefaultClaimBatchSize bounds the rows claimed by one worker pass.
	DefaultClaimBatchSize = 100
	// DefaultLeaseDuration bounds a worker's ownership of a claimed row.
	DefaultLeaseDuration time.Duration = 30 * time.Second
	// DefaultScanInterval is the fallback when a worker is started with an
	// invalid interval or when the scan interval environment variable is absent.
	DefaultScanInterval time.Duration = 3 * time.Minute

	outboxStatusPending   uint8 = 0
	outboxStatusDelivered uint8 = 1
)

var (
	// MaxLen, DeliveredRetention, ClaimBatchSize, and LeaseDuration are package
	// knobs for deployments and focused tests. Non-positive values resolve to
	// their corresponding defaults at use time.
	MaxLen             int64         = DefaultMaxLen
	DeliveredRetention time.Duration = DefaultDeliveredRetention
	ClaimBatchSize     int           = DefaultClaimBatchSize
	LeaseDuration      time.Duration = DefaultLeaseDuration

	stateMu        sync.RWMutex
	dbSession      *dbr.Session
	redisClient    *redis.Client
	targetRegistry map[string][]Target

	outboxLog = log.NewTLog("EventOutbox")
)

// ErrInvalidEvent identifies malformed events and a missing transaction.
var ErrInvalidEvent = errors.New("outbox: invalid event")

// Event describes a domain change. Data is opaque to the outbox component and
// is copied into the JSON envelope delivered to subscribers.
type Event struct {
	Domain     string
	ResourceID string
	EventType  string
	Data       map[string]any
}

// Target is a registered subscriber. A nil Enabled callback means enabled;
// callers that need deployment/configuration gating should provide a callback
// that returns false while the target is unavailable.
type Target struct {
	Service string
	Enabled func() bool
}

type eventEnvelope struct {
	EventID    string         `json:"event_id"`
	Domain     string         `json:"domain"`
	ResourceID string         `json:"resource_id"`
	EventType  string         `json:"event_type"`
	OccurredAt string         `json:"occurred_at"`
	Data       map[string]any `json:"data"`
}

type outboxRow struct {
	ID            uint64         `db:"id"`
	EventID       string         `db:"event_id"`
	Domain        string         `db:"domain"`
	ResourceID    string         `db:"resource_id"`
	EventType     string         `db:"event_type"`
	TargetService string         `db:"target_service"`
	Payload       string         `db:"payload"`
	Status        uint8          `db:"status"`
	Attempts      uint32         `db:"attempts"`
	NextAttemptAt time.Time      `db:"next_attempt_at"`
	LastError     sql.NullString `db:"last_error"`
	LeaseOwner    sql.NullString `db:"lease_owner"`
	LeaseUntil    *time.Time     `db:"lease_until"`
	CreatedAt     time.Time      `db:"created_at"`
	DeliveredAt   *time.Time     `db:"delivered_at"`
}

// RegisterTargets adds or replaces target registrations for a domain. Service
// names are unique within a domain so repeated startup registration cannot
// create duplicate outbox rows for one event.
func RegisterTargets(domain string, targets ...Target) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return
	}

	stateMu.Lock()
	defer stateMu.Unlock()
	if targetRegistry == nil {
		targetRegistry = make(map[string][]Target)
	}
	registered := targetRegistry[domain]
	for _, target := range targets {
		target.Service = strings.TrimSpace(target.Service)
		if target.Service == "" {
			continue
		}
		replaced := false
		for i := range registered {
			if registered[i].Service == target.Service {
				registered[i] = target
				replaced = true
				break
			}
		}
		if !replaced {
			registered = append(registered, target)
		}
	}
	targetRegistry[domain] = registered
}

// Init installs the shared database session and instrumented Redis client used
// by immediate delivery and the fallback worker. The client intentionally lives
// for the process lifetime; callers must not close it during request handling.
func Init(ctx *config.Context) {
	if ctx == nil || ctx.GetConfig() == nil {
		outboxLog.Error("cannot initialize without configuration")
		return
	}
	client := octoredis.NewInstrumentedClient(ctx.GetConfig(), func(options *redis.Options) {
		options.MaxRetries = 1
		options.PoolSize = 10
		options.ReadTimeout = time.Second
		options.WriteTimeout = time.Second
	})

	stateMu.Lock()
	dbSession = ctx.DB()
	redisClient = client
	stateMu.Unlock()
}

// EnqueueTx writes one pending row per enabled target into the caller's
// transaction. It never touches Redis. A caller that commits the transaction
// must invoke DeliverNow with the returned event ID; a rollback removes all
// rows atomically with the business mutation.
func EnqueueTx(tx *dbr.Tx, event Event) (string, error) {
	if tx == nil {
		return "", fmt.Errorf("%w: transaction is nil", ErrInvalidEvent)
	}
	if err := validateEvent(event); err != nil {
		return "", err
	}

	eventID := uuid.New().String()
	targets := enabledTargets(event.Domain)
	if len(targets) == 0 {
		return eventID, nil
	}

	payload, err := json.Marshal(eventEnvelope{
		EventID:    eventID,
		Domain:     event.Domain,
		ResourceID: event.ResourceID,
		EventType:  event.EventType,
		OccurredAt: time.Now().UTC().Format(time.RFC3339),
		Data:       event.Data,
	})
	if err != nil {
		return "", fmt.Errorf("outbox: marshal event %s: %w", eventID, err)
	}

	now := time.Now().UTC()
	for _, target := range targets {
		_, err := tx.InsertBySql(
			"INSERT INTO event_outbox (event_id,domain,resource_id,event_type,target_service,payload,status,attempts,next_attempt_at,created_at) VALUES (?,?,?,?,?,?,?,0,?,?)",
			eventID,
			event.Domain,
			event.ResourceID,
			event.EventType,
			target.Service,
			string(payload),
			outboxStatusPending,
			now,
			now,
		).Exec()
		if err != nil {
			return "", fmt.Errorf("outbox: insert event %s target %s: %w", eventID, target.Service, err)
		}
	}
	return eventID, nil
}

func validateEvent(event Event) error {
	if strings.TrimSpace(event.Domain) == "" {
		return fmt.Errorf("%w: domain is required", ErrInvalidEvent)
	}
	if strings.TrimSpace(event.ResourceID) == "" {
		return fmt.Errorf("%w: resource ID is required", ErrInvalidEvent)
	}
	if strings.TrimSpace(event.EventType) == "" {
		return fmt.Errorf("%w: event type is required", ErrInvalidEvent)
	}
	return nil
}

func enabledTargets(domain string) []Target {
	stateMu.RLock()
	registered := append([]Target(nil), targetRegistry[domain]...)
	stateMu.RUnlock()

	targets := make([]Target, 0, len(registered))
	for _, target := range registered {
		if target.Service == "" {
			continue
		}
		if target.Enabled != nil && !target.Enabled() {
			continue
		}
		targets = append(targets, target)
	}
	return targets
}

func snapshotState() (*dbr.Session, *redis.Client) {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return dbSession, redisClient
}

func effectiveMaxLen() int64 {
	if MaxLen > 0 {
		return MaxLen
	}
	return DefaultMaxLen
}

func effectiveDeliveredRetention() time.Duration {
	if DeliveredRetention > 0 {
		return DeliveredRetention
	}
	return DefaultDeliveredRetention
}

func effectiveClaimBatchSize() int {
	if ClaimBatchSize > 0 {
		return ClaimBatchSize
	}
	return DefaultClaimBatchSize
}

func effectiveLeaseDuration() time.Duration {
	if LeaseDuration > 0 {
		return LeaseDuration
	}
	return DefaultLeaseDuration
}

func normalizeInterval(interval time.Duration) time.Duration {
	if interval > 0 {
		return interval
	}
	return DefaultScanInterval
}

func streamKey(domain, targetService string) string {
	return "event_queue:" + domain + ":" + targetService
}

func errorSummary(err error) string {
	if err == nil {
		return ""
	}
	summary := strings.TrimSpace(err.Error())
	if summary == "" {
		return "unknown error"
	}
	if len(summary) <= 255 {
		return summary
	}
	summary = summary[:255]
	for !utf8.ValidString(summary) {
		summary = summary[:len(summary)-1]
	}
	return summary
}

func logDeliveryError(row outboxRow, err error) {
	outboxLog.Warn("event delivery failed",
		zap.String("event_id", row.EventID),
		zap.String("domain", row.Domain),
		zap.String("target_service", row.TargetService),
		zap.Error(err))
}
