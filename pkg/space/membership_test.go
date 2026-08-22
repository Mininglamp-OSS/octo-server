package space

import (
	"os"
	"strings"
	"testing"
)

func TestCheckMembershipEmptyArgs(t *testing.T) {
	ok, err := CheckMembership(nil, "", "uid1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty spaceID")
	}

	ok, err = CheckMembership(nil, "space1", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty uid")
	}
}

func TestCheckBothMembersEmptyArgs(t *testing.T) {
	ok, err := CheckBothMembers(nil, "", "uid1", "uid2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty spaceID")
	}

	ok, err = CheckBothMembers(nil, "space1", "", "uid2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty uid1")
	}
}

func TestHaveCommonSpaceEmptyArgs(t *testing.T) {
	tests := []struct {
		name string
		uid1 string
		uid2 string
	}{
		{"both_empty", "", ""},
		{"uid1_empty", "", "u2"},
		{"uid2_empty", "u1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := HaveCommonSpace(nil, tt.uid1, tt.uid2)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Errorf("expected false for %s", tt.name)
			}
		})
	}
}

func TestCheckMembershipForCleanupEmptyArgs(t *testing.T) {
	ok, err := CheckMembershipForCleanup(nil, "", "uid1")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty spaceID")
	}

	ok, err = CheckMembershipForCleanup(nil, "space1", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected false for empty uid")
	}
}

// TestCheckMembershipStaysStrict 是一道源码守卫。
//
// Mininglamp-OSS/octo-server#797 一度提出把 CheckMembership 放宽成
// `space.status <> 0`，好让清理管线不再误伤被封禁的空间。那个改法是个安全回归：
// CheckMembership 有 37 个非测试调用点，其中包括 pkg/space/middleware.go 里的
// SpaceMiddleware —— 整个鉴权 API 面的主门。放宽它等于让被封禁的空间在所有
// 已鉴权接口上通行。
//
// 清理管线要的是另一个谓词（CheckMembershipForCleanup），不是放宽这一个。
// 这条测试钉住两者的差异，避免下一个人顺手把它们合并回去。
func TestCheckMembershipStaysStrict(t *testing.T) {
	src, err := os.ReadFile("membership.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	authFn := funcBody(t, body, "func CheckMembership(")
	if !strings.Contains(authFn, "s.status = 1") {
		t.Error("CheckMembership 必须继续要求 space.status = 1：它是 SpaceMiddleware 的鉴权谓词")
	}
	if strings.Contains(authFn, "s.status <> 0") {
		t.Error("CheckMembership 不得放宽到 status <> 0：那会让被封禁的空间通过鉴权门")
	}

	cleanupFn := funcBody(t, body, "func CheckMembershipForCleanup(")
	if !strings.Contains(cleanupFn, "s.status <> 0") {
		t.Error("CheckMembershipForCleanup 必须用 status <> 0：被封禁空间里的成员仍持有席位")
	}
}

// funcBody 截出从 signature 开始到下一个顶层 `\n}` 为止的函数体。
func funcBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("找不到函数 %q", signature)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("函数 %q 没有结束大括号", signature)
	}
	return rest[:end]
}
