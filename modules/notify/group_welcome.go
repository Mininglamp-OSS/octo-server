package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// group_welcome.go — orchestration for the group new-member welcome (群入群欢迎语).
//
// It mirrors the Space welcome engine (space_welcome.go) but:
//   - delivery is a PUBLIC post into the group channel (channel_type=GROUP), not
//     a personal DM;
//   - there is no platform-global fallback — a group's effective config is simply
//     its own row iff enabled;
//   - the body supports a {member} placeholder rendered to the joining member's
//     display name at send time.
//
// The shared status machine, tunables, sender, metrics and helpers
// (sw* constants, callWithTimeout, claimOwnerID, spaceWelcomeSender,
// spaceWelcomeMetrics) come from the space_welcome_* files (same package).

// groupWelcomeMemberToken is the placeholder rendered to the joiner's display
// name. Only this exact token is substituted; any other {...} text is left
// verbatim (no templating engine, no injection surface — the payload is plain
// text). Kept intentionally single and simple (see brief: no general templating).
const groupWelcomeMemberToken = "{member}"

// groupWelcomeRenderedMaxRunes bounds the rendered body so a config full of
// {member} tokens cannot balloon pathologically (raw ≤2000, a name ≤100). This is
// the trust-boundary "bound the payload" guard; truncation is on a rune boundary
// (plain text — there is no markdown/richtext structure to corrupt).
const groupWelcomeRenderedMaxRunes = 4096

// renderGroupWelcomeBody substitutes the {member} placeholder with memberName and
// bounds the result. memberName is a plain-text value carried as a JSON string in
// the payload, so it cannot forge message structure; no markdown is rendered. For
// a coalesced post memberName is a name LIST (see renderGroupWelcomeNameList).
func renderGroupWelcomeBody(message, memberName string) string {
	out := strings.ReplaceAll(message, groupWelcomeMemberToken, memberName)
	if utf8.RuneCountInString(out) > groupWelcomeRenderedMaxRunes {
		runes := []rune(out)
		out = string(runes[:groupWelcomeRenderedMaxRunes])
	}
	return out
}

// renderGroupWelcomeNameList joins the joiner display names for the {member}
// substitution of a coalesced post. Up to groupWelcomeCoalesceNames names are
// listed verbatim ("、"-separated); a larger batch collapses the tail to
// "name1、…、nameK 等 N 人" so one public post naming a big burst stays a single
// short line. Each name is plain text (no markdown), so the joined list cannot
// forge message structure.
func renderGroupWelcomeNameList(names []string) string {
	if len(names) <= groupWelcomeCoalesceNames {
		return strings.Join(names, "、")
	}
	shown := strings.Join(names[:groupWelcomeCoalesceNames], "、")
	return fmt.Sprintf("%s 等 %d 人", shown, len(names))
}

// groupWelcomeService owns the reconciler + worker goroutines and the enqueue
// path for the group welcome. Constructed once by Notify.New; started/stopped via
// the module Start/Stop hooks.
type groupWelcomeService struct {
	ctx      *config.Context
	store    *groupWelcomeStore
	cfgStore *common.GroupWelcomeConfigStore
	settings *common.SystemSettings
	metrics  *spaceWelcomeMetrics
	fromUID  string
	// sendFn posts one message (group channel). Defaults to the notify-local
	// context-aware HTTP sender; overridable in tests.
	sendFn func(ctx context.Context, req *config.MsgSendReq) (*swSendResult, error)
	// botReady reports the notification sender's readiness. Not-ready is a
	// definitive pre-IM failure.
	botReady func() bool
	// now returns the current time; injectable for tests. Always UTC.
	now func() time.Time
	// coalesceWindow holds a freshly-enqueued row back from the worker so a burst
	// of joins collects into one post (see groupWelcomeCoalesceWindow). A field so
	// tests can set it to 0 for deterministic immediate claiming.
	coalesceWindow time.Duration

	// reconcileCursor / workerCursor rotate the per-cycle / per-wake starting
	// group so a shared budget cannot perpetually starve the tail of the enabled
	// set. Each is touched by a single goroutine — no lock needed.
	reconcileCursor int
	workerCursor    int

	mu     sync.Mutex
	cancel context.CancelFunc
	wait   sync.WaitGroup

	log.Log
}

