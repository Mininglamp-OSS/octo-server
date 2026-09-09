package bot_task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/bot_api"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/go-redis/redis"
)

const botTaskIntegrationExpire = 2 * time.Minute

// TestBotTaskMySQLRedisRobotQueueIntegration covers the production path that
// unit fakes cannot: authenticate a source, reserve the real Redis claim,
// atomically CAS it to done while enqueueing a real robot event, consume that
// event through /v1/bot/events, and ACK it from the queue.
func TestBotTaskMySQLRedisRobotQueueIntegration(t *testing.T) {
	ctx, cfg := newBotTaskIntegrationContext(t, botTaskIntegrationExpire)
	nonce := time.Now().UnixNano()
	botID := fmt.Sprintf("bt-%d", nonce)
	botToken := fmt.Sprintf("bf_bt_%d", nonce)
	source := fmt.Sprintf("loop-%d", nonce)
	sourceToken := fmt.Sprintf("source-token-%d-0123456789abcdef", nonce)
	request := validTaskRequest()
	request.Source = source
	request.BotUID = botID
	request.IdempotencyKey = fmt.Sprintf("comment-%d", nonce)
	request.Context = json.RawMessage(`{"issue_id":9007199254740993,"comment_id":"comment-1"}`)
	request.Metadata = json.RawMessage(`{"anchor":"comment-1"}`)

	ensureBotTaskIntegrationSchema(t, ctx)
	prepareBotTaskActiveUserBot(t, ctx, botID, botToken)

	claims := newRedisClaimStore(ctx)
	redisClient := claims.backend.(*redisClaimBackend).client
	t.Cleanup(func() { _ = redisClient.Close() })
	if err := redisClient.Ping().Err(); err != nil {
		t.Fatalf("real Redis is required for bot task integration test at %s: %v", cfg.DB.RedisAddr, err)
	}

	queueKey := "robotEvent:" + botID
	claimKey := taskClaimKey(source, botID, request.IdempotencyKey)
	t.Cleanup(func() {
		_ = redisClient.Del(queueKey, claimKey, "ratelimit:bot_task_source:"+source).Err()
	})

	module := &BotTask{
		ctx:            ctx,
		robots:         robot.NewService(ctx),
		claims:         claims,
		sources:        sourceRegistry{source: {Token: sourceToken, Enabled: true, AllowedBotUIDs: []string{botID}}},
		now:            time.Now,
		notifyBotEvent: func(string) {},
		Log:            log.NewTLog("BotTaskRedisIntegrationTest"),
	}
	router := wkhttp.New()
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	module.Route(router)
	bot_api.NewBotAPI(ctx).Route(router)

	first := doTaskRequest(t, router, sourceToken, request)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first ingress status=%d body=%s", first.Code, first.Body.String())
	}
	accepted := decodeBotTaskIngress(t, first)
	if !accepted.Accepted || accepted.Replay || accepted.EventID <= 0 {
		t.Fatalf("first ingress response=%+v", accepted)
	}
	if got := redisClient.ZCard(queueKey).Val(); got != 1 {
		t.Fatalf("queue size after ingress=%d, want 1", got)
	}
	assertBotTaskRedisTTL(t, redisClient.TTL(queueKey).Val(), cfg.Robot.MessageExpire, "queue")
	assertBotTaskRedisTTL(t, redisClient.TTL(claimKey).Val(), cfg.Robot.MessageExpire, "confirmed claim")

	replay := doTaskRequest(t, router, sourceToken, request)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	replayed := decodeBotTaskIngress(t, replay)
	if !replayed.Accepted || !replayed.Replay || replayed.EventID != accepted.EventID {
		t.Fatalf("replay response=%+v, want event_id=%d", replayed, accepted.EventID)
	}
	if got := redisClient.ZCard(queueKey).Val(); got != 1 {
		t.Fatalf("queue size after replay=%d, want 1", got)
	}

	pulled := doBotTaskBotAPIRequest(t, router, http.MethodPost, "/v1/bot/events", botToken, []byte(`{"event_id":0,"limit":10}`))
	if pulled.Code != http.StatusOK {
		t.Fatalf("pull status=%d body=%s", pulled.Code, pulled.Body.String())
	}
	var events struct {
		Status  int `json:"status"`
		Results []struct {
			EventID   int64           `json:"event_id"`
			EventType string          `json:"event_type"`
			EventData json.RawMessage `json:"event_data"`
		} `json:"results"`
	}
	if err := json.Unmarshal(pulled.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode pulled events: %v body=%s", err, pulled.Body.String())
	}
	if events.Status != 1 || len(events.Results) != 1 {
		t.Fatalf("pulled events=%+v", events)
	}
	event := events.Results[0]
	if event.EventID != accepted.EventID || event.EventType != botTaskEventType {
		t.Fatalf("pulled event id/type=%d/%q", event.EventID, event.EventType)
	}
	assertBotTaskEventData(t, event.EventData, request)

	ackPath := fmt.Sprintf("/v1/bot/events/%d/ack", accepted.EventID)
	acked := doBotTaskBotAPIRequest(t, router, http.MethodPost, ackPath, botToken, nil)
	if acked.Code != http.StatusOK {
		t.Fatalf("ack status=%d body=%s", acked.Code, acked.Body.String())
	}
	if got := redisClient.ZCard(queueKey).Val(); got != 0 {
		t.Fatalf("queue size after ACK=%d, want 0", got)
	}

	afterACKReplay := doTaskRequest(t, router, sourceToken, request)
	if afterACKReplay.Code != http.StatusOK {
		t.Fatalf("post-ACK replay status=%d body=%s", afterACKReplay.Code, afterACKReplay.Body.String())
	}
	if response := decodeBotTaskIngress(t, afterACKReplay); !response.Replay || response.EventID != accepted.EventID {
		t.Fatalf("post-ACK replay response=%+v", response)
	}
	if got := redisClient.ZCard(queueKey).Val(); got != 0 {
		t.Fatalf("queue size after post-ACK replay=%d, want 0", got)
	}

	assertBotTaskRealRedisClaimCAS(t, claims, redisClient)
}

