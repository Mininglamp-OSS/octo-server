package bot_mention

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type stubRobotService struct {
	mu sync.Mutex

	// ownerUID / ownerErr 供 /v1/internal/bot-owner 用例设置。
	ownerUID   string
	ownerErr   error
	ownerCalls int

	exists     bool
	existErr   error
	enqueueErr error
	eventID    int64

	existCalls   int
	enqueueCalls int
	robotID      string
	eventType    string
	eventData    map[string]interface{}

	enqueueStarted  chan struct{}
	enqueueContinue chan struct{}
	beforeEnqueue   func()
	startOnce       sync.Once
}

type leaseRaceRobotService struct {
	mu sync.Mutex

	firstStarted chan struct{}
	releaseFirst chan struct{}
	enqueueCalls int
}

func (s *leaseRaceRobotService) ExistRobot(string) (bool, error) {
	return true, nil
}

func (s *leaseRaceRobotService) GetCreatorUID(string) (string, error) {
	return "", nil
}

func (s *leaseRaceRobotService) PrepareBotTypedEvent(robotID string, _ string, _ map[string]interface{}) (robot.PreparedBotTypedEvent, error) {
	s.mu.Lock()
	s.enqueueCalls++
	call := s.enqueueCalls
	s.mu.Unlock()

	if call == 1 {
		close(s.firstStarted)
		<-s.releaseFirst
	}

	eventID := int64(1000 + call)
	return robot.PreparedBotTypedEvent{
		EventID:  eventID,
		QueueKey: "robotEvent:" + robotID,
		Member:   fmt.Sprintf("event-%d", eventID),
	}, nil
}

type observingClaimBackend struct {
	*memoryClaimBackend
	staleCAS chan struct{}
	once     sync.Once
}

type ambiguousCommitClaimStore struct {
	mu sync.Mutex

	committed    bool
	eventID      int64
	commitCalls  int
	releaseCalls int
}

func (s *ambiguousCommitClaimStore) Lookup(string, string) (claimOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return claimOutcome{State: claimReplay, EventID: s.eventID}, nil
	}
	return claimOutcome{State: claimMissing}, nil
}

func (s *ambiguousCommitClaimStore) Begin(string, string) (claimOutcome, *claimLease, error) {
	return claimOutcome{State: claimAcquired}, &claimLease{}, nil
}

func (s *ambiguousCommitClaimStore) Commit(_ *claimLease, event robot.PreparedBotTypedEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.committed = true
	s.eventID = event.EventID
	s.commitCalls++
	return false, errors.New("redis response lost after atomic commit")
}

func (s *ambiguousCommitClaimStore) Release(*claimLease) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseCalls++
	return true, nil
}

func (s *ambiguousCommitClaimStore) counts() (commits, releases int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitCalls, s.releaseCalls
}

func (b *observingClaimBackend) CompareAndEnqueue(key, oldValue, newValue string, ttl time.Duration, event robot.PreparedBotTypedEvent) (bool, error) {
	ok, err := b.memoryClaimBackend.CompareAndEnqueue(key, oldValue, newValue, ttl, event)
	if err == nil && !ok {
		b.once.Do(func() { close(b.staleCAS) })
	}
	return ok, err
}

func (s *stubRobotService) ExistRobot(uid string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.existCalls++
	return s.exists, s.existErr
}

func (s *stubRobotService) GetCreatorUID(string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerCalls++
	return s.ownerUID, s.ownerErr
}

func (s *stubRobotService) PrepareBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (robot.PreparedBotTypedEvent, error) {
	s.mu.Lock()
	s.enqueueCalls++
	s.robotID = robotID
	s.eventType = eventType
	s.eventData = eventData
	started := s.enqueueStarted
	continuation := s.enqueueContinue
	beforeEnqueue := s.beforeEnqueue
	s.mu.Unlock()

	if beforeEnqueue != nil {
		beforeEnqueue()
	}
	if started != nil {
		s.startOnce.Do(func() { close(started) })
	}
	if continuation != nil {
		<-continuation
	}
	if s.enqueueErr != nil {
		return robot.PreparedBotTypedEvent{}, s.enqueueErr
	}
	return robot.PreparedBotTypedEvent{
		EventID:  s.eventID,
		QueueKey: "robotEvent:" + robotID,
		Member:   fmt.Sprintf("event-%d", s.eventID),
	}, nil
}

