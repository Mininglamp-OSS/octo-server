package group

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

// httperrL is a terse test shim for the no-params/no-details ResponseErrorL
// call shape exercised by the direct-code cases below.
func httperrL(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorL(c, code, nil, nil)
}

// TestGroupNoLegacyResponseError pins the Phase 2.1 contract that the migrated
// modules/group handlers do not regress to legacy octo-lib error responses.
// Comments are stripped first so commented-out breadcrumbs do not trip the
// guard. The c.ResponseError(common.ErrData.Error(), ...) zap LOG calls are not
// responses and are intentionally allowed (they match neither banned token).
func TestGroupNoLegacyResponseError(t *testing.T) {
	files := []string{"api.go", "api_manager.go", "invite.go"}
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
					t.Fatalf("modules/group/%s must use httperr.ResponseErrorL via respondGroup* helpers / errcode.ErrGroup* instead of legacy %s", f, b)
				}
			}
		})
	}
}

// wireI18nRendererForGroupTest injects the i18n ErrorRenderer onto the route
// returned by testutil.NewTestServer, mirroring what main.go does at boot.
// Post-Phase-2.1, modules/group handlers respond via httperr.ResponseErrorL →
// c.RenderError; without a renderer wired the route falls back to the legacy
// {msg,status} envelope carrying the English source DefaultMessage instead of
// the localized zh-CN copy production clients receive. testutil.NewTestServer
// lives in octo-lib and is intentionally not touched from this PR.
func wireI18nRendererForGroupTest(s *server.Server) {
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
}

// envelope is the partial shape of an httperr.ResponseErrorL response. The
// renderer emits both the legacy {msg,status} and the v2 {error.{...}} blocks
// unconditionally (v7.2 dual-envelope contract).
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

// helperHarness mounts a single GET /probe route that invokes the supplied
// helper with the i18n renderer wired, so tests can assert the rendered
// envelope without paying the DB / auth setup cost.
func helperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func TestRespondGroupHelpers(t *testing.T) {
	cases := []struct {
		name            string
		probe           func(c *wkhttp.Context)
		wantCodeID      string
		wantSemStatus   int
		wantTransStatus int    // always 400 for legacy compat (D14)
		wantContains    string // zh-CN substring expected in error.message
		wantNotContains string // forbid leaked English DefaultMessage when Internal=true
		wantDetails     map[string]any
	}{
		{
			name:            "respondGroupRequestInvalid carries the field detail",
			probe:           func(c *wkhttp.Context) { respondGroupRequestInvalid(c, "group_no") },
			wantCodeID:      "err.server.group.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{"field": "group_no"},
		},
		{
			name:            "respondGroupRequestInvalid drops empty field key",
			probe:           func(c *wkhttp.Context) { respondGroupRequestInvalid(c, "") },
			wantCodeID:      "err.server.group.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请求参数有误",
			wantDetails:     map[string]any{},
		},
		{
			name:            "respondGroupForbidden routes to shared.auth.forbidden",
			probe:           func(c *wkhttp.Context) { respondGroupForbidden(c) },
			wantCodeID:      "err.shared.auth.forbidden",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "无权执行此操作",
		},
		{
			name:            "respondGroupNotLoggedIn routes to shared.auth.required",
			probe:           func(c *wkhttp.Context) { respondGroupNotLoggedIn(c) },
			wantCodeID:      "err.shared.auth.required",
			wantSemStatus:   http.StatusUnauthorized,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请先登录",
		},
		{
			name:            "respondGroupMdContentTooLarge surfaces the size cap",
			probe:           func(c *wkhttp.Context) { respondGroupMdContentTooLarge(c, 4096) },
			wantCodeID:      "err.server.group.group_md_content_too_large",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "GROUP.md",
			wantDetails:     map[string]any{"field": "content", "max_size": float64(4096)},
		},
		{
			name:            "respondGroupInfoError maps not-found sentinel to 404",
			probe:           func(c *wkhttp.Context) { respondGroupInfoError(c, errGroupInfoNotFound) },
			wantCodeID:      "err.server.group.not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "群不存在",
		},
		{
			name:            "respondGroupInfoError maps query sentinel to 500 internal",
			probe:           func(c *wkhttp.Context) { respondGroupInfoError(c, errGroupInfoQueryFailed) },
			wantCodeID:      "err.server.group.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "query group data",
		},
		{
			name:            "ErrGroupCreatorOnly surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrGroupCreatorOnly) },
			wantCodeID:      "err.server.group.creator_only",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "只有群主",
		},
		{
			name:            "ErrGroupBotOwnershipDenied surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrGroupBotOwnershipDenied) },
			wantCodeID:      "err.server.group.bot_ownership_denied",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "邀请该机器人",
		},
		{
			name:            "ErrGroupExternalCannotBeAdmin surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrGroupExternalCannotBeAdmin) },
			wantCodeID:      "err.server.group.external_cannot_be_admin",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "外部成员设为管理员",
		},
		{
			name:            "ErrGroupMemberNotInGroup surfaces 404 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrGroupMemberNotInGroup) },
			wantCodeID:      "err.server.group.member_not_in_group",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "该成员不在群内",
		},
		{
			name:            "ErrGroupAlreadyMember surfaces 409 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrGroupAlreadyMember) },
			wantCodeID:      "err.server.group.already_member",
			wantSemStatus:   http.StatusConflict,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "已在群内",
		},
		{
			name:            "ErrGroupInviteModeCannotJoin surfaces 403 zh-CN copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrGroupInviteModeCannotJoin) },
			wantCodeID:      "err.server.group.invite_mode_cannot_join",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "邀请模式",
		},
		{
			name:            "ErrGroupStoreFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrGroupStoreFailed) },
			wantCodeID:      "err.server.group.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "update group data",
		},
		{
			name:            "ErrGroupNotifyFailed (Internal=true) collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrGroupNotifyFailed) },
			wantCodeID:      "err.server.group.notify_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "notification",
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
				t.Fatalf("legacy msg %q != error.message %q (dual envelope must agree)", env.Msg, env.Error.Message)
			}
			if !strings.Contains(env.Error.Message, tc.wantContains) {
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
