package resource_share

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/resourceshare"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/reqid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeShareService struct {
	result  *resourceshare.ShareResult
	err     error
	calls   int
	login   string
	space   string
	intent  string
	request string
}

func (s *fakeShareService) Share(
	_ context.Context,
	loginUID, spaceID, compactIntent, requestID string,
) (*resourceshare.ShareResult, error) {
	s.calls++
	s.login = loginUID
	s.space = spaceID
	s.intent = compactIntent
	s.request = requestID
	return s.result, s.err
}

type resourceShareEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
		Message    string `json:"message"`
	} `json:"error"`
}

func resourceShareHarness(service shareService, verifier *resourceshare.ProofVerifier) (*wkhttp.WKHttp, *API) {
	api := newAPI(nil, service, verifier)
	router := wkhttp.New()
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	router.POST("/v1/resource-shares", func(c *wkhttp.Context) {
		c.Set("uid", "user-a")
		c.Set("space_id", "space-a")
		c.Request = c.Request.WithContext(reqid.WithTraceID(c.Request.Context(), "request-1"))
		api.share(c)
	})
	router.GET("/v1/resource-shares/proof-jwks", api.proofJWKS)
	return router, api
}

func performResourceShare(router http.Handler, body string, spaceHeader string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/resource-shares", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if spaceHeader != "" {
		request.Header.Set("X-Space-ID", spaceHeader)
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestAPI_ShareAcceptsOnlyIntentAndUsesAuthenticatedContext(t *testing.T) {
	service := &fakeShareService{result: &resourceshare.ShareResult{Results: []resourceshare.TargetResult{{
		Target:  resourceshare.Target{Kind: resourceshare.TargetGroup, GroupNo: "group-a"},
		Outcome: resourceshare.ShareSent, MessageID: "99", MessageSeq: 7,
	}}}}
	router, _ := resourceShareHarness(service, nil)

	response := performResourceShare(router, `{"intent":"signed.intent.value"}`, "space-a")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, 1, service.calls)
	assert.Equal(t, "user-a", service.login)
	assert.Equal(t, "space-a", service.space)
	assert.Equal(t, "signed.intent.value", service.intent)
	assert.Equal(t, "request-1", service.request)
	assert.NotContains(t, response.Body.String(), "signed.intent.value")
	assert.NotContains(t, response.Body.String(), resourceshare.ProofField)

	var result resourceshare.ShareResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(t, result.Results, 1)
	assert.Equal(t, resourceshare.ShareSent, result.Results[0].Outcome)
}

func TestAPI_ShareRejectsNilServiceResult(t *testing.T) {
	service := &fakeShareService{}
	router, _ := resourceShareHarness(service, nil)
	response := performResourceShare(router, `{"intent":"a.b.c"}`, "space-a")
	assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assertEnvelope(t, response, "err.server.resource_share.unavailable", http.StatusServiceUnavailable)
}

func TestAPI_ShareRejectsMalformedOrExpandedRequestBeforeService(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "missing intent", body: `{}`},
		{name: "empty intent", body: `{"intent":""}`},
		{name: "unknown field", body: `{"intent":"a.b.c","from_uid":"attacker"}`},
		{name: "caller card", body: `{"intent":"a.b.c","card":{"type":"AdaptiveCard"}}`},
		{name: "second json value", body: `{"intent":"a.b.c"} {}`},
		{name: "wrong intent type", body: `{"intent":{"compact":"a.b.c"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &fakeShareService{}
			router, _ := resourceShareHarness(service, nil)
			response := performResourceShare(router, tt.body, "space-a")
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Equal(t, 0, service.calls)
			assertEnvelope(t, response, "err.server.resource_share.request_invalid", http.StatusBadRequest)
		})
	}
}

func TestAPI_ShareRequiresExactValidatedSpaceHeader(t *testing.T) {
	for _, header := range []string{"", "space-b"} {
		t.Run("header="+header, func(t *testing.T) {
			service := &fakeShareService{}
			router, _ := resourceShareHarness(service, nil)
			response := performResourceShare(router, `{"intent":"a.b.c"}`, header)
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Equal(t, 0, service.calls)
			assertEnvelope(t, response, "err.server.resource_share.request_invalid", http.StatusBadRequest)
		})
	}
}

func TestAPI_ShareCapsBodyBeforeDecode(t *testing.T) {
	service := &fakeShareService{}
	router, _ := resourceShareHarness(service, nil)
	body := `{"intent":"` + strings.Repeat("a", maxRequestBodyBytes) + `"}`
	response := performResourceShare(router, body, "space-a")

	assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Equal(t, 0, service.calls)
	assertEnvelope(t, response, "err.server.resource_share.payload_too_large", http.StatusRequestEntityTooLarge)
}

func TestAPI_ShareMapsRequestWideFailuresWithoutLeakingDetails(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		code       string
		httpStatus int
	}{
		{name: "disabled", err: resourceshare.ErrShareDisabled, code: "err.server.resource_share.disabled", httpStatus: http.StatusForbidden},
		{name: "actor or space", err: resourceshare.ErrShareForbidden, code: "err.server.resource_share.forbidden", httpStatus: http.StatusForbidden},
		{name: "replay", err: resourceshare.ErrIntentReplay, code: "err.server.resource_share.replay_conflict", httpStatus: http.StatusConflict},
		{name: "stale resource", err: resourceshare.ErrProviderRevalidation, code: "err.server.resource_share.resource_unavailable", httpStatus: http.StatusConflict},
		{name: "invalid intent", err: resourceshare.ErrIntentSignature, code: "err.server.resource_share.intent_invalid", httpStatus: http.StatusBadRequest},
		{name: "provider", err: resourceshare.ErrProviderNotFound, code: "err.server.resource_share.intent_invalid", httpStatus: http.StatusBadRequest},
		{name: "store", err: errors.New("database password=secret"), code: "err.server.resource_share.unavailable", httpStatus: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &fakeShareService{err: tt.err}
			router, _ := resourceShareHarness(service, nil)
			response := performResourceShare(router, `{"intent":"a.b.c"}`, "space-a")
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assertEnvelope(t, response, tt.code, tt.httpStatus)
			assert.NotContains(t, response.Body.String(), "database")
			assert.NotContains(t, response.Body.String(), "secret")
		})
	}
}

func TestAPI_ProofJWKSIsPublicCacheableAndNeverContainsPrivateMaterial(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	verifier, err := resourceshare.NewProofVerifier([]resourceshare.ProofVerificationKey{{
		KeyID: "proof-key-1", PublicKey: &privateKey.PublicKey,
	}})
	require.NoError(t, err)
	router, _ := resourceShareHarness(&fakeShareService{}, verifier)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/resource-shares/proof-jwks", nil)
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, "public, max-age=300, must-revalidate", recorder.Header().Get("Cache-Control"))
	assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	assert.Contains(t, recorder.Body.String(), `"kid":"proof-key-1"`)
	assert.NotContains(t, recorder.Body.String(), `"d":`)

	var decoded struct {
		Keys []json.RawMessage `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &decoded))
	assert.Len(t, decoded.Keys, 1)
}

