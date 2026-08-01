package bot_mention

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

const botMentionIntegrationExpire = 2 * time.Minute

func TestNewRejectsInternalTokenCapabilityCollisions(t *testing.T) {
	tests := []struct {
		name         string
		mentionToken string
		notifyToken  string
		docsToken    string
		want         string
	}{
		{name: "independent token enabled", mentionToken: "mention-secret", notifyToken: "notify-secret", docsToken: "docs-notify-secret", want: "mention-secret"},
		{name: "empty token disabled", notifyToken: "notify-secret", docsToken: "docs-notify-secret"},
		{name: "legacy notify collision disabled", mentionToken: "shared-secret", notifyToken: "shared-secret", docsToken: "docs-notify-secret"},
		{name: "docs notify collision disabled", mentionToken: "shared-secret", notifyToken: "notify-secret", docsToken: "shared-secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(internalTokenEnv, tt.mentionToken)
			t.Setenv("NOTIFY_INTERNAL_TOKEN", tt.notifyToken)
			t.Setenv("OCTO_DOCS_NOTIFY_TOKEN", tt.docsToken)

			ctx, _ := newBotMentionIntegrationContext(t, botMentionIntegrationExpire)
			module := New(ctx)
			closeBotMentionClaimClient(t, module.claims)

			if module.internalToken != tt.want {
				t.Fatalf("internal token = %q, want %q", module.internalToken, tt.want)
			}
		})
	}
}

