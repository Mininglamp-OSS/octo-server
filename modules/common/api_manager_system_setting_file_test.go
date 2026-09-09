package common

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

// ---------------------------------------------------------------------------
// 管理台写侧测试（task file-extension-policy-dynamic-config）。
//
// 复用 thread_archive_setting_test.go 的 newSuperAdminServer / postSystemSetting
// —— 它们已经处理了两件必须做的事：出口清理进程级 SystemSettings 单例
// （管理 handler 写的是单例，留下的行会泄漏给后续用例），以及注入 i18n
// ErrorRenderer（否则断言不到 error.code，断的是兜底 renderer）。
//
// 依赖 MySQL + Redis + WuKongIM（testutil.NewTestServer）。
// ---------------------------------------------------------------------------

// file.max_size_kb 的默认值是 102400，会撞上 settingTypeInt 默认的
// [settingIntMin, settingIntMax] = [0, 3650] 上界 —— 只能靠 schema 里的
// Positive:true 跳过。这条用例守住那个标志：漏掉它运维连默认值都写不进去。
func TestManagerSystemSetting_FileMaxSizeAcceptsLargeValue(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"max_size_kb","value":"102400"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// D4：响应回带本实例生效状态，同时保留 status 字段让老管理台前端原样工作。
func TestManagerSystemSetting_ResponseCarriesAppliedFlag(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_blocked_extensions","value":"svg"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.EqualValues(t, http.StatusOK, body["status"],
		"必须保留 status 字段，老管理台前端依赖它")
	applied, ok := body["applied"].(bool)
	require.True(t, ok, "响应必须回带 applied 生效状态：%s", w.Body.String())
	assert.True(t, applied, "reload 正常时 applied 应为 true")
}

// D6：全局上限低于贴纸上限的组合必须在写侧被拒。两个键各自看都完全合法，
// 一旦提交，贴纸会永远传不上去且没有任何报错 —— 只有这里能拦住。
func TestManagerSystemSetting_FileSizeOrderingRejectsFromFileSide(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	// sticker.upload_max_size_kb 默认 1024KB；把全局上限压到 512KB 即冲突。
	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"max_size_kb","value":"512"}
	]}`)
	require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "file_upload_size_ordering",
		"必须返回注册过的 i18n 错误码，而不是裸错误")
}

// 同样要挡住「从 sticker 那一侧把上限抬过全局上限」。只在 batch 碰到 file.* 时
// 校验会留下这条绕过路径 —— 与 thread ordering guard 的双入口理由相同。
func TestManagerSystemSetting_FileSizeOrderingRejectsFromStickerSide(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"max_size_kb","value":"2048"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = postSystemSetting(t, route, `{"items":[
		{"category":"sticker","key":"upload_max_size_kb","value":"4096"}
	]}`)
	assert.NotEqual(t, http.StatusOK, w.Code,
		"从 sticker 侧也必须拦住，否则约束可被绕过")
}

// 同一批次同时提交两个键时按 merge 后的组合校验：合法组合放行，
// 这正是被拒后运维的修复动作。
func TestManagerSystemSetting_FileSizeOrderingAcceptsConsistentBatch(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"max_size_kb","value":"4096"},
		{"category":"sticker","key":"upload_max_size_kb","value":"2048"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// 顺序无关：运维不该背负「先调哪个」的隐式知识。
	w = postSystemSetting(t, route, `{"items":[
		{"category":"sticker","key":"upload_max_size_kb","value":"1024"},
		{"category":"file","key":"max_size_kb","value":"8192"}
	]}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// 写入后 GET 的 effective_value 必须是清洗后的有效值，否则运维看不出自己写的
// 值到底生效没有（例如 "SVG" 会被规范成 ".svg"）。
func TestManagerSystemSetting_FileKeysSurfaceEffectiveValue(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_blocked_extensions","value":" SVG , dwg "}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/manager/common/system_setting", nil)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, "extra_blocked_extensions")
	assert.Contains(t, body, "extra_allowed_extensions")
	assert.Contains(t, body, "max_size_kb")
	assert.Contains(t, body, ".dwg", "effective_value 必须是清洗后的有效值（含前导点、小写）")
	assert.Contains(t, body, ".svg")
}

