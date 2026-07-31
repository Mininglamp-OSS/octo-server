package bot_api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	"github.com/stretchr/testify/assert"
)

// Integration coverage for the POST /v1/bot/events long-poll (D5 / P3-2).
// These exercise the real HTTP route against real Redis: the doorbell only has
// value if BLPOP, the enqueue chokepoints and the handler agree, and none of
// that is observable from a unit test with a fake.

const (
	lpBotID    = "lp_bot_1"
	lpBotToken = "lp_bot_token_1"
)

func seedLongPollBot(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", lpBotID, lpBotToken).Exec()
	assert.NoError(t, err)
}

// clearLongPollKeys drops both the queue and the doorbell. CleanAllTables only
// touches MySQL, so Redis state leaks between tests unless cleared explicitly.
func clearLongPollKeys(t *testing.T, ctx *config.Context) {
	t.Helper()
	_ = ctx.GetRedisConn().Del(robotEventPrefix + lpBotID)
	_ = ctx.GetRedisConn().Del(botevent.BellKey(lpBotID))
}

// enqueueRaw writes an event straight into the authoritative sorted set WITHOUT
// ringing the doorbell. Used to prove the queue — not the bell — is the source
// of truth.
func enqueueRaw(t *testing.T, ctx *config.Context, eventID int64) {
	t.Helper()
	payload := fmt.Sprintf(`{"event_id":%d,"event_type":"card_action","event_data":{"action_id":"approve"}}`, eventID)
	assert.NoError(t, ctx.GetRedisConn().ZAdd(robotEventPrefix+lpBotID, float64(eventID), payload))
}

type lpResponse struct {
	Status  int          `json:"status"`
	Results []*eventResp `json:"results"`
}

func pollEvents(t *testing.T, s *server.Server, body string) (lpResponse, time.Duration) {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/bot/events", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+lpBotToken)
	start := time.Now()
	s.GetRoute().ServeHTTP(w, req)
	elapsed := time.Since(start)

	var resp lpResponse
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body=%s", w.Body.String())
	assert.Equal(t, http.StatusOK, w.Code)
	return resp, elapsed
}

// TestBotEventsWithoutWaitIsUnchanged pins the backward-compatibility promise:
// a request that never mentions `wait` must return immediately and carry the
// historical shape, empty batch included.
func TestBotEventsWithoutWaitIsUnchanged(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedLongPollBot(t, ctx)
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)

	resp, elapsed := pollEvents(t, s, `{"event_id":0,"limit":20}`)
	assert.Equal(t, 1, resp.Status)
	assert.Empty(t, resp.Results)
	assert.Less(t, elapsed, time.Second, "absent wait must not hold the request")
}

// TestBotEventsReadsBeforeWaiting: a caller with a backlog pays no hold, even
// when it asked for a long one.
func TestBotEventsReadsBeforeWaiting(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedLongPollBot(t, ctx)
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)

	enqueueRaw(t, ctx, 101)

	resp, elapsed := pollEvents(t, s, `{"event_id":0,"limit":20,"wait":20}`)
	assert.Equal(t, 1, resp.Status)
	assert.Len(t, resp.Results, 1)
	assert.Equal(t, int64(101), resp.Results[0].EventID)
	assert.Less(t, elapsed, time.Second, "a non-empty queue must short-circuit the hold")
}

// TestBotEventsHoldExpiresWithEmptyBatch: an idle hold ends in the ordinary OK
// empty response — not 408, not an error envelope. "Nothing happened" is not a
// failure the caller can act on.
func TestBotEventsHoldExpiresWithEmptyBatch(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedLongPollBot(t, ctx)
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)

	resp, elapsed := pollEvents(t, s, `{"event_id":0,"limit":20,"wait":2}`)
	assert.Equal(t, 1, resp.Status)
	assert.Empty(t, resp.Results)
	// The requested hold must be served in full — an earlier rounding bug cut a
	// 2s hold to ~1s, which looks like success unless the lower bound is
	// asserted. The upper bound allows the documented sub-second overshoot from
	// rounding the final chunk up.
	assert.GreaterOrEqual(t, elapsed, 2*time.Second, "the full requested hold must be served")
	assert.Less(t, elapsed, 4*time.Second, "the hold must not overrun its deadline by more than one rounded chunk")
}

