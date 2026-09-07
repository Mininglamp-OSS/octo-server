package bot_task

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

type robotService interface {
	ExistRobot(uid string) (bool, error)
	PrepareBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (robot.PreparedBotTypedEvent, error)
}
type claimService interface {
	Lookup(key, sha string) (claimOutcome, error)
	Begin(key, sha string) (claimOutcome, *claimLease, error)
	Commit(lease *claimLease, event robot.PreparedBotTypedEvent) (bool, error)
	Release(lease *claimLease) (bool, error)
}
type BotTask struct {
	ctx            *config.Context
	robots         robotService
	claims         claimService
	sources        sourceRegistry
	now            func() time.Time
	notifyBotEvent func(robotID string)
	sourceLimiter  *ratelimit.Limiter
	sourceFloor    *sourceLocalFloor
	log.Log
}
type ingressResponse struct {
	Accepted bool  `json:"accepted"`
	Replay   bool  `json:"replay"`
	EventID  int64 `json:"event_id,omitempty"`
}

func New(ctx *config.Context) *BotTask {
	logger := log.NewTLog("BotTask")
	sources, err := sourceRegistryFromEnv()
	if err != nil {
		logger.Error(err.Error())
	}
	return &BotTask{
		ctx: ctx, robots: robot.NewService(ctx), claims: newRedisClaimStore(ctx), sources: sources, now: time.Now,
		notifyBotEvent: func(robotID string) { botevent.Notify(ctx.GetConfig(), robotID) }, Log: logger,
	}
}
func (m *BotTask) Route(r *wkhttp.WKHttp) {
	rlRedis := octoredis.NewInstrumentedClient(m.ctx.GetConfig(), func(options *rd.Options) {
		options.MaxRetries = 1
		options.PoolSize = 10
	})
	rps := ratelimit.SanitizeRPS(wkhttp.ParseRPSFromEnv("DM_BOT_TASK_IP_RPS", 100), 100)
	burst := ratelimit.SanitizeBurst(wkhttp.ParseBurstFromEnv("DM_BOT_TASK_IP_BURST", 200), 200)
	ipLimit := r.StrictIPRateLimitMiddleware(context.Background(), rlRedis, "internal_bot_task", rps, burst)
	sourceRPS := ratelimit.SanitizeRPS(wkhttp.ParseRPSFromEnv("DM_BOT_TASK_SOURCE_RPS", 20), 20)
	sourceBurst := ratelimit.SanitizeBurst(wkhttp.ParseBurstFromEnv("DM_BOT_TASK_SOURCE_BURST", 60), 60)
	m.sourceLimiter = ratelimit.New(
		rlRedis,
		"ratelimit:bot_task_source:",
		"bot_task_source",
		func() ratelimit.Params {
			return ratelimit.Params{Enabled: true, RPS: sourceRPS, Burst: sourceBurst}
		},
		nil,
		ratelimit.Params{Enabled: true, RPS: 20, Burst: 60},
	)
	m.sourceFloor = newSourceLocalFloor(m.sources, sourceRPS, sourceBurst)
	// This service-to-service ingress uses the per-source bearer token below
	// instead of end-user AuthMiddleware and does not access Space-scoped data.
	r.Group("/v1/internal").POST(
		"/bot-tasks",
		ipLimit,
		m.sourceAuthMiddleware(),
		m.sourceRateLimitMiddleware(),
		m.create,
	)
}

func (m *BotTask) create(c *wkhttp.Context) {
	authenticatedSource, ok := authenticatedTaskSource(c)
	if !ok {
		respondUnauthorized(c)
		return
	}
	request, err := decodeTaskRequest(c)
	if err != nil {
		respondInvalid(c, "body")
		return
	}
	request.Source = strings.TrimSpace(request.Source)
	if request.Source != authenticatedSource {
		respondUnauthorized(c)
		return
	}
	source := m.sources[authenticatedSource]
	task, err := normalizeTaskRequest(request)
	if err != nil {
		respondInvalid(c, invalidField(err))
		return
	}
	if !source.allowsBot(task.BotUID) {
		respondForbidden(c)
		return
	}

	claimKey := taskClaimKey(task.Source, task.BotUID, task.IdempotencyKey)
	fingerprint := taskFingerprint(task)
	existing, err := m.claims.Lookup(claimKey, fingerprint)
	if err != nil {
		m.Error("bot task idempotency lookup failed", zap.Error(err), zap.String("idempotency_hash", claimLogHash(claimKey)))
		respondStoreFailed(c)
		return
	}
	if m.respondClaimOutcome(c, existing) {
		return
	}

	exists, err := m.robots.ExistRobot(task.BotUID)
	if err != nil {
		m.Error("bot task robot lookup failed", zap.Error(err), zap.String("bot_uid", task.BotUID))
		respondStoreFailed(c)
		return
	}
	if !exists {
		respondNotFound(c)
		return
	}

	outcome, lease, err := m.claims.Begin(claimKey, fingerprint)
	if err != nil {
		m.Error("bot task idempotency claim failed", zap.Error(err), zap.String("idempotency_hash", claimLogHash(claimKey)))
		respondStoreFailed(c)
		return
	}
	if m.respondClaimOutcome(c, outcome) {
		return
	}
	if outcome.State != claimAcquired || lease == nil {
		respondStoreFailed(c)
		return
	}

	prepared, err := m.robots.PrepareBotTypedEvent(task.BotUID, botTaskEventType, taskEventData(task, m.now().Unix()))
	if err != nil {
		if released, releaseErr := m.claims.Release(lease); releaseErr != nil || !released {
			m.Warn("bot task claim release failed", zap.NamedError("release_error", releaseErr), zap.Bool("released", released), zap.String("idempotency_hash", claimLogHash(claimKey)))
		}
		m.Error("bot task event preparation failed", zap.Error(err), zap.String("bot_uid", task.BotUID))
		respondStoreFailed(c)
		return
	}
	committed, commitErr := m.claims.Commit(lease, prepared)
	if commitErr != nil || !committed {
		current, lookupErr := m.claims.Lookup(claimKey, fingerprint)
		if lookupErr == nil && m.respondClaimOutcome(c, current) {
			if current.State == claimReplay {
				m.notify(task.BotUID)
			}
			return
		}
		m.Error("bot task atomic enqueue failed", zap.NamedError("commit_error", commitErr), zap.NamedError("lookup_error", lookupErr), zap.Int64("event_id", prepared.EventID))
		respondStoreFailed(c)
		return
	}

	m.notify(task.BotUID)
	m.Info("bot task ingress completed", zap.Int64("event_id", prepared.EventID), zap.String("source", task.Source), zap.String("task_type", task.TaskType), zap.String("bot_uid", task.BotUID), zap.String("idempotency_hash", claimLogHash(claimKey)))
	c.ResponseWithStatus(http.StatusAccepted, ingressResponse{Accepted: true, Replay: false, EventID: prepared.EventID})
}