func TestBotMentionMySQLRedisRobotQueueIntegration(t *testing.T) {
	ctx, cfg := newBotMentionIntegrationContext(t, botMentionIntegrationExpire)
	nonce := time.Now().UnixNano()
	botID := fmt.Sprintf("bm-%d", nonce)
	botToken := fmt.Sprintf("bf_bm_%d", nonce)
	request := validMentionRequest()
	request.BotUID = botID
	request.IdempotencyKey = fmt.Sprintf("idem-%d", nonce)
	ensureBotMentionIntegrationSchema(t, ctx)
	prepareActiveUserBot(t, ctx, botID, botToken)

	claims := newRedisClaimStore(ctx)
	redisClient := claims.backend.(*redisClaimBackend).client
	t.Cleanup(func() { _ = redisClient.Close() })
	if err := redisClient.Ping().Err(); err != nil {
		t.Fatalf("real Redis is required for bot mention integration test at %s: %v", cfg.DB.RedisAddr, err)
	}

	queueKey := "robotEvent:" + botID
	claimKey := mentionClaimKey(botID, request.IdempotencyKey)
	t.Cleanup(func() { _ = redisClient.Del(queueKey, claimKey).Err() })

	module := &BotMention{
		robots:        robot.NewService(ctx),
		claims:        claims,
		gate:          newFeatureGate(true, "", "*"),
		internalToken: "mention-internal-secret",
		metrics:       &fakeMetricRecorder{},
		now:           time.Now,
		Log:           log.NewTLog("BotMentionRedisIntegrationTest"),
	}
	router := wkhttp.New()
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	module.Route(router)
	bot_api.NewBotAPI(ctx).Route(router)

	first := doMentionRequest(t, router, module.internalToken, request)
	if first.Code != http.StatusOK {
		t.Fatalf("first ingress status = %d, body=%s", first.Code, first.Body.String())
	}
	accepted := decodeMentionResponse(t, first)
	if !accepted.Accepted || accepted.Replay || accepted.EventID <= 0 {
		t.Fatalf("first ingress response = %+v", accepted)
	}

	if got := redisClient.ZCard(queueKey).Val(); got != 1 {
		t.Fatalf("queue size after first ingress = %d, want 1", got)
	}
	assertRedisTTLTracksMessageExpire(t, redisClient.TTL(queueKey).Val(), cfg.Robot.MessageExpire, "queue")
	assertRedisTTLTracksMessageExpire(t, redisClient.TTL(claimKey).Val(), cfg.Robot.MessageExpire, "confirmed claim")
	assertQueuedEventExpireTracksMessageExpire(t, redisClient, queueKey, cfg.Robot.MessageExpire)

	replay := doMentionRequest(t, router, module.internalToken, request)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body=%s", replay.Code, replay.Body.String())
	}
	replayed := decodeMentionResponse(t, replay)
	if !replayed.Accepted || !replayed.Replay || replayed.EventID != accepted.EventID {
		t.Fatalf("replay response = %+v, want event_id %d", replayed, accepted.EventID)
	}
	if got := redisClient.ZCard(queueKey).Val(); got != 1 {
		t.Fatalf("queue size after replay = %d, want 1", got)
	}

	pulled := doBotAPIRequest(t, router, http.MethodPost, "/v1/bot/events", botToken, []byte("{\"event_id\":0,\"limit\":10}"))
	if pulled.Code != http.StatusOK {
		t.Fatalf("pull status = %d, body=%s", pulled.Code, pulled.Body.String())
	}
	var events struct {
		Status  int `json:"status"`
		Results []struct {
			EventID   int64                  `json:"event_id"`
			EventType string                 `json:"event_type"`
			EventData map[string]interface{} `json:"event_data"`
		} `json:"results"`
	}
	if err := json.Unmarshal(pulled.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode pulled events: %v, body=%s", err, pulled.Body.String())
	}
	if events.Status != 1 || len(events.Results) != 1 {
		t.Fatalf("pulled events = %+v", events)
	}
	event := events.Results[0]
	if event.EventID != accepted.EventID || event.EventType != docCommentMentionEventType {
		t.Fatalf("pulled event id/type = %d/%q", event.EventID, event.EventType)
	}
	if event.EventData["idempotency_key"] != request.IdempotencyKey || event.EventData["bot_uid"] != botID {
		t.Fatalf("pulled event data = %#v", event.EventData)
	}

	ackPath := fmt.Sprintf("/v1/bot/events/%d/ack", accepted.EventID)
	acked := doBotAPIRequest(t, router, http.MethodPost, ackPath, botToken, nil)
	if acked.Code != http.StatusOK {
		t.Fatalf("ack status = %d, body=%s", acked.Code, acked.Body.String())
	}
	if got := redisClient.ZCard(queueKey).Val(); got != 0 {
		t.Fatalf("queue size after ACK = %d, want 0", got)
	}

	afterACKReplay := doMentionRequest(t, router, module.internalToken, request)
	if afterACKReplay.Code != http.StatusOK {
		t.Fatalf("post-ACK replay status = %d, body=%s", afterACKReplay.Code, afterACKReplay.Body.String())
	}
	if response := decodeMentionResponse(t, afterACKReplay); !response.Replay || response.EventID != accepted.EventID {
		t.Fatalf("post-ACK replay response = %+v", response)
	}
	if got := redisClient.ZCard(queueKey).Val(); got != 0 {
		t.Fatalf("queue size after post-ACK replay = %d, want 0", got)
	}

	assertRealRedisClaimCAS(t, claims, redisClient)
}

func newBotMentionIntegrationContext(t *testing.T, messageExpire time.Duration) (*config.Context, *config.Config) {
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
		t.Fatalf("real MySQL is required for bot mention integration test: SELECT 1 = %d, %v", one, err)
	}
	t.Cleanup(func() { _ = ctx.DB().Close() })
	return ctx, cfg
}

