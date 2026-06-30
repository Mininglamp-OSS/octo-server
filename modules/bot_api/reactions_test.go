package bot_api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// fakeReactionStore implements reactionStorer for state-machine + handler tests.
type fakeReactionStore struct {
	seq       int64
	existing  *reactionRow
	inserted  *reactionRow
	updated   *reactionRow
	genSeqErr error
}

func (f *fakeReactionStore) genSeq(string) (int64, error) {
	if f.genSeqErr != nil {
		return 0, f.genSeqErr
	}
	f.seq++
	return f.seq, nil
}
func (f *fakeReactionStore) insert(m *reactionRow) error { f.inserted = m; return nil }
func (f *fakeReactionStore) update(m *reactionRow) error { f.updated = m; return nil }
func (f *fakeReactionStore) queryByActorAndMessage(uid, messageID string) (*reactionRow, error) {
	return f.existing, nil
}

func nameResolverStub(robotID string) string { return "BotDisplay(" + robotID + ")" }

func baseInput(remove bool) reactionInput {
	return reactionInput{
		messageID:      "m1",
		storeChannelID: "chan1",
		channelType:    common.ChannelTypeGroup.Uint8(),
		robotID:        "bot1",
		emoji:          "👍",
		remove:         remove,
	}
}

// ---- add branches ----

func TestReaction_Add_NoRow_Inserts(t *testing.T) {
	f := &fakeReactionStore{}
	changed, err := reactionStateTransitionImpl(f, nameResolverStub, nil, baseInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed=true for fresh add")
	}
	if f.inserted == nil {
		t.Fatal("expected an insert")
	}
	if f.inserted.Emoji != "👍" || f.inserted.IsDeleted != 0 {
		t.Fatalf("bad inserted row: %+v", f.inserted)
	}
	// name column must be populated so user clients render the reactor.
	if f.inserted.Name == "" {
		t.Fatal("expected reactor display name to be filled")
	}
}

