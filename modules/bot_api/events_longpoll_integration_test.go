package bot_api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/gin-gonic/gin"
	rd "github.com/go-redis/redis"
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
	assert.NoError(t, botevent.Ring(botevent.RingClient(ctx.GetConfig()), lpBotID))

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
// doorbell acceptable at all: losing it may only cost latency, never an event.
//
// The precise contract is stronger than "you get it on your next request", and
// the first version of this test asserted the weaker claim while accidentally
// passing on the stronger one (PR#685 review, yujiawei). Chunking gives an
// implicit fallback poll: when the bell never rings, the BLPOP simply times out
// into `redis.Nil`, control falls through to the authoritative re-read, and the
// event is returned **within one chunk** — inside the same request. Assert that,
// not the vaguer version.
func TestBotEventsSurvivesLostDoorbell(t *testing.T) {
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
		// wait=8 exceeds one 5s chunk, so a pass cannot come from the hold
		// simply expiring at the same moment.
		resp, elapsed := pollEvents(t, s, `{"event_id":0,"limit":20,"wait":8}`)
		done <- outcome{resp, elapsed}
	}()
	time.Sleep(500 * time.Millisecond)
	enqueueRaw(t, ctx, 303) // deliberately no Ring

	select {
	case got := <-done:
		assert.Equal(t, 1, got.resp.Status)
		// The event is delivered by the same request that missed the bell —
		// the authoritative re-read after the chunk expires finds it.
		assert.Len(t, got.resp.Results, 1, "a lost doorbell must never lose the event")
		assert.Equal(t, int64(303), got.resp.Results[0].EventID)
		// Delivered at the chunk boundary, not at the hold deadline: that
		// bounded fallback is what keeps a silent bell failure at ≤5s.
		assert.Less(t, got.elapsed, 8*time.Second,
			"a lost bell must cost one chunk, not the whole hold")
	case <-time.After(30 * time.Second):
		t.Fatal("hold never returned")
	}
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
// unattended bell must neither grow nor live forever. This is the only place the
// Lua script is executed by a real Redis — the unit test in pkg/botevent models
// the commands rather than running them.
func TestDoorbellStaysBoundedWithTTL(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)

	for i := 0; i < 25; i++ {
		assert.NoError(t, botevent.Ring(botevent.RingClient(ctx.GetConfig()), lpBotID))
	}
	n, err := ctx.GetRedisConn().Llen(botevent.BellKey(lpBotID))
	assert.NoError(t, err)
	assert.Equal(t, int64(1), n, "doorbell must stay at one token regardless of event volume")

	// And the TTL is real, on the bell key itself. A script that pushed to one
	// key and expired another would leave an immortal bell while passing every
	// length assertion above. octo-lib's Conn exposes no TTL read, so go through
	// the chokepoint client.
	raw := octoredis.NewInstrumentedClient(ctx.GetConfig())
	defer func() { _ = raw.Close() }()
	ttl, err := raw.TTL(botevent.BellKey(lpBotID)).Result()
	assert.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0), "the bell must carry a TTL")
	assert.LessOrEqual(t, ttl, botevent.BellTTL, "the TTL must not exceed the declared BellTTL")
	assert.Greater(t, ttl, maxEventWaitSeconds*time.Second,
		"the TTL must outlive one maximum-length hold")
}

// --- Round-4 coverage: the loop's forward-progress guarantee (PR#685 review) ---

// newWaitTestAPI builds a BotAPI that can read the queue and log, without the
// HTTP auth wiring. waitForEvents takes botKind as a parameter, so the App Bot
// branch is reachable without seeding an app_bot row.
func newWaitTestAPI(ctx *config.Context) *BotAPI {
	return &BotAPI{ctx: ctx, Log: log.NewTLog("BotAPITest")}
}

// newWaitTestContext wraps a cancellable context in the wkhttp.Context shape
// waitForEvents expects.
func newWaitTestContext(goCtx context.Context) *wkhttp.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, _ := http.NewRequest("POST", "/v1/bot/events", nil)
	c.Request = req.WithContext(goCtx)
	return &wkhttp.Context{Context: c}
}

