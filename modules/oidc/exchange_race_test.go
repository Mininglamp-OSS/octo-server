package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// identity 并发首登竞态:同一个 (issuer, subject) 的两个请求同时到达,各自
// ResolveOrLink 都判 IsNew、各自建了 user,而 uk_issuer_subject 只让一行
// identity 落库。输家的 user 已 commit 无法回滚,成为 ghost —— 一个**没有
// identity 行**的孤立账号。
//
// recoverFromIdentityRace 的职责是把输家会话改签到赢家 uid 并返回赢家会话。
// 它自己的 doc 写明:"绝不能把 ghost session 写到 ThirdAuthcode —— 那等于给
// 前端发了一个无 OIDC 绑定的孤立账号 token,后续依赖 identity 的业务全部空转"。
//
// callback 路径做对了(`sessResp = recovered`)。两个 exchange 端点只检查返回值
// 非 nil 就把它丢掉,继续用 ghost 的 sessResp 回响应、记审计。下面两条用例把
// "最终交出去的是谁的会话"钉死。
// -----------------------------------------------------------------------------

const (
	raceGhostUID  = "u-race-loser"
	raceWinnerUID = "u-race-winner"
	raceSubject   = "823071756087671700"
)

// seedRaceStore 造出竞态时序:赢家行已在 bindings 里,但第一次 Get 看不到它
// (赢家在我方 Get 之后才 commit),而我方 Insert 会撞 1062。
func seedRaceStore(issuer, subject string) *fakeIdentityStore {
	store := newFakeIdentityStore()
	store.bindings[issuer+"|"+subject] = &IdentityModel{
		UID: raceWinnerUID, Issuer: issuer, Subject: subject,
	}
	store.winnerAppearsAfterFirstGet = true
	store.failInsertWithDuplicate = true
	return store
}

// raceUsers 让两次签发返回可区分的会话 —— 否则"交出赢家"和"交出 ghost"
// 在断言上无法分辨,而那正是被测缺陷。
func raceUsers() *fakeUserLookup {
	return &fakeUserLookup{
		usersByEmail: map[string][]string{},
		usersByPhone: map[string][]string{},
		loginRespByUID: map[string]*IssueSessionResp{
			raceGhostUID: {
				UID:           raceGhostUID,
				IsNewUser:     true,
				LoginRespJSON: `{"token":"ghost-token","uid":"` + raceGhostUID + `"}`,
			},
			raceWinnerUID: {
				UID:           raceWinnerUID,
				LoginRespJSON: `{"token":"winner-token","uid":"` + raceWinnerUID + `"}`,
			},
		},
	}
}

// raceProvider 返回固定 subject 的 provider,并把新建用户的 uid 固定成 ghost。
func raceProvider(issuer, subject string) *fakeProvider {
	return &fakeProvider{
		issuer: issuer,
		identityFn: func(_ context.Context, _ *TokenSet) (*IdentityClaims, error) {
			return &IdentityClaims{Issuer: issuer, Subject: subject, Name: "Race User"}, nil
		},
	}
}

func assertWinnerHandedOut(t *testing.T, w *httptest.ResponseRecorder, audit *fakeAudit, okEvent AuditEvent) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		UID       string `json:"uid"`
		LoginResp string `json:"login_resp"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	if body.UID != raceWinnerUID {
		t.Errorf("response uid = %q, want the race winner %q; handing back the ghost gives "+
			"the client a session for an account with no identity row, and every "+
			"identity-dependent feature then silently no-ops", body.UID, raceWinnerUID)
	}
	if !strings.Contains(body.LoginResp, "winner-token") {
		t.Errorf("login_resp = %q, want the winner's token", body.LoginResp)
	}
	if strings.Contains(body.LoginResp, "ghost-token") {
		t.Errorf("login_resp = %q still carries the ghost session", body.LoginResp)
	}
	uid, found := audit.uidForEvent(okEvent)
	if !found {
		t.Fatalf("no %s audit row written; events=%v", okEvent, audit.events())
	}
	if uid != raceWinnerUID {
		t.Errorf("%s audit uid = %q, want %q — recording success against the ghost makes "+
			"the audit trail unusable for exactly this incident", okEvent, uid, raceWinnerUID)
	}
}

func TestExchange_RaceRecovery_HandsBackWinnerNotGhost(t *testing.T) {
	const issuer = "test-idp"
	users := raceUsers()
	// 新建路径的 uid 由 IssueSession 决定;把无 uid 的请求也映射到 ghost。
	users.loginResp = users.loginRespByUID[raceGhostUID]
	store := seedRaceStore(issuer, raceSubject)

	o := newTestOIDCForExchange(t, raceProvider(issuer, raceSubject), users, store)
	audit := newFakeAudit()
	o.audit = audit

	w := postExchange(o, `{"access_token":"good"}`)
	assertWinnerHandedOut(t, w, audit, EventExchangeOK)
}

func TestExchangeJWT_RaceRecovery_HandsBackWinnerNotGhost(t *testing.T) {
	const secret = "test-jwt-secret-not-real-12345678"
	const issuer = "https://idp-test.example.com#bearer-jwt"
	const userID = int64(987654321)

	users := raceUsers()
	users.loginResp = users.loginRespByUID[raceGhostUID]
	store := seedRaceStore(issuer, "987654321")

	o := newTestOIDCForBearerJWT(t, []byte(secret), issuer, users, store)
	audit := newFakeAudit()
	o.audit = audit

	tok := signBearerTesting(t, []byte(secret), userID, "race.user", time.Now().Add(time.Hour))
	w := postBearerExchange(o, `{"access_token":"`+tok+`"}`)
	assertWinnerHandedOut(t, w, audit, EventBearerExchangeOK)
}

// 竞态审计事件必须归属于发起它的那条路径。
//
// recoverFromIdentityRace 原本硬编码 EventCallbackFail,而它被 callback、
// /exchange、/exchange-jwt 三处调用。于是 exchange 的竞态在审计表里显示成
// "callback 失败",把事后排查引向一条这个请求从未走过的代码路径 —— 而审计行
// 存在的唯一意义就是这种排查。
func TestExchange_RaceRecovery_AuditEventBelongsToCallingPath(t *testing.T) {
	const issuer = "test-idp"
	users := raceUsers()
	users.loginResp = users.loginRespByUID[raceGhostUID]
	store := seedRaceStore(issuer, raceSubject)

	o := newTestOIDCForExchange(t, raceProvider(issuer, raceSubject), users, store)
	audit := newFakeAudit()
	o.audit = audit

	if w := postExchange(o, `{"access_token":"good"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	for _, e := range audit.events() {
		if e == EventCallbackFail {
			t.Errorf("the /exchange path recorded %s; this request never touched the "+
				"callback (events=%v)", e, audit.events())
		}
	}
}