func TestReaction_Add_DeletedRow_Revives(t *testing.T) {
	f := &fakeReactionStore{}
	existing := &reactionRow{MessageID: "m1", UID: "bot1", Emoji: "👍", IsDeleted: 1}
	changed, err := reactionStateTransitionImpl(f, nameResolverStub, existing, baseInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if !changed || f.updated == nil {
		t.Fatal("expected a reviving update")
	}
	if f.updated.IsDeleted != 0 || f.updated.Emoji != "👍" {
		t.Fatalf("bad revived row: %+v", f.updated)
	}
}

func TestReaction_Add_SameEmojiLive_NoOp(t *testing.T) {
	f := &fakeReactionStore{}
	existing := &reactionRow{MessageID: "m1", UID: "bot1", Emoji: "👍", IsDeleted: 0}
	changed, err := reactionStateTransitionImpl(f, nameResolverStub, existing, baseInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected no-op for re-adding the same emoji")
	}
	if f.inserted != nil || f.updated != nil {
		t.Fatal("expected no DB write on no-op")
	}
}

func TestReaction_Add_DifferentEmojiLive_Overwrites(t *testing.T) {
	f := &fakeReactionStore{}
	existing := &reactionRow{MessageID: "m1", UID: "bot1", Emoji: "🎉", IsDeleted: 0}
	changed, err := reactionStateTransitionImpl(f, nameResolverStub, existing, baseInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if !changed || f.updated == nil {
		t.Fatal("expected overwrite update")
	}
	if f.updated.Emoji != "👍" {
		t.Fatalf("expected emoji overwritten to 👍, got %q", f.updated.Emoji)
	}
}

// ---- remove branches ----

func TestReaction_Remove_NoRow_NoOp(t *testing.T) {
	f := &fakeReactionStore{}
	changed, err := reactionStateTransitionImpl(f, nameResolverStub, nil, baseInput(true))
	if err != nil {
		t.Fatal(err)
	}
	if changed || f.updated != nil {
		t.Fatal("expected no-op removing a nonexistent reaction")
	}
}

func TestReaction_Remove_DeletedRow_NoOp(t *testing.T) {
	f := &fakeReactionStore{}
	existing := &reactionRow{MessageID: "m1", UID: "bot1", Emoji: "👍", IsDeleted: 1}
	changed, err := reactionStateTransitionImpl(f, nameResolverStub, existing, baseInput(true))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected no-op removing an already-deleted reaction")
	}
}

func TestReaction_Remove_MatchingEmoji_SoftDeletes(t *testing.T) {
	f := &fakeReactionStore{}
	existing := &reactionRow{MessageID: "m1", UID: "bot1", Emoji: "👍", IsDeleted: 0}
	changed, err := reactionStateTransitionImpl(f, nameResolverStub, existing, baseInput(true))
	if err != nil {
		t.Fatal(err)
	}
	if !changed || f.updated == nil {
		t.Fatal("expected soft-delete update")
	}
	if f.updated.IsDeleted != 1 {
		t.Fatalf("expected is_deleted=1, got %d", f.updated.IsDeleted)
	}
}

func TestReaction_Remove_MismatchedEmoji_NoOp(t *testing.T) {
	f := &fakeReactionStore{}
	existing := &reactionRow{MessageID: "m1", UID: "bot1", Emoji: "🎉", IsDeleted: 0}
	changed, err := reactionStateTransitionImpl(f, nameResolverStub, existing, baseInput(true))
	if err != nil {
		t.Fatal(err)
	}
	if changed || f.updated != nil {
		t.Fatal("expected no-op when removing an emoji the bot didn't set")
	}
}

// ---- store channel id mapping ----

func TestReaction_StoreChannelID_GroupUsesRaw(t *testing.T) {
	got := resolveReactionStoreChannelID("g123", common.ChannelTypeGroup.Uint8(), "bot1")
	if got != "g123" {
		t.Fatalf("group reaction should store under raw channel id, got %q", got)
	}
}

func TestReaction_StoreChannelID_DMUsesFakeChannel(t *testing.T) {
	peer := "u_bob"
	bot := "bot1"
	got := resolveReactionStoreChannelID(peer, common.ChannelTypePerson.Uint8(), bot)
	want := common.GetFakeChannelIDWith(peer, bot)
	if got != want {
		t.Fatalf("DM reaction should store under fake channel id %q, got %q", want, got)
	}
	if got == peer {
		t.Fatal("DM store channel id must NOT be the raw peer id")
	}
}

// ---- handler-level tests (permission, CMD routing, no-op) ----

type reactionCMDCapture struct {
	called bool
	req    config.MsgCMDReq
}

func (rc *reactionCMDCapture) hook(req config.MsgCMDReq) error {
	rc.called = true
	rc.req = req
	return nil
}

func buildReactionCtx(t *testing.T, robotID, botKind, messageID string, body []byte, query, dmCreator string) *wkhttp.Context {
	t.Helper()
	url := "/v1/bot/messages/" + messageID + "/reactions"
	if query != "" {
		url += "?" + query
	}
	method := http.MethodPost
	if body == nil {
		method = http.MethodDelete
	}
	httpReq := httptest.NewRequest(method, url, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	gc.Params = gin.Params{{Key: "message_id", Value: messageID}}
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, robotID)
	c.Set(CtxKeyBotKind, botKind)
	// For User Bot DM tests: mark the peer as the bot's creator so the
	// permission check passes via the isCreator branch (no DB/userService).
	c.Set(CtxKeyRobot, &robotModel{RobotID: robotID, CreatorUID: dmCreator})
	return c
}

// TestReaction_Handler_AppBotGroupRejected — App Bot may only react in DMs.
// Pure-logic permission branch (no DB), so it runs without a live ctx.
func TestReaction_Handler_AppBotGroupRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &reactionCMDCapture{}
	ba := &BotAPI{
		Log:                   log.NewTLog("BotAPI-reaction-test"),
		reactionCMDDispatch:   cap.hook,
		reactionStoreOverride: &fakeReactionStore{},
	}
	c := buildReactionCtx(t, "app_bot1", BotKindApp, "m1",
		[]byte(`{"channel_id":"g1","channel_type":2,"emoji":"👍"}`), "", "")
	ba.addReaction(c)
	if cap.called {
		t.Fatal("App Bot group reaction must be rejected before any CMD broadcast")
	}
}

