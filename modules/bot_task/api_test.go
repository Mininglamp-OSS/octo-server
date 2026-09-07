package bot_task

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
)

const testSourceToken = "0123456789abcdef0123456789abcdef"

type stubTaskRobotService struct {
	mu sync.Mutex

	exists     bool
	existErr   error
	prepareErr error
	eventID    int64

	existCalls   int
	prepareCalls int
	robotID      string
	eventType    string
	eventData    map[string]interface{}

	prepareStarted  chan struct{}
	prepareContinue chan struct{}
	startOnce       sync.Once
}

func (s *stubTaskRobotService) ExistRobot(string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.existCalls++
	return s.exists, s.existErr
}

func (s *stubTaskRobotService) PrepareBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (robot.PreparedBotTypedEvent, error) {
	s.mu.Lock()
	s.prepareCalls++
	s.robotID = robotID
	s.eventType = eventType
	s.eventData = eventData
	started := s.prepareStarted
	continuation := s.prepareContinue
	prepareErr := s.prepareErr
	eventID := s.eventID
	s.mu.Unlock()
	if prepareErr != nil {
		return robot.PreparedBotTypedEvent{}, prepareErr
	}
	if started != nil {
		s.startOnce.Do(func() { close(started) })
	}
	if continuation != nil {
		<-continuation
	}
	return robot.PreparedBotTypedEvent{
		EventID:  eventID,
		QueueKey: "robotEvent:" + robotID,
		Member:   fmt.Sprintf(`{"event_id":%d}`, eventID),
	}, nil
}

func (s *stubTaskRobotService) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.existCalls, s.prepareCalls
}

type taskErrorEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

type ambiguousTaskClaimStore struct {
	committed bool
	eventID   int64
}

func (s *ambiguousTaskClaimStore) Lookup(string, string) (claimOutcome, error) {
	if s.committed {
		return claimOutcome{State: claimReplay, EventID: s.eventID}, nil
	}
	return claimOutcome{State: claimMissing}, nil
}
func (s *ambiguousTaskClaimStore) Begin(string, string) (claimOutcome, *claimLease, error) {
	return claimOutcome{State: claimAcquired}, &claimLease{}, nil
}
func (s *ambiguousTaskClaimStore) Commit(_ *claimLease, event robot.PreparedBotTypedEvent) (bool, error) {
	s.committed = true
	s.eventID = event.EventID
	return false, errors.New("redis response lost after commit")
}
func (s *ambiguousTaskClaimStore) Release(*claimLease) (bool, error) { return true, nil }

func newTestBotTask(robots robotService, claims claimService) *BotTask {
	return &BotTask{
		robots: robots,
		claims: claims,
		sources: sourceRegistry{
			"loop": {Token: testSourceToken, Enabled: true, AllowedBotUIDs: []string{"bot-1"}},
		},
		now: func() time.Time { return time.Unix(1_788_768_000, 0) },
		Log: log.NewTLog("BotTaskTest"),
	}
}

func newTaskRouter(module *BotTask) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	// Route without the Redis-backed rate limiter so these unit tests exercise
	// only the handler contract.
	r.Group("/v1/internal").POST("/bot-tasks", module.create)
	return r
}

func validTaskRequest() taskRequest {
	return taskRequest{
		Source: "loop", TaskType: "loop_issue_comment_mention", IdempotencyKey: "comment-1:bot-1",
		BotUID: "bot-1", ActorUID: "user-1", SessionKey: "issue:1:thread:2",
		Prompt:   "Use octo-cli to handle the task and reply.",
		Context:  json.RawMessage(`{"issue_id":9007199254740993}`),
		Metadata: json.RawMessage(`{"schema_version":"future","anchor":"comment-1"}`),
	}
}

func doTaskRequest(t *testing.T, router *wkhttp.WKHttp, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch value := body.(type) {
	case []byte:
		raw = value
	default:
		var err error
		raw, err = json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/bot-tasks", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func decodeTaskError(t *testing.T, w *httptest.ResponseRecorder) taskErrorEnvelope {
	t.Helper()
	var response taskErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error %s: %v", w.Body.String(), err)
	}
	return response
}

func TestBotTaskAcceptedReplayAndPayload(t *testing.T) {
	backend := &memoryClaimBackend{values: map[string]string{}}
	robots := &stubTaskRobotService{exists: true, eventID: 4242}
	module := newTestBotTask(robots, &claimStore{backend: backend, doneTTL: time.Hour})
	notifications := 0
	module.notifyBotEvent = func(uid string) {
		if uid != "bot-1" {
			t.Fatalf("notified bot = %q", uid)
		}
		notifications++
	}
	router := newTaskRouter(module)

	first := doTaskRequest(t, router, testSourceToken, validTaskRequest())
	if first.Code != http.StatusAccepted {
		t.Fatalf("accepted status=%d body=%s", first.Code, first.Body.String())
	}
	second := doTaskRequest(t, router, testSourceToken, validTaskRequest())
	if second.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", second.Code, second.Body.String())
	}
	var replay ingressResponse
	if err := json.Unmarshal(second.Body.Bytes(), &replay); err != nil || !replay.Accepted || !replay.Replay || replay.EventID != 4242 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	exists, prepares := robots.counts()
	if exists != 1 || prepares != 1 || len(backend.events) != 1 || notifications != 1 {
		t.Fatalf("exists=%d prepares=%d events=%d notifications=%d", exists, prepares, len(backend.events), notifications)
	}
	if robots.robotID != "bot-1" || robots.eventType != botTaskEventType {
		t.Fatalf("target/type=%q/%q", robots.robotID, robots.eventType)
	}
	contextRaw, ok := robots.eventData["context"].(json.RawMessage)
	if !ok || string(contextRaw) != `{"issue_id":9007199254740993}` {
		t.Fatalf("context=%T(%s)", robots.eventData["context"], contextRaw)
	}
}

