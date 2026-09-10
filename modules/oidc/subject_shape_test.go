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

// subject 的上限也必须在信任边界拒绝,理由与 issuer 的 issuerMaxLen 完全相同,
// 而且更强 —— issuer 是运维配置的常量,subject 来自上游响应。
//
// user_oidc_identity.subject 是 VARCHAR(255) 且在 uk_issuer_subject 里。超长时:
//   - 严格 sql_mode:INSERT 报错,但那已经是 IssueSession **建完用户之后** ——
//     留下一个没有 identity 行的孤立用户,客户端拿到 401;
//   - 非严格 sql_mode:静默截断,于是前 255 字节相同的两个 subject 合成同一行,
//     正是本 PR 到处在防的账号接管形态,且不可逆。
//
// checkSubjectShape 只有下限管不到这个:300 位纯数字轻松通过 `>= 10`。
func TestCheckSubjectShape_TooLongIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		wantErr bool
	}{
		// 恰好填满列宽 —— 必须放行,否则守卫会挡掉合法边界值
		{"exactly the column width", strings.Repeat("a", subjectMaxLen), false},
		{"one byte over", strings.Repeat("a", subjectMaxLen+1), true},
		{"far over", strings.Repeat("a", 4096), true},
		// 纯数字的超长值同样要拒:下限检查放它过去
		{"300-digit numeric passes the minimum but not the maximum",
			strings.Repeat("9", 300), true},
		// 多字节字符按**字节**计,因为列宽是字节
		{"multibyte over the byte limit", strings.Repeat("中", subjectMaxLen), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkSubjectShape(c.subject)
			if c.wantErr && err == nil {
				t.Fatalf("a %d-byte subject was accepted; it cannot be stored in a "+
					"%d-byte column without truncating or erroring after the user row "+
					"has already been created", len(c.subject), subjectMaxLen)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("a %d-byte subject was refused: %v", len(c.subject), err)
			}
			if c.wantErr && err != nil && !errors.Is(err, errSubjectTooLong) {
				t.Errorf("err = %v, want it to wrap errSubjectTooLong", err)
			}
		})
	}
}

// 上限必须与 issuer 用同一个列宽常量 —— 两个数字分开维护迟早不一致。
func TestSubjectMaxLen_MatchesTheColumnWidth(t *testing.T) {
	if subjectMaxLen != issuerMaxLen {
		t.Errorf("subjectMaxLen = %d, issuerMaxLen = %d; both columns are VARCHAR(255) "+
			"in the same unique key, so they must not drift apart",
			subjectMaxLen, issuerMaxLen)
	}
}

// 拒绝信息只给长度,不回显 subject —— 超长值进日志既无用又是 PII 面。
func TestCheckSubjectShape_TooLongErrorDoesNotLeakSubject(t *testing.T) {
	subject := strings.Repeat("x", 300)
	err := checkSubjectShape(subject)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), subject) {
		t.Error("the error message echoes the whole subject back")
	}
}
