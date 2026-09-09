package oidc

// identity_case_collision_test.go — 折叠碰撞必须在**任何副作用之前**被拒绝。
//
// 逐字节复核把"静默合并两个人"换成了"响亮拒绝",那个取舍是对的。但代价没算完:
// 拒绝发生的位置在 identity Insert 撞 1062 之后,而 IssueSession(CreateUser=true)
// 排在 Insert **之前**(api.go / exchange_complete.go)。于是每一次登录尝试:
//
//	QueryIdentityExact → (nil,nil)   折叠行看不见
//	ResolveOrLink       → IsNew=true
//	IssueSession        → **用户行已写入**
//	store.Insert        → 1062
//	recoverFromIdentityRace → 仍走逐字节查询 → 看不见 → nil → 拒绝
//
// 净效果是**每次尝试留一个孤立 user 行,然后永久登录失败**,而且可以无限重复
// (callback 只有全局 IP 底)。identity 行倒是被 ci 唯一键挡住了 —— 所以早前
// brief 里"拿到新账号 + 新 identity 行"那句描述也是错的。
//
// 修法:让"折叠行存在"与"无行"成为两个可区分的结果,在 ResolveOrLink 里
// 就拒掉,不让 IsNew=true 传到调用方手上。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// foldedCollisionStore 建模生产行为:ci 查询命中一行,但逐字节复核不通过。
type foldedCollisionStore struct {
	inserted []*IdentityModel
}

func (f *foldedCollisionStore) Get(issuer, subject string) (*IdentityModel, error) {
	// 库里存的是小写;来的是别的大小写 —— 与 DB.QueryIdentityExact 的处境一致。
	stored := &IdentityModel{UID: "u-existing", Issuer: issuer, Subject: "abc123"}
	if identityRowMatches(stored, issuer, subject) {
		return stored, nil
	}
	return nil, ErrIdentityCaseCollision
}

func (f *foldedCollisionStore) Insert(m *IdentityModel) error {
	f.inserted = append(f.inserted, m)
	return nil
}

func (f *foldedCollisionStore) UpdateLogin(int64, string, int, string, int) error { return nil }

// ResolveOrLink 绝不能在折叠碰撞时回 IsNew=true —— 那是调用方建号的开关。
func TestResolveOrLink_RefusesCaseFoldedCollisionBeforeAnySideEffect(t *testing.T) {
	store := &foldedCollisionStore{}
	users := &fakeUserLookup{loginResp: &IssueSessionResp{UID: "u-new", LoginRespJSON: `{}`}}
	svc := newService(ProviderConfig{
		ID: "oidc", Issuer: "https://idp.example.com", AllowNewUser: true,
	}, store, users)

	res, err := svc.ResolveOrLink(context.Background(), &IdentityClaims{
		Issuer: "https://idp.example.com", Subject: "ABC123", // 库里是 abc123
	})

	if err == nil {
		t.Fatalf("a case-folded collision was not refused; got %+v. IsNew=true would then "+
			"make the caller run IssueSession(CreateUser=true) and leave an orphan user row "+
			"on every attempt, because the identity Insert that follows is blocked by the "+
			"case-insensitive unique key", res)
	}
	if !errors.Is(err, ErrIdentityCaseCollision) {
		t.Errorf("error must carry ErrIdentityCaseCollision so callers can tell it from a "+
			"transport failure; got %v", err)
	}
	if res != nil {
		t.Errorf("no result may be returned alongside the refusal: %+v", res)
	}
	if len(users.loginCalls) != 0 {
		t.Errorf("IssueSession ran %d time(s) — the whole point is to refuse before any "+
			"account is created", len(users.loginCalls))
	}
	if len(store.inserted) != 0 {
		t.Errorf("an identity row was written: %+v", store.inserted)
	}
}