func newBotTaskIntegrationContext(t *testing.T, messageExpire time.Duration) (*config.Context, *config.Config) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.Robot.MessageExpire = messageExpire
	cfg.DB.MySQLAddr = "root:demo@tcp(127.0.0.1:3306)/test?charset=utf8mb4&parseTime=true"
	if dsn := os.Getenv("OCTO_TEST_MYSQL_DSN"); dsn != "" {
		cfg.DB.MySQLAddr = dsn
	}
	if addr := os.Getenv("OCTO_TEST_REDIS_ADDR"); addr != "" {
		cfg.DB.RedisAddr = addr
	}
	ctx := config.NewContext(cfg)
	var one int
	if err := ctx.DB().SelectBySql("SELECT 1").LoadOne(&one); err != nil || one != 1 {
		t.Fatalf("real MySQL is required for bot task integration test: SELECT 1=%d, %v", one, err)
	}
	t.Cleanup(func() { _ = ctx.DB().Close() })
	return ctx, cfg
}

func ensureBotTaskIntegrationSchema(t *testing.T, ctx *config.Context) {
	t.Helper()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS robot (
			id BIGINT NOT NULL AUTO_INCREMENT,
			robot_id VARCHAR(40) NOT NULL DEFAULT '',
			token VARCHAR(100) NOT NULL DEFAULT '',
			version BIGINT NOT NULL DEFAULT 0,
			status SMALLINT NOT NULL DEFAULT 1,
			creator_uid VARCHAR(40) NOT NULL DEFAULT '',
			bot_token VARCHAR(100) NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY robot_id_robot_index (robot_id),
			UNIQUE KEY idx_robot_bot_token (bot_token)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
		`CREATE TABLE IF NOT EXISTS app_bot (
			id VARCHAR(40) NOT NULL,
			uid VARCHAR(40) NOT NULL,
			display_name VARCHAR(100) NOT NULL,
			description VARCHAR(500) NOT NULL DEFAULT '',
			avatar VARCHAR(200) NOT NULL DEFAULT '',
			scope VARCHAR(20) NOT NULL DEFAULT 'platform',
			space_id VARCHAR(40) DEFAULT NULL,
			status TINYINT NOT NULL DEFAULT 0,
			token VARCHAR(100) NOT NULL,
			welcome_msg VARCHAR(500) NOT NULL DEFAULT '',
			created_by VARCHAR(40) NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY uk_app_bot_uid (uid),
			UNIQUE KEY uk_app_bot_token (token)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
		`CREATE TABLE IF NOT EXISTS seq (
			id INT NOT NULL AUTO_INCREMENT,
			` + "`key`" + ` VARCHAR(100) NOT NULL DEFAULT '',
			min_seq BIGINT NOT NULL DEFAULT 1000000,
			step INT NOT NULL DEFAULT 1000,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY seq_uidx (` + "`key`" + `)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
		`CREATE TABLE IF NOT EXISTS system_setting (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			category VARCHAR(64) NOT NULL,
			key_name VARCHAR(128) NOT NULL,
			value TEXT NOT NULL,
			value_type VARCHAR(16) NOT NULL DEFAULT 'string',
			description VARCHAR(255) NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY uk_category_key (category, key_name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
	}
	for index, statement := range statements {
		if _, err := ctx.DB().DB.Exec(statement); err != nil {
			t.Fatalf("create integration schema statement %d: %v", index, err)
		}
	}
}

func prepareBotTaskActiveUserBot(t *testing.T, ctx *config.Context, botID, botToken string) {
	t.Helper()
	if _, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		botID, "bot-task-integration-owner", botToken,
	).Exec(); err != nil {
		t.Fatalf("insert integration User Bot: %v", err)
	}
	seqKey := "seq:" + common.RobotEventSeqKey + botID
	t.Cleanup(func() {
		if _, err := ctx.DB().DeleteFrom("seq").Where("`key`=?", seqKey).Exec(); err != nil {
			t.Errorf("cleanup integration sequence: %v", err)
		}
		if _, err := ctx.DB().DeleteFrom("robot").Where("robot_id=?", botID).Exec(); err != nil {
			t.Errorf("cleanup integration User Bot: %v", err)
		}
	})
}

func decodeBotTaskIngress(t *testing.T, response *httptest.ResponseRecorder) ingressResponse {
	t.Helper()
	var decoded ingressResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode ingress response: %v body=%s", err, response.Body.String())
	}
	return decoded
}

func doBotTaskBotAPIRequest(t *testing.T, router *wkhttp.WKHttp, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func assertBotTaskRedisTTL(t *testing.T, got, want time.Duration, kind string) {
	t.Helper()
	if got <= want-3*time.Second || got > want {
		t.Fatalf("%s TTL=%v, want within (%v, %v]", kind, got, want-3*time.Second, want)
	}
}

func assertBotTaskEventData(t *testing.T, raw json.RawMessage, request taskRequest) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var data map[string]interface{}
	if err := decoder.Decode(&data); err != nil {
		t.Fatalf("decode event_data: %v data=%s", err, raw)
	}
	if data["source"] != request.Source || data["task_type"] != request.TaskType ||
		data["idempotency_key"] != request.IdempotencyKey || data["bot_uid"] != request.BotUID ||
		data["actor_uid"] != request.ActorUID || data["session_key"] != request.SessionKey ||
		data["prompt"] != request.Prompt {
		t.Fatalf("event_data fields=%#v", data)
	}
	context, ok := data["context"].(map[string]interface{})
	if !ok || context["issue_id"] != json.Number("9007199254740993") || context["comment_id"] != "comment-1" {
		t.Fatalf("event_data context=%#v", data["context"])
	}
	metadata, ok := data["metadata"].(map[string]interface{})
	if !ok || metadata["anchor"] != "comment-1" {
		t.Fatalf("event_data metadata=%#v", data["metadata"])
	}
	if _, ok := data["enqueued_at"].(json.Number); !ok {
		t.Fatalf("event_data enqueued_at=%#v", data["enqueued_at"])
	}
}

func assertBotTaskRealRedisClaimCAS(t *testing.T, claims *claimStore, client *redis.Client) {
	t.Helper()
	key := claimPrefix + fmt.Sprintf("cas-%d", time.Now().UnixNano())
	queueKey := "robotEvent:" + key
	t.Cleanup(func() { _ = client.Del(key, queueKey).Err() })
	outcome, lease, err := claims.Begin(key, "sha-original")
	if err != nil || outcome.State != claimAcquired || lease == nil {
		t.Fatalf("real Redis begin=%+v lease=%+v err=%v", outcome, lease, err)
	}
	assertBotTaskRedisTTL(t, client.TTL(key).Val(), claimPendingTTL, "pending claim")
	replacement := `{"state":"pending","sha":"sha-replacement","token":"replacement"}`
	if err := client.Set(key, replacement, claimPendingTTL).Err(); err != nil {
		t.Fatalf("replace pending claim: %v", err)
	}
	if released, err := claims.Release(lease); err != nil || released {
		t.Fatalf("stale real Redis release=%v, %v; want false, nil", released, err)
	}
	prepared := robot.PreparedBotTypedEvent{EventID: 99, QueueKey: queueKey, Member: `{"event_id":99}`}
	if committed, err := claims.Commit(lease, prepared); err != nil || committed {
		t.Fatalf("stale real Redis commit=%v, %v; want false, nil", committed, err)
	}
	if got := client.ZCard(queueKey).Val(); got != 0 {
		t.Fatalf("stale real Redis commit queued %d events, want 0", got)
	}
	if got, err := client.Get(key).Result(); err != nil || !strings.Contains(got, "sha-replacement") {
		t.Fatalf("replacement claim after stale CAS=%q, %v", got, err)
	}
}
