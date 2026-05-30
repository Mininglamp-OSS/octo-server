package message

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// TestMessageNoLegacyResponseError pins the Phase 2.1 contract that the
// migrated modules/message handlers do not regress to legacy octo-lib error
// responses. Comments are stripped first so commented-out breadcrumbs do not
// trip the guard, and the m.Error(common.ErrData.Error(), ...) log calls are
// not responses (they match none of the banned tokens).
func TestMessageNoLegacyResponseError(t *testing.T) {
	files := []string{
		"api.go", "api_manager.go", "api_pinned.go", "api_conversation.go",
		"api_message_get.go", "api_reminders.go", "api_channel_files.go", "api_sidebar.go",
	}
	banned := []string{".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(", "c.Response(\""}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
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
					t.Fatalf("modules/message/%s must use httperr.ResponseErrorL via respondMessage* helpers / errcode.ErrMessage* instead of legacy %s", f, b)
				}
			}
			// Also forbid raw non-OK c.JSON(http.Status…) error responses — these
			// bypass the i18n envelope just as completely as c.ResponseError but
			// don't match the substrings above (reviewer finding on api_message_get.go).
			for _, line := range strings.Split(cleaned, "\n") {
				if strings.Contains(line, "c.JSON(http.Status") && !strings.Contains(line, "c.JSON(http.StatusOK") {
					t.Fatalf("modules/message/%s must not emit raw non-OK c.JSON(http.Status…) error responses; use httperr.ResponseErrorL: %s", f, strings.TrimSpace(line))
				}
			}
		})
	}
}

// wireI18nRendererForMessageTest injects the i18n ErrorRenderer onto the route
// returned by testutil.NewTestServer, mirroring what main.go does at boot.
// Without it the route falls back to the legacy {msg,status} envelope carrying
// the English source DefaultMessage instead of the localized zh-CN copy.
func wireI18nRendererForMessageTest(s *server.Server) {
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
}

type envelope struct {
	Error struct {
		Code       string         `json:"code"`
		Message    string         `json:"message"`
		Details    map[string]any `json:"details"`
		HTTPStatus int            `json:"http_status"`
	} `json:"error"`
	Msg    string `json:"msg"`
	Status int    `json:"status"`
}

func decodeEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, body)
	}
	return env
}

func helperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func httperrL(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorL(c, code, nil, nil)
}

func TestRespondMessageHelpers(t *testing.T) {
	cases := []struct {
		name            string
		probe           func(c *wkhttp.Context)
		wantCodeID      string
		wantSemStatus   int
		wantTransStatus int
		wantContains    string
		wantNotContains string
		wantDetails     map[string]any
	}{
		{
			name:            "respondMessageRequestInvalid carries the field detail",
			probe:           func(c *wkhttp.Context) { respondMessageRequestInvalid(c, "channel_id") },
			wantCodeID:      "err.server.message.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{"field": "channel_id"},
		},
		{
			name:            "respondMessageRequestInvalid drops empty field",
			probe:           func(c *wkhttp.Context) { respondMessageRequestInvalid(c, "") },
			wantCodeID:      "err.server.message.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{},
		},
		{
			name:            "respondMessageNotLoggedIn → shared.auth.required",
			probe:           func(c *wkhttp.Context) { respondMessageNotLoggedIn(c) },
			wantCodeID:      "err.shared.auth.required",
			wantSemStatus:   http.StatusUnauthorized,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请先登录",
		},
		{
			name:            "respondMessageTokenInvalid → shared.auth.token_invalid",
			probe:           func(c *wkhttp.Context) { respondMessageTokenInvalid(c) },
			wantCodeID:      "err.shared.auth.token_invalid",
			wantSemStatus:   http.StatusUnauthorized,
			wantTransStatus: http.StatusBadRequest,
		},
		{
			name:            "respondMessageForbidden → shared.auth.forbidden",
			probe:           func(c *wkhttp.Context) { respondMessageForbidden(c) },
			wantCodeID:      "err.shared.auth.forbidden",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "无权执行此操作",
		},
		{
			name:            "respondMessagePinnedLimitExceeded carries max",
			probe:           func(c *wkhttp.Context) { respondMessagePinnedLimitExceeded(c, 10) },
			wantCodeID:      "err.server.message.pinned_limit_exceeded",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "置顶消息数量已达上限",
			wantDetails:     map[string]any{"max": float64(10)},
		},
		{
			name:            "ErrMessageNotFriend → 403 zh-CN",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrMessageNotFriend) },
			wantCodeID:      "err.server.message.not_friend",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "好友",
		},
		{
			name:            "ErrMessageConversationForbidden → 403 zh-CN",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrMessageConversationForbidden) },
			wantCodeID:      "err.server.message.conversation_forbidden",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "会话",
		},
		{
			name:            "ErrMessageNotFound → 404 zh-CN",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrMessageNotFound) },
			wantCodeID:      "err.server.message.not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "消息不存在",
		},
		{
			name:            "ErrMessageRecallTimeExceeded → 400 zh-CN",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrMessageRecallTimeExceeded) },
			wantCodeID:      "err.server.message.recall_time_exceeded",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "撤回",
		},
		{
			name:            "ErrMessageQueryFailed (Internal) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrMessageQueryFailed) },
			wantCodeID:      "err.server.message.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "query message data",
		},
		{
			name:            "ErrMessageSearchFailed (Internal) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrMessageSearchFailed) },
			wantCodeID:      "err.server.message.search_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "search failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := helperHarness(tc.probe)
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			req.Header.Set("Accept-Language", "zh-CN")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantTransStatus {
				t.Fatalf("HTTP status = %d, want %d; body=%s", rec.Code, tc.wantTransStatus, rec.Body.String())
			}
			env := decodeEnvelope(t, rec.Body.Bytes())
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantSemStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantSemStatus)
			}
			if env.Status != tc.wantTransStatus {
				t.Fatalf("legacy status = %d, want %d (D14 transport=400 compat)", env.Status, tc.wantTransStatus)
			}
			if env.Msg != env.Error.Message {
				t.Fatalf("legacy msg %q != error.message %q", env.Msg, env.Error.Message)
			}
			if tc.wantContains != "" && !strings.Contains(env.Error.Message, tc.wantContains) {
				t.Fatalf("error.message = %q, want substring %q", env.Error.Message, tc.wantContains)
			}
			if tc.wantNotContains != "" && strings.Contains(env.Error.Message, tc.wantNotContains) {
				t.Fatalf("error.message = %q must not contain %q (Internal leak)", env.Error.Message, tc.wantNotContains)
			}
			if tc.wantDetails != nil {
				got := env.Error.Details
				if got == nil {
					got = map[string]any{}
				}
				if len(got) != len(tc.wantDetails) {
					t.Fatalf("error.details = %#v, want %#v", got, tc.wantDetails)
				}
				for k, v := range tc.wantDetails {
					if got[k] != v {
						t.Fatalf("error.details[%q] = %#v, want %#v", k, got[k], v)
					}
				}
			}
		})
	}
}