// TestReaction_Handler_DM_AddBroadcastsOriginalChannelID — the sync CMD
// must route to the ORIGINAL peer channel id (not the fake store id).
func TestReaction_Handler_DM_AddBroadcastsOriginalChannelID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &reactionCMDCapture{}
	fs := &fakeReactionStore{}
	ba := &BotAPI{
		Log:                   log.NewTLog("BotAPI-reaction-test"),
		reactionCMDDispatch:   cap.hook,
		reactionStoreOverride: fs,
		messageExistsOverride: func(string, uint8, string) (bool, error) { return true, nil },
	}
	peer := "u_bob"
	// User Bot DM where the peer is the bot's creator → permission passes via
	// the isCreator branch (no userService/DB needed).
	c := buildReactionCtx(t, "bot1", BotKindUser, "m1",
		[]byte(`{"channel_id":"`+peer+`","channel_type":1,"emoji":"👍"}`), "", peer)
	ba.addReaction(c)
	if !cap.called {
		t.Fatal("expected a sync CMD on a fresh add")
	}
	if cap.req.ChannelID != peer {
		t.Fatalf("CMD must use original peer channel id %q, got %q", peer, cap.req.ChannelID)
	}
	if cap.req.FromUID != "bot1" {
		t.Fatalf("CMD FromUID should be the bot, got %q", cap.req.FromUID)
	}
	// The stored row must use the fake channel id, not the raw peer.
	if fs.inserted == nil || fs.inserted.ChannelID == peer {
		t.Fatalf("DM reaction row must store under fake channel id, got %+v", fs.inserted)
	}
}

// TestReaction_Handler_NoOpSkipsCMD — re-adding the same live emoji is a no-op
// and must NOT broadcast a CMD.
func TestReaction_Handler_NoOpSkipsCMD(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &reactionCMDCapture{}
	fs := &fakeReactionStore{
		existing: &reactionRow{MessageID: "m1", UID: "bot1", Emoji: "👍", IsDeleted: 0},
	}
	ba := &BotAPI{
		Log:                   log.NewTLog("BotAPI-reaction-test"),
		reactionCMDDispatch:   cap.hook,
		reactionStoreOverride: fs,
		messageExistsOverride: func(string, uint8, string) (bool, error) { return true, nil },
	}
	peer := "u_bob"
	c := buildReactionCtx(t, "bot1", BotKindUser, "m1",
		[]byte(`{"channel_id":"`+peer+`","channel_type":1,"emoji":"👍"}`), "", peer)
	ba.addReaction(c)
	if cap.called {
		t.Fatal("re-adding the same emoji is a no-op and must not broadcast a CMD")
	}
}

// TestReaction_Handler_MessageNotInChannel_Rejected — a bot with send
// permission to the request channel must NOT be able to react to a message_id
// that does not belong to that channel (cross-channel reaction injection
// guard). No store write, no CMD broadcast.
func TestReaction_Handler_MessageNotInChannel_Rejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &reactionCMDCapture{}
	fs := &fakeReactionStore{}
	ba := &BotAPI{
		Log:                   log.NewTLog("BotAPI-reaction-test"),
		reactionCMDDispatch:   cap.hook,
		reactionStoreOverride: fs,
		// Permission passes (peer == bot creator), but the message does not
		// live in this conversation.
		messageExistsOverride: func(string, uint8, string) (bool, error) { return false, nil },
	}
	peer := "u_bob"
	c := buildReactionCtx(t, "bot1", BotKindUser, "m_in_other_channel",
		[]byte(`{"channel_id":"`+peer+`","channel_type":1,"emoji":"👍"}`), "", peer)
	ba.addReaction(c)
	if cap.called {
		t.Fatal("must not broadcast a CMD for a message_id that is not in this channel")
	}
	if fs.inserted != nil || fs.updated != nil {
		t.Fatalf("must not write a reaction row for a cross-channel message_id, got inserted=%+v updated=%+v", fs.inserted, fs.updated)
	}
}

// TestReaction_Handler_DM_NonFriendRejected — a User Bot DM where the peer
// is neither the bot's creator nor a friend must be rejected, with no store
// write and no CMD. Uses friendCheckOverride (the seam isFriendOrOBOBypass
// consults) so no live user service is needed.
func TestReaction_Handler_DM_NonFriendRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &reactionCMDCapture{}
	fs := &fakeReactionStore{}
	ba := &BotAPI{
		Log:                   log.NewTLog("BotAPI-reaction-test"),
		reactionCMDDispatch:   cap.hook,
		reactionStoreOverride: fs,
		friendCheckOverride:   func(uid, toUID string) (bool, error) { return false, nil },
	}
	// dmCreator is a DIFFERENT uid than the peer, so the isCreator bypass does
	// not apply and the (failing) friend check decides.
	c := buildReactionCtx(t, "bot1", BotKindUser, "m1",
		[]byte(`{"channel_id":"u_bob","channel_type":1,"emoji":"👍"}`), "", "someone_else")
	ba.addReaction(c)
	if cap.called {
		t.Fatal("non-friend DM reaction must be rejected before any CMD broadcast")
	}
	if fs.inserted != nil || fs.updated != nil {
		t.Fatal("rejected reaction must not write to the store")
	}
}
