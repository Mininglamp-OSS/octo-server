//go:build integration

package cardactiondispatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	_ "github.com/Mininglamp-OSS/octo-server/modules/app_bot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/modules/notify"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/thread"
	_ "github.com/Mininglamp-OSS/octo-server/modules/webhook"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type callbackE2ESender struct {
	targets []carddispatch.Target
	cards   []carddispatch.Card
}

func (s *callbackE2ESender) Send(_ context.Context, target carddispatch.Target, card carddispatch.Card) (*carddispatch.Result, error) {
	s.targets = append(s.targets, target)
	s.cards = append(s.cards, card)
	return &carddispatch.Result{MessageID: 9802, MessageSeq: 1}, nil
}

type callbackE2EFinalizer struct {
	delegate cardactiondispatch.Finalizer
	events   []cardactiondispatch.Event
}

type callbackE2EMutator struct {
	requests []carddispatch.CardMutationRequest
}

func (m *callbackE2EMutator) Mutate(_ context.Context, request carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error) {
	m.requests = append(m.requests, request)
	return carddispatch.CardMutationResult{Applied: true}, nil
}

func (f *callbackE2EFinalizer) Finalize(ctx context.Context, event cardactiondispatch.Event, result cardactiondispatch.DecisionResult) error {
	f.events = append(f.events, event)
	return f.delegate.Finalize(ctx, event, result)
}

type docsCallbackCapture struct {
	method         string
	path           string
	request        cardactiondispatch.DecisionRequest
	signatureValid bool
	err            error
}

// TestCardActionCallbackOrchestrationWithMockDocs exercises the octo-server
// orchestration boundary end to end. The docs domain endpoint and applicant IM
// send are test doubles; HTTP routing, MySQL authority lookup, Redis queueing,
// signed callback delivery, typed-result decoding, WuKongIM lookup, card
// mutation persistence, and finalization are real.
func TestCardActionCallbackOrchestrationWithMockDocs(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("NOTIFY_INTERNAL_TOKEN", "legacy-notify-token")
	t.Setenv("OCTO_DOCS_NOTIFY_TOKEN", "docs-notify-token")
	const secret = "0123456789abcdef0123456789abcdef"

	callbackCalls := make(chan docsCallbackCapture, 1)
	docsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := docsCallbackCapture{method: r.Method, path: r.URL.EscapedPath()}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			capture.err = err
		} else if err := json.Unmarshal(body, &capture.request); err != nil {
			capture.err = err
		}
		capture.signatureValid = cardactiondispatch.Verify(
			secret,
			r.Header.Get(cardactiondispatch.HeaderSignature),
			r.Method,
			r.URL.EscapedPath(),
			r.Header.Get(cardactiondispatch.HeaderTimestamp),
			r.Header.Get(cardactiondispatch.HeaderEventID),
			body,
		)
		callbackCalls <- capture
		if capture.err != nil || !capture.signatureValid {
			http.Error(w, "invalid callback", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disposition":"applied","state":"approved","requester_uid":"user-a","display":{"title":"Roadmap"}}`))
	}))
	defer docsServer.Close()
	callbackURL := docsServer.URL + "/v1/card-actions/decide"

	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	prefix := fmt.Sprintf("test:card_action_callback_e2e:%d", time.Now().UnixNano())
	cleanupRedisPatterns(t, rds, "ratelimit:uid:*", "cardaction:*", "robotEvent:*", prefix+"*")
	t.Cleanup(func() {
		cleanupRedisPatterns(t, rds, "ratelimit:uid:*", "cardaction:*", "robotEvent:*", prefix+"*")
		_ = rds.Close()
	})

	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{{
		SenderUID: "notification", Owner: "docs", ActionType: "access_request.decision",
		URL: callbackURL, SecretEnv: "OCTO_DOCS_CARD_ACTION_SECRET",
	}}, []string{callbackURL}, func(string) string { return secret })
	require.NoError(t, err)
	queue, err := cardactiondispatch.NewRedisQueue(rds, cardactiondispatch.QueueConfig{
		Prefix: prefix, LiveTTL: ctx.GetConfig().Robot.MessageExpire, DLQRetention: 30 * 24 * time.Hour,
	})
	require.NoError(t, err)
	service, err := cardactiondispatch.NewService(registry, queue, ctx)
	require.NoError(t, err)
	require.NoError(t, cardactiondispatch.Install(ctx, service))

	sender := &callbackE2ESender{}
	docsFinalizer, err := notify.NewDocsActionFinalizer(ctx, carddispatch.NewCardMutator(ctx), sender)
	require.NoError(t, err)
	finalizer := &callbackE2EFinalizer{delegate: docsFinalizer}
	dispatcher, err := cardactiondispatch.NewDispatcher(
		queue,
		registry,
		cardactiondispatch.NewHTTPDeliverer(docsServer.Client().Transport, time.Now),
		finalizer,
		cardactiondispatch.DispatcherConfig{},
	)
	require.NoError(t, err)

	messageID := seedCallbackE2EBotAndMessage(t, ctx, cardV2InternalEnvelope(t))

	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/v1/message/card/action", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"message_id": messageID, "channel_id": "notification", "channel_type": common.ChannelTypePerson.Uint8(),
		"action_id": "approve", "client_token": "token-" + messageID,
	}))))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	processed, err := dispatcher.ProcessOne(context.Background(), time.Now().Add(time.Second))
	require.NoError(t, err)
	require.True(t, processed)

	callback := <-callbackCalls
	require.NoError(t, callback.err)
	assert.Equal(t, http.MethodPost, callback.method)
	assert.Equal(t, "/v1/card-actions/decide", callback.path)
	assert.True(t, callback.signatureValid)
	assert.True(t, callback.request.EventID > 0)
	assert.Equal(t, "approve", callback.request.ActionID)
	assert.Equal(t, "approve", callback.request.Decision)
	assert.Equal(t, testutil.UID, callback.request.OperatorUID)
	assert.Equal(t, "doc-1", callback.request.DocID)
	assert.Equal(t, "request-1", callback.request.RequestID)
	assert.Equal(t, messageID, callback.request.MessageID)
	assert.Equal(t, "notification", callback.request.ChannelID)
	assert.Equal(t, common.ChannelTypePerson.Uint8(), callback.request.ChannelType)
	assert.Equal(t, "space-1", callback.request.SpaceID)
	assert.NotZero(t, callback.request.ActedAt)

	require.Len(t, finalizer.events, 1)
	assert.Equal(t, "notification", finalizer.events[0].SenderUID)
	assert.Equal(t, "docs", finalizer.events[0].Owner)
	assert.Equal(t, "access_request.decision", finalizer.events[0].ActionType)

	var mutation struct {
		ChannelID   string `db:"channel_id"`
		ContentEdit string `db:"content_edit"`
	}
	require.NoError(t, ctx.DB().Select("channel_id", "content_edit").From("message_extra").
		Where("message_id=?", messageID).LoadOne(&mutation))
	assert.Equal(t, common.GetFakeChannelIDWith("notification", testutil.UID), mutation.ChannelID)
	var terminal map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(mutation.ContentEdit), &terminal))
	assert.Equal(t, float64(callback.request.EventID), terminal["card_seq"])
	card, ok := terminal["card"].(map[string]interface{})
	require.True(t, ok)
	_, hasActions := card["actions"]
	assert.False(t, hasActions)

	require.Len(t, sender.targets, 1)
	require.Len(t, sender.cards, 1)
	assert.Equal(t, "user-a", sender.targets[0].ChannelID)
	assert.Equal(t, "space-1", sender.targets[0].SpaceID)
	assert.Equal(t, cardmsg.ProfileV1, sender.cards[0].Profile)

	depths, err := queue.Depths()
	require.NoError(t, err)
	assert.Equal(t, cardactiondispatch.QueueDepths{}, depths)
}

