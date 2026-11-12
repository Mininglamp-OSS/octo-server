package cliv2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codex.mlamp.cn/dmwork/octo-auth-sdk/contract"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/stretchr/testify/require"
)

const (
	testServiceSecret = "cliv2-test-service-secret-at-least-32-bytes"
	testBusinessToken = "business-token-from-cli"
)

type authHarness struct {
	source *wkhttp.WKHttp
	target *wkhttp.WKHttp
	server *httptest.Server
	calls  atomic.Int32
}

func newAuthHarness(t *testing.T, access contract.AccessContext, withoutLimiter ...bool) *authHarness {
	t.Helper()
	h := &authHarness{source: wkhttp.New()}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v2/internal/tokens/_resolve" {
			t.Errorf("unexpected auth request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testServiceSecret {
			t.Errorf("auth request Authorization = %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var input contract.ResolveInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Errorf("decode resolve request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if input.Token != testBusinessToken || input.Domains != nil {
			t.Errorf("resolve input = %#v", input)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(contract.Data[contract.AccessContext]{Data: access}); err != nil {
			t.Errorf("encode auth response: %v", err)
		}
	}))
	t.Cleanup(h.server.Close)

	var err error
	h.target, err = NewRouter(h.server.URL, testServiceSecret)
	require.NoError(t, err)
	if len(withoutLimiter) == 0 || !withoutLimiter[0] {
		require.NoError(t, SetUIDRateLimiter(h.target, func(c *wkhttp.Context) {
			if c.GetLoginUID() == "" {
				Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
				c.Abort()
				return
			}
			c.Next()
		}))
	}
	return h
}

func validAccess(actions []string, bots []contract.BotBinding) contract.AccessContext {
	return contract.AccessContext{
		Principal: contract.Principal{
			TokenID:     "token-1",
			SpaceID:     "space-1",
			OwnerID:     "user-1",
			OwnerName:   "User One",
			SubjectType: "user",
			SubjectID:   "user-1",
		},
		Permissions: []contract.PermissionGroup{{
			Domain:  "im.messages",
			Actions: actions,
		}},
		BotBindings: bots,
	}
}

func request(h http.Handler, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(`{"action":"im.messages:read"}`))
	req.Header.Set("Authorization", "Bearer "+testBusinessToken)
	h.ServeHTTP(recorder, req)
	return recorder
}

func TestMountedRouteAuthenticatesWithSDKAndSelectsBoundBot(t *testing.T) {
	h := newAuthHarness(t, validAccess(
		[]string{"im.messages:write"},
		[]contract.BotBinding{{BotID: "bot-1", BotName: "Bot One"}},
	))
	var middlewareCalled bool
	userHandler := func(c *wkhttp.Context) {
		Respond(c, map[string]string{"handler": "user"})
	}
	botHandler := func(c *wkhttp.Context) {
		principal := PrincipalFrom(c)
		Respond(c, map[string]string{
			"handler":            "bot",
			"login_uid":          c.GetLoginUID(),
			"user_id":            principal.UserID,
			"user_name":          principal.UserName,
			"space_id":           space.GetSpaceID(c),
			"principal_bot_name": principal.BotName,
			"principal_bot":      principal.BotID,
		})
	}
	Add(h.source, Route{
		Method: http.MethodPost,
		Path:   "/v2/cli/messages",
		Action: "im.messages:write",
		User:   userHandler,
		Middlewares: []wkhttp.HandlerFunc{func(c *wkhttp.Context) {
			middlewareCalled = PrincipalFrom(c) == (Principal{UserID: "user-1", UserName: "User One", BotName: "Bot One", SpaceID: "space-1", BotID: "bot-1"})
			c.Next()
		}},
	})
	Add(h.source, Route{
		Method: http.MethodPost,
		Path:   "/v2/cli/messages",
		Action: "im.messages:write",
		Bot:    botHandler,
	})
	require.NoError(t, Mount(h.source, h.target))

	resp := request(h.target, http.MethodPost, "/v2/cli/messages")
	require.Equal(t, http.StatusOK, resp.Code)
	require.True(t, middlewareCalled, "route middleware must run after trusted principal setup")
	require.EqualValues(t, 1, h.calls.Load())
	var got struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))
	require.Equal(t, map[string]string{
		"handler":            "bot",
		"login_uid":          "bot-1",
		"user_id":            "user-1",
		"user_name":          "User One",
		"space_id":           "space-1",
		"principal_bot":      "bot-1",
		"principal_bot_name": "Bot One",
	}, got.Data)

	// Contributions are mounted only on the independent CLI router, not the source router.
	unmounted := request(h.source, http.MethodPost, "/v2/cli/messages")
	require.Equal(t, http.StatusNotFound, unmounted.Code)
}