func (s *stubRobotService) counts() (exists, enqueues int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.existCalls, s.enqueueCalls
}

type fakeMetricRecorder struct {
	mu       sync.Mutex
	ingress  []string
	enqueues []string
}

type capturedLogEntry struct {
	level  string
	msg    string
	fields map[string]interface{}
}

type capturedLog struct {
	mu      sync.Mutex
	entries []capturedLogEntry
}

func (l *capturedLog) append(level, msg string, fields ...zap.Field) {
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range fields {
		field.AddTo(encoder)
	}
	l.mu.Lock()
	l.entries = append(l.entries, capturedLogEntry{level: level, msg: msg, fields: encoder.Fields})
	l.mu.Unlock()
}

func (l *capturedLog) Info(msg string, fields ...zap.Field) {
	l.append("info", msg, fields...)
}

func (l *capturedLog) Debug(msg string, fields ...zap.Field) {
	l.append("debug", msg, fields...)
}

func (l *capturedLog) Error(msg string, fields ...zap.Field) {
	l.append("error", msg, fields...)
}

func (l *capturedLog) Warn(msg string, fields ...zap.Field) {
	l.append("warn", msg, fields...)
}

func (l *capturedLog) snapshot() []capturedLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]capturedLogEntry(nil), l.entries...)
}

func (m *fakeMetricRecorder) ObserveIngress(result string, _ time.Duration) {
	m.mu.Lock()
	m.ingress = append(m.ingress, result)
	m.mu.Unlock()
}

func (m *fakeMetricRecorder) ObserveEnqueue(result string, _ time.Duration) {
	m.mu.Lock()
	m.enqueues = append(m.enqueues, result)
	m.mu.Unlock()
}

func (m *fakeMetricRecorder) snapshot() ([]string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.ingress...), append([]string(nil), m.enqueues...)
}

type scriptedClaimStore struct {
	lookupOutcome claimOutcome
	lookupErr     error
	beginOutcome  claimOutcome
	beginErr      error
	commitDenied  bool
	commitErr     error
	releaseOK     bool
	releaseErr    error

	lease        *claimLease
	commitCalls  int
	releaseCalls int
}

func (s *scriptedClaimStore) Lookup(_, _ string) (claimOutcome, error) {
	return s.lookupOutcome, s.lookupErr
}

func (s *scriptedClaimStore) Begin(_, _ string) (claimOutcome, *claimLease, error) {
	outcome := s.beginOutcome
	if outcome.State == claimMissing {
		outcome.State = claimAcquired
	}
	lease := s.lease
	if lease == nil {
		lease = &claimLease{}
	}
	return outcome, lease, s.beginErr
}

func (s *scriptedClaimStore) Commit(_ *claimLease, _ robot.PreparedBotTypedEvent) (bool, error) {
	s.commitCalls++
	return !s.commitDenied, s.commitErr
}

func (s *scriptedClaimStore) Release(_ *claimLease) (bool, error) {
	s.releaseCalls++
	return s.releaseOK, s.releaseErr
}

type mentionResponse struct {
	Accepted bool   `json:"accepted"`
	Replay   bool   `json:"replay"`
	EventID  int64  `json:"event_id"`
	Reason   string `json:"reason"`
}

type mentionErrorEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

func newTestBotMention(robots botMentionRobotService, claims botMentionClaimStore, gate featureGate, metrics botMentionMetricRecorder) *BotMention {
	return &BotMention{
		robots:        robots,
		claims:        claims,
		gate:          gate,
		internalToken: "internal-secret",
		metrics:       metrics,
		now:           func() time.Time { return time.Unix(1_785_460_000, 0) },
		Log:           log.NewTLog("BotMentionTest"),
	}
}

