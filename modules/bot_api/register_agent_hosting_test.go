package bot_api

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 说明：register 的**落库**与 HTTP 行为测试不在本包，而在 modules/botfather
// （agent_hosting_test.go）。原因是 robot 表的 agent_* 列由 botfather 模块的迁移
// 拥有，而本包的测试二进制不 link botfather 的 init，因此 NewTestServer 建出来的
// robot 表压根没有这些列（实测 1054 Unknown column）。测试放在拥有该 schema 的包里，
// 而不是给本包 blank import botfather 去把一堆无关迁移拉进所有 bot_api 测试。

// ============ 纯函数：白名单就是信任边界 ============

// TestNormalizeAgentHosting 钉住**形状**校验（不是取值枚举）。
//
// 取值刻意开放：托管方会增加，把取值做成服务端枚举等于每来一个托管方发一次版，
// 也会把每个 vendor 名硬编码进这个开源仓。所以 `<vendor>_hosted` 一律放行，
// 挡的是形状 —— 引号/尖括号/空格/控制字符/连字符/大写以外的东西。
//
// 这是**数据质量**约束，不是授权约束：任何持该 Bot bf_ token 的调用方都能声称
// octo_hosted，白名单同样挡不住（它校验"值在集合内"，不校验"你有资格声称"）。
func TestNormalizeAgentHosting(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		// —— 放行 ——
		{"self_hosted", "self_hosted", AgentHostingSelfHosted, true},
		{"octo_hosted", "octo_hosted", AgentHostingOctoHosted, true},
		{"第三方托管方无需改服务端即可自报", "vendor_hosted", "vendor_hosted", true},
		{"任意 vendor slug", "acme_corp_hosted", "acme_corp_hosted", true},
		{"带数字", "vendor2_hosted", "vendor2_hosted", true},
		{"大小写折叠", "Self_Hosted", AgentHostingSelfHosted, true},
		{"全大写折叠", "VENDOR_HOSTED", "vendor_hosted", true},
		{"首尾空格被 trim", "  self_hosted\t", AgentHostingSelfHosted, true},
		{"空串是合法的清空", "", "", true},
		{"纯空格等于清空", "   ", "", true},
		// 开放取值的**已知代价**：当初否掉 local/cloud 的理由（私有化部署下 cloud
		// 说错话）仍成立，但从此只是客户端命名约定，服务端不再强制。刻意不做黑名单：
		// 既然放弃了值域枚举，再留个半吊子黑名单两头不靠。
		{"cloud 现在合法（约定不再由服务端强制）", "cloud", "cloud", true},
		{"local 同上", "local", "local", true},
		// —— 拒绝：形状非法 ——
		{"内嵌空格", "self hosted", "", false},
		{"连字符（约定用下划线）", "self-hosted", "", false},
		{"数字开头", "1host", "", false},
		{"下划线开头", "_hosted", "", false},
		{"注入形状的串", `<script>alert(1)</script>`, "", false},
		{"含引号", `self_hosted"`, "", false},
		{"控制字符", "self\x00hosted", "", false},
		{"Unicode 混淆字符", "self_hosted\u200b", "", false},
		{"中文", "自运维", "", false},
		{"恰好列宽的合法 slug 放行", strings.Repeat("a", maxAgentHostingLen), strings.Repeat("a", maxAgentHostingLen), true},
		{"超列宽 1 字节：被长度门拦下", strings.Repeat("a", maxAgentHostingLen+1), "", false},
		{"超长串在 ToLower 之前就被拦下", strings.Repeat("x", 100000), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeAgentHosting(tc.in)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestAgentHostingBoundMatchesColumnWidth —— 校验上界必须**等于**列宽。
//
// 跨文件不变量：列宽在 modules/botfather/sql/20260903000001_botfather_agent_hosting.sql，
// 上界在本包。上界 > 列宽 ⇒ 过了校验的值写库 1406，而 agent_* 共用一条 UPDATE，
// 那次失败会连带挡掉同一请求里的 agent_platform/version/plugin_version
// （见 brief 的已知限制）；上界 < 列宽 ⇒ 白占列宽还拒掉本可存下的 vendor slug。
// 断言相等而非 <=，让两者只能一起改。
func TestAgentHostingBoundMatchesColumnWidth(t *testing.T) {
	raw, err := os.ReadFile("../botfather/sql/20260903000001_botfather_agent_hosting.sql")
	require.NoError(t, err)
	m := regexp.MustCompile("`agent_hosting`\\s+VARCHAR\\((\\d+)\\)").FindStringSubmatch(string(raw))
	require.Len(t, m, 2, "迁移里没找到 agent_hosting 的列宽声明")
	width, err := strconv.Atoi(m[1])
	require.NoError(t, err)

	assert.Equal(t, width, maxAgentHostingLen,
		"maxAgentHostingLen 必须与列宽严格相等 —— 大了会让过校验的值在写库时 1406 并连带挡住整组 agent_* 更新，小了则白拒本可存下的 vendor slug")
	for _, known := range []string{AgentHostingSelfHosted, AgentHostingOctoHosted} {
		normalized, ok := normalizeAgentHosting(known)
		assert.True(t, ok, "本项目自己用的取值 %q 必须通过校验", known)
		assert.Equal(t, known, normalized)
	}
}

// ============ 源码守卫 ============

// TestAppBotSchemaHasNoAgentColumns —— 钉住方案 A 的前提。
//
// App Bot 不支持 agent_* 是**显式决定**而非遗漏：app_bot 表没有这些列，
// 所以 registerAppBot 只打 Warn。谁哪天给 app_bot 加了列却没接上写入路径，
// 这条会红，提示他要么做完对称支持、要么别加列。
func TestAppBotSchemaHasNoAgentColumns(t *testing.T) {
	entries, err := os.ReadDir("../app_bot/sql")
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile("../app_bot/sql/" + e.Name())
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "agent_hosting",
			"app_bot 迁移出现了 agent_hosting —— 方案 A（App Bot 只 Warn 不落库）的前提被打破，"+
				"要么补齐 registerAppBot 的写入路径，要么撤回这一列（见 brief 的 App Bot 决策）")
	}
}

// TestAgentHostingNotExposedOnImUserFaces —— 托管形态是部署拓扑信息，刻意不下发给
// 「任何能看到这个 bot 的 IM 用户」。
//
// modules/user 那条路（GET /v1/users/:uid 的 extraMap、POST /v1/users/batch、
// GET /v1/channels/:id/:type）今天已经在下发 bot_agent_platform / _version /
// _plugin_version，受众包含外部 Space 成员。顺手把 hosting 也加上去很自然，
// 所以用源码断言拦住：真要下发，先过产品评审，再改这条测试。
func TestAgentHostingNotExposedOnImUserFaces(t *testing.T) {
	for _, f := range []string{"../user/service.go", "../user/1module.go"} {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "agent_hosting",
			f+" 出现了 agent_hosting —— IM 全员面刻意不下发托管形态（见 brief 的 load-bearing 与 out-of-scope）")
	}
}
