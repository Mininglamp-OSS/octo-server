package file

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 端到端：管理台写入 → 上传策略 + appconfig 同步变化。
//
// 这个文件的存在本身是一次回归教训。首版实现里 File.New() 忘了调
// SetPolicySettings()，整个动态配置在生产上是死的：管理台写入照常落库、照常
// 返回 200，但 currentPolicy() 一直走「未挂载」分支（env + baseline + 默认
// 100MB），没有任何上传入口会跟着变，也没有任何报错。
//
// 之所以没被测出来：当时**每一条**用例都用 useSettings() 手动注入 fake，
// 没有一条走过真实装配路径（module.Setup → New(ctx)）。手动注入的测试再多，
// 也证明不了生产上那根线接上了。
//
// 代价是本包从此依赖 MySQL/Redis/WuKongIM。接受 —— 上面那个 P0 说明这个代价
// 该付。其余用例仍是纯单测，只有本文件需要 infra。
// ---------------------------------------------------------------------------

func newPolicyIntegrationServer(t *testing.T) (*wkhttp.WKHttp, *common.SystemSettings) {
	t.Helper()
	return newPolicyIntegrationServerWithEnv(t, "", "")
}

// newPolicyIntegrationServerWithEnv 起一台带指定 DM_FILE_EXTRA_* 的测试服务器。
// 生产环境这两个 env 通常是有值的（历史上运维在 configmap 里放开过格式），
// 所以「env 有值 + 管理台再写一笔」才是真实形态，必须能整条链路跑通。
func newPolicyIntegrationServerWithEnv(t *testing.T, envAllowed, envBlocked string) (*wkhttp.WKHttp, *common.SystemSettings) {
	t.Helper()
	// common.Route 启动时会 insertAppConfigIfNeed，加密 RSA 私钥需要 master key。
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("DM_FILE_EXTRA_ALLOWED", envAllowed)
	t.Setenv("DM_FILE_EXTRA_BLOCKED", envBlocked)

	// 不要额外 CleanAllTables：NewTestServer 内部已经清过，且 common.Route
	// 随后插入了 app_config 行；再清一次 appconfig 会 400。
	s, ctx := testutil.NewTestServer()
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	settings := common.EnsureSystemSettings(ctx)
	require.NoError(t, settings.Reload())
	cached.Store(nil)

	// testutil.NewTestServer 不注入 i18n renderer，兜底 renderer 只输出
	// {msg,status} 且不做 params 插值。要断言用户实际看到的文案与 details，
	// 就必须装上与 main.go 同一个 renderer。
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer("zh-CN")))

	t.Cleanup(func() {
		// 管理 handler 写的是进程级 SystemSettings 单例，留下的 file.* 行会
		// 泄漏给后续用例。写空值即「未配置」。
		for _, key := range []string{"extra_blocked_extensions", "extra_allowed_extensions", "max_size_kb"} {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/v1/manager/common/system_setting",
				strings.NewReader(`{"items":[{"category":"file","key":"`+key+`","value":""}]}`))
			req.Header.Set("token", testutil.Token)
			req.Header.Set("Content-Type", "application/json")
			s.GetRoute().ServeHTTP(w, req)
		}
		_ = settings.Reload()
		cached.Store(nil)
	})
	return s.GetRoute(), settings
}

func writeSetting(t *testing.T, route *wkhttp.WKHttp, key, value string) {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/manager/common/system_setting",
		strings.NewReader(`{"items":[{"category":"file","key":"`+key+`","value":"`+value+`"}]}`))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// New(ctx) 必须把 SystemSettings 挂到包级策略上。这是 P0 的直接回归：
// 装配路径一断，管理台的所有写入都不生效。
func TestNewMountsPolicySettings(t *testing.T) {
	resetPolicyForTest(t)
	require.Nil(t, provider.Load(), "前置条件：未挂载")

	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	_ = New(ctx)

	p := provider.Load()
	require.NotNil(t, p, "New(ctx) 必须调用 SetPolicySettings，否则动态配置整体不生效")
	require.NotNil(t, p.settings)
}