func newMentionRouter(module *BotMention) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	module.Route(r)
	return r
}

func validMentionRequest() mentionRequest {
	return mentionRequest{
		IdempotencyKey: "idem-1",
		DocID:          "doc-1",
		CommentID:      "comment-2",
		ParentID:       "comment-root",
		FromUID:        "human-1",
		BotUID:         "bot-1",
		Text:           "@bot please update the introduction",
		URL:            "https://docs.example.test/doc-1?comment=comment-2",
		SpaceID:        "space-1",
	}
}

func doMentionRequest(t *testing.T, r *wkhttp.WKHttp, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case []byte:
		raw = v
	default:
		var err error
		raw, err = json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/bot-mentions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(internalTokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeMentionResponse(t *testing.T, w *httptest.ResponseRecorder) mentionResponse {
	t.Helper()
	var response mentionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %s: %v", w.Body.String(), err)
	}
	return response
}

func decodeMentionError(t *testing.T, w *httptest.ResponseRecorder) mentionErrorEnvelope {
	t.Helper()
	var response mentionErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error %s: %v", w.Body.String(), err)
	}
	return response
}

func TestBotMentionAcceptedAndReplay(t *testing.T) {
	backend := newMemoryClaimBackend()
	claims := newClaimStore(backend, 7*24*time.Hour, deterministicTokens("lease-1"))
	robots := &stubRobotService{exists: true, eventID: 4242}
	metrics := &fakeMetricRecorder{}
	module := newTestBotMention(robots, claims, newFeatureGate(true, "space-1", ""), metrics)
	notifications := 0
	module.notifyBotEvent = func(robotID string) {
		if robotID != "bot-1" {
			t.Fatalf("notified robot = %q, want bot-1", robotID)
		}
		notifications++
	}
	router := newMentionRouter(module)

	first := doMentionRequest(t, router, "internal-secret", validMentionRequest())
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body=%s", first.Code, first.Body.String())
	}
	firstResp := decodeMentionResponse(t, first)
	if !firstResp.Accepted || firstResp.Replay || firstResp.EventID != 4242 {
		t.Fatalf("first response = %+v", firstResp)
	}

	second := doMentionRequest(t, router, "internal-secret", validMentionRequest())
	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body=%s", second.Code, second.Body.String())
	}
	replay := decodeMentionResponse(t, second)
	if !replay.Accepted || !replay.Replay || replay.EventID != 4242 {
		t.Fatalf("replay response = %+v", replay)
	}

	existCalls, enqueueCalls := robots.counts()
	if existCalls != 1 || enqueueCalls != 1 {
		t.Fatalf("robot calls exist=%d enqueue=%d, want 1/1", existCalls, enqueueCalls)
	}
	if robots.robotID != "bot-1" || robots.eventType != docCommentMentionEventType {
		t.Fatalf("enqueued target/type = %q/%q", robots.robotID, robots.eventType)
	}
	if robots.eventData["thread_id"] != "comment-root" || robots.eventData["idempotency_key"] != "idem-1" {
		t.Fatalf("event data missing canonical identity: %#v", robots.eventData)
	}
	if robots.eventData["enqueued_at"] != int64(1_785_460_000) {
		t.Fatalf("enqueued_at = %#v", robots.eventData["enqueued_at"])
	}
	ingress, enqueues := metrics.snapshot()
	if fmt.Sprint(ingress) != "[accepted replay]" || fmt.Sprint(enqueues) != "[accepted]" {
		t.Fatalf("metrics ingress=%v enqueue=%v", ingress, enqueues)
	}
	if notifications != 1 {
		t.Fatalf("doorbell notifications = %d, want 1 for the committed event", notifications)
	}
}

