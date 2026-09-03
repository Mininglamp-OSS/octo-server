package oidc

// identity_bounds_test.go — (issuer, subject) 必须在**产生任何副作用之前**就被
// 确认为可落库。
//
// user_oidc_identity 的两列都是 VARCHAR(255),同处唯一键 uk_issuer_subject。
// 超长值的两种结局都很糟,而且都发生在 IssueSession 已经建完用户之后:
//   - 严格 sql_mode:INSERT 失败,留下一个没有 identity 行的孤立用户,客户端只见 401;
//   - 非严格 sql_mode:静默截断,前 255 字节相同的两个身份合成同一行 = 账号接管,不可逆。
//
// 上一轮加的 checkSubjectShape 只挂在 plain-OAuth2 的信任边界上
// (parseUserInfoEnvelope),于是标准 OIDC kind 那条路完全没有上限 —— 一张签名
// 合法、sub 300 字节的 id_token 照样走到建号和 INSERT。签名证明来源,不证明可存储。
//
// 所以守卫必须放在**协议中立**的位置。选中的位置是 claims 进入有状态路径的两个
// 入口:Service.ResolveOrLink 与 BindService.IssueWithReason —— 它们今天各自已经
// 写了一份一模一样的 "iss/sub 非空" 校验,把上限并进去顺手也把那份重复收成一处。
// -----------------------------------------------------------------------------

import (
	"context"
	"strings"
	"testing"
)

// overlongValue 造一个刚好超过列宽的 ASCII 值。
func overlongValue(prefix string) string {
	return prefix + strings.Repeat("x", issuerMaxLen+1-len(prefix))
}

func newBoundsService() (*Service, *fakeIdentityStore, *fakeUserLookup) {
	store := &fakeIdentityStore{bindings: map[string]*IdentityModel{}}
	users := &fakeUserLookup{
		loginResp: &IssueSessionResp{UID: "u-must-not-be-created", LoginRespJSON: `{"token":"t"}`},
	}
	svc := newService(ProviderConfig{
		AllowNewUser: true, AutoLinkByEmail: true, RequireEmailVerified: true,
	}, store, users)
	return svc, store, users
}

// ResolveOrLink 必须拒绝超长 subject —— 与 provider kind 无关。
func TestResolveOrLink_RefusesOverlongSubject(t *testing.T) {
	svc, store, users := newBoundsService()
	sub := strings.Repeat("9", subjectMaxLen+1)

	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer: "https://idp.example.com", Subject: sub,
		Email: "who@example.com", EmailVerified: true,
	})
	if err == nil {
		t.Fatalf("ResolveOrLink accepted a %d-byte subject (column width is %d); it would be "+
			"truncated or rejected by MySQL only *after* IssueSession created the user",
			len(sub), subjectMaxLen)
	}
	if res != nil {
		t.Errorf("ResolveOrLink returned a result alongside the error: %+v", res)
	}
	if len(store.written) != 0 {
		t.Errorf("an identity row was written for an unstorable subject: %+v", store.written)
	}
	if len(users.loginCalls) != 0 {
		t.Errorf("IssueSession ran for an unstorable subject: %+v", users.loginCalls)
	}
	// 长度进错误信息,取值不进 —— 超长 subject 对排障无用,只扩大 PII 面。
	if strings.Contains(err.Error(), sub) {
		t.Error("the error message echoes the whole subject value")
	}
}

// 同一道守卫也必须管 issuer:它同在唯一键里,截断后两个命名空间会塌成一个。
func TestResolveOrLink_RefusesOverlongIssuer(t *testing.T) {
	svc, store, users := newBoundsService()
	iss := overlongValue("https://idp.example.com/")

	if _, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer: iss, Subject: "1234567890",
	}); err == nil {
		t.Fatalf("ResolveOrLink accepted a %d-byte issuer (column width is %d)", len(iss), issuerMaxLen)
	}
	if len(store.written) != 0 || len(users.loginCalls) != 0 {
		t.Error("an unstorable issuer produced side effects")
	}
}

