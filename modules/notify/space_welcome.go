package notify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// space_welcome.go — orchestration for the Space new-user welcome feature.
//
// Reliability contract (minimal implementation): first-join human → exactly one
// ledger row → member re-check before send. Send semantics are at-most-once;
// transport-ambiguous outcomes become `unknown` and are never auto-retried.
//
// The three moving parts share one ledger (octo_space_welcome_delivery):
//   - the low-latency event path (handleMemberJoin) upserts a pending row;
//   - the reconciler catches dropped events / restarts / the addMembers path;
//   - the send worker claims pending rows, re-checks eligibility, and delivers.

const langResolveTimeout = 2 * time.Second

// swBotReadyTimeout caps the (context-less) bot-readiness probe so a hung
// WuKongIM / DB dependency cannot wedge the single worker goroutine or Stop().
const swBotReadyTimeout = 5 * time.Second

// callWithTimeout runs fn in a goroutine and returns (result, true) if it
// completes within d, or (zero, false) on timeout. On timeout the goroutine is
// left to finish in the background (a bounded leak on a degenerate hung
// dependency) — the point is that the caller (the worker) is never blocked
// beyond d, so pre-IM retry / clean Stop always make progress.
func callWithTimeout[T any](d time.Duration, fn func() T) (T, bool) {
	ch := make(chan T, 1)
	go func() { ch <- fn() }()
	select {
	case v := <-ch:
		return v, true
	case <-time.After(d):
		var zero T
		return zero, false
	}
}

// spaceWelcomeService owns the reconciler + worker goroutines and the enqueue
// path. It is constructed once per process by Notify.New and started/stopped via
// the module Start/Stop hooks.
type spaceWelcomeService struct {
	ctx      *config.Context
	store    *spaceWelcomeStore
	settings *common.SystemSettings
	langSvc  *user.LanguageService
	metrics  *spaceWelcomeMetrics
	fromUID  string
	// sendFn delivers one personal DM. Defaults to the notify-local
	// context-aware HTTP sender; overridable in tests.
	sendFn func(ctx context.Context, req *config.MsgSendReq) (*swSendResult, error)
	// botReady provisions (idempotent, retriable) and reports the notification
	// bot readiness. A not-ready bot is a definitive pre-IM failure.
	botReady func() bool
	// now returns the current time; injectable for tests. Always UTC.
	now func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	wait   sync.WaitGroup

	log.Log
}

func newSpaceWelcomeService(ctx *config.Context, settings *common.SystemSettings, langSvc *user.LanguageService, fromUID string, botReady func() bool) *spaceWelcomeService {
	owner := claimOwnerID()
	sender := newSpaceWelcomeSender(ctx)
	return &spaceWelcomeService{
		ctx:      ctx,
		store:    newSpaceWelcomeStore(ctx.DB(), owner),
		settings: settings,
		langSvc:  langSvc,
		metrics:  newSpaceWelcomeMetrics(),
		fromUID:  fromUID,
		sendFn:   sender.send,
		botReady: botReady,
		now:      func() time.Time { return time.Now().UTC() },
		Log:      log.NewTLog("SpaceWelcome"),
	}
}

// claimOwnerID builds a <hostname>:<pid> owner tag (K8s pod names can approach
// 63 chars; the column is VARCHAR(128)).
func claimOwnerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// Start launches the reconciler and worker goroutines. Idempotent.
func (s *spaceWelcomeService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wait.Add(2)
	go s.reconcileLoop(ctx)
	go s.workerLoop(ctx)
	s.Info("space welcome service started", zap.String("claim_owner", s.store.claimOwner))
}

// Stop cancels the goroutines and waits for a clean shutdown.
func (s *spaceWelcomeService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		s.wait.Wait()
	}
}

// ---------------------------------------------------------------------------
// Event path (low latency)
// ---------------------------------------------------------------------------