func TestBotMentionConcurrentDuplicateHasSingleEnqueue(t *testing.T) {
	backend := newMemoryClaimBackend()
	claims := newClaimStore(backend, time.Hour, deterministicTokens("lease-1"))
	robots := &stubRobotService{
		exists:          true,
		eventID:         4242,
		enqueueStarted:  make(chan struct{}),
		enqueueContinue: make(chan struct{}),
	}
	module := newTestBotMention(robots, claims, newFeatureGate(true, "", "doc-1"), &fakeMetricRecorder{})
	router := newMentionRouter(module)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- doMentionRequest(t, router, "internal-secret", validMentionRequest())
	}()
	<-robots.enqueueStarted

	const duplicates = 8
	results := make(chan *httptest.ResponseRecorder, duplicates)
	for i := 0; i < duplicates; i++ {
		go func() {
			results <- doMentionRequest(t, router, "internal-secret", validMentionRequest())
		}()
	}
	for i := 0; i < duplicates; i++ {
		w := <-results
		if w.Code != http.StatusConflict {
			t.Fatalf("in-flight duplicate status = %d, body=%s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "2" {
			t.Fatalf("Retry-After = %q, want 2", got)
		}
	}
	close(robots.enqueueContinue)
	if w := <-firstDone; w.Code != http.StatusOK {
		t.Fatalf("first status = %d, body=%s", w.Code, w.Body.String())
	}
	_, enqueueCalls := robots.counts()
	if enqueueCalls != 1 {
		t.Fatalf("enqueue calls = %d, want 1", enqueueCalls)
	}
}

func TestBotMentionAuthFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		serverToken  string
		requestToken string
	}{
		{name: "token not configured", serverToken: "", requestToken: "internal-secret"},
		{name: "token missing", serverToken: "internal-secret"},
		{name: "token wrong", serverToken: "internal-secret", requestToken: "wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			robots := &stubRobotService{exists: true, eventID: 1}
			module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
			module.internalToken = tc.serverToken
			w := doMentionRequest(t, newMentionRouter(module), tc.requestToken, validMentionRequest())
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			if got := decodeMentionError(t, w).Error.Code; got != "err.shared.auth.token_invalid" {
				t.Fatalf("code = %q", got)
			}
			if _, enqueues := robots.counts(); enqueues != 0 {
				t.Fatalf("enqueue calls = %d", enqueues)
			}
		})
	}
}

func TestBotMentionRejectsInvalidRequests(t *testing.T) {
	base := validMentionRequest()
	oversized := validMentionRequest()
	oversized.Text = strings.Repeat("x", maxRequestBodyBytes)
	badURL := validMentionRequest()
	badURL.URL = "file:///etc/passwd"
	missingBot := validMentionRequest()
	missingBot.BotUID = ""
	unknownField := map[string]interface{}{
		"idempotency_key": "idem-1",
		"doc_id":          "doc-1",
		"comment_id":      "comment-1",
		"from_uid":        "human-1",
		"bot_uid":         "bot-1",
		"text":            "update it",
		"thread_id":       "caller-controlled-thread",
	}
	validRaw, err := json.Marshal(validMentionRequest())
	if err != nil {
		t.Fatal(err)
	}
	trailingJSON := append(append([]byte(nil), validRaw...), []byte(`{"extra":true}`)...)

	tests := []struct {
		name string
		body interface{}
	}{
		{name: "malformed json", body: []byte("{not-json")},
		{name: "oversized body", body: oversized},
		{name: "unsafe url", body: badURL},
		{name: "missing required field", body: missingBot},
		{name: "unknown field", body: unknownField},
		{name: "trailing json value", body: trailingJSON},
		{name: "empty body", body: []byte{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			robots := &stubRobotService{exists: true, eventID: 1}
			module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
			w := doMentionRequest(t, newMentionRouter(module), "internal-secret", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			if got := decodeMentionError(t, w).Error.Code; got != "err.shared.param.invalid" {
				t.Fatalf("code = %q", got)
			}
			if _, enqueues := robots.counts(); enqueues != 0 {
				t.Fatalf("enqueue calls = %d", enqueues)
			}
		})
	}
	_ = base
}

func TestBotMentionDisabledIsDistinguishableAndDoesNotClaim(t *testing.T) {
	backend := newMemoryClaimBackend()
	claims := newClaimStore(backend, time.Hour, deterministicTokens("unused"))
	robots := &stubRobotService{exists: true, eventID: 1}
	module := newTestBotMention(robots, claims, newFeatureGate(false, "", "*"), &fakeMetricRecorder{})
	w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	response := decodeMentionResponse(t, w)
	if response.Accepted || response.Replay || response.Reason != "disabled" {
		t.Fatalf("response = %+v", response)
	}
	if len(backend.values) != 0 {
		t.Fatalf("disabled request wrote claims: %#v", backend.values)
	}
	if exists, enqueues := robots.counts(); exists != 0 || enqueues != 0 {
		t.Fatalf("robot calls exist=%d enqueue=%d", exists, enqueues)
	}
}

func TestBotMentionRejectsUnavailableUserBotAndDependencyFailure(t *testing.T) {
	t.Run("non active user bot is opaque not found", func(t *testing.T) {
		robots := &stubRobotService{exists: false}
		metrics := &fakeMetricRecorder{}
		module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), metrics)
		w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
		if w.Code != http.StatusNotFound || decodeMentionError(t, w).Error.Code != "err.shared.not_found" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		ingress, _ := metrics.snapshot()
		if fmt.Sprint(ingress) != "[not_found]" {
			t.Fatalf("ingress metrics = %v, want [not_found]", ingress)
		}
	})

	t.Run("robot lookup failure is internal", func(t *testing.T) {
		robots := &stubRobotService{existErr: errors.New("mysql unavailable")}
		module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
		w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
		if w.Code != http.StatusInternalServerError || decodeMentionError(t, w).Error.Code != "err.server.bot_mention.store_failed" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	})
}

