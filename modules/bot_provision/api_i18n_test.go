package bot_provision_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBotProvisionNoLegacyResponseError is the source guard (CLAUDE.md): the
// handler file must route every user-facing error through
// httperr.ResponseErrorL / ResponseErrorLWithStatus with a registered
// errcode.ErrBotProvision* / shared code — never a legacy raw response. The
// lint-direct-error-response gate only counts c.AbortWithStatus*, so the
// c.ResponseError* family is enforced here instead.
func TestBotProvisionNoLegacyResponseError(t *testing.T) {
	files := []string{"bot_api.go"}
	banned := []string{".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(", "c.Response(\"", ".AbortWithStatusJSON(", ".AbortWithStatus("}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			data, err := os.ReadFile(f)
			require.NoErrorf(t, err, "read %s", f)
			// Strip line comments so the doc comments referencing the old
			// pattern names don't trip the guard.
			var clean strings.Builder
			for _, line := range strings.Split(string(data), "\n") {
				if idx := strings.Index(line, "//"); idx >= 0 {
					line = line[:idx]
				}
				clean.WriteString(line)
				clean.WriteByte('\n')
			}
			cleaned := clean.String()
			for _, b := range banned {
				if strings.Contains(cleaned, b) {
					t.Fatalf("modules/bot_provision/%s must use httperr.ResponseErrorL / ResponseErrorLWithStatus + errcode.ErrBotProvision* / shared codes instead of legacy %s", f, b)
				}
			}
		})
	}
}

// TestBotProvisionZhParity guards against the silent en-US fallback: make
// i18n-extract only maintains the en markers, while active.zh-CN.toml is hand
// maintained, so a missing zh entry would render English with no gate catching
// it. Assert every bot_provision code (the 5 new ones + the two shared codes it
// reuses) renders a Chinese string distinct from its English DefaultMessage.
func TestBotProvisionZhParity(t *testing.T) {
	loc := i18n.NewLocalizer(i18n.DefaultLanguage)
	cases := []struct {
		id     string
		wantZh string
		enSrc  string
	}{
		{"err.server.bot_provision.request_invalid", "请求参数有误。", "Invalid request."},
		{"err.server.bot_provision.auth_failed", "认证失败。", "Authentication failed."},
		{"err.server.bot_provision.space_forbidden", "你不是该空间的成员，无法在此创建 Bot。", "You are not a member of this space."},
		{"err.server.bot_provision.bot_forbidden", "无权访问该 Bot。", "Not authorized for this bot."},
		{"err.server.bot_provision.bot_not_found", "Bot 不存在。", "Bot not found."},
		// shared codes reused by bot_provision
		{"err.shared.auth.required", "请先登录！", ""},
		{"err.shared.internal", "服务器内部错误。", ""},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got := loc.Translate(tc.id, "zh-CN", nil)
			assert.Equalf(t, tc.wantZh, got, "zh-CN render of %s", tc.id)
			if tc.enSrc != "" {
				assert.NotEqualf(t, tc.enSrc, got, "%s fell back to English — missing zh entry in active.zh-CN.toml", tc.id)
			}
		})
	}
}

// errEnvelope is the partial shape of an httperr.ResponseErrorL response
// (dual-envelope: legacy {msg,status} + v2 {error.{...}}).
type errEnvelope struct {
	Error struct {
		Code       string         `json:"code"`
		Message    string         `json:"message"`
		Details    map[string]any `json:"details"`
		HTTPStatus int            `json:"http_status"`
	} `json:"error"`
	Msg    string `json:"msg"`
	Status int    `json:"status"`
}

// probeHarness mounts a single GET /probe route with the i18n renderer wired
// (mirroring main.go), so a probe can exercise an errcode + facade and we can
// assert the rendered envelope without DB / auth setup.
func probeHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func doProbe(t *testing.T, r *wkhttp.WKHttp) (int, errEnvelope) {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/probe", nil)
	require.NoError(t, err)
	r.ServeHTTP(w, req)
	var env errEnvelope
	require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &env), "decode envelope: %s", w.Body.String())
	return w.Code, env
}