// ensureBotMentionIntegrationSchema keeps this package test self-contained.
// CI drops and recreates the shared test database before every Go package, so
// the bot_mention package cannot rely on robot/common migrations run elsewhere.
// IF NOT EXISTS preserves the production-like local schema when it is present.
func ensureBotMentionIntegrationSchema(t *testing.T, ctx *config.Context) {
	t.Helper()
	statements := []struct {
		name string
		sql  string
	}{
		{
			name: "robot",
			sql: `CREATE TABLE IF NOT EXISTS robot (
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
		},
		{
			name: "seq",
			sql: `CREATE TABLE IF NOT EXISTS seq (
				id INT NOT NULL AUTO_INCREMENT,
				` + "`key`" + ` VARCHAR(100) NOT NULL DEFAULT '',
				min_seq BIGINT NOT NULL DEFAULT 1000000,
				step INT NOT NULL DEFAULT 1000,
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (id),
				UNIQUE KEY seq_uidx (` + "`key`" + `)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
		},
		{
			name: "system_setting",
			sql: `CREATE TABLE IF NOT EXISTS system_setting (
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
		},
	}
	for _, statement := range statements {
		if _, err := ctx.DB().DB.Exec(statement.sql); err != nil {
			t.Fatalf("create integration %s table: %v", statement.name, err)
		}
	}
}

func prepareActiveUserBot(t *testing.T, ctx *config.Context, botID, botToken string) {
	t.Helper()
	if _, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		botID, "bot-mention-integration-owner", botToken,
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

func closeBotMentionClaimClient(t *testing.T, store botMentionClaimStore) {
	t.Helper()
	claims, ok := store.(*claimStore)
	if !ok {
		return
	}
	backend, ok := claims.backend.(*redisClaimBackend)
	if ok {
		t.Cleanup(func() { _ = backend.client.Close() })
	}
}

func doBotAPIRequest(t *testing.T, router *wkhttp.WKHttp, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func assertRedisTTLTracksMessageExpire(t *testing.T, got, want time.Duration, kind string) {
	t.Helper()
	if got <= want-3*time.Second || got > want {
		t.Fatalf("%s TTL = %v, want within (%v, %v]", kind, got, want-3*time.Second, want)
	}
}

func assertQueuedEventExpireTracksMessageExpire(t *testing.T, client *redis.Client, queueKey string, expire time.Duration) {
	t.Helper()
	values, err := client.ZRange(queueKey, 0, -1).Result()
	if err != nil || len(values) != 1 {
		t.Fatalf("read queued event = %v, %v", values, err)
	}
	var record struct {
		Expire int64 `json:"expire"`
	}
	if err := json.Unmarshal([]byte(values[0]), &record); err != nil {
		t.Fatalf("decode queued event: %v", err)
	}
	want := time.Now().Add(expire).Unix()
	if record.Expire < want-3 || record.Expire > want+1 {
		t.Fatalf("queued event expire = %d, want around %d", record.Expire, want)
	}
}

func assertRealRedisClaimCAS(t *testing.T, claims *claimStore, client *redis.Client) {
	t.Helper()
	key := mentionClaimPrefix + fmt.Sprintf("cas-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = client.Del(key).Err() })

	outcome, lease, err := claims.Begin(key, "sha-original")
	if err != nil || outcome.State != claimAcquired || lease == nil {
		t.Fatalf("real Redis begin = %+v, lease=%+v, err=%v", outcome, lease, err)
	}
	assertRedisTTLTracksMessageExpire(t, client.TTL(key).Val(), claimPendingTTL, "pending claim")

	replacement := "{\"state\":\"pending\",\"sha\":\"sha-replacement\",\"token\":\"replacement\"}"
	if err := client.Set(key, replacement, claimPendingTTL).Err(); err != nil {
		t.Fatalf("replace pending claim: %v", err)
	}
	if released, err := claims.Release(lease); err != nil || released {
		t.Fatalf("stale real Redis release = %v, %v; want false, nil", released, err)
	}
	if confirmed, err := claims.Confirm(lease, 99); err != nil || confirmed {
		t.Fatalf("stale real Redis confirm = %v, %v; want false, nil", confirmed, err)
	}
	if got, err := client.Get(key).Result(); err != nil || !strings.Contains(got, "sha-replacement") {
		t.Fatalf("replacement claim after stale CAS = %q, %v", got, err)
	}
}
