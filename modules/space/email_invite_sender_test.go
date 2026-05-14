package space

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// recordingInviteSender 用于测试：把每次调用的入参塞到 channel，单测可阻塞读取。
type recordingInviteSender struct {
	mu    sync.Mutex
	calls []sentInviteEmail
	err   error
	done  chan struct{}
}

type sentInviteEmail struct {
	To      string
	Subject string
	Body    string
}

func newRecordingSender() *recordingInviteSender {
	return &recordingInviteSender{done: make(chan struct{}, 4)}
}

func (r *recordingInviteSender) SendHTMLEmail(_ context.Context, to, subject, body string) error {
	r.mu.Lock()
	r.calls = append(r.calls, sentInviteEmail{to, subject, body})
	r.mu.Unlock()
	select {
	case r.done <- struct{}{}:
	default:
	}
	return r.err
}

func (r *recordingInviteSender) waitOne(t *testing.T) sentInviteEmail {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("等待邮件发送回调超时")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

// withRecordingSender 把测试期间的全局发送器替换为 recorder，并在测试结束后还原。
//
// NOTE: 共享全局 override + 单例 Space，使用本 helper 的测试不要 t.Parallel()。
func withRecordingSender(t *testing.T) *recordingInviteSender {
	t.Helper()
	rec := newRecordingSender()
	prev := getInviteEmailSenderForTest()
	SetInviteEmailSenderForTest(rec)
	t.Cleanup(func() { SetInviteEmailSenderForTest(prev) })
	return rec
}

// withBaseURL 让测试用例临时把 External.BaseURL 改为非空，结束时还原。
// 邮件接受链接由后端在 {BaseURL}/v1/space/email-invite 提供，dispatch 也读这个字段。
//
// NOTE: 直接写共享 *config.Config，使用本 helper 的测试不要 t.Parallel()。
func withBaseURL(t *testing.T, sp *Space, base string) {
	t.Helper()
	cfg := sp.ctx.GetConfig()
	prev := cfg.External.BaseURL
	cfg.External.BaseURL = base
	t.Cleanup(func() { cfg.External.BaseURL = prev })
}

func TestDispatchOwnerInviteEmail_SendsExpectedFields(t *testing.T) {
	_, sp, err := setup(t)
	assert.NoError(t, err)
	rec := withRecordingSender(t)
	withBaseURL(t, sp, "https://h5.example.com")

	rawToken := "raw-tok-owner-123"
	inv := &spaceEmailInviteModel{
		Email:              "owner@example.com",
		PlannedName:        "Acme Owner Space",
		PlannedDescription: "for testing",
		InviteType:         EmailInviteTypeOwner,
		CreatedBy:          "admin-uid",
	}
	sp.dispatchInviteEmail(inv, rawToken)
	got := rec.waitOne(t)

	assert.Equal(t, "owner@example.com", got.To)
	assert.Contains(t, got.Subject, "Acme Owner Space")
	assert.Contains(t, got.Body, "Acme Owner Space")
	assert.Contains(t, got.Body, "token="+rawToken,
		"邮件正文必须包含 raw token，否则收件人无法接受邀请")
}

func TestDispatchMemberInviteEmail_UsesSpaceName(t *testing.T) {
	_, sp, err := setup(t)
	assert.NoError(t, err)
	rec := withRecordingSender(t)
	withBaseURL(t, sp, "https://h5.example.com")

	// 准备一个真实空间，让 dispatch 能查到 spaceName
	const spaceId = "sp-dispatch-1"
	assert.NoError(t, testSpaceDB.insertSpaceNoTx(&SpaceModel{
		SpaceId: spaceId, Name: "我的会议空间", Creator: "owner-x", Status: SpaceStatusNormal,
	}))

	inv := &spaceEmailInviteModel{
		Email:      "newmember@example.com",
		SpaceId:    spaceId,
		Role:       EmailInviteRoleMember,
		InviteType: EmailInviteTypeMember,
		CreatedBy:  "owner-x",
	}
	sp.dispatchInviteEmail(inv, "tok-mem-1")
	got := rec.waitOne(t)

	assert.Equal(t, "newmember@example.com", got.To)
	assert.Contains(t, got.Body, "我的会议空间")
	assert.Contains(t, got.Body, "tok-mem-1")
}

func TestDispatchInviteEmail_NoBaseURLSkips(t *testing.T) {
	_, sp, err := setup(t)
	assert.NoError(t, err)
	rec := withRecordingSender(t)

	// 显式清空 BaseURL
	withBaseURL(t, sp, "")

	inv := &spaceEmailInviteModel{
		Email: "x@example.com", PlannedName: "X", InviteType: EmailInviteTypeOwner,
	}
	sp.dispatchInviteEmail(inv, "tok-skip")

	// 不应触发发送（没有可点击链接）
	select {
	case <-rec.done:
		t.Fatal("BaseURL 为空时不应发送邮件")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestCreateOwnerEmailInvite_TriggersEmail(t *testing.T) {
	srv, sp, err := setup(t)
	assert.NoError(t, err)
	rec := withRecordingSender(t)
	withBaseURL(t, sp, "https://h5.example.com")
	tk := adminToken(t)

	w := postJSON(t, srv, "/v1/manager/spaces/invites", tk, map[string]interface{}{
		"email":             "owner-e2e@example.com",
		"planned_name":      "团队 A",
		"planned_max_users": 50,
		"planned_join_mode": 0,
	})
	assert.Equal(t, 200, w.Code, w.Body.String())

	got := rec.waitOne(t)
	assert.Equal(t, "owner-e2e@example.com", got.To)
	assert.Contains(t, got.Subject, "团队 A")
	assert.Contains(t, got.Body, "https://h5.example.com/v1/space/email-invite?token=")
}

func TestCreateMemberEmailInvite_TriggersEmail(t *testing.T) {
	srv, sp, err := setup(t)
	assert.NoError(t, err)
	rec := withRecordingSender(t)
	withBaseURL(t, sp, "https://h5.example.com")

	const spaceId = "sp-mem-e2e"
	// testutil.UID 作为 admin (role=1)，creator 是另一人
	seedSpaceWithMemberRole(t, spaceId, "owner-mem", 1)

	w := postJSON(t, srv, "/v1/space/"+spaceId+"/email-invites",
		testutil.Token, map[string]interface{}{
			"email": "member-e2e@example.com",
			"role":  EmailInviteRoleAdmin,
		})
	assert.Equal(t, 200, w.Code, w.Body.String())

	got := rec.waitOne(t)
	assert.Equal(t, "member-e2e@example.com", got.To)
	assert.Contains(t, got.Body, "管理员")
	assert.Contains(t, got.Body, "测试空间") // seedSpaceWithMemberRole 设的名字
}

// chdirRepoRoot 落地页 handler 用 ./assets/... 相对路径读文件，需要切到 repo 根。
func chdirRepoRoot(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	assert.NoError(t, err)
	if err := os.Chdir("../.."); err != nil {
		t.Fatalf("chdir to repo root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

func TestEmailInvitePage_RendersHTMLWithAPIBase(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	srv, sp, err := setup(t)
	assert.NoError(t, err)
	chdirRepoRoot(t)

	cfg := sp.ctx.GetConfig()
	prev := cfg.External.BaseURL
	cfg.External.BaseURL = "https://api.test.example.com"
	t.Cleanup(func() { cfg.External.BaseURL = prev })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/space/email-invite?token=anything", nil)
	srv.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `var API_BASE = "https://api.test.example.com"`,
		"API_BASE_URL 占位符应被替换为带引号的 BaseURL 字面量")
	assert.NotContains(t, body, "{{API_BASE_URL}}", "占位符应已被替换")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")

	// CSP 必须钉死 connect-src 到 self（防止 token 外发到第三方域）
	// 和 frame-ancestors 'none'（防 clickjacking）。
	csp := w.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "connect-src 'self'", "fetch 必须限制到同源，否则 XSS 可外发 token")
	assert.Contains(t, csp, "frame-ancestors 'none'", "防 clickjacking")
	assert.Contains(t, csp, "default-src 'self'", "默认策略应是同源 only")
	// rate-limit / server error / network error 三个状态块必须存在（参见 group_invite.html YUJ-42）。
	assert.Contains(t, body, `id="state-rate-limited"`)
	assert.Contains(t, body, `id="state-server-error"`)
	assert.Contains(t, body, `id="state-network-error"`)

	// findTokenAndSid 必须扫 sessionStorage（dmwork-web / admin 实际写入的位置），
	// 同时支持 dm-admin-auth JSON fallback。回归 im-test 验证发现的 localStorage-only 漏洞。
	assert.Contains(t, body, "scanByPrefix(sessionStorage)",
		"必须先扫 sessionStorage，否则 admin 后台 / dmwork-web 的会话读不到")
	assert.Contains(t, body, "dm-admin-auth",
		"必须支持 admin 后台的 dm-admin-auth JSON 结构作为 fallback")
}

func TestEmailInvitePage_PathDoesNotShadowPreview(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	// 防御回归：/v1/space/email-invite 与 /v1/space/email-invite/:token 必须分别匹配
	srv, _, err := setup(t)
	assert.NoError(t, err)
	chdirRepoRoot(t)

	wPage := httptest.NewRecorder()
	reqPage, _ := http.NewRequest("GET", "/v1/space/email-invite", nil)
	srv.GetRoute().ServeHTTP(wPage, reqPage)
	assert.Equal(t, http.StatusOK, wPage.Code)
	assert.Contains(t, wPage.Header().Get("Content-Type"), "text/html")

	wPreview := httptest.NewRecorder()
	reqPreview, _ := http.NewRequest("GET", "/v1/space/email-invite/non-existent-token-xxx", nil)
	srv.GetRoute().ServeHTTP(wPreview, reqPreview)
	assert.Equal(t, http.StatusOK, wPreview.Code)
	assert.Contains(t, wPreview.Header().Get("Content-Type"), "application/json")
}

func TestDispatchInviteEmail_SendErrorDoesNotPanic(t *testing.T) {
	_, sp, err := setup(t)
	assert.NoError(t, err)
	rec := withRecordingSender(t)
	withBaseURL(t, sp, "https://h5.example.com")
	rec.err = errors.New("smtp down")

	inv := &spaceEmailInviteModel{
		Email: "x@example.com", PlannedName: "X", InviteType: EmailInviteTypeOwner,
	}
	// 不应抛 panic 或上抛错误
	assert.NotPanics(t, func() { sp.dispatchInviteEmail(inv, "tok-fail") })
	got := rec.waitOne(t)
	assert.True(t, strings.Contains(got.Body, "X"))
}
