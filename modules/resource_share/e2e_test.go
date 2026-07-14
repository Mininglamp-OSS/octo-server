package resource_share

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/resourceshare"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	jose "github.com/go-jose/go-jose/v3"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	resourceShareE2ESpace  = "resource-share-e2e-space"
	resourceShareE2EPeer   = "resource-share-e2e-peer"
	resourceShareE2EGroup  = "resource-share-e2e-group"
	resourceShareE2EThread = "resource-share-e2e-thread"
)

type resourceShareE2EAdapter struct{}

func (resourceShareE2EAdapter) Revalidate(
	_ context.Context,
	verified resourceshare.VerifiedIntent,
) (*resourceshare.RevalidatedResource, error) {
	var claims map[string]interface{}
	if err := json.Unmarshal(verified.Intent.Claims, &claims); err != nil {
		return nil, err
	}
	title, ok := claims["title"].(string)
	if !ok || title == "" {
		return nil, errors.New("title is required")
	}
	disclosures := make([]resourceshare.TargetDisclosure, 0, len(verified.Intent.Targets))
	for _, target := range verified.Intent.Targets {
		disclosures = append(disclosures, resourceshare.TargetDisclosure{Target: target, Allowed: true})
	}
	return &resourceshare.RevalidatedResource{
		Card: resourceshare.ResourceCardInput{
			Title: title, Description: "E2E verified resource share",
		},
		Disclosures: disclosures,
	}, nil
}

func TestResourceShareE2E_HTTPToWuKongIMAndIdempotentRetry(t *testing.T) {
	requireWuKongIME2E(t)
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("DM_THREAD_ON", "true")

	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	resetResourceShareE2ERedis(t, ctx)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		resetResourceShareE2ERedis(t, ctx)
	})
	seedResourceShareE2EAccess(t, ctx)

	intentKey := newProofTestKey(t)
	proofKey := newProofTestKey(t)
	proofSigningFile := writeJSONFile(t, "proof-signing.jwk", jose.JSONWebKey{
		Key: proofKey, KeyID: "resource-share-e2e-proof", Algorithm: string(jose.ES256), Use: "sig",
	})
	provider := resourceShareE2EProvider(t, intentKey)
	api, err := newProductionAPI(ctx, runtimeConfig{
		FeatureEnabled:          true,
		MaxConcurrentDispatches: 4,
		GlobalBudget:            resourceshare.RateBudget{RatePerSecond: 100, Burst: 100},
		DMBudget:                resourceshare.RateBudget{RatePerSecond: 100, Burst: 100},
		ChannelBudget:           resourceshare.RateBudget{RatePerSecond: 100, Burst: 100},
		LimitFailureRetry:       time.Second,
		ProofSigningJWKFile:     proofSigningFile,
	}, []resourceshare.ProviderSpec{provider})
	require.NoError(t, err)
	require.NotNil(t, api.verifier)

	router := wkhttp.New()
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	api.Route(router)

	intent := resourceShareE2EIntent()
	compact := signResourceShareE2EIntent(t, intentKey, intent)
	first := performResourceShareE2E(t, router, compact)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var sent resourceshare.ShareResult
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &sent))
	require.Len(t, sent.Results, len(intent.Targets))
	for index, result := range sent.Results {
		assert.Equal(t, intent.Targets[index], result.Target)
		assert.Equal(t, resourceshare.ShareSent, result.Outcome)
		assert.NotEmpty(t, result.MessageID)
		message := waitForResourceShareE2EMessage(t, ctx, result)
		assert.Equal(t, testutil.UID, message.FromUID)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(message.Payload, &payload))
		assert.Equal(t, float64(cardmsg.InteractiveCard.Int()), payload["type"])
		assert.Equal(t, resourceShareE2ESpace, payload["space_id"])
		observation := resourceshare.ProofObservation{
			ActorUID: testutil.UID, SpaceID: resourceShareE2ESpace, Target: result.Target,
		}
		if result.Target.Kind == resourceshare.TargetDM {
			observation.ViewerUID = testutil.UID
		}
		require.NoError(t, api.verifier.Verify(payload, observation))
	}

	retry := performResourceShareE2E(t, router, compact)
	require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
	var replay resourceshare.ShareResult
	require.NoError(t, json.Unmarshal(retry.Body.Bytes(), &replay))
	require.Len(t, replay.Results, len(sent.Results))
	for index, result := range replay.Results {
		assert.Equal(t, resourceshare.ShareAlreadySent, result.Outcome)
		assert.Equal(t, sent.Results[index].MessageID, result.MessageID)
		assert.Equal(t, sent.Results[index].MessageSeq, result.MessageSeq)
	}

	var deliveries, intents int
	require.NoError(t, ctx.DB().SelectBySql("SELECT COUNT(*) FROM resource_share_intent").LoadOne(&intents))
	require.NoError(t, ctx.DB().SelectBySql("SELECT COUNT(*) FROM resource_share_delivery").LoadOne(&deliveries))
	assert.Equal(t, 1, intents)
	assert.Equal(t, len(intent.Targets), deliveries)
}