func TestBotTaskSameKeyDifferentLargeNumberConflicts(t *testing.T) {
	backend := &memoryClaimBackend{values: map[string]string{}}
	module := newTestBotTask(&stubTaskRobotService{exists: true, eventID: 9}, &claimStore{backend: backend, doneTTL: time.Hour})
	router := newTaskRouter(module)
	if w := doTaskRequest(t, router, testSourceToken, validTaskRequest()); w.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", w.Code, w.Body.String())
	}
	changed := validTaskRequest()
	changed.Context = json.RawMessage(`{"issue_id":9007199254740992}`)
	w := doTaskRequest(t, router, testSourceToken, changed)
	if w.Code != http.StatusConflict || decodeTaskError(t, w).Error.Code != "err.server.bot_task.idempotency_conflict" {
		t.Fatalf("conflict status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBotTaskConcurrentDuplicateEnqueuesOnce(t *testing.T) {
	backend := &memoryClaimBackend{values: map[string]string{}}
	robots := &stubTaskRobotService{
		exists:          true,
		eventID:         42,
		prepareStarted:  make(chan struct{}),
		prepareContinue: make(chan struct{}),
	}
	module := newTestBotTask(robots, &claimStore{backend: backend, doneTTL: time.Hour})
	router := newTaskRouter(module)
	body, err := json.Marshal(validTaskRequest())
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- doTaskRequest(t, router, testSourceToken, body) }()
	select {
	case <-robots.prepareStarted:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach event preparation")
	}

	for range 4 {
		w := doTaskRequest(t, router, testSourceToken, body)
		if w.Code != http.StatusConflict || w.Header().Get("Retry-After") != "2" {
			t.Fatalf("duplicate status=%d retry-after=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	}
	close(robots.prepareContinue)
	if w := <-firstDone; w.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", w.Code, w.Body.String())
	}
	_, prepares := robots.counts()
	if prepares != 1 {
		t.Fatalf("prepare calls=%d, want 1", prepares)
	}
}

func TestBotTaskRecoversAmbiguousCommitAsReplay(t *testing.T) {
	store := &ambiguousTaskClaimStore{}
	module := newTestBotTask(&stubTaskRobotService{exists: true, eventID: 77}, store)
	notifications := 0
	module.notifyBotEvent = func(string) { notifications++ }
	w := doTaskRequest(t, newTaskRouter(module), testSourceToken, validTaskRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response ingressResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || !response.Replay || response.EventID != 77 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if notifications != 1 {
		t.Fatalf("notifications=%d, want 1", notifications)
	}
}

func TestBotTaskRejectsUnauthorizedDisallowedAndMissingBot(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		request    taskRequest
		robots     *stubTaskRobotService
		wantStatus int
		wantCode   string
	}{
		{name: "missing token", request: validTaskRequest(), robots: &stubTaskRobotService{exists: true}, wantStatus: http.StatusUnauthorized, wantCode: "err.shared.auth.token_invalid"},
		{name: "wrong token", token: strings.Repeat("x", 32), request: validTaskRequest(), robots: &stubTaskRobotService{exists: true}, wantStatus: http.StatusUnauthorized, wantCode: "err.shared.auth.token_invalid"},
		{name: "disallowed bot", token: testSourceToken, request: func() taskRequest { r := validTaskRequest(); r.BotUID = "bot-2"; return r }(), robots: &stubTaskRobotService{exists: true}, wantStatus: http.StatusForbidden, wantCode: "err.server.bot_task.forbidden"},
		{name: "missing bot", token: testSourceToken, request: validTaskRequest(), robots: &stubTaskRobotService{exists: false}, wantStatus: http.StatusNotFound, wantCode: "err.shared.not_found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := doTaskRequest(t, newTaskRouter(newTestBotTask(tc.robots, &claimStore{backend: &memoryClaimBackend{values: map[string]string{}}, doneTTL: time.Hour})), tc.token, tc.request)
			if w.Code != tc.wantStatus || decodeTaskError(t, w).Error.Code != tc.wantCode {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestBotTaskRejectsMalformedAndOversizeBodies(t *testing.T) {
	module := newTestBotTask(&stubTaskRobotService{exists: true}, &claimStore{backend: &memoryClaimBackend{values: map[string]string{}}, doneTTL: time.Hour})
	router := newTaskRouter(module)
	for _, body := range [][]byte{[]byte("{not-json"), []byte(strings.Repeat("x", maxRequestBodyBytes+1))} {
		w := doTaskRequest(t, router, testSourceToken, body)
		if w.Code != http.StatusBadRequest || decodeTaskError(t, w).Error.Code != "err.shared.param.invalid" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestBotTaskPrepareFailureReleasesClaim(t *testing.T) {
	backend := &memoryClaimBackend{values: map[string]string{}}
	module := newTestBotTask(&stubTaskRobotService{exists: true, prepareErr: errors.New("allocate failed")}, &claimStore{backend: backend, doneTTL: time.Hour})
	w := doTaskRequest(t, newTaskRouter(module), testSourceToken, validTaskRequest())
	if w.Code != http.StatusInternalServerError || decodeTaskError(t, w).Error.Code != "err.server.bot_task.store_failed" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(backend.values) != 0 {
		t.Fatalf("claim was not released: %#v", backend.values)
	}
}