func newGroupWelcomeService(ctx *config.Context, settings *common.SystemSettings, fromUID string, botReady func() bool) *groupWelcomeService {
	sender := newSpaceWelcomeSender(ctx) // generic *config.MsgSendReq HTTP sender
	return &groupWelcomeService{
		ctx:            ctx,
		store:          newGroupWelcomeStore(ctx.DB(), claimOwnerID()),
		cfgStore:       common.NewGroupWelcomeConfigStore(ctx.DB()),
		settings:       settings,
		metrics:        newSpaceWelcomeMetrics(),
		fromUID:        fromUID,
		sendFn:         sender.send,
		botReady:       botReady,
		now:            func() time.Time { return time.Now().UTC() },
		coalesceWindow: groupWelcomeCoalesceWindow,
		Log:            log.NewTLog("GroupWelcome"),
	}
}

// Start launches the reconciler and worker goroutines. Idempotent.
func (s *groupWelcomeService) Start() {
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
	s.Info("group welcome service started", zap.String("claim_owner", s.store.claimOwner))
}

// Stop cancels the goroutines and waits for a clean shutdown.
func (s *groupWelcomeService) Stop() {
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

// effectiveConfig resolves the config in effect for groupNo: the per-group row iff
// present, else an empty (disabled) config. No global fallback. A DB error is
// returned so callers can fail closed.
func (s *groupWelcomeService) effectiveConfig(ctx context.Context, groupNo string) (common.GroupWelcomeConfig, error) {
	row, found, err := s.cfgStore.Get(ctx, groupNo)
	if err != nil {
		return common.GroupWelcomeConfig{}, err
	}
	if !found || row == nil {
		return common.GroupWelcomeConfig{GroupNo: groupNo}, nil
	}
	return *row, nil
}

// enabledEffectiveConfigs returns the effective config for every group with an
// enabled welcome row (no global fallback to fold in).
func (s *groupWelcomeService) enabledEffectiveConfigs(ctx context.Context) ([]common.GroupWelcomeConfig, error) {
	lc, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	defer cancel()
	return s.cfgStore.ListEnabled(lc)
}

// handleMemberJoin is invoked from the GroupMemberAdd listener for each joining
// uid. When the group's config is enabled and the joiner is a first-join human
// member, it upserts a pending ledger row. Enqueue failure never blocks the join.
func (s *groupWelcomeService) handleMemberJoin(groupNo, uid string) {
	if s == nil || groupNo == "" || uid == "" {
		return
	}
	// Platform master switch (dark-launch default off). When off the event path
	// enqueues nothing, so no ledger rows accrue while the feature is dark.
	if !s.settings.GroupWelcomeEnabled() {
		return
	}
	cfgCtx, cancelCfg := context.WithTimeout(context.Background(), swDBCallTimeout)
	cfg, err := s.effectiveConfig(cfgCtx, groupNo)
	cancelCfg()
	if err != nil {
		s.Warn("group welcome enqueue effective config read failed",
			zap.String("stage", swStageEnqueue), zap.String("group_no", groupNo), zap.String("uid", uid), zap.Error(err))
		return
	}
	if !cfg.Enabled {
		return
	}
	if field := common.ValidateGroupWelcomeCombination(cfg); field != "" {
		s.metrics.incConfigInvalid()
		return
	}
	activeFrom, ok := cfg.ParsedActiveFrom()
	if !ok {
		s.metrics.incConfigInvalid()
		return
	}
	// System-bot exclusion is scoped strictly to the joiner uid.
	if spacepkg.IsSystemBot(uid) {
		return
	}

	callCtx, cancel := context.WithTimeout(context.Background(), swDBCallTimeout)
	defer cancel()
	eligible, err := s.store.firstJoinHumanMember(callCtx, groupNo, uid, activeFrom)
	if err != nil {
		s.Warn("group welcome enqueue eligibility check failed",
			zap.String("stage", swStageEnqueue), zap.String("group_no", groupNo), zap.String("uid", uid), zap.Error(err))
		return
	}
	if !eligible {
		return
	}
	s.enqueue(callCtx, groupNo, uid, swSourceEvent)
}

// enqueue upserts a pending row and splits enqueue_total vs enqueue_dedup_total.
// The row is held back from the worker until now+coalesceWindow so co-arriving
// joins collect into one post (see claimBatch / dispatchBatch).
func (s *groupWelcomeService) enqueue(ctx context.Context, groupNo, uid, source string) {
	now := s.now()
	inserted, err := s.store.upsertPending(ctx, groupNo, uid, now, now.Add(s.coalesceWindow))
	if err != nil {
		s.Warn("group welcome enqueue upsert failed",
			zap.String("stage", swStageEnqueue), zap.String("source", source),
			zap.String("group_no", groupNo), zap.String("uid", uid), zap.Error(err))
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

func (s *groupWelcomeService) reconcileLoop(ctx context.Context) {
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

// runReconcileOnce enumerates every group with an enabled config, re-validates
// each (fail closed per group), and scans it for active human members lacking a
// ledger row, enqueueing them under a shared global budget + per-group cap +
// rotating start cursor (fairness).
func (s *groupWelcomeService) runReconcileOnce(ctx context.Context) {
	// Platform master switch (dark-launch default off): no catch-up enqueue while
	// the feature is dark.
	if !s.settings.GroupWelcomeEnabled() {
		return
	}
	configs, err := s.enabledEffectiveConfigs(ctx)
	if err != nil {
		s.Warn("group welcome reconcile list enabled failed", zap.String("stage", swStageEnqueue), zap.Error(err))
		return
	}
	n := len(configs)
	if n == 0 {
		return
	}
	start := s.reconcileCursor % n
	s.reconcileCursor++
	budget := swReconcileCap

	for i := 0; i < n && budget > 0; i++ {
		if ctx.Err() != nil {
			return
		}
		cfg := configs[(start+i)%n]
		if !s.validateRuntime(ctx, cfg) {
			continue
		}
		activeFrom, _ := cfg.ParsedActiveFrom()
		per := budget
		if per > swReconcilePerSpaceCap {
			per = swReconcilePerSpaceCap
		}

		scanCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
		uids, scanErr := s.store.reconcileScan(scanCtx, cfg.GroupNo, activeFrom, spacepkg.SystemBotList(), per)
		cancel()
		if scanErr != nil {
			s.Warn("group welcome reconcile scan failed",
				zap.String("stage", swStageEnqueue), zap.String("group_no", cfg.GroupNo), zap.Error(scanErr))
			continue
		}
		for _, uid := range uids {
			if ctx.Err() != nil {
				return
			}
			callCtx, c := context.WithTimeout(ctx, swDBCallTimeout)
			s.enqueue(callCtx, cfg.GroupNo, uid, swSourceReconciler)
			c()
		}
		budget -= len(uids)
		if len(uids) > 0 {
			s.Info("group welcome reconcile enqueued",
				zap.String("group_no", cfg.GroupNo), zap.Int("candidates", len(uids)))
		}
	}
}

// ---------------------------------------------------------------------------
// Send worker
// ---------------------------------------------------------------------------

func (s *groupWelcomeService) workerLoop(ctx context.Context) {
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

// runWorkerWake sweeps stale rows across ALL groups, then round-robins over the
// enabled groups delivering ONE coalesced post per group per wake (up to
// swWorkerWakeCap posts total). Each group's due pending rows are claimed as a
// single batch and named in one post, so a burst of joins does not spam the
// channel; a group whose burst exceeds groupWelcomeCoalesceMax simply spills its
// tail into later wakes. Per-group enabled/validity is re-checked immediately
// before the claim so an admin disable / delete mid-wake takes effect at once (a
// batch already claimed completes). The rotating cursor keeps the enabled set
// fairly served when there are more groups than the per-wake post budget.
func (s *groupWelcomeService) runWorkerWake(ctx context.Context) {
	// Cross-group sweep runs unconditionally (even with the master switch off, so
	// lease-expired in-flight rows still reach a terminal/pending state).
	s.sweepAll(ctx)

	// Platform master switch (dark-launch default off): stop posting immediately.
	if !s.settings.GroupWelcomeEnabled() {
		return
	}

	configs, err := s.enabledEffectiveConfigs(ctx)
	if err != nil {
		s.Warn("group welcome worker list enabled failed", zap.String("stage", swStageClaim), zap.Error(err))
		return
	}
	if len(configs) == 0 {
		return
	}

	valid := make([]common.GroupWelcomeConfig, 0, len(configs))
	for _, cfg := range configs {
		if s.validateRuntime(ctx, cfg) {
			valid = append(valid, cfg)
		}
	}
	if len(valid) == 0 {
		return
	}

	vn := len(valid)
	wstart := s.workerCursor % vn
	s.workerCursor++

	posts := 0
	for i := 0; i < vn; i++ {
		if ctx.Err() != nil || posts >= swWorkerWakeCap {
			return
		}
		cfg := valid[(wstart+i)%vn]

		// Re-resolve the config right before claiming so a mid-wake disable/delete
		// or message edit takes effect immediately (the fresh body is what dispatch
		// posts).
		curCtx, curCancel := context.WithTimeout(ctx, swDBCallTimeout)
		cur, curErr := s.effectiveConfig(curCtx, cfg.GroupNo)
		curCancel()
		if curErr != nil {
			s.Warn("group welcome worker config re-read failed", zap.String("stage", swStageClaim), zap.String("group_no", cfg.GroupNo), zap.Error(curErr))
			continue
		}
		if !cur.Enabled {
			continue // disabled/deleted mid-wake
		}
		if field := common.ValidateGroupWelcomeCombination(cur); field != "" {
			s.metrics.incConfigInvalid()
			continue
		}

		claimCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
		batch, claimErr := s.store.claimBatch(claimCtx, cur.GroupNo, groupWelcomeCoalesceMax, s.now())
		cancel()
		if claimErr != nil {
			s.Warn("group welcome claim failed", zap.String("stage", swStageClaim), zap.String("group_no", cur.GroupNo), zap.Error(claimErr))
			continue
		}
		if len(batch) == 0 {
			continue // this group has nothing due — next group
		}
		posts++
		s.dispatchBatch(ctx, cur, batch)
	}
}

// sweepAll reclaims lease-expired claimed rows (-> pending) and promotes
// lease-expired dispatching rows (-> unknown) across ALL groups.
func (s *groupWelcomeService) sweepAll(ctx context.Context) {
	now := s.now()
	c1, cancel1 := context.WithTimeout(ctx, swDBCallTimeout)
	reclaimed, err := s.store.sweepClaimedAll(c1, now)
	cancel1()
	if err != nil {
		s.Warn("group welcome sweep claimed failed", zap.String("stage", swStageSweep), zap.Error(err))
	} else {
		s.metrics.addSweepClaimed(reclaimed)
	}

	c2, cancel2 := context.WithTimeout(ctx, swDBCallTimeout)
	promoted, err := s.store.sweepDispatchingAll(c2, now)
	cancel2()
	if err != nil {
		s.Warn("group welcome sweep dispatching failed", zap.String("stage", swStageSweep), zap.Error(err))
	} else if promoted > 0 {
		s.metrics.addSweepDispatching(promoted)
		s.Warn("group welcome dispatching rows swept to unknown", zap.Int64("count", promoted))
	}
}

// dispatch delivers a single claimed row (a batch of one). Retained as the
// one-row entry point used by focused tests; the worker always uses dispatchBatch.
func (s *groupWelcomeService) dispatch(ctx context.Context, cfg common.GroupWelcomeConfig, row *groupWelcomeRow) {
	s.dispatchBatch(ctx, cfg, []*groupWelcomeRow{row})
}

// dispatchBatch takes a batch of rows claimed for ONE group through the pre-send
// joiner re-check, the dispatching transition, a SINGLE coalesced group-channel
// post naming every eligible joiner, and the terminal transitions. Coalescing is
// what keeps a bulk invite from spamming the channel; each row still transitions
// independently (its own CAS), so at-most-once per (group_no, uid) holds.
func (s *groupWelcomeService) dispatchBatch(ctx context.Context, cfg common.GroupWelcomeConfig, rows []*groupWelcomeRow) {
	if len(rows) == 0 {
		return
	}

	// 1) Per-joiner pre-send re-check (bypasses cache; resolves the {member} name
	//    + eligibility). Ineligible joiners are skipped individually and excluded
	//    from the post; a DB error is a definitive pre-IM failure for that row.
	type eligibleRow struct {
		row  *groupWelcomeRow
		name string
	}
	eligible := make([]eligibleRow, 0, len(rows))
	for _, row := range rows {
		pcCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
		pc, err := s.store.precheckRecipient(pcCtx, row.UID, spacepkg.IsSystemBot)
		cancel()
		if err != nil {
			s.Warn("group welcome precheck failed", zap.String("stage", swStagePrecheck), zap.Int64("delivery_id", row.ID),
				zap.String("group_no", cfg.GroupNo), zap.String("uid", row.UID), zap.Error(err))
			s.preIMFailure(ctx, row, swErrConfigRead)
			continue
		}
		if !pc.eligible {
			s.skip(ctx, cfg, row, pc.errClass)
			continue
		}
		eligible = append(eligible, eligibleRow{row: row, name: pc.name})
	}
	if len(eligible) == 0 {
		return
	}

	// 2) Preflight the sender ONCE for the whole batch. Not ready / unconfigured is
	//    a definitive pre-IM failure for every eligible row (they back off to
	//    pending and are retried on a later wake).
	if s.botReady != nil {
		ready, ok := callWithTimeout(swBotReadyTimeout, s.botReady)
		if !ok || !ready {
			for _, e := range eligible {
				s.preIMFailure(ctx, e.row, swErrBotNotReady)
			}
			return
		}
	}
	if s.ctx.GetConfig().WuKongIM.APIURL == "" {
		for _, e := range eligible {
			s.preIMFailure(ctx, e.row, swErrConfigRead)
		}
		return
	}

	// 3) CAS each eligible row claimed -> dispatching. Only rows we still own are
	//    delivered (a peer sweep after lease expiry could have reclaimed one). The
	//    name list is built from the winners so the post names exactly who it is
	//    delivered to.
	winners := make([]*groupWelcomeRow, 0, len(eligible))
	names := make([]string, 0, len(eligible))
	for _, e := range eligible {
		dsCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
		ok, err := s.store.casToDispatching(dsCtx, e.row.ID, s.now())
		cancel()
		if err != nil {
			s.Warn("group welcome cas to dispatching failed", zap.String("stage", swStageDispatch), zap.Int64("delivery_id", e.row.ID), zap.Error(err))
			s.preIMFailure(ctx, e.row, swErrConfigRead)
			continue
		}
		if !ok {
			s.Warn("group welcome dispatch ownership lost before send", zap.String("stage", swStageDispatch), zap.Int64("delivery_id", e.row.ID))
			continue
		}
		winners = append(winners, e.row)
		names = append(names, e.name)
	}
	if len(winners) == 0 {
		return
	}

	// 4) ONE public group-channel post. The {member} placeholder renders to the
	//    joined name list (plain-text, no markdown). payload.type=1 Text; the
	//    notify-local sender base64-encodes Payload for /message/send.
	body := renderGroupWelcomeBody(cfg.Message, renderGroupWelcomeNameList(names))
	payload := map[string]interface{}{"type": 1, "content": body}
	req := &config.MsgSendReq{
		Header:      config.MsgHeader{RedDot: 1},
		FromUID:     s.fromUID,
		ChannelID:   cfg.GroupNo,
		ChannelType: libcommon.ChannelTypeGroup.Uint8(),
		Payload:     []byte(util.ToJson(payload)),
	}

	sendCtx := context.WithoutCancel(ctx)
	result, sendErr := s.sendFn(sendCtx, req)
	if sendErr != nil {
		class := swErrIMTimeout
		var se *swSendError
		if errors.As(sendErr, &se) {
			class = se.class
		}
		for _, row := range winners {
			s.toUnknown(ctx, row, class)
		}
		return
	}

	// 5) Persist success on every winner, sharing the one message's identifiers.
	for _, row := range winners {
		persistCtx, cancel := context.WithTimeout(ctx, swDBCallTimeout)
		sentOK, persistErr := s.store.casToSent(persistCtx, row.ID, result.messageID, result.clientMsgNo, s.now())
		cancel()
		if persistErr != nil {
			s.Error("group welcome sent persist failed", zap.String("stage", swStagePersist), zap.Int64("delivery_id", row.ID), zap.Error(persistErr))
			s.toUnknown(ctx, row, swErrSentPersist)
			continue
		}
		if !sentOK {
			s.Warn("group welcome sent persist ownership lost", zap.String("stage", swStagePersist), zap.Int64("delivery_id", row.ID))
			continue
		}
		s.metrics.incSendSuccess()
		s.Info("group welcome delivered", zap.String("stage", swStagePersist), zap.Int64("delivery_id", row.ID),
			zap.String("group_no", cfg.GroupNo), zap.String("uid", row.UID))
	}
}

// skip transitions a claimed row to skipped (joiner ineligible at pre-send) and
// records the metric. Shared by the batch dispatch path.
func (s *groupWelcomeService) skip(ctx context.Context, cfg common.GroupWelcomeConfig, row *groupWelcomeRow, errClass string) {
	skCtx, c := context.WithTimeout(ctx, swDBCallTimeout)
	ok, casErr := s.store.casToSkipped(skCtx, row.ID, errClass, s.now())
	c()
	if casErr != nil {
		s.Warn("group welcome cas to skipped failed", zap.String("stage", swStagePrecheck), zap.Int64("delivery_id", row.ID), zap.Error(casErr))
		return
	}
	if ok {
		s.metrics.incSkipNonMember()
		s.Info("group welcome joiner skipped", zap.String("stage", swStagePrecheck), zap.Int64("delivery_id", row.ID),
			zap.String("group_no", cfg.GroupNo), zap.String("uid", row.UID), zap.String("error_class", errClass))
	}
}

// preIMFailure records a definitive pre-IM failure (the only place attempts grows).
func (s *groupWelcomeService) preIMFailure(ctx context.Context, row *groupWelcomeRow, class string) {
	c, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	ok, err := s.store.casPreIMFailure(c, row.ID, row.Attempts, class, s.now())
	cancel()
	if err != nil {
		s.Warn("group welcome cas pre-IM failure failed", zap.Int64("delivery_id", row.ID), zap.String("error_class", class), zap.Error(err))
		return
	}
	if ok && row.Attempts+1 > swMaxPreIMAttempts {
		s.metrics.incSendFailed()
		s.Warn("group welcome delivery failed (retry budget exhausted)", zap.Int64("delivery_id", row.ID), zap.String("error_class", class))
	}
}

// toUnknown records a transport-ambiguous outcome (never auto-retried).
func (s *groupWelcomeService) toUnknown(ctx context.Context, row *groupWelcomeRow, class string) {
	c, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	ok, err := s.store.casToUnknown(c, row.ID, class, s.now())
	cancel()
	if err != nil {
		s.Error("group welcome cas to unknown failed", zap.Int64("delivery_id", row.ID), zap.String("error_class", class), zap.Error(err))
		return
	}
	if ok {
		s.metrics.incSendUnknown()
		s.Warn("group welcome delivery outcome unknown", zap.String("stage", swStageDispatch), zap.Int64("delivery_id", row.ID), zap.String("error_class", class))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// validateRuntime re-validates the enabled config (static combination) and that
// the target group exists and is active. Any failure fails the cycle closed; a
// config failure also increments config_invalid_total.
func (s *groupWelcomeService) validateRuntime(ctx context.Context, cfg common.GroupWelcomeConfig) bool {
	if field := common.ValidateGroupWelcomeCombination(cfg); field != "" {
		s.metrics.incConfigInvalid()
		s.Warn("group welcome config invalid; failing closed", zap.String("group_no", cfg.GroupNo), zap.String("field", field))
		return false
	}
	c, cancel := context.WithTimeout(ctx, swDBCallTimeout)
	active, err := s.store.groupActive(c, cfg.GroupNo)
	cancel()
	if err != nil {
		s.Warn("group welcome runtime group check failed", zap.String("group_no", cfg.GroupNo), zap.Error(err))
		return false
	}
	if !active {
		s.metrics.incConfigInvalid()
		s.Warn("group welcome target group inactive; failing closed", zap.String("group_no", cfg.GroupNo))
		return false
	}
	return true
}

// safeRun runs fn with panic recovery so one bad cycle cannot kill the loop.
func (s *groupWelcomeService) safeRun(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.Error("group welcome cycle panic", zap.String("cycle", what), zap.Any("recover", r))
		}
	}()
	fn()
}