func resourceShareE2EProvider(t *testing.T, key *ecdsa.PrivateKey) resourceshare.ProviderSpec {
	t.Helper()
	return resourceshare.ProviderSpec{
		ID:            "smart-summary",
		Enabled:       true,
		ResourceType:  "smart-summary",
		Issuer:        "https://smart-summary.e2e.internal",
		Audience:      "octo-server:resource-share",
		IntentVersion: resourceshare.PlatformIntentVersion,
		VerificationKeys: []resourceshare.VerificationKey{{
			KeyID: "resource-share-e2e-intent", Algorithm: jose.ES256, PublicKey: &key.PublicKey,
		}},
		Templates: []resourceshare.TemplateRef{{ID: "summary-share", Version: 1}},
		Limits: resourceshare.ProviderLimits{
			MaxClaimsBytes: 1024, MaxTargets: 3, MaxIntentLifetime: 2 * time.Minute,
			ClockSkew: 5 * time.Second, TargetBudget: resourceshare.RateBudget{RatePerSecond: 100, Burst: 100},
		},
		ValidateClaims: func(raw json.RawMessage) error {
			var claims map[string]interface{}
			if err := json.Unmarshal(raw, &claims); err != nil {
				return err
			}
			title, ok := claims["title"].(string)
			if !ok || title == "" {
				return errors.New("title is required")
			}
			return nil
		},
		BuildDeepLink: func(ref resourceshare.ResourceRef) (*url.URL, error) {
			return url.Parse("https://app.example.test/summaries/" + url.PathEscape(ref.ID))
		},
		RenderCard: func(input resourceshare.ResourceCardInput, link *url.URL) (map[string]interface{}, error) {
			return map[string]interface{}{
				"type": "AdaptiveCard", "version": "1.5",
				"body": []interface{}{
					map[string]interface{}{"type": "TextBlock", "text": input.Title},
					map[string]interface{}{"type": "TextBlock", "text": input.Description},
				},
				"actions": []interface{}{
					map[string]interface{}{"type": "Action.OpenUrl", "title": "Open", "url": link.String()},
				},
			}, nil
		},
		Adapter: resourceShareE2EAdapter{},
	}
}

func resourceShareE2EIntent() resourceshare.Intent {
	now := time.Now()
	claims, _ := json.Marshal(map[string]string{"title": "E2E smart summary"})
	return resourceshare.Intent{
		Version: resourceshare.PlatformIntentVersion, Provider: "smart-summary",
		Issuer: "https://smart-summary.e2e.internal", Audience: "octo-server:resource-share",
		ActorUID: testutil.UID, SpaceID: resourceShareE2ESpace,
		IssuedAt: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(90 * time.Second).Unix(),
		Nonce: "nonce-" + util.GenerUUID(), IdempotencyKey: "idem-" + util.GenerUUID(),
		Resource: resourceshare.ResourceRef{Type: "smart-summary", ID: "summary-e2e", Revision: "revision-1"},
		Template: resourceshare.TemplateRef{ID: "summary-share", Version: 1},
		Targets: []resourceshare.Target{
			{Kind: resourceshare.TargetDM, PeerUID: resourceShareE2EPeer},
			{Kind: resourceshare.TargetGroup, GroupNo: resourceShareE2EGroup},
			{Kind: resourceshare.TargetThread, GroupNo: resourceShareE2EGroup, ShortID: resourceShareE2EThread},
		},
		Claims: claims,
	}
}