// handleMemberJoin is invoked from the SpaceMemberJoin listener. When the event
// matches the enabled config and the recipient is a first-join human member of
// the target Space, it upserts a pending ledger row. Enqueue failure never
// blocks or rolls back the completed join — it is logged and dropped (the
// reconciler will catch it up).
func (s *spaceWelcomeService) handleMemberJoin(spaceID, uid string) {
	if s == nil || spaceID == "" || uid == "" {
		return
	}
	cfg := s.settings.SpaceWelcomeConfig()
	if !cfg.Enabled || spaceID != cfg.SpaceID {
		return
	}
	// Static combination re-validation (no space-existence DB check on the hot
	// path — the reconciler enforces that). Fail closed on an invalid config.
	if field, _ := common.ValidateSpaceWelcomeCombination(cfg, nil); field != "" {
		s.metrics.incConfigInvalid()
		return
	}
	activeFrom, ok := cfg.ParsedActiveFrom()
	if !ok {
		s.metrics.incConfigInvalid()
		return
	}

	// System-bot exclusion is scoped strictly to the recipient uid.
	if spacepkg.IsSystemBot(uid) {
		return
	}

	callCtx, cancel := context.WithTimeout(context.Background(), swDBCallTimeout)
	defer cancel()
	eligible, err := s.store.firstJoinHumanMember(callCtx, cfg.SpaceID, uid, activeFrom)
	if err != nil {
		s.Warn("welcome enqueue eligibility check failed",
			zap.String("stage", swStageEnqueue), zap.String("space_id", cfg.SpaceID), zap.String("uid", uid), zap.Error(err))
		return
	}
	if !eligible {
		return
	}
	s.enqueue(callCtx, cfg.SpaceID, uid, swSourceEvent)
}

// enqueue upserts a pending row and splits enqueue_total vs enqueue_dedup_total.
func (s *spaceWelcomeService) enqueue(ctx context.Context, spaceID, uid, source string) {
	inserted, err := s.store.upsertPending(ctx, spaceID, uid, s.now())
	if err != nil {
		s.Warn("welcome enqueue upsert failed",
			zap.String("stage", swStageEnqueue), zap.String("source", source),
			zap.String("space_id", spaceID), zap.String("uid", uid), zap.Error(err))
		return
	}
	if inserted {
		s.metrics.incEnqueue(source)
	} else {
		s.metrics.incEnqueueDedup(source)
	}
}

// ---------------------------------------------------------------------------
// Reconciler
// ---------------------------------------------------------------------------

func (s *spaceWelcomeService) reconcileLoop(ctx context.Context) {
	defer s.wait.Done()
	ticker := time.NewTicker(swReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.safeRun("reconcile", func() { s.runReconcileOnce(ctx) })
		}
	}
}

// runReconcileOnce reads the config, re-validates (fail closed), scans the
// target Space for active human members lacking a ledger row, and enqueues up
// to swReconcileCap of them.
func (s *spaceWelcomeService) runReconcileOnce(ctx context.Context) {
	cfg := s.settings.SpaceWelcomeConfig()
	if !cfg.Enabled {
		return
	}
	if !s.validateRuntime(ctx, cfg) {
		return
	}
	activeFrom, _ := cfg.ParsedActiveFrom()

	scanCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	uids, err := s.store.reconcileScan(scanCtx, cfg.SpaceID, activeFrom, spacepkg.SystemBotList(), swReconcileCap)
	cancel()
	if err != nil {
		s.Warn("welcome reconcile scan failed",
			zap.String("stage", swStageEnqueue), zap.String("space_id", cfg.SpaceID), zap.Error(err))
		return
	}
	for _, uid := range uids {
		if ctx.Err() != nil {
			return
		}
		callCtx, c := context.WithTimeout(ctx, swDBCallTimeout)
		s.enqueue(callCtx, cfg.SpaceID, uid, swSourceReconciler)
		c()
	}
	if len(uids) > 0 {
		s.Info("welcome reconcile enqueued",
			zap.String("space_id", cfg.SpaceID), zap.Int("candidates", len(uids)))
	}
}

// ---------------------------------------------------------------------------
// Send worker
// ---------------------------------------------------------------------------

func (s *spaceWelcomeService) workerLoop(ctx context.Context) {
	defer s.wait.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		s.safeRun("worker", func() { s.runWorkerWake(ctx) })
		select {
		case <-ctx.Done():
			return
		case <-time.After(swIdleSleep):
		}
	}
}