// 管理台封堵一个扩展名 → 上传门立即拒绝，且 appconfig 下发清单同步收窄。
// 这条走的是真实装配（module.Setup → New(ctx)），不注入任何 fake。
func TestManagerBlockingTakesEffectOnUploadGateAndAppConfig(t *testing.T) {
	route, _ := newPolicyIntegrationServer(t)

	require.True(t, IsAllowedExtension(".pdf"), "前置条件：.pdf 默认可传")

	writeSetting(t, route, "extra_blocked_extensions", "pdf")

	assert.True(t, IsBlockedExtension(".pdf"), "管理台封堵后上传门必须立即拒绝")
	assert.False(t, IsAllowedExtension(".pdf"))
	assert.NotContains(t, EffectiveAllowedExtensions(), ".pdf")

	// appconfig 下发清单同步收窄，客户端不会再把 .pdf 当作可传。
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/common/appconfig", nil)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		FileUploadLimits *struct {
			MaxSizeKB         int      `json:"max_size_kb"`
			AllowedExtensions []string `json:"allowed_extensions"`
		} `json:"file_upload_limits"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.FileUploadLimits)
	assert.NotContains(t, body.FileUploadLimits.AllowedExtensions, ".pdf")
	assert.Contains(t, body.FileUploadLimits.AllowedExtensions, ".jpg")
}

// 管理台放开一个 baseline 之外的扩展名 → 上传门立即放行。这是本任务要交付的
// 核心能力：不重启、不发版。
func TestManagerAllowingTakesEffectOnUploadGate(t *testing.T) {
	route, _ := newPolicyIntegrationServer(t)

	require.False(t, IsAllowedExtension(".dwg"), "前置条件：.dwg 不在 baseline")

	writeSetting(t, route, "extra_allowed_extensions", "dwg")

	assert.True(t, IsAllowedExtension(".dwg"), "管理台放开后必须立即可传")
	assert.Contains(t, EffectiveAllowedExtensions(), ".dwg")
}

// 管理台调整大小上限 → MaxUploadSize 与 appconfig 同步变化。
func TestManagerSizeCapTakesEffect(t *testing.T) {
	route, _ := newPolicyIntegrationServer(t)

	require.Equal(t, MaxFileSize, MaxUploadSize(), "前置条件：默认 100MB")

	// 4096KB 高于 sticker 默认上限 1024KB，不触发组合守卫。
	writeSetting(t, route, "max_size_kb", "4096")

	assert.Equal(t, int64(4096*1024), MaxUploadSize())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/common/appconfig", nil)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		FileUploadLimits *struct {
			MaxSizeKB int `json:"max_size_kb"`
		} `json:"file_upload_limits"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.FileUploadLimits)
	assert.Equal(t, 4096, body.FileUploadLimits.MaxSizeKB)
}

// 上限展示精度的端到端回归（review P2）。
//
// file.max_size_kb 接受任意 KB 值；改动前提示按 bytes/1024/1024 整除，配成
// 1536KB 时服务端实际放行 1.5MB，却告诉客户端「不能超过 1MB」—— 一个服务端
// 并不执行的上限。这里走真实 renderer，断言用户看到的文案与结构化详情都精确。
func TestPresignedOversizeReportsExactCap(t *testing.T) {
	route, _ := newPolicyIntegrationServer(t)

	// 1536KB = 1.5MB，故意选一个不是整数 MB 的上限。
	writeSetting(t, route, "max_size_kb", "1536")
	require.Equal(t, int64(1536*1024), MaxUploadSize())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet,
		"/v1/file/upload/presigned?type=chat&filename=photo.jpg&fileSize=2000000", nil)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)

	body := w.Body.String()
	require.NotEqual(t, http.StatusOK, w.Code, body)
	assert.Contains(t, body, "1.5 MB", "提示必须给出精确上限：%s", body)
	assert.NotContains(t, body, "1 MB。", "不得把 1.5MB 的上限整除成 1MB")
	assert.NotContains(t, body, "{{.", "params 必须被渲染，不能把模板漏给用户")

	var payload struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	assert.Equal(t, "err.server.file.upload_too_large", payload.Error.Code)
	assert.EqualValues(t, 1536, payload.Error.Details["max_size_kb"], "max_size_kb 必须精确")
	assert.EqualValues(t, 1, payload.Error.Details["max_mb"], "max_mb 保留整除截断以兼容老客户端")
}

