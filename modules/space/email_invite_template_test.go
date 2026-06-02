package space

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEmailInviteAcceptURL(t *testing.T) {
	t.Run("无尾斜杠", func(t *testing.T) {
		got := emailInviteAcceptURL("https://h5.example.com", "abc", "")
		assert.Equal(t, "https://h5.example.com/v1/space/email-invite?token=abc", got)
	})
	t.Run("有尾斜杠", func(t *testing.T) {
		got := emailInviteAcceptURL("https://h5.example.com/", "abc", "")
		assert.Equal(t, "https://h5.example.com/v1/space/email-invite?token=abc", got)
	})
	t.Run("token 走 URL 转义（防止 ?/& 截断查询串）", func(t *testing.T) {
		got := emailInviteAcceptURL("https://h5.example.com", "a/b?c=d", "")
		assert.Contains(t, got, "token=a%2Fb%3Fc%3Dd")
	})
	t.Run("lang 非空时追加 &lang=", func(t *testing.T) {
		got := emailInviteAcceptURL("https://h5.example.com", "abc", "en-US")
		assert.Equal(t, "https://h5.example.com/v1/space/email-invite?token=abc&lang=en-US", got)
	})
	t.Run("空 base 返回空串", func(t *testing.T) {
		assert.Equal(t, "", emailInviteAcceptURL("", "abc", "zh-CN"))
		assert.Equal(t, "", emailInviteAcceptURL("   ", "abc", "zh-CN"))
	})
}

func TestBuildOwnerInviteEmail_ContainsKeyFields(t *testing.T) {
	inv := &spaceEmailInviteModel{
		Email:              "to@example.com",
		PlannedName:        "我的团队",
		PlannedDescription: "做大事",
		InviteType:         EmailInviteTypeOwner,
	}
	link := "https://h5.example.com/space-email-invite.html?token=tok123"
	got, err := buildOwnerInviteEmail(inv, "Alice", "zh-CN", link)
	assert.NoError(t, err)

	assert.Contains(t, got.Subject, "我的团队")
	assert.Contains(t, got.HTML, "我的团队")
	assert.Contains(t, got.HTML, "Alice")
	assert.Contains(t, got.HTML, link)
	assert.Contains(t, got.HTML, "做大事")
	// plaintext 兜底部分也应带链接与空间名
	assert.Contains(t, got.Text, link)
	assert.Contains(t, got.Text, "我的团队")
}

func TestBuildOwnerInviteEmail_EnglishLang(t *testing.T) {
	inv := &spaceEmailInviteModel{
		PlannedName: "My Team",
		InviteType:  EmailInviteTypeOwner,
	}
	got, err := buildOwnerInviteEmail(inv, "Alice", "en-US", "https://h5.example.com/x?token=t")
	assert.NoError(t, err)
	assert.Contains(t, got.Subject, "My Team")
	assert.Contains(t, got.HTML, "invites you to create")
}

func TestBuildOwnerInviteEmail_EscapesHTML(t *testing.T) {
	inv := &spaceEmailInviteModel{
		PlannedName:        "<script>alert(1)</script>",
		PlannedDescription: "<img onerror=x>",
		InviteType:         EmailInviteTypeOwner,
	}
	got, err := buildOwnerInviteEmail(inv, "B<o>b", "zh-CN", "https://h5.example.com/x?token=t")
	assert.NoError(t, err)

	// 危险标签必须被转义，绝不出现在 HTML 原文中
	assert.NotContains(t, got.HTML, "<script>alert(1)</script>")
	assert.NotContains(t, got.HTML, "<img onerror=x>")
	assert.Contains(t, got.HTML, "&lt;script&gt;")
	assert.NotContains(t, got.HTML, "B<o>b")
}

func TestBuildOwnerInviteEmail_AnonymousInviterLocalized(t *testing.T) {
	inv := &spaceEmailInviteModel{PlannedName: "X", InviteType: EmailInviteTypeOwner}
	zh, err := buildOwnerInviteEmail(inv, "", "zh-CN", "https://h5.example.com/?token=t")
	assert.NoError(t, err)
	assert.Contains(t, zh.HTML, "Octo 管理员")

	en, err := buildOwnerInviteEmail(inv, "", "en-US", "https://h5.example.com/?token=t")
	assert.NoError(t, err)
	assert.Contains(t, en.HTML, "An Octo admin")
}

func TestBuildMemberInviteEmail_RoleLabel(t *testing.T) {
	link := "https://h5.example.com/?token=t"

	t.Run("普通成员", func(t *testing.T) {
		inv := &spaceEmailInviteModel{Role: EmailInviteRoleMember, InviteType: EmailInviteTypeMember}
		got, err := buildMemberInviteEmail(inv, "Alice", "Acme", "zh-CN", link)
		assert.NoError(t, err)
		assert.Contains(t, got.Subject, "Acme")
		assert.Contains(t, got.HTML, "Acme")
		assert.Contains(t, got.HTML, link)
		assert.Contains(t, got.HTML, "Alice")
		assert.True(t, strings.Contains(got.HTML, "成员") && !strings.Contains(got.HTML, "管理员"))
	})

	t.Run("管理员", func(t *testing.T) {
		inv := &spaceEmailInviteModel{Role: EmailInviteRoleAdmin, InviteType: EmailInviteTypeMember}
		got, err := buildMemberInviteEmail(inv, "Alice", "Acme", "zh-CN", link)
		assert.NoError(t, err)
		assert.Contains(t, got.HTML, "管理员")
	})

	t.Run("英文-管理员", func(t *testing.T) {
		inv := &spaceEmailInviteModel{Role: EmailInviteRoleAdmin, InviteType: EmailInviteTypeMember}
		got, err := buildMemberInviteEmail(inv, "Alice", "Acme", "en-US", link)
		assert.NoError(t, err)
		assert.Contains(t, got.HTML, "administrator")
	})
}

func TestBuildMemberInviteEmail_EscapesHTML(t *testing.T) {
	inv := &spaceEmailInviteModel{Role: EmailInviteRoleMember, InviteType: EmailInviteTypeMember}
	got, err := buildMemberInviteEmail(inv, "<img>", "<svg/onload=x>", "zh-CN", "https://h5.example.com/?token=t")
	assert.NoError(t, err)
	assert.NotContains(t, got.HTML, "<svg/onload=x>")
	assert.Contains(t, got.HTML, "&lt;svg")
}