// TestBotProvisionErrorEnvelopes validates each code's render contract: the
// transport (wire) status the facade choice produces, the body code ID and
// canonical http_status, the request_invalid field detail, and that the
// Internal shared code hides its message/details. The transport-vs-canonical
// split is the load-bearing invariant — the daemon branches on the wire status,
// so ResponseErrorL must pin 400 (D14) while ResponseErrorLWithStatus must emit
// the code's real status.
func TestBotProvisionErrorEnvelopes(t *testing.T) {
	t.Run("request_invalid carries the field detail (ResponseErrorL → wire 400)", func(t *testing.T) {
		status, env := doProbe(t, probeHarness(func(c *wkhttp.Context) {
			httperr.ResponseErrorL(c, errcode.ErrBotProvisionRequestInvalid, nil, i18n.Details{"field": "display_name"})
		}))
		assert.Equal(t, http.StatusBadRequest, status, "transport status (D14 legacy 400)")
		assert.Equal(t, "err.server.bot_provision.request_invalid", env.Error.Code)
		assert.Equal(t, http.StatusBadRequest, env.Error.HTTPStatus)
		assert.Equal(t, "display_name", env.Error.Details["field"])
		assert.Contains(t, env.Error.Message, "请求参数有误")
	})

	t.Run("request_invalid drops empty/nil details", func(t *testing.T) {
		_, env := doProbe(t, probeHarness(func(c *wkhttp.Context) {
			httperr.ResponseErrorL(c, errcode.ErrBotProvisionRequestInvalid, nil, nil)
		}))
		assert.Equal(t, "err.server.bot_provision.request_invalid", env.Error.Code)
		_, hasField := env.Error.Details["field"]
		assert.False(t, hasField, "nil details must not surface an empty field key")
	})

	t.Run("auth_failed is a 401 on the wire (ResponseErrorLWithStatus)", func(t *testing.T) {
		status, env := doProbe(t, probeHarness(func(c *wkhttp.Context) {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotProvisionAuthFailed, nil, nil)
		}))
		assert.Equal(t, http.StatusUnauthorized, status, "transport status preserved for daemon")
		assert.Equal(t, "err.server.bot_provision.auth_failed", env.Error.Code)
		assert.Equal(t, http.StatusUnauthorized, env.Error.HTTPStatus)
	})

	t.Run("bot_not_found is a 404 on the wire", func(t *testing.T) {
		status, env := doProbe(t, probeHarness(func(c *wkhttp.Context) {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotProvisionBotNotFound, nil, nil)
		}))
		assert.Equal(t, http.StatusNotFound, status)
		assert.Equal(t, "err.server.bot_provision.bot_not_found", env.Error.Code)
		assert.Equal(t, http.StatusNotFound, env.Error.HTTPStatus)
	})

	t.Run("bot_forbidden is a 403 on the wire", func(t *testing.T) {
		status, env := doProbe(t, probeHarness(func(c *wkhttp.Context) {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotProvisionBotForbidden, nil, nil)
		}))
		assert.Equal(t, http.StatusForbidden, status)
		assert.Equal(t, "err.server.bot_provision.bot_forbidden", env.Error.Code)
		assert.Equal(t, http.StatusForbidden, env.Error.HTTPStatus)
	})

	t.Run("space_forbidden is a 403 on the wire", func(t *testing.T) {
		status, env := doProbe(t, probeHarness(func(c *wkhttp.Context) {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrBotProvisionSpaceForbidden, nil, nil)
		}))
		assert.Equal(t, http.StatusForbidden, status)
		assert.Equal(t, "err.server.bot_provision.space_forbidden", env.Error.Code)
		assert.Equal(t, http.StatusForbidden, env.Error.HTTPStatus)
	})

	t.Run("mint missing-uid guard reuses shared auth_required (401 on the wire)", func(t *testing.T) {
		status, env := doProbe(t, probeHarness(func(c *wkhttp.Context) {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedAuthRequired, nil, nil)
		}))
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.Equal(t, "err.shared.auth.required", env.Error.Code)
		assert.Equal(t, http.StatusUnauthorized, env.Error.HTTPStatus)
	})

	t.Run("internal: ResponseErrorL pins wire 400 while http_status stays 500, message+details hidden", func(t *testing.T) {
		status, env := doProbe(t, probeHarness(func(c *wkhttp.Context) {
			httperr.ResponseErrorL(c, errcode.ErrSharedInternal, nil, i18n.Details{"field": "leak"})
		}))
		assert.Equal(t, http.StatusBadRequest, status, "internal site kept its legacy wire 400 (was c.ResponseError)")
		assert.Equal(t, "err.shared.internal", env.Error.Code)
		assert.Equal(t, http.StatusInternalServerError, env.Error.HTTPStatus)
		assert.NotContains(t, env.Error.Message, "leak")
		_, hasField := env.Error.Details["field"]
		assert.False(t, hasField, "Internal code must not surface details")
	})
}

// TestBotToken_NoBearer_AuthFailedEndToEnd proves the real botToken handler
// (not just an isolated probe) routes a missing-Bearer credential through
// ResponseErrorLWithStatus(ErrBotProvisionAuthFailed): wire 401 + the
// anti-enumeration code. This locks the facade choice at the actual call site —
// a future flip to ResponseErrorL would drop the wire status to 400 and fail
// here. botToken validates the api_key inline (no AuthMiddleware), and the
// missing-Bearer branch returns before any DB access, so no seed is needed.
func TestBotToken_NoBearer_AuthFailedEndToEnd(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	w := doBotToken(t, s, "bot_any", "" /* no bearer */)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	var env errEnvelope
	require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &env), "decode envelope: %s", w.Body.String())
	assert.Equal(t, "err.server.bot_provision.auth_failed", env.Error.Code)
}