func signResourceShareE2EIntent(t *testing.T, key *ecdsa.PrivateKey, intent resourceshare.Intent) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), "resource-share-e2e-intent")
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, opts)
	require.NoError(t, err)
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	signed, err := signer.Sign(raw)
	require.NoError(t, err)
	compact, err := signed.CompactSerialize()
	require.NoError(t, err)
	return compact
}

func performResourceShareE2E(t *testing.T, router http.Handler, compact string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"intent": compact})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/resource-shares", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("token", testutil.Token)
	request.Header.Set("X-Space-ID", resourceShareE2ESpace)
	router.ServeHTTP(recorder, request)
	return recorder
}

func seedResourceShareE2EAccess(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space(space_id,name,status) VALUES(?,?,1)", resourceShareE2ESpace, "Resource Share E2E",
	).Exec()
	require.NoError(t, err)
	for _, uid := range []string{testutil.UID, resourceShareE2EPeer} {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO space_member(space_id,uid,role,status,created_at,updated_at) VALUES(?,?,0,1,NOW(),NOW())",
			resourceShareE2ESpace, uid,
		).Exec()
		require.NoError(t, err)
	}
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO \x60group\x60(group_no,name,creator,status,version,space_id) VALUES(?,?,?,?,1,?)",
		resourceShareE2EGroup, "Resource Share E2E Group", testutil.UID, group.GroupStatusNormal, resourceShareE2ESpace,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member(group_no,uid,role,status,is_deleted,version) VALUES(?,?,0,1,0,1)",
		resourceShareE2EGroup, testutil.UID,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO thread(short_id,group_no,name,creator_uid,status,version) VALUES(?,?,?,?,?,1)",
		resourceShareE2EThread, resourceShareE2EGroup, "Resource Share E2E Thread", testutil.UID, thread.ThreadStatusActive,
	).Exec()
	require.NoError(t, err)
}

func waitForResourceShareE2EMessage(
	t *testing.T,
	ctx *config.Context,
	result resourceshare.TargetResult,
) *config.MessageResp {
	t.Helper()
	messageID, err := strconv.ParseInt(result.MessageID, 10, 64)
	require.NoError(t, err)
	channelID, channelType := resourceShareE2EChannel(result.Target)
	var found *config.MessageResp
	require.Eventually(t, func() bool {
		response, searchErr := ctx.IMSearchMessages(&config.MsgSearchReq{
			LoginUID: testutil.UID, ChannelID: channelID, ChannelType: channelType,
			MessageIds: []int64{messageID},
		})
		if searchErr != nil || response == nil || len(response.Messages) == 0 {
			return false
		}
		found = response.Messages[0]
		return found.MessageSeq > 0
	}, 10*time.Second, 200*time.Millisecond, "resource share message %s was not persisted by WuKongIM", result.MessageID)
	return found
}

func resourceShareE2EChannel(target resourceshare.Target) (string, uint8) {
	switch target.Kind {
	case resourceshare.TargetDM:
		return target.PeerUID, common.ChannelTypePerson.Uint8()
	case resourceshare.TargetGroup:
		return target.GroupNo, common.ChannelTypeGroup.Uint8()
	case resourceshare.TargetThread:
		return thread.BuildChannelID(target.GroupNo, target.ShortID), common.ChannelTypeCommunityTopic.Uint8()
	default:
		return "", 0
	}
}

func resetResourceShareE2ERedis(t *testing.T, ctx *config.Context) {
	t.Helper()
	client := rd.NewClient(&rd.Options{
		Addr: ctx.GetConfig().DB.RedisAddr, Password: ctx.GetConfig().DB.RedisPass,
	})
	defer client.Close()
	for _, pattern := range []string{"ratelimit:uid:*", "resource-share:v1:*", "space:member:*"} {
		keys, err := client.Keys(pattern).Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, client.Del(keys...).Err())
		}
	}
}

func requireWuKongIME2E(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:5001/health")
	if err != nil {
		t.Skip("WuKongIM is not running on 127.0.0.1:5001")
	}
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
}
