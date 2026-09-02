package oidc

// phone_autolink_behaviour_test.go — 把新手机号解析对**自动绑号**的影响钉死。
//
// 这条是本改动对存量 kind=oidc 部署的行为变更,而变更清单里原本没有它。
//
// callback 走的就是 extractZone/extractPhone(api.go),而它们现在委派 normalizePhone。
// AutoLinkByPhone 默认 true,条件是 `AutoLinkByPhone && PhoneNumber != "" && PhoneVerified`。
// 于是:
//
//   - **放宽**:IdP 返回 `8613…` / `008613…` / 带分隔符的形态,以前解不出号码、
//     autolink 不触发、首登建新账号;现在解得出 → **绑进已有账号**。那是一次
//     不可逆的身份写入,方向和"新建账号"完全相反。
//   - **收紧**:`+86` 后接固话(如 `+862112345678`)以前会解出 `2112345678`,
//     现在返空 —— 以前能绑上的现在绑不上。
//
// 之前只有 phone_normalize_test.go 单测这个函数,没有任何测试把它接到 ResolveOrLink,
// 所以"谁拥有哪个账号"这件事的变化没有任何东西钉住。这个文件补上。
//
// 这里不改行为:按已验证手机号绑号本来就是 AutoLinkByPhone 的语义,放宽后的
// 结果更接近意图。要钉的是"它确实会这样",以及让下一个人改解析器时看到代价。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// autolinkPhoneFixture 一个只按手机号绑号的 Service:库里已有一个持该号码的账号。
func autolinkPhoneFixture(t *testing.T, existingUID string) (*Service, *fakeIdentityStore) {
	t.Helper()
	store := &fakeIdentityStore{bindings: map[string]*IdentityModel{}}
	users := &fakeUserLookup{
		usersByPhone: map[string][]string{"0086|13812345678": {existingUID}},
		loginResp:    &IssueSessionResp{UID: existingUID, LoginRespJSON: `{}`},
	}
	cfg := ProviderConfig{
		ID: "oidc", Issuer: "https://idp.example.com",
		AutoLinkByPhone: true, AllowNewUser: true,
	}
	return newService(cfg, store, users), store
}

// 各种上游形态 → 是否绑进已有账号。
func TestAutoLinkByPhone_UpstreamPhoneFormsThatNowLink(t *testing.T) {
	cases := map[string]struct {
		claimPhone string
		wantLinked bool
		note       string
	}{
		"plus-86 mobile (linked before this change too)": {
			"+8613812345678", true, "baseline behaviour, unchanged"},
		"bare 86 prefix": {
			"8613812345678", true,
			"NEW: previously yielded no phone, so autolink did not fire and a first SSO " +
				"login created a second account instead of linking"},
		"0086 prefix": {
			"008613812345678", true, "NEW: same as above"},
		"plus-86 with separators": {
			"+86 138-1234-5678", true, "NEW: separators are now stripped"},
		"plus-86 landline": {
			"+862112345678", false,
			"STRICTER: previously yielded 2112345678 and could link; now refused because " +
				"the body is not a mainland mobile number"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			svc, _ := autolinkPhoneFixture(t, "u-existing")
			res, err := svc.ResolveOrLink(context.Background(), &IdentityClaims{
				Issuer: "https://idp.example.com", Subject: "100000000000000001",
				PhoneNumber: c.claimPhone, PhoneVerified: true,
			})
			require.NoError(t, err)

			linked := res.UID == "u-existing" && !res.IsNew
			if linked != c.wantLinked {
				t.Errorf("phone %q: linked=%v want %v (uid=%q isNew=%v). %s",
					c.claimPhone, linked, c.wantLinked, res.UID, res.IsNew, c.note)
			}
		})
	}
}

// 未验证的手机号绝不触发绑号 —— 放宽解析不能顺带放宽准入。
func TestAutoLinkByPhone_UnverifiedPhoneNeverLinks(t *testing.T) {
	svc, _ := autolinkPhoneFixture(t, "u-existing")
	res, err := svc.ResolveOrLink(context.Background(), &IdentityClaims{
		Issuer: "https://idp.example.com", Subject: "100000000000000002",
		PhoneNumber: "8613812345678", PhoneVerified: false,
	})
	require.NoError(t, err)
	if res.UID == "u-existing" {
		t.Error("an unverified phone linked into an existing account; widening the parser " +
			"must not widen the admission condition")
	}
}