// enqueueMessageEvent writes a message-shaped event with the given channel type.
// channelType 2 (group) is what the App Bot DM-only filter drops.
func enqueueMessageEvent(t *testing.T, ctx *config.Context, eventID int64, channelType uint8) {
	t.Helper()
	payload := fmt.Sprintf(
		`{"event_id":%d,"message":{"message_id":%d,"message_seq":%d,"from_uid":"u1","channel_id":"c1","channel_type":%d,"timestamp":1}}`,
		eventID, eventID, eventID, channelType)
	assert.NoError(t, ctx.GetRedisConn().ZAdd(robotEventPrefix+lpBotID, float64(eventID), payload))
}

// enqueueUndecodable writes a member that survives in the sorted set but cannot
// be decoded — the shape getEventsResult silently skips.
// The payload must stay distinct per event: a sorted set stores unique members,
// so two identical values would collapse into one re-scored member and the page
// would never fill.
func enqueueUndecodable(t *testing.T, ctx *config.Context, eventID int64) {
	t.Helper()
	assert.NoError(t, ctx.GetRedisConn().ZAdd(
		robotEventPrefix+lpBotID, float64(eventID), fmt.Sprintf("not json at all #%d", eventID)))
}

// TestReadEventPageAdvancesTheCursorFromTheReadNotTheACK pins the property the
// whole loop's termination argument rests on: the cursor is derived from what
// the read observed, before the App Bot filter and independent of the auto-ACK
// ZREM the filter issues afterwards (events.go only warns when that ZREM fails).
//
// Re-seeding the same events and reading again from the original cursor is
// exactly what "the ZREM did nothing" looks like from the loop's point of view;
// the cursor must still advance.
func TestReadEventPageAdvancesTheCursorFromTheReadNotTheACK(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)
	ba := newWaitTestAPI(ctx)

	for id := int64(1); id <= 3; id++ {
		enqueueMessageEvent(t, ctx, id, 2) // group events: invisible to an App Bot
	}

	page, err := ba.readEventPage(lpBotID, BotKindApp, 0, 20)
	assert.NoError(t, err)
	assert.Empty(t, page.visible, "group events must not be visible to an App Bot")
	assert.Equal(t, int64(3), page.cursor, "the cursor must advance past filtered events")
	assert.False(t, page.full, "3 of a 20-event page is not full")

	// Put them back: the queue now looks exactly as it would if every auto-ACK
	// ZREM had failed (MISCONF, a READONLY replica, a write-denying ACL).
	for id := int64(1); id <= 3; id++ {
		enqueueMessageEvent(t, ctx, id, 2)
	}
	again, err := ba.readEventPage(lpBotID, BotKindApp, 0, 20)
	assert.NoError(t, err)
	assert.Empty(t, again.visible)
	assert.Equal(t, int64(3), again.cursor,
		"the cursor must come from the read, so a failed ZREM cannot stall the loop")

	// Reading from the advanced cursor sees nothing, which is what lets the loop
	// go back to blocking instead of re-serving the same page forever.
	beyond, err := ba.readEventPage(lpBotID, BotKindApp, again.cursor, 20)
	assert.NoError(t, err)
	assert.Empty(t, beyond.visible)
	assert.Equal(t, again.cursor, beyond.cursor, "an empty read must not move the cursor")
}

// TestReadEventPageAdvancesPastUndecodableMembers: a member that fails to decode
// is dropped from the results but still occupied a slot in the page. If the
// cursor did not clear it, a queue whose head is corrupt would starve every
// later event — the read would keep returning the same `limit` bad members.
func TestReadEventPageAdvancesPastUndecodableMembers(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)
	ba := newWaitTestAPI(ctx)

	enqueueUndecodable(t, ctx, 5)
	enqueueMessageEvent(t, ctx, 6, 1) // a DM: visible to every bot kind

	page, err := ba.readEventPage(lpBotID, BotKindUser, 0, 2)
	assert.NoError(t, err)
	assert.Len(t, page.visible, 1, "the decodable event must still be delivered")
	assert.Equal(t, int64(6), page.visible[0].EventID)
	assert.Equal(t, int64(6), page.cursor)
	// Fullness is counted from raw members, not decoded events: one of the two
	// members vanished in decoding, and treating the page as short would stall
	// the drain of whatever sits behind it.
	assert.True(t, page.full, "2 raw members at limit=2 is a full page")
}