// 未注册的 (category, key) 仍然要被拒 —— schema 白名单是写侧第一道门。
func TestManagerSystemSetting_RejectsUnknownFileKey(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"no_such_key","value":"1"}
	]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// 封堵键写入后必须真的改变读侧判定（本实例 reload 成功的路径）。
func TestManagerSystemSetting_BlockedExtensionTakesEffectOnRead(t *testing.T) {
	route, ctx := newSuperAdminServer(t)

	settings := EnsureSystemSettings(ctx)
	require.NotContains(t, settings.FileExtraBlockedExtensions(), ".pdf")

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_blocked_extensions","value":"pdf"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Contains(t, settings.FileExtraBlockedExtensions(), ".pdf",
		"写入提交且 reload 成功后，本实例必须立即看到封堵")
}

// ----- 审计脱敏 -----

// 审计要回答的是「谁在什么时候改了哪个键」，不是把密文/明文抄进日志；
// 但「未配置 → 已配置」这个状态变化本身对审计有意义，所以空值保持空。
func TestMaskIfSet(t *testing.T) {
	assert.Equal(t, "", maskIfSet(""))
	assert.Equal(t, secretMask, maskIfSet("super-secret"))
	assert.Equal(t, secretMask, maskIfSet("any-ciphertext"))
}

// 审计字段：字段名与脱敏行为是安全相关输出，钉死它们。
func TestSettingAuditFields(t *testing.T) {
	fields := settingAuditFields(
		settingAuditEntry{key: "file.extra_blocked_extensions", before: "", after: ".svg"},
		"10000", "trace-abc", false,
	)
	got := map[string]interface{}{}
	for _, f := range fields {
		switch f.Type {
		case zapcore.StringType:
			got[f.Key] = f.String
		case zapcore.BoolType:
			got[f.Key] = f.Integer == 1
		}
	}
	assert.Equal(t, "trace-abc", got["trace_id"])
	assert.Equal(t, "10000", got["operator"], "审计必须记录操作者")
	assert.Equal(t, "file.extra_blocked_extensions", got["setting"])
	assert.Equal(t, "", got["before"])
	assert.Equal(t, ".svg", got["after"])
	assert.Equal(t, false, got["applied_on_this_instance"],
		"reload 失败时审计要能说明改动当时未在本实例生效")
}

// 加密类型的前后值一律脱敏后再进审计，密文与明文都不得落日志。
func TestSettingAuditFields_EncryptedValuesAreMasked(t *testing.T) {
	fields := settingAuditFields(
		settingAuditEntry{key: "support.email_pwd", before: maskIfSet("old-cipher"), after: maskIfSet("new-cipher")},
		"10000", "trace-abc", true,
	)
	for _, f := range fields {
		if f.Key == "before" || f.Key == "after" {
			assert.Equal(t, secretMask, f.String)
		}
		assert.NotContains(t, f.String, "cipher")
	}
}

