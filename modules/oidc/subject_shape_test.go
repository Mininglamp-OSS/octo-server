package oidc

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckSubjectShape(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		wantErr bool
	}{
		// 文档示例的形态 —— 必须放行,否则守卫会挡掉正常登录
		{"18-digit sub from the docs", "823071756087671783", false},
		{"20-digit id (ou_id magnitude)", "12345678901234567890", false},
		{"exactly at the threshold", "1234567890", false},

		// 明确危险:工号量级的纯数字
		{"7-digit employee-number shape", "7654321", true},
		{"1 digit", "1", true},
		{"just below the threshold", "123456789", true},

		// 含非数字 → 不可能是工号,长度不再是信号
		{"uuid", "b3f1c2d4-5e6f-7a8b-9c0d-1e2f3a4b5c6d", false},
		{"short alphanumeric", "a1b2c", false},
		{"employee-number-like but prefixed", "EMP7654321", false},

		// 空串由上游单独拒绝,这里不该被当成"合法"
		{"empty is not this guard's job but must not pass as digits", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkSubjectShape(c.subject)
			if c.wantErr && err == nil {
				t.Fatalf("checkSubjectShape(%q) = nil, want an error", c.subject)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("checkSubjectShape(%q) = %v, want nil", c.subject, err)
			}
			if c.wantErr && !errors.Is(err, errSubjectTooShort) {
				t.Errorf("err = %v, want it to wrap errSubjectTooShort", err)
			}
		})
	}
}

// 错误信息不能回显 subject 本身 —— 它是用户标识,会随 error 进日志。
func TestCheckSubjectShape_ErrorDoesNotLeakSubject(t *testing.T) {
	const subject = "7654321"
	err := checkSubjectShape(subject)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), subject) {
		t.Errorf("error message %q contains the raw subject; log the length, not the value",
			err.Error())
	}
}

// 端到端:上游返回工号形态的 sub 时,登录必须失败,而且不写 identity 行。
//
// 这是这个守卫存在的全部理由 —— 把"静默把新入职者接到离职者账号上"换成
// "登录失败 + 告警"。所以断言的重点是 identityStore 一行都没写。
func TestOAuth2Callback_EmployeeNumberSubjectIsRefused(t *testing.T) {
	mp := newMockOAuth2Provider(t)
	mp.UserInfoBody = `{
	  "success": true,
	  "code": "200",
	  "requestId": "req-empno",
	  "data": {"sub": "7654321", "nickname": "new-hire", "email": "hire@example.com"}
	}`

	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-must-not-be-created", LoginRespJSON: `{"token":"t"}`},
	}
	store := newFakeIdentityStore()
	o := newTestOIDCOAuth2(t, mp, users, store)
	r := newOAuth2TestRouter(o)

	state, _ := authorizeAndGetState(t, r, "authcode=front-ac-empno&return_to=/home")
	w := newRecorderForCallback(t, r, state, "code-empno")

	if len(users.loginCalls) != 0 {
		t.Errorf("IssueSession ran %d times; an employee-number-shaped subject must not log in",
			len(users.loginCalls))
	}
	if len(store.written) != 0 {
		t.Errorf("identity rows written = %d, want 0 — writing one would permanently bind "+
			"this employee number to an account", len(store.written))
	}
	if w.Code == 302 && !strings.Contains(w.Header().Get("Location"), "oidc_error") {
		t.Errorf("redirect = %q, want an oidc_error marker", w.Header().Get("Location"))
	}
}

// 反面:18 位 sub 走同一条路必须成功建号 —— 证明守卫没有把正常形态一起挡掉。
func TestOAuth2Callback_LongNumericSubjectStillLogsIn(t *testing.T) {
	mp := newMockOAuth2Provider(t)
	mp.SubjectForUserInfo = "823071756087671783"

	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-ok", LoginRespJSON: `{"token":"t-ok"}`},
	}
	store := newFakeIdentityStore()
	o := newTestOIDCOAuth2(t, mp, users, store)
	r := newOAuth2TestRouter(o)

	state, _ := authorizeAndGetState(t, r, "authcode=front-ac-ok&return_to=/home")
	w := newRecorderForCallback(t, r, state, "code-ok")

	if w.Code != 302 {
		t.Fatalf("callback status = %d, want 302; body=%s", w.Code, w.Body.String())
	}
	if len(users.loginCalls) != 1 {
		t.Errorf("IssueSession calls = %d, want 1", len(users.loginCalls))
	}
	if len(store.written) != 1 {
		t.Errorf("identity rows = %d, want 1", len(store.written))
	}
}