// TestReadEventPageCannotAdvancePastAWhollyUndecodablePage documents the one
// case the cursor cannot rescue, and why the loop guards on it. A full page in
// which *nothing* decodes leaves the cursor where it was; the wait loop only
// skips its BLPOP when the cursor actually moved, so this degrades to
// chunk-paced re-reads rather than an unpaced spin.
func TestReadEventPageCannotAdvancePastAWhollyUndecodablePage(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)
	ba := newWaitTestAPI(ctx)

	for id := int64(1); id <= 2; id++ {
		enqueueUndecodable(t, ctx, id)
	}
	page, err := ba.readEventPage(lpBotID, BotKindUser, 0, 2)
	assert.NoError(t, err)
	assert.Empty(t, page.visible)
	assert.True(t, page.full)
	assert.Equal(t, int64(0), page.cursor, "nothing decoded, so nothing can advance the cursor")

	// The guard: a full page that advanced nothing must not license skipping the
	// block. waitForEvents computes `full && advanced`; assert the conjunction is
	// false here, which is what keeps the loop paced.
	assert.False(t, page.full && page.cursor > 0,
		"a full page that advanced nothing must not permit an unpaced re-read")
}

// TestWaitForEventsDrainsAFilteredBacklogWithoutBlocking is the entry-path half
// of forward progress (PR#685 review P2-a). An App Bot whose queue holds a full
// page of group events followed by a deliverable DM has everything it needs
// *before* the request arrives — no new bell will ring for any of it. Blocking
// on the doorbell first would defer that DM by a whole chunk.
func TestWaitForEventsDrainsAFilteredBacklogWithoutBlocking(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)
	resetEventHoldState(t, 4)
	ba := newWaitTestAPI(ctx)

	const limit = int64(20)
	for id := int64(1); id <= limit; id++ {
		enqueueMessageEvent(t, ctx, id, 2) // a full page, all filtered
	}
	enqueueMessageEvent(t, ctx, limit+1, 1) // the DM behind it

	// The entry read getEvents performs, threaded in exactly as the handler does.
	entry, err := ba.readEventPage(lpBotID, BotKindApp, 0, limit)
	assert.NoError(t, err)
	assert.Empty(t, entry.visible)
	assert.True(t, entry.full, "the backlog must fill the entry page for this test to mean anything")

	start := time.Now()
	got, err := ba.waitForEvents(newWaitTestContext(context.Background()),
		lpBotID, BotKindApp, limit, 20*time.Second, entry)
	elapsed := time.Since(start)
	assert.NoError(t, err)
	assert.Len(t, got, 1, "the DM behind the filtered backlog must be delivered")
	assert.Equal(t, limit+1, got[0].EventID)
	assert.Less(t, elapsed, eventWaitChunk,
		"a backlog already in the queue must not wait out a chunk on the doorbell")
}

// TestWaitForEventsPacesAFailingDoorbell is the regression test for PR#685
// review P1-1. redis.Nil is the *normal* BLPOP timeout and is excluded from the
// error branch, so every error that reaches it is a dial, write, protocol or
// server error — which go-redis returns after MinRetryBackoff (~8ms), not after
// the chunk. `ERR max number of clients reached` is the ordinary trigger, and it
// hits the dedicated wait pool while the shared pool keeps serving reads.
//
// Left unpaced the loop ran at ~50-100Hz for the whole hold, issuing a full
// ZRangeByScore and a log line each time: a Redis capacity incident answered
// with read amplification. Assert the pacing by counting the reads Redis
// actually served.
func TestWaitForEventsPacesAFailingDoorbell(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)
	resetEventHoldState(t, 4)
	ba := newWaitTestAPI(ctx)

	// Point the dedicated wait client at a closed port: BLPOP fails instantly,
	// while ba.ctx.GetRedisConn() (the shared pool) keeps working. That
	// asymmetry is exactly what a connection-budget incident looks like.
	restore := swapEventWaitRedis(t, rd.NewClient(&rd.Options{
		Addr:       "127.0.0.1:1",
		MaxRetries: 1,
	}))
	defer restore()

	before := zrangeByScoreCalls(t, ctx)
	const wait = 8 * time.Second
	start := time.Now()
	got, err := ba.waitForEvents(newWaitTestContext(context.Background()),
		lpBotID, BotKindUser, 20, wait, eventPage{})
	elapsed := time.Since(start)
	after := zrangeByScoreCalls(t, ctx)

	assert.NoError(t, err, "a broken doorbell must not fail the request; the queue is the authority")
	assert.Empty(t, got)
	// The hold is still served in full rather than collapsing to ~0s.
	assert.GreaterOrEqual(t, elapsed, wait, "a doorbell failure must not collapse the hold")
	assert.Less(t, elapsed, wait+2*eventWaitChunk, "and must not extend it either")

	// One read per chunk, plus slack. Before the fix this was in the thousands.
	reads := after - before
	maxReads := int64(wait/eventWaitChunk) + 3
	assert.LessOrEqual(t, reads, maxReads,
		"a failing doorbell issued %d authoritative reads in %v; the error path must pay out its chunk", reads, elapsed)
	assert.GreaterOrEqual(t, reads, int64(1), "the loop must still re-read the authoritative queue")
}