// runWorkerWake sweeps stale rows (always, even when disabled, so lease-expired
// rows still reach a terminal state), then — when enabled and valid — drains up
// to swWorkerWakeCap claimed rows. The per-wake cap is a safety valve; the next
// wake resumes after the idle sleep.
func (s *spaceWelcomeService) runWorkerWake(ctx context.Context) {
	cfg := s.settings.SpaceWelcomeConfig()

	// Sweep runs against the configured Space regardless of enabled, so that a
	// disabled feature still promotes stale claimed/dispatching rows.
	if cfg.SpaceID != "" {
		s.sweep(ctx, cfg.SpaceID)
	}

	if !cfg.Enabled || !s.validateRuntime(ctx, cfg) {
		return
	}

	for claimed := 0; claimed < swWorkerWakeCap; claimed++ {
		if ctx.Err() != nil {
			return
		}
		// Re-read the config each iteration (a cheap atomic snapshot read) so an
		// admin disable or Space switch stops NEW claims immediately on this
		// replica, rather than after the current wake's 20-row budget drains.
		cur := s.settings.SpaceWelcomeConfig()
		if !cur.Enabled || cur.SpaceID != cfg.SpaceID {
			return
		}
		claimCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
		row, err := s.store.claimOne(claimCtx, cfg.SpaceID, s.now())
		cancel()
		if err != nil {
			s.Warn("welcome claim failed", zap.String("stage", swStageClaim), zap.String("space_id", cfg.SpaceID), zap.Error(err))
			return
		}
		if row == nil {
			return // idle — fall through to sleep
		}
		s.dispatch(ctx, cfg, row)
	}
}

// sweep reclaims lease-expired claimed rows (-> pending) and promotes
// lease-expired dispatching rows (-> unknown). Runs once per wake.
func (s *spaceWelcomeService) sweep(ctx context.Context, spaceID string) {
	now := s.now()
	c1, cancel1 := context.WithTimeout(ctx, swDBCallTimeout)
	reclaimed, err := s.store.sweepClaimed(c1, spaceID, now)
	cancel1()
	if err != nil {
		s.Warn("welcome sweep claimed failed", zap.String("stage", swStageSweep), zap.String("space_id", spaceID), zap.Error(err))
	} else {
		s.metrics.addSweepClaimed(reclaimed)
	}

	c2, cancel2 := context.WithTimeout(ctx, swDBCallTimeout)
	promoted, err := s.store.sweepDispatching(c2, spaceID, now)
	cancel2()
	if err != nil {
		s.Warn("welcome sweep dispatching failed", zap.String("stage", swStageSweep), zap.String("space_id", spaceID), zap.Error(err))
	} else if promoted > 0 {
		s.metrics.addSweepDispatching(promoted)
		s.Warn("welcome dispatching rows swept to unknown", zap.String("space_id", spaceID), zap.Int64("count", promoted))
	}
}