func TestCardActionCallbackOrchestrationUsesStandardFinalizerForNewOwner(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	const secret = "0123456789abcdef0123456789abcdef"
	callbackCalls := make(chan docsCallbackCapture, 1)
	consumer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		capture := docsCallbackCapture{method: r.Method, path: r.URL.EscapedPath(), err: err}
		if err == nil {
			capture.err = json.Unmarshal(body, &capture.request)
		}
		capture.signatureValid = cardactiondispatch.Verify(
			secret, r.Header.Get(cardactiondispatch.HeaderSignature), r.Method, r.URL.EscapedPath(),
			r.Header.Get(cardactiondispatch.HeaderTimestamp), r.Header.Get(cardactiondispatch.HeaderEventID), body,
		)
		callbackCalls <- capture
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disposition":"applied","state":"approved","requester_uid":"user-a","display":{"title":"Deploy release"}}`))
	}))
	defer consumer.Close()

	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	rds := redis.NewClient(&redis.Options{Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass})
	prefix := fmt.Sprintf("test:card_action_generic_e2e:%d", time.Now().UnixNano())
	cleanupRedisPatterns(t, rds, prefix+"*")
	t.Cleanup(func() {
		cleanupRedisPatterns(t, rds, prefix+"*")
		_ = rds.Close()
	})

	callbackURL := consumer.URL + "/v1/card-actions/decide"
	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{{
		SenderUID: "notification", Owner: "tasks", ActionType: "task.decision",
		URL: callbackURL, SecretEnv: "OCTO_TASKS_CARD_ACTION_SECRET",
	}}, []string{callbackURL}, func(string) string { return secret })
	require.NoError(t, err)
	queue, err := cardactiondispatch.NewRedisQueue(rds, cardactiondispatch.QueueConfig{
		Prefix: prefix, LiveTTL: time.Hour, DLQRetention: 30 * 24 * time.Hour,
	})
	require.NoError(t, err)
	service, err := cardactiondispatch.NewService(registry, queue, ctx)
	require.NoError(t, err)
	eventID, err := service.Enqueue(cardactiondispatch.Event{
		SenderUID: "notification", Owner: "tasks", ActionType: "task.decision",
		MessageID: "task-message-1", ChannelID: "notification", ChannelType: common.ChannelTypePerson.Uint8(),
		SpaceID: "space-1", ActionID: "approve", OperatorUID: "user-b", ActedAt: time.Now().Unix(),
		Data: map[string]interface{}{"owner": "tasks", "action_type": "task.decision", "decision": "approve", "task_id": "task-1"},
	})
	require.NoError(t, err)

	mutator := &callbackE2EMutator{}
	sender := &callbackE2ESender{}
	standard, err := notify.NewStandardActionFinalizer(mutator, sender)
	require.NoError(t, err)
	finalizers, err := cardactiondispatch.NewFinalizerRegistry(standard, nil)
	require.NoError(t, err)
	dispatcher, err := cardactiondispatch.NewDispatcher(
		queue, registry, cardactiondispatch.NewHTTPDeliverer(consumer.Client().Transport, time.Now), finalizers,
		cardactiondispatch.DispatcherConfig{},
	)
	require.NoError(t, err)
	processed, err := dispatcher.ProcessOne(context.Background(), time.Now().Add(time.Second))
	require.NoError(t, err)
	require.True(t, processed)

	callback := <-callbackCalls
	require.NoError(t, callback.err)
	assert.True(t, callback.signatureValid)
	assert.Equal(t, eventID, callback.request.EventID)
	assert.Equal(t, "approve", callback.request.Decision)
	assert.Equal(t, "task-1", callback.request.Data["task_id"])
	require.Len(t, mutator.requests, 1)
	assert.Equal(t, "user-b", mutator.requests[0].ChannelID)
	assert.Contains(t, mutator.requests[0].ContentEdit, "approval.approved")
	require.Len(t, sender.targets, 1)
	assert.Equal(t, "user-a", sender.targets[0].ChannelID)
	assert.Equal(t, cardmsg.ProfileV1, sender.cards[0].Profile)
	depths, err := queue.Depths()
	require.NoError(t, err)
	assert.Equal(t, cardactiondispatch.QueueDepths{}, depths)
}