func TestUserOnlyRouteKeepsUserActorWhenBotIsBound(t *testing.T) {
	h := newAuthHarness(t, validAccess(
		[]string{"im.messages:read"},
		[]contract.BotBinding{{BotID: "bot-1", BotName: "Bot One"}},
	))
	Add(h.source, Route{
		Method:   http.MethodPost,
		Path:     "/v2/cli/messages/_search",
		Action:   "im.messages:read",
		UserOnly: true,
		User: func(c *wkhttp.Context) {
			p := PrincipalFrom(c)
			Respond(c, map[string]string{
				"actor":              c.GetLoginUID(),
				"principal_user":     p.UserID,
				"principal_bot":      p.BotID,
				"principal_bot_name": p.BotName,
				"space_id":           space.GetSpaceID(c),
			})
		},
	})
	require.NoError(t, Mount(h.source, h.target))

	resp := request(h.target, http.MethodPost, "/v2/cli/messages/_search")
	require.Equal(t, http.StatusOK, resp.Code)
	var got struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))
	require.Equal(t, map[string]string{
		"actor":              "user-1",
		"principal_user":     "user-1",
		"principal_bot":      "bot-1",
		"principal_bot_name": "Bot One",
		"space_id":           "space-1",
	}, got.Data)
}

func TestDomainWildcardPermissionUsesSDKMatcher(t *testing.T) {
	h := newAuthHarness(t, validAccess(
		[]string{"im.messages:*"},
		[]contract.BotBinding{},
	))
	var handlerCalled bool
	Add(h.source, Route{
		Method:   http.MethodPost,
		Path:     "/v2/cli/messages",
		Action:   "im.messages:write",
		UserOnly: true,
		User: func(c *wkhttp.Context) {
			handlerCalled = true
			Respond(c, map[string]bool{"called": true})
		},
	})
	require.NoError(t, Mount(h.source, h.target))

	resp := request(h.target, http.MethodPost, "/v2/cli/messages")
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	require.True(t, handlerCalled)
}

func TestActionMismatchFailsBeforeRouteMiddlewareOrHandler(t *testing.T) {
	h := newAuthHarness(t, validAccess(
		[]string{"im.messages:read"},
		[]contract.BotBinding{{BotID: "bot-1", BotName: "Bot One"}},
	), true)
	var middlewareCalled, handlerCalled bool
	var limiterUID, limiterSpaceID string
	var limiterPrincipal Principal
	var limiterCalls int
	require.NoError(t, SetUIDRateLimiter(h.target, func(c *wkhttp.Context) {
		limiterCalls++
		limiterUID = c.GetLoginUID()
		limiterSpaceID = space.GetSpaceID(c)
		limiterPrincipal = PrincipalFrom(c)
		c.Next()
	}))

	Add(h.source, Route{
		Method:   http.MethodPost,
		Path:     "/v2/cli/messages/edit",
		Action:   "im.messages:write",
		UserOnly: true,
		User: func(c *wkhttp.Context) {
			handlerCalled = true
			Respond(c, map[string]bool{"called": true})
		},
		Middlewares: []wkhttp.HandlerFunc{func(c *wkhttp.Context) {
			middlewareCalled = true
			c.Next()
		}},
	})
	require.NoError(t, Mount(h.source, h.target))

	resp := request(h.target, http.MethodPost, "/v2/cli/messages/edit")
	require.Equal(t, http.StatusForbidden, resp.Code)
	require.Equal(t, 1, limiterCalls)
	require.Equal(t, "user-1", limiterUID, "a valid token without the required action must still consume the User actor's UID bucket")
	require.Equal(t, "space-1", limiterSpaceID, "the authenticated Space must be available to the limiter")
	require.Equal(t, Principal{UserID: "user-1", UserName: "User One", BotName: "Bot One", SpaceID: "space-1", BotID: "bot-1"}, limiterPrincipal)
	require.False(t, middlewareCalled)
	require.False(t, handlerCalled, "body-supplied action must not override the registered action")
	var got struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))
	require.Equal(t, "FORBIDDEN", got.Error.Code)
	require.NotEmpty(t, got.Error.Message)
	require.NotNil(t, got.Error.Details)
}

