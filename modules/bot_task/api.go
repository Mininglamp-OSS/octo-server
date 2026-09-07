package bot_task

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	rps := ratelimit.SanitizeRPS(wkhttp.ParseRPSFromEnv("DM_BOT_TASK_IP_RPS", 20), 20)
	burst := ratelimit.SanitizeBurst(wkhttp.ParseBurstFromEnv("DM_BOT_TASK_IP_BURST", 60), 60)
	ipLimit := r.StrictIPRateLimitMiddleware(context.Background(), rlRedis, "internal_bot_task", rps, burst)
	// This service-to-service ingress uses the per-source bearer token below
	// instead of end-user AuthMiddleware and does not access Space-scoped data.
	r.Group("/v1/internal").POST("/bot-tasks", ipLimit, m.create)
}

func (m *BotTask) create(c *wkhttp.Context) {
	request, err := decodeTaskRequest(c)
	if err != nil {
		respondInvalid(c, "body")
		return
	}
	request.Source = strings.TrimSpace(request.Source)
	source, ok := m.sources[request.Source]
	if !ok || !source.Enabled || !validBearerToken(c.GetHeader("Authorization"), source.Token) {
		respondUnauthorized(c)
		return
	}
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
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return taskRequest{}, errors.New("bot task request contains multiple JSON values")
		}
		return taskRequest{}, err
	}
	return request, nil
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