// 把内置黑名单项写进 extra_allowed 必须当场被拒。
//
// 那张黑名单不可通过配置撤销，所以这种写入注定不生效：值存进去了、
// effective_value 不显示它、上传照样被拒，而没有任何地方解释原因。
// 写侧拒绝是唯一能让操作者当场知道的位置。
func TestManagerSystemSetting_RejectsAllowlistingBuiltinBlockedExtension(t *testing.T) {
	route, _ := newSuperAdminServer(t)
	withBuiltinBlockedProbe(t, ".exe", ".php")

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_allowed_extensions","value":"dwg,exe"}
	]}`)
	require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "file_extension_not_allowlistable")
	assert.Contains(t, w.Body.String(), ".exe", "错误详情要指出是哪个扩展名")
}

// 放开一个普通扩展名必须成功 —— 这正是本任务要交付的能力：不重启、不发版。
func TestManagerSystemSetting_AllowsArbitraryExtensionWithoutRedeploy(t *testing.T) {
	route, ctx := newSuperAdminServer(t)
	withBuiltinBlockedProbe(t, ".exe", ".php")

	settings := EnsureSystemSettings(ctx)
	require.NotContains(t, settings.FileExtraAllowedExtensions(), ".dwg")

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_allowed_extensions","value":"dwg,psd"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, []string{".dwg", ".psd"}, settings.FileExtraAllowedExtensions(),
		"写入提交且 reload 成功后，本实例必须立即看到放开")

	// 并集语义（D9）：再加一项时，先前放开的不能失效。这正是覆盖语义会踩的坑 ——
	// 运维只填新增的那个，原有格式当场作废，用户立刻传不了。
	w = postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_allowed_extensions","value":"dwg,psd,step"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{".dwg", ".psd", ".step"}, settings.FileExtraAllowedExtensions())
}

// ----- M1：写侧的条数 / 单项长度上限 -----

// 超量的扩展名清单必须在写侧被拒。system_setting.value 是 TEXT(64KB)，
// 而这份清单会原样下发到无鉴权的 /v1/common/appconfig —— 一次误配就能把公开
// 端点的响应撑到数十 KB。
func TestManagerSystemSetting_RejectsOversizedExtensionList(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	many := make([]string, 100)
	for i := range many {
		many[i] = fmt.Sprintf("ext%d", i)
	}
	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_allowed_extensions","value":"`+strings.Join(many, ",")+`"}
	]}`)
	require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "extension_list_too_large")
}

// 单项超长同样拒绝。
func TestManagerSystemSetting_RejectsOverlongExtension(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_blocked_extensions","value":"`+strings.Repeat("a", 40)+`"}
	]}`)
	require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "extension_list_too_large")
}

// 上限之内的正常写入不受影响。
func TestManagerSystemSetting_AcceptsListWithinBounds(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	many := make([]string, fileExtensionListMaxEntries)
	for i := range many {
		many[i] = fmt.Sprintf("ext%d", i)
	}
	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_allowed_extensions","value":"`+strings.Join(many, ",")+`"}
	]}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// P1 回归：错误消息必须渲染出真实上限，而不是 "<no value>"。
//
// 之前两个上限只走 details、params 传 nil，而消息模板吃的是 params ——
// go-i18n 拿到 nil TemplateData，text/template 把 {{.max_entries}} 解析成零值
// 并渲染成 "<no value>"，运维看到一条断串。原来的用例只断言了错误码
// （"extension_list_too_large"），所以全绿。断错误码不等于断用户看到的东西。
func TestManagerSystemSetting_ExtensionListErrorRendersLimits(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	many := make([]string, 100)
	for i := range many {
		many[i] = fmt.Sprintf("ext%d", i)
	}
	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_allowed_extensions","value":"`+strings.Join(many, ",")+`"}
	]}`)
	require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())

	body := w.Body.String()
	assert.NotContains(t, body, "<no value>", "模板变量必须经 params 渲染：%s", body)
	assert.NotContains(t, body, "{{.", "不得把模板原样漏给用户：%s", body)
	assert.Contains(t, body, strconv.Itoa(fileExtensionListMaxEntries), "消息里要给出条数上限")
	assert.Contains(t, body, strconv.Itoa(fileExtensionMaxLength), "消息里要给出单项长度上限")
}

// 原始串超长必须被拒 —— 条数与单项长度只看解析后的结果，而入库并被反复 split
// 的是原始串；几万个畸形 token 解析出 0 项，能过条数检查。
func TestManagerSystemSetting_RejectsOversizedRawValue(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	// 全是非法 token（含路径分隔符），解析后 0 项，但原始串远超字节上限。
	junk := strings.Repeat("a/b,", 3000)
	w := postSystemSetting(t, route, `{"items":[
		{"category":"file","key":"extra_allowed_extensions","value":"`+junk+`"}
	]}`)
	require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	// 单独的 code：超的是原始长度，不是条数/单项字符数。
	assert.Contains(t, w.Body.String(), "extension_list_too_long")
	assert.NotContains(t, w.Body.String(), "<no value>")
	assert.Contains(t, w.Body.String(), strconv.Itoa(fileExtensionListMaxBytes),
		"消息里要给出字节上限")
}