// env 与管理台配置并存时的端到端（D9 并集语义）。
//
// 这是**生产的真实形态**：configmap 里历史上放开过若干格式，运维之后想再加一个。
// 覆盖语义下运维只填新增的那一个，configmap 里的格式会当场失效、用户立刻传不了，
// 而他不会想到这一层 —— 并集语义就是为堵这个而改的，所以必须有容器级验证，
// 不能只停在读侧单测。
//
// env 值取自部署环境实测（2026-08-26）。
func TestEnvAndManagerAllowlistsUnionAtUploadGate(t *testing.T) {
	route, _ := newPolicyIntegrationServerWithEnv(t, ".tgz,.xlsm", "")

	// 前置：env 放开的两个格式可传，.dwg 尚不可传。
	require.True(t, IsAllowedExtension(".tgz"), "env 放开的 .tgz 必须可传")
	require.True(t, IsAllowedExtension(".xlsm"))
	require.False(t, IsAllowedExtension(".dwg"))

	// 运维在管理台只填新增的那一个。
	writeSetting(t, route, "extra_allowed_extensions", "dwg")

	assert.True(t, IsAllowedExtension(".dwg"), "新加的 .dwg 必须生效")
	assert.True(t, IsAllowedExtension(".tgz"),
		"env 放开的 .tgz 不能因为管理台加了一项就失效（覆盖语义会踩的坑）")
	assert.True(t, IsAllowedExtension(".xlsm"))

	// appconfig 下发的清单同样要三者齐全。
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/common/appconfig", nil)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		FileUploadLimits *struct {
			AllowedExtensions []string `json:"allowed_extensions"`
		} `json:"file_upload_limits"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.FileUploadLimits)
	for _, ext := range []string{".tgz", ".xlsm", ".dwg"} {
		assert.Contains(t, body.FileUploadLimits.AllowedExtensions, ext)
	}
}

// 收回一个 env 放开项的正确姿势：写进封堵栏（黑名单优先级最高）。
// 并集语义下这是唯一的「减」入口，端到端确认它真能压过 env。
func TestManagerBlocklistRevokesEnvAllowedExtension(t *testing.T) {
	route, _ := newPolicyIntegrationServerWithEnv(t, ".tgz,.xlsm", "")
	require.True(t, IsAllowedExtension(".tgz"))

	writeSetting(t, route, "extra_blocked_extensions", "tgz")

	assert.True(t, IsBlockedExtension(".tgz"), "封堵必须压过 env 的放开")
	assert.False(t, IsAllowedExtension(".tgz"))
	assert.True(t, IsAllowedExtension(".xlsm"), "同一 env 里的其它格式不受影响")
}

// env 封堵的项无法从管理台解封（D1 并集语义的已知代价，刻意如此）。
// 端到端钉住它，避免有人后来「顺手」改成覆盖语义。
func TestManagerCannotUnblockEnvBlockedExtension(t *testing.T) {
	route, _ := newPolicyIntegrationServerWithEnv(t, "", ".tgz")
	require.True(t, IsBlockedExtension(".tgz"))

	// 管理台写入另一个封堵项，不得把 env 里已封的 .tgz 解封。
	writeSetting(t, route, "extra_blocked_extensions", "xlsm")

	assert.True(t, IsBlockedExtension(".tgz"), "env 封堵项不得被管理台写入静默解封")
	assert.True(t, IsBlockedExtension(".xlsm"))
}

// 硬上限的 env 层端到端（部署容量决策）。
//
// 天花板由运维经 OCTO_FILE_MAX_SIZE_KB_HARD_CAP 决定（改 configmap + 重启），
// 超管在管理台调实际值且超不过它。这条走真实装配，确认三层职责真的串起来了：
// env 天花板 → 管理台写入 → 上传门 → appconfig 下发。
func TestEnvHardCapRaisesTheCeilingForManagerWrites(t *testing.T) {
	// 天花板抬到 1GB，否则 600000KB(~586MB) 会被默认的 512MB 钳掉。
	t.Setenv("OCTO_FILE_MAX_SIZE_KB_HARD_CAP", "1048576")
	route, _ := newPolicyIntegrationServer(t)

	writeSetting(t, route, "max_size_kb", "600000")

	assert.Equal(t, int64(600000)*1024, MaxUploadSize(),
		"抬高天花板后，管理台的值应原样生效")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/common/appconfig", nil)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		FileUploadLimits *struct {
			MaxSizeKB int `json:"max_size_kb"`
		} `json:"file_upload_limits"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.FileUploadLimits)
	assert.Equal(t, 600000, body.FileUploadLimits.MaxSizeKB, "下发值须与服务端一致")
}

// 未抬高天花板时，同一个写入被钳到默认的 512MB —— 天花板确实在起作用。
func TestManagerWriteClampedByDefaultHardCap(t *testing.T) {
	t.Setenv("OCTO_FILE_MAX_SIZE_KB_HARD_CAP", "")
	route, _ := newPolicyIntegrationServer(t)

	writeSetting(t, route, "max_size_kb", "600000")

	assert.Equal(t, int64(common.FileMaxSizeKBHardCap())*1024, MaxUploadSize(),
		"未抬天花板时应被钳到 512MB，而不是原样服务")
}

// ---------------------------------------------------------------------------
// 天花板低于贴纸上限时的三面一致性（round-4 review P1）
//
// 天花板可配之后，「全局上限 < 贴纸上限」这个 D6 明确命名的致命组合，不再
// 只能由管理台写入达成：空表 + 低天花板开机即违规。而三个面里只有闸门看得见
// 天花板 —— appconfig 照旧广播未收敛的贴纸上限，管理台 effective_value 也是。
// 客户端拿着 1024 做预校验，上传到服务端被前置的全局门以「文件过大」拒掉。
//
// 下面两条从真实装配路径验证：读侧收敛让三个面重新一致，写侧守卫仍然拦得住
// 一次无法兑现的写入。
// ---------------------------------------------------------------------------