// dispatch takes one claimed row through pre-send re-check, dispatching
// transition, IM delivery, and the terminal ledger transition.
func (s *spaceWelcomeService) dispatch(ctx context.Context, cfg common.SpaceWelcomeConfig, row *spaceWelcomeRow) {
	// Pre-send eligibility re-check (bypasses stale cache; recipient-only
	// system-bot scope). A DB error here is a definitive pre-IM failure.
	pcCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	pc, err := s.store.precheckRecipient(pcCtx, cfg.SpaceID, row.UID, spacepkg.IsSystemBot)
	cancel()
	if err != nil {
		s.Warn("welcome precheck failed", zap.String("stage", swStagePrecheck), zap.Int64("delivery_id", row.ID),
			zap.String("space_id", cfg.SpaceID), zap.String("uid", row.UID), zap.Error(err))
		s.preIMFailure(ctx, row, swErrConfigRead)
		return
	}
	if !pc.eligible {
		skCtx, c := context.WithTimeout(ctx, swDBCallTimeout)
		ok, casErr := s.store.casToSkipped(skCtx, row.ID, pc.errClass, s.now())
		c()
		if casErr != nil {
			s.Warn("welcome cas to skipped failed", zap.String("stage", swStagePrecheck), zap.Int64("delivery_id", row.ID), zap.Error(casErr))
			return
		}
		if ok {
			s.metrics.incSkipNonMember()
			s.Info("welcome recipient skipped", zap.String("stage", swStagePrecheck), zap.Int64("delivery_id", row.ID),
				zap.String("space_id", cfg.SpaceID), zap.String("uid", row.UID), zap.String("error_class", pc.errClass))
		}
		return
	}

	// Bot must be ready before we cross into dispatching. Not ready → pre-IM.
	// Bounded: ensureNotifyBotReady issues DB + network calls with no context of
	// its own, so we cap it here — a hung dependency must not wedge the single
	// worker (nor Stop()). A timeout is treated as bot-not-ready (pre-IM retry).
	if s.botReady != nil {
		ready, ok := callWithTimeout(swBotReadyTimeout, s.botReady)
		if !ok || !ready {
			s.preIMFailure(ctx, row, swErrBotNotReady)
			return
		}
	}

	// Preflight the IM endpoint BEFORE crossing into dispatching. An unconfigured
	// APIURL means no HTTP will be sent — that is a definitive pre-IM failure
	// (retryable once config is fixed), NOT a transport-ambiguous unknown.
	if s.ctx.GetConfig().WuKongIM.APIURL == "" {
		s.preIMFailure(ctx, row, swErrConfigRead)
		return
	}

	// Language resolution is bounded the same way: the 2s context inside
	// resolveLanguage may not interrupt a hung cache/DB, so cap the whole call
	// and fall back to the default language on timeout (never a retryable error).
	lang, langOK := callWithTimeout(langResolveTimeout+time.Second, func() string { return s.resolveLanguage(row.UID) })
	if !langOK {
		lang = i18n.OutboundLanguage(context.Background())
	}

	dsCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	ok, err := s.store.casToDispatching(dsCtx, row.ID, lang, s.now())
	cancel()
	if err != nil {
		s.Warn("welcome cas to dispatching failed", zap.String("stage", swStageDispatch), zap.Int64("delivery_id", row.ID), zap.Error(err))
		s.preIMFailure(ctx, row, swErrConfigRead)
		return
	}
	if !ok {
		// Ownership lost (a peer sweep reclaimed the row after lease expiry).
		// Do not send — avoids a double delivery.
		s.Warn("welcome dispatch ownership lost before send", zap.String("stage", swStageDispatch), zap.Int64("delivery_id", row.ID))
		return
	}

	// Build the personal DM. NewPersonalMsgSendReq is authoritative for
	// payload.space_id; red_dot=1; payload.type=Text plain body.
	payload := map[string]interface{}{"type": 1, "content": s.messageForLang(cfg, lang)}
	req := config.NewPersonalMsgSendReq(row.UID, s.fromUID, payload, cfg.SpaceID,
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}})

	// The IM call must not be aborted by a shutdown mid-flight (the request is
	// on the wire); the 15s client timeout bounds it. Stop is therefore bounded
	// by the HTTP timeout, not left dangling.
	sendCtx := context.WithoutCancel(ctx)
	result, sendErr := s.sendFn(sendCtx, req)
	if sendErr != nil {
		// Any failure AFTER the dispatching transition is transport-ambiguous.
		class := swErrIMTimeout
		var se *swSendError
		if errors.As(sendErr, &se) {
			class = se.class
		}
		s.toUnknown(ctx, row, class)
		return
	}

	// Success → persist identifiers. A failure of THIS ledger write is itself
	// transport-ambiguous (message was sent, but we could not record it).
	persistCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	sentOK, persistErr := s.store.casToSent(persistCtx, row.ID, result.messageID, result.clientMsgNo, s.now())
	cancel()
	if persistErr != nil {
		s.Error("welcome sent persist failed", zap.String("stage", swStagePersist), zap.Int64("delivery_id", row.ID), zap.Error(persistErr))
		s.toUnknown(ctx, row, swErrSentPersist)
		return
	}
	if !sentOK {
		// Ownership lost between dispatch and persist (a sweep already moved the
		// row to unknown). Leave it; do not resurrect.
		s.Warn("welcome sent persist ownership lost", zap.String("stage", swStagePersist), zap.Int64("delivery_id", row.ID))
		return
	}
	s.metrics.incSendSuccess()
	s.Info("welcome delivered", zap.String("stage", swStagePersist), zap.Int64("delivery_id", row.ID),
		zap.String("space_id", cfg.SpaceID), zap.String("uid", row.UID), zap.String("lang", lang))
}