func seedCallbackE2EBotAndMessage(t *testing.T, ctx *config.Context, payload []byte) string {
	t.Helper()
	_, err := ctx.DB().InsertBySql("INSERT IGNORE INTO robot(robot_id,status) VALUES(?,1)", "notification").Exec()
	require.NoError(t, err)
	var envelope map[string]interface{}
	require.NoError(t, json.Unmarshal(payload, &envelope))
	sent, err := ctx.SendMessageWithResult(config.NewPersonalMsgSendReq(
		testutil.UID, "notification", envelope, "space-1", config.PersonalMsgOptions{},
	))
	require.NoError(t, err)
	require.NotZero(t, sent.MessageID)
	var messageSeq uint32
	require.Eventually(t, func() bool {
		response, searchErr := ctx.IMSearchMessages(&config.MsgSearchReq{
			LoginUID: "notification", ChannelID: testutil.UID,
			ChannelType: common.ChannelTypePerson.Uint8(), MessageIds: []int64{sent.MessageID},
		})
		if searchErr != nil || response == nil || len(response.Messages) == 0 {
			return false
		}
		messageSeq = response.Messages[0].MessageSeq
		return messageSeq > 0
	}, 6*time.Second, 100*time.Millisecond, "card message was not persisted by WuKongIM")
	messageID := fmt.Sprintf("%d", sent.MessageID)
	channelID := common.GetFakeChannelIDWith(testutil.UID, "notification")
	table := "message"
	if count := ctx.GetConfig().TablePartitionConfig.MessageTableCount; count > 0 {
		if index := crc32.ChecksumIEEE([]byte(channelID)) % uint32(count); index > 0 {
			table = fmt.Sprintf("message%d", index)
		}
	}
	_, err = ctx.DB().InsertBySql(
		fmt.Sprintf("INSERT INTO `%s` (message_id,message_seq,client_msg_no,from_uid,channel_id,channel_type,timestamp,payload,is_deleted) VALUES (?,?,?,?,?,?,?,?,0)", table),
		sent.MessageID, messageSeq, "callback-e2e-"+messageID, "notification", channelID, common.ChannelTypePerson.Uint8(), time.Now().Unix(), payload,
	).Exec()
	require.NoError(t, err)
	return messageID
}

func cardV2InternalEnvelope(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
		"profile": cardmsg.ProfileV2, "plain": "Document access request", "space_id": "space-1",
		"card": map[string]interface{}{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "Document access request"}},
			"actions": []interface{}{map[string]interface{}{
				"type": "Action.Submit", "id": "approve", "title": "Allow",
				"data": map[string]interface{}{
					"owner": "docs", "action_type": "access_request.decision", "decision": "approve",
					"doc_id": "doc-1", "request_id": "request-1",
				},
			}},
		},
	})
	require.NoError(t, err)
	return raw
}

func cleanupRedisPatterns(t *testing.T, client *redis.Client, patterns ...string) {
	t.Helper()
	for _, pattern := range patterns {
		keys, err := client.Keys(pattern).Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, client.Del(keys...).Err())
		}
	}
}