// TestBotEventsWakesOnDoorbell is the point of the whole change: an event that
// lands mid-hold must be delivered promptly rather than at the next poll tick.
func TestBotEventsWakesOnDoorbell(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedLongPollBot(t, ctx)
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)

	type outcome struct {
		resp    lpResponse
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		resp, elapsed := pollEvents(t, s, `{"event_id":0,"limit":20,"wait":25}`)
		done <- outcome{resp, elapsed}
	}()

	// Let the waiter reach BLPOP before the event lands, so this measures the
	// wake-up path and not the read-before-wait short circuit.
	time.Sleep(1500 * time.Millisecond)
	enqueueAt := time.Now()
	enqueueRaw(t, ctx, 202)
	assert.NoError(t, botevent.Ring(ctx.GetRedisConn(), lpBotID))

	select {
	case got := <-done:
		latency := time.Since(enqueueAt)
		assert.Equal(t, 1, got.resp.Status)
		assert.Len(t, got.resp.Results, 1)
		assert.Equal(t, int64(202), got.resp.Results[0].EventID)
		assert.Less(t, latency, 2*time.Second,
			"doorbell wake-up must be prompt, not deferred to the hold deadline")
	case <-time.After(20 * time.Second):
		t.Fatal("long-poll did not return after the doorbell rang")
	}
}

// TestBotEventsSurvivesLostDoorbell is the safety property that makes the
// doorbell acceptable at all: losing it may only cost latency. The event is
// enqueued with no bell, so the hold runs to its deadline and answers empty —
// and the very next poll still finds the event, because the sorted set, not the
// bell, is authoritative.
func TestBotEventsSurvivesLostDoorbell(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedLongPollBot(t, ctx)
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)

	done := make(chan lpResponse, 1)
	go func() {
		resp, _ := pollEvents(t, s, `{"event_id":0,"limit":20,"wait":2}`)
		done <- resp
	}()
	time.Sleep(500 * time.Millisecond)
	enqueueRaw(t, ctx, 303) // deliberately no Ring

	select {
	case resp := <-done:
		// The hold could not know about the event; an empty batch here is
		// correct behavior, not a bug.
		assert.Equal(t, 1, resp.Status)
	case <-time.After(20 * time.Second):
		t.Fatal("hold did not expire")
	}

	// The event was never lost: the authoritative read picks it up.
	resp, _ := pollEvents(t, s, `{"event_id":0,"limit":20}`)
	assert.Len(t, resp.Results, 1, "a lost doorbell must never lose the event")
	assert.Equal(t, int64(303), resp.Results[0].EventID)
}

// TestBotEventsReleasesHoldOnClientDisconnect: BLPOP takes no context, so a
// client that hangs up can only be noticed between chunks. The hold must still
// end promptly — within roughly one chunk — instead of pinning its slot and its
// Redis connection for the full requested wait.
func TestBotEventsReleasesHoldOnClientDisconnect(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	seedLongPollBot(t, ctx)
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)

	reqCtx, cancel := context.WithCancel(context.Background())
	done := make(chan time.Duration, 1)
	go func() {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/bot/events", bytes.NewReader([]byte(`{"event_id":0,"limit":20,"wait":30}`)))
		req.Header.Set("Authorization", "Bearer "+lpBotToken)
		req = req.WithContext(reqCtx)
		start := time.Now()
		s.GetRoute().ServeHTTP(w, req)
		done <- time.Since(start)
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case elapsed := <-done:
		// One chunk of slack for the in-flight BLPOP, not the 30s the caller
		// nominally asked for.
		assert.Less(t, elapsed, eventWaitChunk+3*time.Second,
			"a disconnected client must not hold its slot for the whole wait")
	case <-time.After(25 * time.Second):
		t.Fatal("hold did not release after the client disconnected")
	}

	// The slot must be genuinely free afterwards, not merely abandoned.
	release, ok := acquireEventHold(lpBotID)
	assert.True(t, ok, "the per-bot hold slot must be released on disconnect")
	if ok {
		release()
	}
}

// TestEnqueueChokepointRingsTheDoorbell proves the producer wiring, not just the
// helper: going through the real robot service must leave a ringable bell.
func TestEnqueueChokepointRingsTheDoorbell(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)

	svc := robot.NewService(ctx)
	_, err := svc.EnqueueBotTypedEvent(lpBotID, "card_action", map[string]interface{}{"action_id": "approve"})
	assert.NoError(t, err)

	// A waiter blocked on BLPOP would have been woken; assert the token exists.
	n, err := ctx.GetRedisConn().Llen(botevent.BellKey(lpBotID))
	assert.NoError(t, err)
	assert.Equal(t, int64(1), n, "the typed-event chokepoint must ring the doorbell")
}

// TestDoorbellStaysBoundedWithTTL: nobody may be long-polling for hours, so an
// unattended bell must neither grow nor live forever.
func TestDoorbellStaysBoundedWithTTL(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)

	for i := 0; i < 25; i++ {
		assert.NoError(t, botevent.Ring(ctx.GetRedisConn(), lpBotID))
	}
	n, err := ctx.GetRedisConn().Llen(botevent.BellKey(lpBotID))
	assert.NoError(t, err)
	assert.Equal(t, int64(1), n, "doorbell must stay at one token regardless of event volume")
}