func TestMissingBearerDoesNotCallAuthServer(t *testing.T) {
	h := newAuthHarness(t, validAccess([]string{"im.messages:write"}, []contract.BotBinding{}))
	Add(h.source, Route{
		Method: http.MethodPost,
		Path:   "/v2/cli/messages",
		Action: "im.messages:write",
		User:   func(c *wkhttp.Context) { Respond(c, map[string]bool{"called": true}) },
		Bot:    func(c *wkhttp.Context) { Respond(c, map[string]bool{"called": true}) },
	})
	require.NoError(t, Mount(h.source, h.target))

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/cli/messages", nil)
	h.target.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.EqualValues(t, 0, h.calls.Load())
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
	require.Equal(t, "AUTH_REQUIRED", got.Error.Code)
}

func TestHTTPErrorsUseRegisteredSemanticStatusAndSafeDetails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        codes.Code
		withStatus  bool
		wantStatus  int
		wantCode    string
		wantMessage string
		wantDetails map[string]any
	}{
		{
			name:        "registered forbidden uses semantic 403 instead of legacy transport 400",
			code:        registeredCode(t, "err.shared.auth.forbidden"),
			wantStatus:  http.StatusForbidden,
			wantCode:    "FORBIDDEN",
			wantMessage: "You do not have permission to perform this action.",
			wantDetails: map[string]any{},
		},
		{
			name:        "registered not found uses semantic 404",
			code:        registeredCode(t, "err.shared.not_found"),
			wantStatus:  http.StatusNotFound,
			wantCode:    "NOT_FOUND",
			wantMessage: "Resource not found.",
			wantDetails: map[string]any{},
		},
		{
			name:        "registered validation preserves safe field and drops unsafe details",
			code:        registeredCode(t, "err.shared.param.invalid"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    "VALIDATION_ERROR",
			wantMessage: "Invalid request parameter.",
			wantDetails: map[string]any{"field": "body"},
		},
		{
			name:        "internal error is generic and has no details",
			code:        registeredCode(t, "err.shared.internal"),
			withStatus:  true,
			wantStatus:  http.StatusInternalServerError,
			wantCode:    "INTERNAL_ERROR",
			wantMessage: "Internal server error.",
			wantDetails: map[string]any{},
		},
		{
			name:        "registered readiness 503 remains upstream unavailable",
			code:        errcode.ErrProjectNotReady,
			withStatus:  true,
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    "UPSTREAM_UNAVAILABLE",
			wantMessage: "The project workspace is not ready yet.",
			wantDetails: map[string]any{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthHarness(t, validAccess([]string{"im.messages:write"}, []contract.BotBinding{}))
			Add(h.source, Route{
				Method:   http.MethodPost,
				Path:     "/v2/cli/messages",
				Action:   "im.messages:write",
				UserOnly: true,
				User: func(c *wkhttp.Context) {
					details := i18n.Details{"field": "body", "raw_err": "private"}
					if tc.withStatus {
						httperr.ResponseErrorLWithStatus(c, tc.code, nil, details)
						return
					}
					httperr.ResponseErrorL(c, tc.code, nil, details)
				},
			})
			require.NoError(t, Mount(h.source, h.target))

			resp := request(h.target, http.MethodPost, "/v2/cli/messages")
			require.Equal(t, tc.wantStatus, resp.Code)
			var got struct {
				Error struct {
					Code    string         `json:"code"`
					Message string         `json:"message"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))
			require.Equal(t, tc.wantCode, got.Error.Code)
			require.Equal(t, tc.wantMessage, got.Error.Message)
			require.Equal(t, tc.wantDetails, got.Error.Details)
		})
	}
}

func registeredCode(t *testing.T, id string) codes.Code {
	t.Helper()
	code, ok := codes.Lookup(id)
	require.True(t, ok, "error code %q must be registered", id)
	return code
}

func TestUIDLimiterAppliesSharedUserBucket(t *testing.T) {
	t.Run("one user bucket spans routes and runs after authentication", func(t *testing.T) {
		h := newAuthHarness(t, validAccess(
			[]string{"im.messages:write", "im.messages:read"},
			[]contract.BotBinding{},
		), true)
		hits := make(map[string]int)
		require.NoError(t, SetUIDRateLimiter(h.target, func(c *wkhttp.Context) {
			uid := c.GetLoginUID()
			if uid == "" {
				Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
				c.Abort()
				return
			}
			hits[uid]++
			if hits[uid] > 1 {
				Fail(c, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests, please try again later.")
				c.Abort()
				return
			}
			c.Next()
		}))
		var handlerCalls int
		Add(h.source, Route{
			Method: http.MethodPost,
			Path:   "/v2/cli/messages",
			Action: "im.messages:write",
			User: func(c *wkhttp.Context) {
				handlerCalls++
				Respond(c, map[string]bool{"called": true})
			},
			Bot: func(c *wkhttp.Context) {
				handlerCalls++
				Respond(c, map[string]bool{"called": true})
			},
		})
		Add(h.source, Route{
			Method:   http.MethodPost,
			Path:     "/v2/cli/messages/_search",
			Action:   "im.messages:read",
			UserOnly: true,
			User: func(c *wkhttp.Context) {
				handlerCalls++
				Respond(c, map[string]bool{"called": true})
			},
		})
		require.NoError(t, Mount(h.source, h.target))

		first := request(h.target, http.MethodPost, "/v2/cli/messages")
		second := request(h.target, http.MethodPost, "/v2/cli/messages/_search")
		require.Equal(t, http.StatusOK, first.Code)
		require.Equal(t, http.StatusTooManyRequests, second.Code)
		var got struct {
			Error struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
			LegacyMessage string `json:"msg"`
			LegacyStatus  int    `json:"status"`
		}
		require.NoError(t, json.Unmarshal(second.Body.Bytes(), &got))
		require.Equal(t, "RATE_LIMITED", got.Error.Code)
		require.NotNil(t, got.Error.Details)
		require.Empty(t, got.LegacyMessage, "v2 errors must not use the v1 compatibility envelope")
		require.Zero(t, got.LegacyStatus)
		require.Equal(t, 2, hits["user-1"])
		require.Equal(t, 1, handlerCalls)
		require.EqualValues(t, 2, h.calls.Load())
	})
}

func TestMultipleBotBindingsFailClosed(t *testing.T) {
	h := newAuthHarness(t, validAccess(
		[]string{"im.messages:write"},
		[]contract.BotBinding{{BotID: "bot-1", BotName: "Bot One"}, {BotID: "bot-2", BotName: "Bot Two"}},
	))
	var handlerCalled bool
	Add(h.source, Route{
		Method: http.MethodPost,
		Path:   "/v2/cli/messages",
		Action: "im.messages:write",
		User:   func(c *wkhttp.Context) { handlerCalled = true },
		Bot:    func(c *wkhttp.Context) { handlerCalled = true },
	})
	require.NoError(t, Mount(h.source, h.target))

	resp := request(h.target, http.MethodPost, "/v2/cli/messages")
	require.Equal(t, http.StatusServiceUnavailable, resp.Code)
	require.False(t, handlerCalled, "ambiguous Bot binding must not fall back to User")
}
func TestUnmatchedCLIV2PathAndMethodReturnNotFoundEnvelope(t *testing.T) {
	h := newAuthHarness(t, validAccess([]string{"im.messages:write"}, []contract.BotBinding{}))
	Add(h.source, Route{
		Method: http.MethodPost,
		Path:   "/v2/cli/messages",
		Action: "im.messages:write",
		User:   func(c *wkhttp.Context) { Respond(c, map[string]bool{"called": true}) },
		Bot:    func(c *wkhttp.Context) { Respond(c, map[string]bool{"called": true}) },
	})
	require.NoError(t, Mount(h.source, h.target))

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "unknown path", method: http.MethodGet, path: "/v2/cli/not-a-route"},
		{name: "method not registered for path", method: http.MethodGet, path: "/v2/cli/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := request(h.target, tc.method, tc.path)
			require.Equal(t, http.StatusNotFound, resp.Code, resp.Body.String())
			require.Contains(t, resp.Header().Get("Content-Type"), "application/json")
			var got struct {
				Error struct {
					Code    string         `json:"code"`
					Message string         `json:"message"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))
			require.Equal(t, "NOT_FOUND", got.Error.Code)
			require.Equal(t, "Resource not found.", got.Error.Message)
			require.Empty(t, got.Error.Details)
		})
	}
}
