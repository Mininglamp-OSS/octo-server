package oidc

import (
	"strings"
	"testing"
)

// parseUserInfoEnvelope 是 plain-OAuth2 IdP 唯一的身份来源,所以它同时是
// 解析器和信任边界。标准 OIDC 靠 id_token 验签免费获得的三件事,在这里必须
// 由本函数自己做,任何一条漏掉都是可登录的安全缺陷:
//
//  1. 私有信封的 success/code 判定 —— IdP 会用 HTTP 200 承载 {"success":false},
//     不判就等于把失败当成功登录。
//  2. subject 非空 —— user_oidc_identity.subject 是 NOT NULL DEFAULT ” 且带
//     UNIQUE(issuer,subject);放进空串会让所有空 sub 用户塌成同一行,互相登进
//     对方账号。
//  3. issuer 由我方注入 —— 该协议没有 iss claim,若从响应体取值,IdP(或中间人)
//     就能自己指定身份命名空间。
//
// 另外 EmailVerified/PhoneVerified 必须恒 false:本协议不提供 verified 语义,
// 响应里若出现同名字段也不具备可信含义,采信它等于给 autolink 开了账号接管入口。
// 注:各用例里的 sub 占位值(1000000001 等)必须是"至少 10 位纯数字"或含非数字
// 字符 —— 短纯数字会被 checkSubjectShape 以"像工号"为由拒绝(见 subject_shape.go),
// 那样用例就会失败在形态守卫上,而不是它本来要测的那条分支。
func TestParseUserInfoEnvelope(t *testing.T) {
	const testIssuer = "test-idp"

	// 对方文档给出的完整成功响应形态(值已改为非真实数据)。
	okBody := `{
	  "success": true,
	  "code": "200",
	  "message": null,
	  "requestId": "REQ-0001",
	  "data": {
	    "sub": "100000000000000001",
	    "user_id": "123",
	    "ou_id": "20000000000000000002",
	    "nickname": "Test User",
	    "phone_number": "13000000000",
	    "ou_name": "Example Org",
	    "email": "test@example.com",
	    "username": "0000001"
	  }
	}`

	cases := []struct {
		name    string
		body    string
		wantErr bool
		// 仅在 wantErr=false 时校验
		wantSubject string
		wantName    string
		wantEmail   string
		wantPhone   string
	}{
		{
			name:        "ok_full_envelope",
			body:        okBody,
			wantSubject: "100000000000000001",
			wantName:    "Test User",
			wantEmail:   "test@example.com",
			wantPhone:   "13000000000",
		},
		{
			// HTTP 200 + success=false:必须拒绝,否则失败被当成登录成功。
			name:    "reject_success_false",
			body:    `{"success":false,"code":"200","data":{"sub":"1000000001"}}`,
			wantErr: true,
		},
		{
			name:    "reject_code_not_200",
			body:    `{"success":true,"code":"500","data":{"sub":"1000000001"}}`,
			wantErr: true,
		},
		{
			name:    "reject_code_missing",
			body:    `{"success":true,"data":{"sub":"1000000001"}}`,
			wantErr: true,
		},
		{
			name:    "reject_empty_sub",
			body:    `{"success":true,"code":"200","data":{"sub":""}}`,
			wantErr: true,
		},
		{
			name:    "reject_missing_sub",
			body:    `{"success":true,"code":"200","data":{"nickname":"x"}}`,
			wantErr: true,
		},
		{
			name:    "reject_sub_whitespace_only",
			body:    `{"success":true,"code":"200","data":{"sub":"   "}}`,
			wantErr: true,
		},
		{
			name:    "reject_missing_data",
			body:    `{"success":true,"code":"200"}`,
			wantErr: true,
		},
		{
			name:    "reject_invalid_json",
			body:    `{"success":true,`,
			wantErr: true,
		},
		{
			name:    "reject_empty_body",
			body:    ``,
			wantErr: true,
		},
		{
			// 文档把 user_id 标为 Integer 但示例是字符串,两种形态都不能让
			// 整体解析失败(我们并不消费该字段)。
			name:        "tolerant_user_id_as_number",
			body:        `{"success":true,"code":"200","data":{"sub":"1000000001","user_id":123}}`,
			wantSubject: "1000000001",
		},
		{
			name:        "tolerant_unknown_extra_fields",
			body:        `{"success":true,"code":"200","data":{"sub":"1000000001","brand_new_field":{"a":1}},"extra":"x"}`,
			wantSubject: "1000000001",
		},
		{
			name:        "sub_is_trimmed",
			body:        `{"success":true,"code":"200","data":{"sub":"  1000000001  "}}`,
			wantSubject: "1000000001",
		},
		{
			// 文档把 code 标为 String 且示例为 "200"。同一份文档里 user_id 已经
			// 出现过标注与示例不符的先例,而 wire type 一变就是全站登录失败
			// (本模块 aud 字段踩过同类坑),所以两种形态都要接。
			name:        "tolerant_code_as_number",
			body:        `{"success":true,"code":200,"data":{"sub":"1000000001"}}`,
			wantSubject: "1000000001",
		},
		{
			// success 缺失时按 false 处理(fail-closed):宁可拒登,
			// 不可把一个形态未知的响应当成功。
			name:    "reject_success_missing",
			body:    `{"code":"200","data":{"sub":"1000000001"}}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUserInfoEnvelope([]byte(tc.body), testIssuer)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (claims=%+v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("claims is nil without error")
			}
			if got.Subject != tc.wantSubject {
				t.Errorf("Subject = %q, want %q", got.Subject, tc.wantSubject)
			}
			if tc.wantName != "" && got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
			if tc.wantEmail != "" && got.Email != tc.wantEmail {
				t.Errorf("Email = %q, want %q", got.Email, tc.wantEmail)
			}
			if tc.wantPhone != "" && got.PhoneNumber != tc.wantPhone {
				t.Errorf("PhoneNumber = %q, want %q", got.PhoneNumber, tc.wantPhone)
			}
			// issuer 必须是我方注入值。
			if got.Issuer != testIssuer {
				t.Errorf("Issuer = %q, want %q (issuer must be injected, never taken from the response)", got.Issuer, testIssuer)
			}
		})
	}
}

// 该协议不提供 verified 语义。即使响应里出现 email_verified/phone_number_verified,
// 也必须落 false —— 采信它会让 autolink 拿一个未经验证的邮箱去认领已有账号。
func TestParseUserInfoEnvelope_VerifiedFlagsAlwaysFalse(t *testing.T) {
	body := `{
	  "success": true, "code": "200",
	  "data": {
	    "sub": "1000000001",
	    "email": "a@example.com", "email_verified": true,
	    "phone_number": "13000000000", "phone_number_verified": true
	  }
	}`
	claims, err := parseUserInfoEnvelope([]byte(body), "test-idp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.EmailVerified {
		t.Error("EmailVerified = true; this protocol has no verified semantics, it must stay false (fail-closed)")
	}
	if claims.PhoneVerified {
		t.Error("PhoneVerified = true; this protocol has no verified semantics, it must stay false (fail-closed)")
	}
}

// 响应体自带 iss 时不得覆盖我方注入的 issuer:否则 IdP 侧一次配置变更
// 就能把身份写进另一个命名空间,绕过 (issuer,subject) 的唯一性语义。
func TestParseUserInfoEnvelope_IssuerNotTakenFromBody(t *testing.T) {
	body := `{"success":true,"code":"200","data":{"sub":"1000000001","iss":"attacker-controlled"}}`
	claims, err := parseUserInfoEnvelope([]byte(body), "ours")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.Issuer != "ours" {
		t.Fatalf("Issuer = %q, want %q", claims.Issuer, "ours")
	}
}

// 错误信息不得回显响应体:body 里含 access_token 换来的用户 PII,
// 且该 error 会被上层 zap.Error 打进日志。
func TestParseUserInfoEnvelope_ErrorDoesNotEchoBody(t *testing.T) {
	body := `{"success":false,"code":"403","message":"user 13000000000 is locked","data":{"sub":"secret-subject-value"}}`
	_, err := parseUserInfoEnvelope([]byte(body), "test-idp")
	if err == nil {
		t.Fatal("want error")
	}
	for _, leak := range []string{"13000000000", "secret-subject-value"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error message leaks %q: %v", leak, err)
		}
	}
}