func decodeTaskRequest(c *wkhttp.Context) (taskRequest, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var request taskRequest
	if err := decoder.Decode(&request); err != nil {
		return taskRequest{}, err
	}
	// Inspect only bytes the decoder already buffered. Reading decoder.Buffered
	// cannot wait on the socket, so a complete object still returns promptly,
	// while an already-received second value or oversized tail is rejected.
	trailing, err := io.ReadAll(decoder.Buffered())
	if err != nil {
		return taskRequest{}, err
	}
	if len(bytes.TrimSpace(trailing)) > 0 {
		return taskRequest{}, errors.New("bot task request contains trailing data")
	}
	return request, nil
}

const authenticatedTaskSourceKey = "bot_task_source"

func (m *BotTask) sourceAuthMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		header := c.GetHeader("Authorization")
		authenticatedSource := ""
		for source, cfg := range m.sources {
			if cfg.Enabled && validBearerToken(header, cfg.Token) {
				authenticatedSource = source
			}
		}
		if authenticatedSource == "" {
			respondUnauthorized(c)
			c.Abort()
			return
		}
		c.Set(authenticatedTaskSourceKey, authenticatedSource)
		c.Next()
	}
}

func authenticatedTaskSource(c *wkhttp.Context) (string, bool) {
	value, ok := c.Get(authenticatedTaskSourceKey)
	source, typeOK := value.(string)
	return source, ok && typeOK && source != ""
}

func (m *BotTask) sourceRateLimitMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		source, ok := authenticatedTaskSource(c)
		if !ok {
			respondUnauthorized(c)
			c.Abort()
			return
		}
		if m.sourceFloor != nil && !m.sourceFloor.Allow(source) {
			c.Header("Retry-After", "1")
			respondRateLimited(c)
			c.Abort()
			return
		}
		if m.sourceLimiter != nil {
			result := m.sourceLimiter.Check(source)
			if result.ShouldSetHeaders() {
				c.Header("X-RateLimit-Limit", strconv.Itoa(result.Burst))
				c.Header("X-RateLimit-Remaining", strconv.Itoa(result.Remaining))
				c.Header("X-RateLimit-Scope", "source")
				if result.RetryAfter > 0 {
					c.Header("Retry-After", strconv.Itoa(result.RetryAfter))
				}
			}
			if result.ShouldReject() {
				respondRateLimited(c)
				c.Abort()
				return
			}
		}
		c.Next()
	}
}
func validBearerToken(header, expected string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) || expected == "" {
		return false
	}
	actual := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}
func taskEventData(task normalizedTask, enqueuedAt int64) map[string]interface{} {
	data := map[string]interface{}{
		"source": task.Source, "task_type": task.TaskType, "idempotency_key": task.IdempotencyKey,
		"bot_uid": task.BotUID, "actor_uid": task.ActorUID, "session_key": task.SessionKey,
		"prompt": task.Prompt, "context": json.RawMessage(task.Context), "enqueued_at": enqueuedAt,
	}
	if len(task.Metadata) > 0 {
		data["metadata"] = json.RawMessage(task.Metadata)
	}
	return data
}
func (m *BotTask) respondClaimOutcome(c *wkhttp.Context, outcome claimOutcome) bool {
	switch outcome.State {
	case claimMissing, claimAcquired:
		return false
	case claimPending:
		respondInProgress(c)
	case claimReplay:
		c.Response(ingressResponse{Accepted: true, Replay: true, EventID: outcome.EventID})
	case claimConflict:
		respondConflict(c)
	default:
		respondStoreFailed(c)
	}
	return true
}
func (m *BotTask) notify(botUID string) {
	if m.notifyBotEvent != nil {
		m.notifyBotEvent(botUID)
	}
}