func TestAPI_ProofJWKSWithoutConfiguredKeysReturnsEmptySet(t *testing.T) {
	router, _ := resourceShareHarness(&fakeShareService{}, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/resource-shares/proof-jwks", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{"keys":[]}`, recorder.Body.String())
}

func TestFeatureFlagDefaultsOffAndRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("DM_RESOURCE_SHARE_ENABLED", "")
	enabled, err := featureEnabledFromEnv()
	require.NoError(t, err)
	assert.False(t, enabled)

	api := New(nil)
	_, err = api.service.Share(context.Background(), "user-a", "space-a", "a.b.c", "request-1")
	assert.ErrorIs(t, err, resourceshare.ErrShareDisabled)

	t.Setenv("DM_RESOURCE_SHARE_ENABLED", "definitely-not-a-bool")
	_, err = featureEnabledFromEnv()
	assert.Error(t, err)
	assert.Panics(t, func() { New(nil) })

	t.Setenv("DM_RESOURCE_SHARE_ENABLED", "true")
	enabled, err = featureEnabledFromEnv()
	require.NoError(t, err)
	assert.True(t, enabled)
	assert.Panics(t, func() { New(nil) }, "enabling without provider and signing dependencies must fail startup")
}

func TestAPI_RouteMountsPublicJWKSAndProtectedShare(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	api := newAPI(ctx, &fakeShareService{}, nil)
	router := wkhttp.New()
	router.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	api.Route(router)

	jwks := httptest.NewRecorder()
	router.ServeHTTP(jwks, httptest.NewRequest(http.MethodGet, "/v1/resource-shares/proof-jwks", nil))
	assert.Equal(t, http.StatusOK, jwks.Code, jwks.Body.String())

	share := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/resource-shares", strings.NewReader(`{"intent":"a.b.c"}`))
	request.Header.Set("X-Space-ID", "space-a")
	router.ServeHTTP(share, request)
	assert.NotEqual(t, http.StatusNotFound, share.Code)
}

func TestValidSpaceHeaderRejectsControlsAndOversize(t *testing.T) {
	assert.True(t, validSpaceHeader("space-a"))
	assert.False(t, validSpaceHeader("space\nforged"))
	assert.False(t, validSpaceHeader(strings.Repeat("s", 129)))
}

func TestResourceShareNoLegacyResponseError(t *testing.T) {
	for _, file := range []string{"api.go", "api_i18n.go"} {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		cleaned := stripLineComments(string(data))
		for _, banned := range []string{
			".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(",
			".AbortWithStatusJSON(", ".AbortWithStatus(", "c.Response(\"",
		} {
			assert.NotContains(t, cleaned, banned, "%s must use the localized error envelope", file)
		}
	}
}

func TestResourceShareRouteSourcePinsMiddlewareOrderAndPublicJWKS(t *testing.T) {
	data, err := os.ReadFile("api.go")
	require.NoError(t, err)
	source := string(data)
	auth := strings.Index(source, "AuthMiddleware")
	uidLimit := strings.Index(source, "SharedUIDRateLimiter")
	space := strings.Index(source, "SpaceMiddleware")
	post := strings.Index(source, `POST("", a.share)`)
	jwks := strings.Index(source, `GET("/v1/resource-shares/proof-jwks", a.proofJWKS)`)
	assert.True(t, auth >= 0 && uidLimit > auth && space > uidLimit && post > space,
		"share route must mount Auth -> shared UID limit -> Space -> handler")
	assert.True(t, jwks >= 0 && jwks < auth, "proof JWKS must be mounted outside authenticated group")
}

func assertEnvelope(t *testing.T, response *httptest.ResponseRecorder, code string, semanticStatus int) {
	t.Helper()
	var envelope resourceShareEnvelope
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope), response.Body.String())
	assert.Equal(t, code, envelope.Error.Code)
	assert.Equal(t, semanticStatus, envelope.Error.HTTPStatus)
}

func stripLineComments(source string) string {
	var cleaned bytes.Buffer
	for _, line := range strings.Split(source, "\n") {
		if index := strings.Index(line, "//"); index >= 0 {
			line = line[:index]
		}
		cleaned.WriteString(line)
		cleaned.WriteByte('\n')
	}
	return cleaned.String()
}
