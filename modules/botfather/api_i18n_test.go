package botfather

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// httperrL / httperrLWS are terse shims for the two ResponseErrorL call shapes
// (D14 fixed-400 vs status-preserving) exercised by the direct-code cases.
func httperrL(c *wkhttp.Context, code codes.Code) { httperr.ResponseErrorL(c, code, nil, nil) }
func httperrLWS(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorLWithStatus(c, code, nil, nil)
}

// TestBotfatherNoLegacyResponseError pins the Phase 2 contract that every
// migrated modules/botfather handler renders through the i18n envelope and never
// regresses to a legacy octo-lib error response or a raw non-OK gin write.
// Comments are stripped first so commented-out breadcrumbs do not trip the
// guard. bf.Error(...)/bf.Warn(...) zap LOG calls are not responses and are
// intentionally allowed. Success writes (c.JSON(http.StatusOK,...), c.Response,
// c.Redirect, c.String) are allowed; only NON-OK c.JSON(...) is banned.
//
// Known exemption (tracked as a follow-up, outside D23): getEvents still emits
// an HTTP-200 in-band error via c.Response(gin.H{"status":0,"msg":err.Error()}),
// which is not an error *response* (wire 200) and so is not bannable here.
func TestBotfatherNoLegacyResponseError(t *testing.T) {
	files := []string{
		"api.go", "api_bot.go", "api_apply.go", "api_user.go", "api_bot_thread.go",
	}
	banned := []string{
		".ResponseError(",
		".ResponseErrorf(",
		".ResponseErrorWithStatus(",
		".AbortWithStatusJSON(",
		".AbortWithStatus(",
		// raw non-OK c.JSON — every error status must go through the envelope
		"c.JSON(http.StatusBadRequest",
		"c.JSON(http.StatusUnauthorized",
		"c.JSON(http.StatusForbidden",
		"c.JSON(http.StatusNotFound",
		"c.JSON(http.StatusConflict",
		"c.JSON(http.StatusInternalServerError",
		"c.JSON(http.StatusBadGateway",
	}
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
					t.Fatalf("modules/botfather/%s must render via httperr.ResponseErrorL / respondBotfather* helpers / errcode.ErrBotfather* instead of legacy %s", f, b)
				}
			}
		})
	}
}

// errEnvelopeI18n is the partial shape of an httperr.ResponseErrorL response.
// The renderer emits both the legacy {msg,status} and the v2 {error.{...}} blocks
// unconditionally (dual-envelope contract).
type errEnvelopeI18n struct {
	Error struct {
		Code       string         `json:"code"`
		Message    string         `json:"message"`
		Details    map[string]any `json:"details"`
		HTTPStatus int            `json:"http_status"`
	} `json:"error"`
	Msg    string `json:"msg"`
	Status int    `json:"status"`
}