// 同样的 subject 精确命中时必须照常绑定 —— 别把正常登录一起拒了。
func TestResolveOrLink_ExactMatchStillResolves(t *testing.T) {
	store := &foldedCollisionStore{}
	users := &fakeUserLookup{loginResp: &IssueSessionResp{UID: "u-existing", LoginRespJSON: `{}`}}
	svc := newService(ProviderConfig{
		ID: "oidc", Issuer: "https://idp.example.com", AllowNewUser: true,
	}, store, users)

	res, err := svc.ResolveOrLink(context.Background(), &IdentityClaims{
		Issuer: "https://idp.example.com", Subject: "abc123",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	if res.UID != "u-existing" || res.IsNew {
		t.Errorf("an exact match must resolve to the bound account: %+v", res)
	}
}

// **生产函数**必须把折叠碰撞报成一个可区分的结果。
//
// 上面那个 fake 只能表达"修好之后的契约";它自己返回错误,所以它证明不了
// QueryIdentityExact 真的这么做。这条用真库:插一行小写,用别的大小写去查。
// ci collation 下 SQL 会命中它,而复核会否掉 —— 关键在于**否掉之后返回什么**。
func TestQueryIdentityExact_ReportsCaseFoldedCollisionDistinctly_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-existing", Issuer: "https://idp.example.com", Subject: "abc123",
		LinkedAt: time.Now(),
	}))

	// 先确认 ci 查询确实命中了 —— 否则这条用例什么都没测。
	raw, rerr := d.queryIdentityByIssuerSubject("https://idp.example.com", "ABC123")
	require.NoError(t, rerr)
	require.NotNil(t, raw,
		"the ci collation must match here, otherwise this test does not exercise the recheck")

	got, err := d.QueryIdentityExact("https://idp.example.com", "ABC123")
	require.Nil(t, got, "a folded row must never be returned as a match")
	if err == nil {
		t.Fatal("a folded collision was reported as 'no row'. ResolveOrLink then returns " +
			"IsNew=true, the caller runs IssueSession(CreateUser=true) and writes a user row, " +
			"and only then does the identity Insert hit 1062 on the case-insensitive unique " +
			"key — so every login attempt leaves an orphan user and fails, indefinitely")
	}
	if !errors.Is(err, ErrIdentityCaseCollision) {
		t.Errorf("the collision must be distinguishable from a transport error; got %v", err)
	}
}

// 精确命中在真库上照常返回行 —— 别把正常登录一起拒了。
func TestQueryIdentityExact_ExactMatchStillReturnsTheRow_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	require.NoError(t, d.InsertIdentity(&IdentityModel{
		UID: "u-existing", Issuer: "https://idp.example.com", Subject: "abc123",
		LinkedAt: time.Now(),
	}))

	got, err := d.QueryIdentityExact("https://idp.example.com", "abc123")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "u-existing", got.UID)
}

// 真的没有行时必须仍然是 (nil, nil) —— 那是"首次登录",不能和碰撞混为一谈,
// 否则每个新用户都登不进来。
func TestQueryIdentityExact_GenuinelyAbsentStaysNilNil_Integration(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	d := NewDB(ctx)

	got, err := d.QueryIdentityExact("https://idp.example.com", "never-seen")
	require.NoError(t, err, "a first-time login must not look like a collision")
	require.Nil(t, got)
}

// 碰撞绝不能被引流进自助绑定。
//
// 自助绑定的出口之一是 /bind/create —— **建一个新账号**。若碰撞走到那里,同一个
// 折叠 subject 就会得到第二个账号,把"拒绝合并"变成"制造分裂",比原缺陷更糟。
// ShouldHandle 只认 ErrUnknownUser / ErrConflictNeedManual,这条把它钉住。
func TestBindShouldHandle_DoesNotTakeOverACaseFoldedCollision(t *testing.T) {
	h := newBindHarness(t)
	claims := &IDTokenClaims{Issuer: "https://idp.example", Subject: "ABC123"}

	if h.svc.ShouldHandle(ErrIdentityCaseCollision, claims) {
		t.Error("a case-folded collision was routed into self-service bind, whose /bind/create " +
			"exit creates a new account — that would turn 'refuse to merge two people' into " +
			"'give one person a second account'")
	}
	// 对照:真正的未知用户仍然要被接管,否则自助绑定整条失效。
	if !h.svc.ShouldHandle(ErrUnknownUser, claims) {
		t.Error("ErrUnknownUser must still be taken over; otherwise self-service bind is dead")
	}
}