func TestBotMentionStructuredOutcomeLogsDoNotLeakSensitivePayload(t *testing.T) {
	request := validMentionRequest()
	request.IdempotencyKey = "secret-idempotency-key"
	request.Text = "sensitive full comment body"
	request.URL = "https://docs.example.test/sensitive-location"

	logger := &capturedLog{}
	backend := newMemoryClaimBackend()
	claims := newClaimStore(backend, time.Hour, deterministicTokens("lease-1"))
	module := newTestBotMention(
		&stubRobotService{exists: true, eventID: 4242},
		claims,
		newFeatureGate(true, "", "*"),
		&fakeMetricRecorder{},
	)
	module.Log = logger
	router := newMentionRouter(module)

	for _, wantReplay := range []bool{false, true} {
		response := doMentionRequest(t, router, "internal-secret", request)
		if response.Code != http.StatusOK {
			t.Fatalf("ingress status = %d, body=%s", response.Code, response.Body.String())
		}
		if got := decodeMentionResponse(t, response).Replay; got != wantReplay {
			t.Fatalf("replay = %v, want %v", got, wantReplay)
		}
	}

	disabled := newTestBotMention(
		&stubRobotService{exists: true, eventID: 1},
		newClaimStore(newMemoryClaimBackend(), time.Hour, deterministicTokens("unused")),
		newFeatureGate(false, "", "*"),
		&fakeMetricRecorder{},
	)
	disabled.Log = logger
	if response := doMentionRequest(t, newMentionRouter(disabled), "internal-secret", request); response.Code != http.StatusOK {
		t.Fatalf("disabled status = %d, body=%s", response.Code, response.Body.String())
	}

	failed := newTestBotMention(
		&stubRobotService{existErr: errors.New("mysql unavailable")},
		&scriptedClaimStore{},
		newFeatureGate(true, "", "*"),
		&fakeMetricRecorder{},
	)
	failed.Log = logger
	if response := doMentionRequest(t, newMentionRouter(failed), "internal-secret", request); response.Code != http.StatusInternalServerError {
		t.Fatalf("failed status = %d, body=%s", response.Code, response.Body.String())
	}

	entries := logger.snapshot()
	wantResults := map[string]bool{"accepted": false, "replay": false, "disabled": false}
	for _, entry := range entries {
		result, _ := entry.fields["result"].(string)
		if _, ok := wantResults[result]; !ok || entry.level != "info" {
			continue
		}
		wantResults[result] = true
		if entry.fields["bot_uid"] != request.BotUID {
			t.Fatalf("%s bot_uid = %#v", result, entry.fields["bot_uid"])
		}
		if _, ok := entry.fields["event_id"]; !ok {
			t.Fatalf("%s log missing event_id: %#v", result, entry.fields)
		}
		hash, _ := entry.fields["idempotency_hash"].(string)
		if len(hash) != 12 || strings.Contains(hash, request.IdempotencyKey) {
			t.Fatalf("%s idempotency_hash = %q", result, hash)
		}
	}
	for result, seen := range wantResults {
		if !seen {
			t.Fatalf("missing structured %s outcome log: %#v", result, entries)
		}
	}

	serialized := fmt.Sprint(entries)
	for _, sensitive := range []string{"internal-secret", request.IdempotencyKey, request.Text, request.URL} {
		if strings.Contains(serialized, sensitive) {
			t.Fatalf("logs leaked sensitive value %q: %s", sensitive, serialized)
		}
	}
}

func TestBotMentionClaimOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		store      *scriptedClaimStore
		wantStatus int
		wantCode   string
		wantReplay bool
	}{
		{name: "lookup pending", store: &scriptedClaimStore{lookupOutcome: claimOutcome{State: claimPending}}, wantStatus: http.StatusConflict, wantCode: "err.server.bot_mention.in_progress"},
		{name: "lookup conflict", store: &scriptedClaimStore{lookupOutcome: claimOutcome{State: claimConflict}}, wantStatus: http.StatusConflict, wantCode: "err.server.bot_mention.idempotency_conflict"},
		{name: "lookup failure", store: &scriptedClaimStore{lookupErr: errors.New("redis unavailable")}, wantStatus: http.StatusInternalServerError, wantCode: "err.server.bot_mention.store_failed"},
		{name: "lookup replay", store: &scriptedClaimStore{lookupOutcome: claimOutcome{State: claimReplay, EventID: 99}}, wantStatus: http.StatusOK, wantReplay: true},
		{name: "begin pending race", store: &scriptedClaimStore{beginOutcome: claimOutcome{State: claimPending}}, wantStatus: http.StatusConflict, wantCode: "err.server.bot_mention.in_progress"},
		{name: "begin conflict race", store: &scriptedClaimStore{beginOutcome: claimOutcome{State: claimConflict}}, wantStatus: http.StatusConflict, wantCode: "err.server.bot_mention.idempotency_conflict"},
		{name: "begin failure", store: &scriptedClaimStore{beginErr: errors.New("redis unavailable")}, wantStatus: http.StatusInternalServerError, wantCode: "err.server.bot_mention.store_failed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			robots := &stubRobotService{exists: true, eventID: 1}
			module := newTestBotMention(robots, tc.store, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
			w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantReplay {
				response := decodeMentionResponse(t, w)
				if !response.Accepted || !response.Replay || response.EventID != 99 {
					t.Fatalf("response = %+v", response)
				}
				return
			}
			if got := decodeMentionError(t, w).Error.Code; got != tc.wantCode {
				t.Fatalf("code=%q want=%q", got, tc.wantCode)
			}
		})
	}
}