// i18nHelperHarness mounts a single GET /probe route with the i18n renderer
// wired, so tests can assert the rendered envelope without DB / auth setup.
func i18nHelperHarness(probe func(c *wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

func TestRespondBotfatherHelpers(t *testing.T) {
	// bf carries only a logger — enough for the helpers that log (identity
	// guard) without standing up DB / redis / IM.
	bf := &BotFather{Log: log.NewTLog("BotFather-i18n-test")}

	cases := []struct {
		name            string
		probe           func(c *wkhttp.Context)
		wantCodeID      string
		wantSemStatus   int
		wantTransStatus int // 400 for D14 ResponseErrorL; real status for WithStatus
		wantContains    string
		wantNotContains string // forbid leaked English DefaultMessage when Internal=true
		wantDetails     map[string]any
	}{
		// ---- detail-carrying validation helpers (400, D14) -------------------
		{
			name:            "respondBotfatherRequestInvalid carries the field detail",
			probe:           func(c *wkhttp.Context) { respondBotfatherRequestInvalid(c, "channel_id") },
			wantCodeID:      "err.server.botfather.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "数据格式有误",
			wantDetails:     map[string]any{"field": "channel_id"},
		},
		{
			name:            "respondBotfatherRequestInvalid drops empty field key",
			probe:           func(c *wkhttp.Context) { respondBotfatherRequestInvalid(c, "") },
			wantCodeID:      "err.server.botfather.request_invalid",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "数据格式有误",
			wantDetails:     map[string]any{},
		},
		{
			name:            "respondBotfatherContentTooLarge surfaces field + max_bytes",
			probe:           func(c *wkhttp.Context) { respondBotfatherContentTooLarge(c, "content", 4096) },
			wantCodeID:      "err.server.botfather.content_too_large",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "内容大小超过限制",
			wantDetails:     map[string]any{"field": "content", "max_bytes": float64(4096)},
		},
		{
			name:            "respondBotfatherFileTooLarge surfaces the MB cap",
			probe:           func(c *wkhttp.Context) { respondBotfatherFileTooLarge(c, 100) },
			wantCodeID:      "err.server.botfather.file_too_large",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "文件大小超过限制",
			wantDetails:     map[string]any{"max_mb": float64(100)},
		},
		// ---- direct business codes: 400 / 403 / 404 / 409 (D14, wire 400) ----
		{
			name:            "ErrBotfatherBotNotGroupMember 403 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherBotNotGroupMember) },
			wantCodeID:      "err.server.botfather.bot_not_group_member",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "机器人不是该群成员",
		},
		{
			name:            "ErrBotfatherNotOwner 403 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherNotOwner) },
			wantCodeID:      "err.server.botfather.not_owner",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "你不是该 AI 的 Owner",
		},
		{
			name:            "ErrBotfatherMessageEditForbidden 403 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherMessageEditForbidden) },
			wantCodeID:      "err.server.botfather.message_edit_forbidden",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "只能编辑自己发送的消息",
		},
		{
			name:            "ErrBotfatherMemberNotHuman 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherMemberNotHuman) },
			wantCodeID:      "err.server.botfather.member_not_human",
			wantSemStatus:   http.StatusBadRequest,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "只能通过机器人 API 添加真人成员",
		},
		{
			name:            "ErrBotfatherRobotNotFound 404 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherRobotNotFound) },
			wantCodeID:      "err.server.botfather.robot_not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "机器人不存在或已被删除",
		},
		{
			name:            "ErrBotfatherApplyProcessed 409 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherApplyProcessed) },
			wantCodeID:      "err.server.botfather.apply_processed",
			wantSemStatus:   http.StatusConflict,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "该申请已被处理",
		},
		{
			name:            "ErrBotfatherAlreadyFriends 409 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherAlreadyFriends) },
			wantCodeID:      "err.server.botfather.already_friends",
			wantSemStatus:   http.StatusConflict,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "你们已经是好友了",
		},
		// ---- internal codes (500, Internal=true): collapse + no English leak --
		{
			name:            "ErrBotfatherQueryFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherQueryFailed) },
			wantCodeID:      "err.server.botfather.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "query data",
		},
		{
			name:            "ErrBotfatherStoreFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherStoreFailed) },
			wantCodeID:      "err.server.botfather.store_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "update data",
		},
		{
			name:            "ErrBotfatherSendFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherSendFailed) },
			wantCodeID:      "err.server.botfather.send_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "send the message",
		},
		{
			name:            "ErrBotfatherUploadFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherUploadFailed) },
			wantCodeID:      "err.server.botfather.upload_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "process the file",
		},
		{
			name:            "ErrBotfatherIMTokenFailed collapses to shared internal copy",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrBotfatherIMTokenFailed) },
			wantCodeID:      "err.server.botfather.im_token_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "IM token",
		},
		// ---- status-preserving middleware helpers (real wire status) ---------
		{
			name:            "respondBotfatherAuthFailed preserves 401 (external adapters)",
			probe:           respondBotfatherAuthFailed,
			wantCodeID:      "err.server.botfather.auth_failed",
			wantSemStatus:   http.StatusUnauthorized,
			wantTransStatus: http.StatusUnauthorized,
			wantContains:    "机器人认证失败",
		},
		{
			name:            "respondBotfatherBotNotGroupMember preserves 403",
			probe:           respondBotfatherBotNotGroupMember,
			wantCodeID:      "err.server.botfather.bot_not_group_member",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusForbidden,
			wantContains:    "机器人不是该群成员",
		},
		{
			name:            "respondBotfatherBotNotGroupAdmin preserves 403",
			probe:           respondBotfatherBotNotGroupAdmin,
			wantCodeID:      "err.server.botfather.bot_not_group_admin",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusForbidden,
			wantContains:    "机器人不是该群管理员",
		},
		// ---- status-preserving direct codes (User-API + getUserInfo) ---------
		{
			name:            "respondBotfatherUsernameTaken preserves 409 + username detail",
			probe:           func(c *wkhttp.Context) { respondBotfatherUsernameTaken(c, "demo_bot") },
			wantCodeID:      "err.server.botfather.username_taken",
			wantSemStatus:   http.StatusConflict,
			wantTransStatus: http.StatusConflict,
			wantContains:    "用户名已被占用",
			wantDetails:     map[string]any{"username": "demo_bot"},
		},
		{
			name:            "ErrBotfatherBotNotFound preserves 404",
			probe:           func(c *wkhttp.Context) { httperrLWS(c, errcode.ErrBotfatherBotNotFound) },
			wantCodeID:      "err.server.botfather.bot_not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusNotFound,
			wantContains:    "Bot 不存在或无权限",
		},
		{
			name:            "ErrBotfatherBotNotInSpace preserves 403",
			probe:           func(c *wkhttp.Context) { httperrLWS(c, errcode.ErrBotfatherBotNotInSpace) },
			wantCodeID:      "err.server.botfather.bot_not_in_space",
			wantSemStatus:   http.StatusForbidden,
			wantTransStatus: http.StatusForbidden,
			wantContains:    "该 Bot 不属于当前 Space",
		},
		{
			name:            "ErrBotfatherUserNotFound preserves 404",
			probe:           func(c *wkhttp.Context) { httperrLWS(c, errcode.ErrBotfatherUserNotFound) },
			wantCodeID:      "err.server.botfather.user_not_found",
			wantSemStatus:   http.StatusNotFound,
			wantTransStatus: http.StatusNotFound,
			wantContains:    "用户不存在",
		},
		// ---- shared auth-required (apply login guard, wire 400 D14) -----------
		{
			name:            "ErrSharedAuthRequired 401 semantic, wire 400",
			probe:           func(c *wkhttp.Context) { httperrL(c, errcode.ErrSharedAuthRequired) },
			wantCodeID:      "err.shared.auth.required",
			wantSemStatus:   http.StatusUnauthorized,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "请先登录",
		},
		// ---- defensive identity guard ----------------------------------------
		{
			name:            "respondBotfatherIdentityMissing renders generic internal, no leak",
			probe:           func(c *wkhttp.Context) { bf.respondBotfatherIdentityMissing(c) },
			wantCodeID:      "err.server.botfather.query_failed",
			wantSemStatus:   http.StatusInternalServerError,
			wantTransStatus: http.StatusBadRequest,
			wantContains:    "服务器内部错误",
			wantNotContains: "query data",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := i18nHelperHarness(tc.probe)
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			req.Header.Set("Accept-Language", "zh-CN")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantTransStatus {
				t.Fatalf("HTTP status = %d, want %d; body=%s", rec.Code, tc.wantTransStatus, rec.Body.String())
			}
			var env errEnvelopeI18n
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v\nbody: %s", err, rec.Body.String())
			}
			if env.Error.Code != tc.wantCodeID {
				t.Fatalf("error.code = %q, want %q", env.Error.Code, tc.wantCodeID)
			}
			if env.Error.HTTPStatus != tc.wantSemStatus {
				t.Fatalf("error.http_status = %d, want %d", env.Error.HTTPStatus, tc.wantSemStatus)
			}
			if env.Status != tc.wantTransStatus {
				t.Fatalf("legacy status = %d, want %d", env.Status, tc.wantTransStatus)
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
