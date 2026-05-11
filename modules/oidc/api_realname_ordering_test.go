package oidc

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// YUJ-413 R5 Critical #1 — 源码契约锁:OIDC callback 必须在 upsert 成功后
// 调 patchLoginRespJSONWithRealname,再喂给 SetAuthcode。
//
// 这个测试不依赖 DB,CI 一定跑。如果有人把 patch 这一步删掉 / 把 SetAuthcode
// 移到 upsert 之前,这里会 fire,避免时序 bug 回归。

func TestCallback_UpsertThenPatchThenSetAuthcode_Ordering(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	// 三个关键调用点的 call site 位置必须按序:
	//   UpsertVerificationFromOIDC(...) → patchLoginRespJSONWithRealname(...) → SetAuthcode(..., sessResp.LoginRespJSON, ...)
	// 用精确的 call-site 前缀避开 interface decl / func decl 的误匹配。
	var upsertIdx, patchIdx int

	// SetAuthcode 和 sessResp.LoginRespJSON 在同一行调用的 call site
	// (不跨行,避开 authcodeWriter interface 声明的误匹配)。
	setAuthWithLoginJSON := regexp.MustCompile(`SetAuthcode\([^\n]*sessResp\.LoginRespJSON`)
	matchIdx := setAuthWithLoginJSON.FindStringIndex(body)
	require.NotNil(t, matchIdx, "SetAuthcode(..., sessResp.LoginRespJSON, ...) 调用必须存在")
	setAuthcodeIdx := matchIdx[0]

	// 同样,UpsertVerificationFromOIDC call site(不是 interface 方法签名) —
	// 锁成 `.UpsertVerificationFromOIDC(` 前缀有 `.`,即方法调用形式。
	upsertCallRe := regexp.MustCompile(`\.UpsertVerificationFromOIDC\(`)
	upsertCallIdx := upsertCallRe.FindStringIndex(body)
	require.NotNil(t, upsertCallIdx, "o.verification.UpsertVerificationFromOIDC 调用必须存在")
	upsertIdx = upsertCallIdx[0]

	// patchLoginRespJSONWithRealname call site(不是 func decl) — 前缀有 `:= `
	// 或 `= `,即赋值调用形式。
	patchCallRe := regexp.MustCompile(`=\s*patchLoginRespJSONWithRealname\(`)
	patchCallIdx := patchCallRe.FindStringIndex(body)
	require.NotNil(t, patchCallIdx, "patchLoginRespJSONWithRealname 调用 call site 必须存在")
	patchIdx = patchCallIdx[0]

	assert.Less(t, upsertIdx, patchIdx,
		"patchLoginRespJSONWithRealname 必须在 UpsertVerificationFromOIDC 之后调用（时序 #1 → #2），否则 patch 的是未写入的数据")
	assert.Less(t, patchIdx, setAuthcodeIdx,
		"SetAuthcode(sessResp.LoginRespJSON) 必须在 patchLoginRespJSONWithRealname 之后（时序 #2 → #3），否则缓存的是 stale JSON —— 违反 fresh login 实名契约")
}

// TestCallback_PatchOnlyOnUpsertSuccess_SourceGrep 锁定 patch 调用是在
// upsert 的 else 分支里(即 upsert 成功路径),而不是无条件执行 —— 避免
// upsert 失败时仍改 JSON,导致 DB 和 LoginRespJSON 不一致。
func TestCallback_PatchOnlyOnUpsertSuccess_SourceGrep(t *testing.T) {
	src, err := os.ReadFile("api.go")
	require.NoError(t, err)
	body := string(src)

	// 匹配:UpsertVerificationFromOIDC 行起到 patchLoginRespJSONWithRealname 行结束的文本窗口
	// 里必须出现 `} else {` —— 表示 patch 在 upsert 的成功分支里。
	re := regexp.MustCompile(`(?s)UpsertVerificationFromOIDC\([^)]+\).*?\}\s*else\s*\{[^}]*?patchLoginRespJSONWithRealname\(`)
	assert.Regexp(t, re, body,
		"patchLoginRespJSONWithRealname 必须在 UpsertVerificationFromOIDC 失败分支 (} else {) 之外调用；即 upsert 成功才 patch。直接无条件 patch 会在 upsert 失败时污染 JSON。")
}