func TestBotMentionPrepareFailureReleasesClaim(t *testing.T) {
	store := &scriptedClaimStore{releaseOK: true}
	robots := &stubRobotService{exists: true, enqueueErr: errors.New("allocate event ID failed")}
	module := newTestBotMention(robots, store, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
	if w.Code != http.StatusInternalServerError || decodeMentionError(t, w).Error.Code != "err.server.bot_mention.store_failed" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if store.releaseCalls != 1 {
		t.Fatalf("release calls = %d, want 1", store.releaseCalls)
	}
}

func TestBotMentionStaleCommitReturnsInProgress(t *testing.T) {
	store := &scriptedClaimStore{commitDenied: true}
	robots := &stubRobotService{exists: true, eventID: 4242}
	module := newTestBotMention(robots, store, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	response := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
	if response.Code != http.StatusConflict || decodeMentionError(t, response).Error.Code != "err.server.bot_mention.in_progress" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if store.commitCalls != 1 || store.releaseCalls != 0 {
		t.Fatalf("commit=%d release=%d, want 1 and 0", store.commitCalls, store.releaseCalls)
	}
}

func TestBotMentionLostLeaseDuringBlockedEnqueueReplaysSingleVisibleEvent(t *testing.T) {
	backend := &observingClaimBackend{
		memoryClaimBackend: newMemoryClaimBackend(),
		staleCAS:           make(chan struct{}),
	}
	store := newClaimStore(backend, time.Hour, deterministicTokens("lease-old", "lease-new"))
	robots := &leaseRaceRobotService{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	module := newTestBotMention(robots, store, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	router := newMentionRouter(module)
	request := validMentionRequest()
	claimKey := mentionClaimKey(request.BotUID, request.IdempotencyKey)

	firstResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstResult <- doMentionRequest(t, router, "internal-secret", request)
	}()
	select {
	case <-robots.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first enqueue did not block")
	}

	backend.forceDelete(claimKey)
	second := doMentionRequest(t, router, "internal-secret", request)
	if second.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", second.Code, second.Body.String())
	}
	secondResponse := decodeMentionResponse(t, second)
	if !secondResponse.Accepted || secondResponse.Replay {
		t.Fatalf("retry response=%+v, want newly accepted event", secondResponse)
	}
	close(robots.releaseFirst)
	select {
	case <-backend.staleCAS:
	case <-time.After(time.Second):
		t.Fatal("blocked attempt did not observe lease loss")
	}

	var first *httptest.ResponseRecorder
	select {
	case first = <-firstResult:
	case <-time.After(time.Second):
		t.Fatal("first request did not finish")
	}
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	firstResponse := decodeMentionResponse(t, first)
	if !firstResponse.Accepted || !firstResponse.Replay || firstResponse.EventID != secondResponse.EventID {
		t.Fatalf("first response=%+v, want replay of event_id %d", firstResponse, secondResponse.EventID)
	}
	if visible := backend.queuedEventIDs("robotEvent:" + request.BotUID); len(visible) != 1 || visible[0] != secondResponse.EventID {
		t.Fatalf("visible event IDs=%v, want only %d", visible, secondResponse.EventID)
	}
}

func TestBotMentionAtomicCommitFailureFailsClosedWithoutRelease(t *testing.T) {
	store := &scriptedClaimStore{commitErr: errors.New("redis eval failed")}
	robots := &stubRobotService{exists: true, eventID: 777}
	module := newTestBotMention(robots, store, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
	if w.Code != http.StatusInternalServerError || decodeMentionError(t, w).Error.Code != "err.server.bot_mention.store_failed" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if store.commitCalls != 1 || store.releaseCalls != 0 {
		t.Fatalf("commit=%d release=%d, want 1 and 0", store.commitCalls, store.releaseCalls)
	}
}

func TestBotMentionRecoversAmbiguousAtomicCommitAsReplay(t *testing.T) {
	store := &ambiguousCommitClaimStore{}
	robots := &stubRobotService{exists: true, eventID: 777}
	module := newTestBotMention(robots, store, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	notifications := 0
	module.notifyBotEvent = func(string) { notifications++ }
	w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	response := decodeMentionResponse(t, w)
	if !response.Accepted || !response.Replay || response.EventID != 777 {
		t.Fatalf("response=%+v, want replay of event 777", response)
	}
	commits, releases := store.counts()
	if commits != 1 || releases != 0 {
		t.Fatalf("commit=%d release=%d, want 1 and 0", commits, releases)
	}
	if notifications != 1 {
		t.Fatalf("doorbell notifications = %d, want 1 after ambiguous commit recovery", notifications)
	}
}

const ownerProbeToken = "owner-probe-token"

// ── POST /v1/internal/bot-owner ────────────────────────────────────────────────
// 回答「某个 Bot 的主人是谁」。存在的理由:HTML 文档的权限判定要的是 Bot 的主人,而
// octo-doc 只拿到一个 bot_uid —— 它换 OwnerUID 的唯一途径 VerifyBot 需要 Bot 的 token,
// 服务间调用没有。归属数据(robot.creator_uid)只在这一侧。
//
// 最该钉住的是**判不出时不能装作判出来**:调用方拿这个答案决定放不放行,
// 空串对它就是「不知道」⇒ 拒绝。所以查不到给空串 + 200,查错了必须报错而不是给空串。

func postBotOwner(t *testing.T, r *wkhttp.WKHttp, token, botUID string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"bot_uid": botUID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/bot-owner", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(internalTokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func ownerUIDFromResponse(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		OwnerUID string `json:"owner_uid"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode owner response: %v (body=%s)", err, w.Body.String())
	}
	return body.OwnerUID
}

func TestBotOwnerReturnsCreatorUID(t *testing.T) {
	robots := &stubRobotService{exists: true, ownerUID: "human-1"}
	module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	module.internalToken = ownerProbeToken
	w := postBotOwner(t, newMentionRouter(module), ownerProbeToken, "bot-1")
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200,得到 %d (%s)", w.Code, w.Body.String())
	}
	if got := ownerUIDFromResponse(t, w); got != "human-1" {
		t.Fatalf("owner_uid = %q, want human-1", got)
	}
}

func TestBotOwnerUnknownBotIsEmptyNotError(t *testing.T) {
	// ★ 查不到给空串 + 200,而不是 404。调用方对空串的处理是「判不出 ⇒ 拒绝」;
	// 用 404 会被它当成「服务不可用」而重试,那是另一种语义。
	robots := &stubRobotService{exists: true, ownerUID: ""}
	module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	module.internalToken = ownerProbeToken
	w := postBotOwner(t, newMentionRouter(module), ownerProbeToken, "bot-unknown")
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200,得到 %d", w.Code)
	}
	if got := ownerUIDFromResponse(t, w); got != "" {
		t.Fatalf("owner_uid = %q, want 空串", got)
	}
}