// preIMFailure records a definitive pre-IM failure (the only place attempts
// grows): backs off to pending, or fails terminally after the retry budget.
func (s *spaceWelcomeService) preIMFailure(ctx context.Context, row *spaceWelcomeRow, class string) {
	c, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	ok, err := s.store.casPreIMFailure(c, row.ID, row.Attempts, class, s.now())
	cancel()
	if err != nil {
		s.Warn("welcome cas pre-IM failure failed", zap.Int64("delivery_id", row.ID), zap.String("error_class", class), zap.Error(err))
		return
	}
	// Only count a terminal failure when THIS replica actually made the
	// transition. ok=false means a peer's sweep reclaimed the row (it may still
	// be retried by its new owner), so counting send_failed here would
	// over-report terminal failures in a multi-replica deployment.
	if ok && row.Attempts+1 > swMaxPreIMAttempts {
		s.metrics.incSendFailed()
		s.Warn("welcome delivery failed (retry budget exhausted)", zap.Int64("delivery_id", row.ID), zap.String("error_class", class))
	}
}

// toUnknown records a transport-ambiguous outcome (never auto-retried).
func (s *spaceWelcomeService) toUnknown(ctx context.Context, row *spaceWelcomeRow, class string) {
	c, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	ok, err := s.store.casToUnknown(c, row.ID, class, s.now())
	cancel()
	if err != nil {
		s.Error("welcome cas to unknown failed", zap.Int64("delivery_id", row.ID), zap.String("error_class", class), zap.Error(err))
		return
	}
	// Only count when THIS replica made the transition; ok=false means a peer
	// sweep already moved the row, so counting here would over-report (mirrors
	// the CAS-gating in preIMFailure).
	if ok {
		s.metrics.incSendUnknown()
		s.Warn("welcome delivery outcome unknown", zap.String("stage", swStageDispatch), zap.Int64("delivery_id", row.ID), zap.String("error_class", class))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// validateRuntime re-validates the enabled config including that the target
// Space exists and is active. Any config failure increments config_invalid_total
// and fails the cycle closed; an infra error (space lookup) also fails closed
// for the cycle without counting as a config error. The space check is bounded
// by swDBCallTimeout.
func (s *spaceWelcomeService) validateRuntime(ctx context.Context, cfg common.SpaceWelcomeConfig) bool {
	field, err := common.ValidateSpaceWelcomeCombination(cfg, func(spaceID string) (bool, error) {
		c, cancel := context.WithTimeout(ctx, swDBCallTimeout)
		defer cancel()
		return s.store.spaceActive(c, spaceID)
	})
	if err != nil {
		s.Warn("welcome runtime space check failed", zap.String("space_id", cfg.SpaceID), zap.Error(err))
		return false
	}
	if field != "" {
		s.metrics.incConfigInvalid()
		s.Warn("welcome config invalid; failing closed", zap.String("field", field))
		return false
	}
	return true
}

// resolveLanguage resolves the recipient's outbound language (zh-CN/en-US). On
// any lookup failure or unset preference it falls back to OCTO_DEFAULT_LANGUAGE
// — language lookup is never a retryable error.
func (s *spaceWelcomeService) resolveLanguage(uid string) string {
	if s.langSvc != nil {
		rctx, cancel := context.WithTimeout(context.Background(), langResolveTimeout)
		lang, err := s.langSvc.Resolve(rctx, uid)
		cancel()
		if err == nil && lang != "" {
			return lang
		}
	}
	return i18n.OutboundLanguage(context.Background())
}

// messageForLang picks the localized body. zh-* → zh copy, otherwise en copy.
func (s *spaceWelcomeService) messageForLang(cfg common.SpaceWelcomeConfig, lang string) string {
	if strings.HasPrefix(strings.ToLower(lang), "zh") {
		return cfg.MessageZhCN
	}
	return cfg.MessageEnUS
}

// safeRun runs fn with panic recovery so one bad cycle cannot kill the loop.
func (s *spaceWelcomeService) safeRun(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.Error("space welcome cycle panic", zap.String("cycle", what), zap.Any("recover", r))
		}
	}()
	fn()
}