// TestWaitForEventsDoesNotSpinOnAPageItCannotAdvancePast is the regression test
// for PR#685 review P1-2. The loop skips its BLPOP when the previous read filled
// its page — that is how a backlog drains. The earlier revision keyed that
// decision on page fullness alone, so a full page the read could not advance
// past meant a completely unpaced loop for the rest of the hold: no BLPOP, no
// backoff, not even go-redis's 8ms.
//
// The reviewer's example was an auto-ACK ZREM that failed while reads kept
// working (MISCONF, a READONLY replica, a write-denying ACL), leaving the same
// filtered page in place forever. A page of undecodable members reproduces the
// same shape deterministically: nothing is removed, nothing is visible, and the
// cursor cannot move — so it is the strictest version of the case. The loop must
// fall back to chunk pacing.
func TestWaitForEventsDoesNotSpinOnAPageItCannotAdvancePast(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	clearLongPollKeys(t, ctx)
	defer clearLongPollKeys(t, ctx)
	resetEventHoldState(t, 4)
	ba := newWaitTestAPI(ctx)

	const limit = int64(5)
	for id := int64(1); id <= limit; id++ {
		enqueueUndecodable(t, ctx, id)
	}
	entry, err := ba.readEventPage(lpBotID, BotKindUser, 0, limit)
	assert.NoError(t, err)
	assert.Empty(t, entry.visible)
	assert.True(t, entry.full, "the page must be full for this test to exercise the skip path")
	assert.Equal(t, int64(0), entry.cursor, "and it must be unable to advance the cursor")

	before := zrangeByScoreCalls(t, ctx)
	const wait = 6 * time.Second
	start := time.Now()
	got, err := ba.waitForEvents(newWaitTestContext(context.Background()),
		lpBotID, BotKindUser, limit, wait, entry)
	elapsed := time.Since(start)
	reads := zrangeByScoreCalls(t, ctx) - before

	assert.NoError(t, err)
	assert.Empty(t, got)
	assert.GreaterOrEqual(t, elapsed, wait, "the hold must still be served in full")
	maxReads := int64(wait/eventWaitChunk) + 3
	assert.LessOrEqual(t, reads, maxReads,
		"the loop issued %d reads in %v; a page it cannot advance past must not be re-read unpaced", reads, elapsed)
}

// swapEventWaitRedis replaces the process-wide BLPOP client. The singleton is a
// package-level var for the same reason the hold budgets are: one per process.
func swapEventWaitRedis(t *testing.T, client *rd.Client) func() {
	t.Helper()
	eventWaitRedisOnce.Do(func() {}) // consume the Once so the swap survives
	prev := eventWaitRedis
	eventWaitRedis = client
	return func() {
		eventWaitRedis = prev
		eventWaitRedisOnce = sync.Once{}
		_ = client.Close()
	}
}

// zrangeByScoreCalls reads Redis's own command counter, so the assertion is
// about what the server was asked to do rather than what the test believes the
// code did.
func zrangeByScoreCalls(t *testing.T, ctx *config.Context) int64 {
	t.Helper()
	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	defer func() { _ = client.Close() }()
	info, err := client.Info("commandstats").Result()
	assert.NoError(t, err)
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "cmdstat_zrangebyscore:") {
			continue
		}
		for _, field := range strings.Split(line[strings.Index(line, ":")+1:], ",") {
			if !strings.HasPrefix(field, "calls=") {
				continue
			}
			n, err := strconv.ParseInt(strings.TrimPrefix(field, "calls="), 10, 64)
			assert.NoError(t, err)
			return n
		}
	}
	return 0
}