func TestBotOwnerLookupFailureIsAnError(t *testing.T) {
	// 查询本身失败**不能**降级成空串:那会让调用方把「数据库抖了一下」当成
	// 「这个 Bot 没有主人」,并据此永久拒绝。必须让它看到 5xx 并重试。
	robots := &stubRobotService{exists: true, ownerErr: errors.New("db down")}
	module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	module.internalToken = ownerProbeToken
	w := postBotOwner(t, newMentionRouter(module), ownerProbeToken, "bot-1")
	if w.Code < 400 {
		t.Fatalf("查询失败应当是错误响应,得到 %d", w.Code)
	}
	if got := ownerUIDFromResponse(t, w); got != "" {
		t.Fatalf("失败时不该带 owner_uid,得到 %q", got)
	}
}

func TestBotOwnerRequiresInternalToken(t *testing.T) {
	robots := &stubRobotService{exists: true, ownerUID: "human-1"}
	module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	module.internalToken = ownerProbeToken
	for _, tok := range []string{"", "wrong-token"} {
		w := postBotOwner(t, newMentionRouter(module), tok, "bot-1")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("token=%q 应当 401,得到 %d", tok, w.Code)
		}
		if robots.ownerCalls != 0 {
			t.Fatal("鉴权失败时不该去查归属")
		}
	}
}

func TestBotOwnerValidatesBotUID(t *testing.T) {
	robots := &stubRobotService{exists: true, ownerUID: "human-1"}
	module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	module.internalToken = ownerProbeToken
	for _, uid := range []string{"", "   "} {
		w := postBotOwner(t, newMentionRouter(module), ownerProbeToken, uid)
		if w.Code < 400 {
			t.Fatalf("bot_uid=%q 应当 4xx,得到 %d", uid, w.Code)
		}
	}
}
