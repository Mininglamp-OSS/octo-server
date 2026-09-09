package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// 该 IdP 的官方参考实现把 client_secret / access_token 放在 query string 上,
// 且没有可用的替代形态,所以凭据必然出现在 req.URL 上。
//
// Go 的 http.Client 在**传输层**失败(DNS / 连接超时 / TLS / 连接重置)时返回
// *url.Error,其 Error() 会打印 e.URL —— 而 net/http 只脱 userinfo 里的密码,
// query string 原样保留。这个 error 会被现有的 zap.Error(err) 记录(callback 的
// Exchange 失败分支、userinfo 失败分支、sync worker 都有),于是 client_secret
// 明文进日志。而这恰好发生在 IdP 抖动、日志量最大、最容易被外部日志系统收走的时刻。
//
// 因此不能依赖"调用方记得别打 err":要把不安全的 error 值消灭在 provider 边界内。
func TestSanitizeTransportErr_StripsCredentialBearingURL(t *testing.T) {
	const (
		secret = "s3cr3t-client-secret"
		token  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	cases := []struct {
		name string
		raw  error
		// leaks 中任一子串出现在 sanitize 后的 Error() 里即失败
		leaks []string
	}{
		{
			name: "token_endpoint_url_error",
			raw: &url.Error{
				Op:  "Post",
				URL: "https://idp.example.com/oauth/token?grant_type=authorization_code&code=abc&client_id=cid&client_secret=" + secret + "&redirect_uri=https%3A%2F%2Fapp.example.com%2Fcb",
				Err: errors.New("dial tcp 10.0.0.1:443: i/o timeout"),
			},
			leaks: []string{secret, "client_secret", "idp.example.com", "code=abc"},
		},
		{
			name: "userinfo_url_error",
			raw: &url.Error{
				Op:  "Get",
				URL: "https://idp.example.com/api/oauth2/userinfo?access_token=" + token,
				Err: errors.New("connection reset by peer"),
			},
			leaks: []string{token, "access_token", "idp.example.com"},
		},
		{
			name: "wrapped_url_error_is_still_stripped",
			raw: fmt.Errorf("fetch userinfo: %w", &url.Error{
				Op:  "Get",
				URL: "https://idp.example.com/userinfo?access_token=" + token,
				Err: errors.New("EOF"),
			}),
			leaks: []string{token, "access_token"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeTransportErr("exchange", tc.raw)
			if got == nil {
				t.Fatal("sanitizeTransportErr returned nil for a non-nil error")
			}
			msg := got.Error()
			for _, leak := range tc.leaks {
				if strings.Contains(msg, leak) {
					t.Errorf("sanitised error still leaks %q:\n  %s", leak, msg)
				}
			}
		})
	}
}

// 剥掉 URL 不能把可诊断性也剥掉:运维要能从日志分辨"连不上"和"超时",
// 上层也可能用 errors.Is/As 做判定,所以底层原因必须保持在 wrap 链上。
func TestSanitizeTransportErr_PreservesDiagnosability(t *testing.T) {
	sentinel := errors.New("i/o timeout")
	raw := &url.Error{
		Op:  "Post",
		URL: "https://idp.example.com/oauth/token?client_secret=zzz",
		Err: sentinel,
	}
	got := sanitizeTransportErr("exchange", raw)

	if !errors.Is(got, sentinel) {
		t.Error("errors.Is lost the underlying cause; upper layers can no longer classify the failure")
	}
	msg := got.Error()
	for _, want := range []string{"exchange", "i/o timeout"} {
		if !strings.Contains(msg, want) {
			t.Errorf("sanitised error dropped %q, leaving nothing to diagnose:\n  %s", want, msg)
		}
	}
	// 传输层动作(Post/Get)本身不敏感,保留有助于定位是哪一步。
	if !strings.Contains(msg, "Post") {
		t.Errorf("sanitised error dropped the HTTP op:\n  %s", msg)
	}
}

// context 取消/超时是 sync worker 与 callback 都会遇到的正常路径,
// 必须仍能被 errors.Is 识别,否则会被误判成 IdP 故障。
func TestSanitizeTransportErr_PreservesContextErrors(t *testing.T) {
	for _, ctxErr := range []error{context.DeadlineExceeded, context.Canceled} {
		raw := &url.Error{Op: "Get", URL: "https://idp.example.com/userinfo?access_token=t", Err: ctxErr}
		got := sanitizeTransportErr("userinfo", raw)
		if !errors.Is(got, ctxErr) {
			t.Errorf("errors.Is(%v) lost after sanitising", ctxErr)
		}
		if strings.Contains(got.Error(), "access_token") {
			t.Errorf("leaked access_token: %s", got.Error())
		}
	}
}

// 非传输层错误(例如我们自己的信封校验失败)不该被这个函数改变语义,
// 只加一层操作名上下文。
func TestSanitizeTransportErr_PassesThroughNonURLError(t *testing.T) {
	base := &envelopeError{reason: "subject is empty", Code: "200", RequestID: "REQ-1"}
	got := sanitizeTransportErr("userinfo", base)

	var ee *envelopeError
	if !errors.As(got, &ee) {
		t.Fatal("errors.As lost *envelopeError; callers can no longer distinguish protocol from transport failures")
	}
	if !strings.Contains(got.Error(), "subject is empty") {
		t.Errorf("dropped the original reason: %s", got.Error())
	}
}

func TestSanitizeTransportErr_NilStaysNil(t *testing.T) {
	if got := sanitizeTransportErr("exchange", nil); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}