// 下限(短纯数字像工号)**刻意不**在这里生效。
//
// 这条断言是反过来写的,因为我第一版把它写反了:把工号启发式一起放进共享入口,
// 结果业务 JWT 那条路整体登不进来 —— 它的 subject 是我方业务库主键
// (strconv.FormatInt(userId)),测试里的 42 / 54321 全被当成"像工号"拒掉。
//
// 论证只在"这个数字由人事系统分配、会在离职/入职之间复用"时成立。数据库主键不
// 复用,小取值只说明部署年轻。所以这条规则属于**上游断言的 subject**,留在两个
// provider 里,由 AuthProvider 契约测试钉住(见 provider_bounds_and_endsession_test.go)。
func TestResolveOrLink_DoesNotApplyTheEmployeeNumberHeuristic(t *testing.T) {
	svc, _, _ := newBoundsService()
	// 业务 JWT 路径的形态:issuer 带 #bearer-jwt 后缀,subject 是短数字主键。
	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:  "https://idp.example.com" + bearerJWTIssuerSuffix,
		Subject: "42",
	})
	if err != nil {
		t.Fatalf("ResolveOrLink refused a short numeric subject from our own namespace: %v.\n"+
			"The employee-number heuristic must not apply here: this subject is our business "+
			"database's primary key, which is never reused. Applying it would lock every "+
			"deployment whose user ids are short out of the desktop credential path", err)
	}
	if res == nil || !res.IsNew {
		t.Errorf("expected a new identity, got %+v", res)
	}
}

// 合法形态必须照常通过 —— 否则这道守卫就把登录整体弄坏了。
func TestResolveOrLink_AcceptsStorableIdentity(t *testing.T) {
	svc, _, users := newBoundsService()
	res, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
		Issuer:  "https://idp.example.com",
		Subject: "823071756087671783",
	})
	if err != nil {
		t.Fatalf("ResolveOrLink refused a storable identity: %v", err)
	}
	if !res.IsNew {
		t.Errorf("expected IsNew for an unseen identity, got %+v", res)
	}
	_ = users
}

// 边界:正好等于列宽必须放行,列宽 +1 必须拒绝。
func TestResolveOrLink_BoundaryIsExactlyTheColumnWidth(t *testing.T) {
	for _, c := range []struct {
		name   string
		length int
		wantOK bool
	}{
		{"exactly the column width", subjectMaxLen, true},
		{"one byte over", subjectMaxLen + 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, _, _ := newBoundsService()
			// 用字母,避开"短纯数字"那条下限,单独考察上限。
			sub := strings.Repeat("a", c.length)
			_, err := svc.ResolveOrLink(context.Background(), &IDTokenClaims{
				Issuer: "https://idp.example.com", Subject: sub,
			})
			if c.wantOK && err != nil {
				t.Errorf("a %d-byte subject equal to the column width was refused: %v", c.length, err)
			}
			if !c.wantOK && err == nil {
				t.Errorf("a %d-byte subject exceeding the column width was accepted", c.length)
			}
		})
	}
}

// 绑定路径是第三个 claims 入口(既不走 callback 的 ResolveOrLink 成功分支,也不走
// exchange)。它把 claims 快照存进 BindSession,之后 Create 拿快照建号并落 identity 行。
// 所以同一道守卫必须在这里也生效,否则超长 subject 会从这条路进去。
func TestBindIssue_RefusesUnstorableIdentity(t *testing.T) {
	for name, claims := range map[string]*IDTokenClaims{
		"overlong subject": {
			Issuer: "https://idp.example.com", Subject: strings.Repeat("9", subjectMaxLen+1)},
		"overlong issuer": {
			Issuer: overlongValue("https://idp.example.com/"), Subject: "823071756087671783"},
		// 刻意不列"短纯数字 subject":那条规则属于上游断言的 subject,已在 provider
		// 层拒掉,claims 根本到不了绑定这一步。在这里再断言一遍会把守卫的适用范围
		// 又推广一次 —— 正是本轮要避免的那个错误。
	} {
		t.Run(name, func(t *testing.T) {
			svc, store, _, _ := newTestBindService(t)
			jti, err := svc.IssueWithReason(context.Background(), claims,
				&StateData{IP: "10.0.0.1"}, BindReasonUnknownUser)
			if err == nil {
				t.Fatalf("IssueWithReason accepted an unstorable identity (jti=%s); the value "+
					"would only fail at INSERT, after Create already built the user", jti)
			}
			if len(store.sessions) != 0 {
				t.Errorf("a bind session was persisted for an unstorable identity: %d", len(store.sessions))
			}
		})
	}
}