// writeSettingRaw 提交任意 category/key 组合，返回原始响应，供断言拒绝路径。
func writeSettingRaw(t *testing.T, route *wkhttp.WKHttp, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/v1/manager/common/system_setting",
		strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	route.ServeHTTP(w, req)
	return w
}

// appconfigLimits 取一次 appconfig，返回文件与贴纸两个上限。
func appconfigLimits(t *testing.T, route *wkhttp.WKHttp) (fileKB, stickerKB int) {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/common/appconfig", nil)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		FileUploadLimits *struct {
			MaxSizeKB int `json:"max_size_kb"`
		} `json:"file_upload_limits"`
		StickerUploadLimits *struct {
			MaxSizeKB int `json:"max_size_kb"`
		} `json:"sticker_upload_limits"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.FileUploadLimits)
	require.NotNil(t, body.StickerUploadLimits)
	return body.FileUploadLimits.MaxSizeKB, body.StickerUploadLimits.MaxSizeKB
}

// 一次管理台写入都没有：低天花板下 appconfig 广播的贴纸上限必须与闸门一致。
func TestLoweredCeilingKeepsStickerLimitCoherent(t *testing.T) {
	t.Setenv("OCTO_FILE_MAX_SIZE_KB_HARD_CAP", "512")
	route, _ := newPolicyIntegrationServer(t)

	require.Equal(t, int64(512)*1024, MaxUploadSize(),
		"前置条件：闸门被天花板钳到 512KB")

	fileKB, stickerKB := appconfigLimits(t, route)
	assert.Equal(t, 512, fileKB)
	assert.Equal(t, 512, stickerKB,
		"贴纸上限的下发值必须收敛到全局上限：客户端按 1024 预校验会白传一次，"+
			"服务端在贴纸门之前就用「文件过大」拒了")
	assert.LessOrEqual(t, stickerKB, fileKB, "三面一致：下发的贴纸上限 ≤ 全局上限")
}

// 贴纸门自己拿到的上限同样必须收敛 —— stickerLimits() 走的是同一个 getter。
func TestLoweredCeilingConvergesStickerGateLimit(t *testing.T) {
	t.Setenv("OCTO_FILE_MAX_SIZE_KB_HARD_CAP", "512")
	_, settings := newPolicyIntegrationServer(t)

	assert.Equal(t, 512, settings.StickerUploadMaxSizeKB(),
		"贴纸门与全局门必须落在同一个数上，否则贴纸门永远是死代码")
}

// 写侧：钳位后落到贴纸上限之下的写入必须被拒。
// 改动前 prospective 拿的是**原样** 4096，守卫看到 {4096, 1024} 判定合法，
// 超管拿到 200 {"applied":true}，而运行时钳位后是 {512, 1024}。
func TestManagerWriteRejectedWhenClampedFileCapFallsBelowSticker(t *testing.T) {
	t.Setenv("OCTO_FILE_MAX_SIZE_KB_HARD_CAP", "512")
	route, _ := newPolicyIntegrationServer(t)

	w := writeSettingRaw(t, route,
		`{"items":[{"category":"file","key":"max_size_kb","value":"4096"}]}`)

	body := w.Body.String()
	require.NotEqual(t, http.StatusOK, w.Code,
		"钳位后 512 < 贴纸 1024，这次写入必须被拒，实际：%s", body)
	assert.Contains(t, body, "全局单文件上传上限不能小于自定义贴纸的上传上限",
		"必须给出 D6 的组合冲突文案：%s", body)
}

// 同一批里把两个键一起提交则应放行 —— D6 的文案「请同时提交这两项」必须真的成立，
// 否则低天花板的部署将永远无法写入 file.max_size_kb。
func TestManagerBatchWriteWithLoweredStickerIsAccepted(t *testing.T) {
	t.Setenv("OCTO_FILE_MAX_SIZE_KB_HARD_CAP", "512")
	route, _ := newPolicyIntegrationServer(t)

	w := writeSettingRaw(t, route, `{"items":[`+
		`{"category":"file","key":"max_size_kb","value":"4096"},`+
		`{"category":"sticker","key":"upload_max_size_kb","value":"256"}]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	t.Cleanup(func() {
		writeSettingRaw(t, route,
			`{"items":[{"category":"sticker","key":"upload_max_size_kb","value":""}]}`)
	})

	fileKB, stickerKB := appconfigLimits(t, route)
	assert.Equal(t, 512, fileKB, "4096 仍被天花板钳到 512")
	assert.Equal(t, 256, stickerKB, "贴纸值低于全局上限，原样生效")
}
