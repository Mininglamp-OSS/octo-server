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
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

type stubRobotService struct {
	mu sync.Mutex

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
	startOnce       sync.Once
}

func (s *stubRobotService) ExistRobot(uid string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.existCalls++
	return s.exists, s.existErr
}

func (s *stubRobotService) EnqueueBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (int64, error) {
	s.mu.Lock()
	s.enqueueCalls++
	s.robotID = robotID
	s.eventType = eventType
	s.eventData = eventData
	started := s.enqueueStarted
	continuation := s.enqueueContinue
	s.mu.Unlock()

	if started != nil {
		s.startOnce.Do(func() { close(started) })
	}
	if continuation != nil {
		<-continuation
	}
	if s.enqueueErr != nil {
		return 0, s.enqueueErr
	}
	return s.eventID, nil
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

func (m *fakeMetricRecorder) ObserveIngress(result string, _ time.Duration) {
	m.mu.Lock()
	m.ingress = append(m.ingress, result)
	m.mu.Unlock()
}

func (m *fakeMetricRecorder) ObserveEnqueue(result string) {
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
	confirmOK     bool
	confirmErr    error
	releaseOK     bool
	releaseErr    error

	lease        *claimLease
	confirmCalls int
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

func (s *scriptedClaimStore) Confirm(_ *claimLease, _ int64) (bool, error) {
	s.confirmCalls++
	return s.confirmOK, s.confirmErr
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
	r.GetRoute().ServeHTTP(w, req)
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
		if got := w.Header().Get("Retry-After"); got != "60" {
			t.Fatalf("Retry-After = %q, want 60", got)
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

	tests := []struct {
		name string
		body interface{}
	}{
		{name: "malformed json", body: []byte("{not-json")},
		{name: "oversized body", body: oversized},
		{name: "unsafe url", body: badURL},
		{name: "missing required field", body: missingBot},
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
		module := newTestBotMention(robots, &scriptedClaimStore{}, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
		w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
		if w.Code != http.StatusNotFound || decodeMentionError(t, w).Error.Code != "err.shared.not_found" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
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

func TestBotMentionEnqueueFailureReleasesClaim(t *testing.T) {
	store := &scriptedClaimStore{confirmOK: true, releaseOK: true}
	robots := &stubRobotService{exists: true, enqueueErr: errors.New("redis zadd failed")}
	module := newTestBotMention(robots, store, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
	if w.Code != http.StatusInternalServerError || decodeMentionError(t, w).Error.Code != "err.server.bot_mention.store_failed" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if store.releaseCalls != 1 {
		t.Fatalf("release calls = %d, want 1", store.releaseCalls)
	}
}

func TestBotMentionConfirmFailureDoesNotRollBackEnqueuedEvent(t *testing.T) {
	store := &scriptedClaimStore{confirmErr: errors.New("redis cas failed")}
	robots := &stubRobotService{exists: true, eventID: 777}
	module := newTestBotMention(robots, store, newFeatureGate(true, "", "*"), &fakeMetricRecorder{})
	w := doMentionRequest(t, newMentionRouter(module), "internal-secret", validMentionRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	response := decodeMentionResponse(t, w)
	if !response.Accepted || response.Replay || response.EventID != 777 {
		t.Fatalf("response=%+v", response)
	}
	if store.confirmCalls != 1 || store.releaseCalls != 0 {
		t.Fatalf("confirm=%d release=%d", store.confirmCalls, store.releaseCalls)
	}
}
