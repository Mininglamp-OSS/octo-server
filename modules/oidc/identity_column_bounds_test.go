package oidc

// identity_column_bounds_test.go — 可存储守卫要盖住它所保护的那一行的**每一列**。
//
// 守卫现在只查 issuer/subject,而两个 inserter 写的是四列:
//
//	email  VARCHAR(255)
//	phone  VARCHAR(32)    ← 写的是**未归一化的原始** claims.PhoneNumber
//
// 超长的 email/phone 会重演已经被修过两次的那个形态:IssueSession 先建号,
// 之后 INSERT 失败(严格模式)或截断(非严格),而下一次登录会 autolink 到那个
// 孤立账号上、再一次失败同一个 INSERT —— 永久卡住。
//
// phone 那一列尤其容易踩:32 字节很窄,而写进去的是原始值而不是 normalizePhone
// 的产物,所以一个带很多分隔符或前缀的上游值就能超。

import (
	"strings"
	"testing"
)

func TestRequireStorableIdentity_BoundsEveryColumnItInsertsInto(t *testing.T) {
	base := func() *IDTokenClaims {
		return &IDTokenClaims{
			Issuer:  "https://idp.example.com",
			Subject: "823071756087671783",
		}
	}

	t.Run("email within the column is accepted", func(t *testing.T) {
		c := base()
		c.Email = strings.Repeat("a", identityEmailMaxLen-len("@e.com")) + "@e.com"
		if err := requireStorableIdentity(c); err != nil {
			t.Errorf("a storable email was refused: %v", err)
		}
	})

	t.Run("email over the column is refused", func(t *testing.T) {
		c := base()
		c.Email = strings.Repeat("a", identityEmailMaxLen+1)
		if err := requireStorableIdentity(c); err == nil {
			t.Errorf("an email of %d bytes was accepted; user_oidc_identity.email is "+
				"VARCHAR(%d), so IssueSession creates the account and the identity INSERT "+
				"then fails or truncates — and the next login autolinks onto that orphan and "+
				"fails the same INSERT again, permanently",
				len(c.Email), identityEmailMaxLen)
		}
	})

	t.Run("raw phone over the column is refused", func(t *testing.T) {
		c := base()
		// 未归一化的原始值 —— inserter 写的就是这个,不是 normalizePhone 的产物。
		c.PhoneNumber = "+86 " + strings.Repeat("1-", identityPhoneMaxLen)
		if err := requireStorableIdentity(c); err == nil {
			t.Errorf("a raw phone of %d bytes was accepted; the column is VARCHAR(%d) and "+
				"the inserters write claims.PhoneNumber verbatim, not the normalised value",
				len(c.PhoneNumber), identityPhoneMaxLen)
		}
	})

	t.Run("ordinary phone forms stay accepted", func(t *testing.T) {
		for _, p := range []string{"+8613812345678", "8613812345678", "+86 138-1234-5678", ""} {
			c := base()
			c.PhoneNumber = p
			if err := requireStorableIdentity(c); err != nil {
				t.Errorf("phone %q was refused: %v", p, err)
			}
		}
	})

	t.Run("errors carry lengths, never the value", func(t *testing.T) {
		c := base()
		c.Email = strings.Repeat("z", identityEmailMaxLen+9)
		err := requireStorableIdentity(c)
		if err == nil {
			t.Fatal("expected refusal")
		}
		if strings.Contains(err.Error(), c.Email) {
			t.Errorf("the email value leaked into the error: %v", err)
		}
	})
}
