package user

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// YUJ-413 R5 Blocking #2 — loginWithAuthCode 必须下发三个实名字段。
//
// loginWithAuthCode 走手写 map response,不经过 newLoginUserDetailResp。
// 之前完全没有实名字段,扫码登录进来的客户端永远拿不到 self 实名态。
//
// 源码契约锁(无 DB 依赖,CI 一定跑):
// 1. loginWithAuthCode handler 返回前必须调 u.applyRealnameToAuthCodeMap(...)
// 2. applyRealnameToAuthCodeMap 必须一律写 realname_verified(保留三态 key),
//    已实名才加 real_name / realname_verified_at(omitempty 语义)
// 3. 查询失败只 warn 不 return err,不阻断登录

func TestLoginWithAuthCode_CallsApplyRealnameToAuthCodeMap(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	// 定位到 loginWithAuthCode handler 函数体
	fnStart := strings.Index(body, "func (u *User) loginWithAuthCode(")
	require.NotEqual(t, -1, fnStart, "loginWithAuthCode handler 必须存在")
	fnEnd := strings.Index(body[fnStart+len("func (u *User) loginWithAuthCode("):], "\nfunc ")
	require.NotEqual(t, -1, fnEnd)
	fnBody := body[fnStart : fnStart+len("func (u *User) loginWithAuthCode(")+fnEnd]

	// 必须调 applyRealnameToAuthCodeMap。
	assert.Contains(t, fnBody, "u.applyRealnameToAuthCodeMap(",
		"loginWithAuthCode 必须调 applyRealnameToAuthCodeMap 补实名字段(YUJ-413 R5 Blocking #2)")

	// 不能漏掉 c.Response(resp) 把 resp 下发。
	assert.Regexp(t,
		regexp.MustCompile(`c\.Response\(resp\)`),
		fnBody,
		"loginWithAuthCode 必须 c.Response(resp) 下发加了实名字段的 map")
}

// TestApplyRealnameToAuthCodeMap_AlwaysSetsVerifiedKey 源码级锁:未实名
// 路径也必须写 realname_verified=false(三态语义,key 必存在)。
func TestApplyRealnameToAuthCodeMap_AlwaysSetsVerifiedKey(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	fnStart := strings.Index(body, "func (u *User) applyRealnameToAuthCodeMap(")
	require.NotEqual(t, -1, fnStart, "applyRealnameToAuthCodeMap helper 必须存在(YUJ-413 R5 Blocking #2)")
	fnEnd := strings.Index(body[fnStart:], "\nfunc ")
	require.NotEqual(t, -1, fnEnd)
	helperBody := body[fnStart : fnStart+fnEnd]

	// 默认写 false,保证即使 DB 查询失败也有 sentinel。
	assert.Regexp(t,
		regexp.MustCompile(`m\["realname_verified"\]\s*=\s*false`),
		helperBody,
		"applyRealnameToAuthCodeMap 必须在查询前先写 realname_verified=false,保证 key 一定存在")

	// 已实名分支必须写 true。
	assert.Regexp(t,
		regexp.MustCompile(`m\["realname_verified"\]\s*=\s*true`),
		helperBody,
		"applyRealnameToAuthCodeMap 已实名分支必须写 realname_verified=true")

	// real_name 只在非空时写(omitempty 语义)。
	assert.Regexp(t,
		regexp.MustCompile(`if\s+vr\.RealName\s*!=\s*""`),
		helperBody,
		"real_name 必须判 RealName 非空才写,对齐 loginUserDetailResp omitempty 语义")
}

// TestApplyRealnameToAuthCodeMap_NilMapGuard 空输入不 panic。
func TestApplyRealnameToAuthCodeMap_NilMapGuard(t *testing.T) {
	// 直接调 helper,nil/空 uid 必须安全返回。需要构造 *User 但只读取
	// verificationDB 字段 —— 这里零值 *User 的 verificationDB 为 nil,
	// 如果 guard 失效调用到 nil.QueryByUID 会 panic。
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	fnStart := strings.Index(body, "func (u *User) applyRealnameToAuthCodeMap(")
	require.NotEqual(t, -1, fnStart)
	fnEnd := strings.Index(body[fnStart:], "\nfunc ")
	helperBody := body[fnStart : fnStart+fnEnd]

	assert.Regexp(t,
		regexp.MustCompile(`if\s+m\s*==\s*nil\s*\|\|\s*uid\s*==\s*""`),
		helperBody,
		"applyRealnameToAuthCodeMap 必须有 nil map / 空 uid guard,否则查询分支会 nil-deref panic")
}
