package common

import (
	"strconv"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

// 管理台写侧针对 file.* 键的校验与通用变更审计。
//
// 单独成文件而不是继续堆进 updateSystemSettings —— 那个 handler 已经数百行，
// 每加一组键就往里塞一段是它长成现在这样的原因。

const (
	// fileExtensionListMaxEntries / fileExtensionMaxLength 限制两个扩展名 CSV 的
	// 规模。system_setting.value 是 TEXT（64KB），而 settingTypeString 的写侧
	// 是「anything goes」，所以在加限制之前，一次超管误配就能写进数千个扩展名：
	// 每个上传请求要拿它构建同等规模的 map，更要命的是这份清单会原样下发到
	// **无鉴权且被所有客户端高频调用**的 /v1/common/appconfig，把响应从约 1KB
	// 撑到数十 KB。
	//
	// 贴纸那组键没有这个问题：StickerUploadAllowedFormats 与固定 5 项的内置
	// 位图白名单求交，天然有上界。file.extra_allowed_extensions 取消候选集后
	// 就失去了那道天然上界，于是必须在写侧显式补上。
	//
	// 64 项 / 32 字符是宽松但有限的界：内置白名单本身才 74 项，真实的「额外
	// 放开/封堵」需求远小于此；最长的常见扩展名（.appimage）9 个字符。
	fileExtensionListMaxEntries = 64
	fileExtensionMaxLength      = 32
	// fileExtensionListMaxBytes 约束**原始** CSV 长度。条数与单项长度只看解析
	// 后的结果，而入库和每次重新 split 的是原始串。64 项 × 32 字符 + 分隔符
	// 有充裕余量。
	fileExtensionListMaxBytes = 4096
)

// rejectInvalidFileSettingWrites 校验本批次里所有 file.* 键的写入。
// 返回 true 表示已经向客户端写出错误响应，调用方应立即 return。
func (m *Manager) rejectInvalidFileSettingWrites(c *wkhttp.Context, plans []preparedSetting) bool {
	for _, p := range plans {
		if p.def.Category != "file" {
			continue
		}
		switch p.def.Key {
		case "extra_allowed_extensions", "extra_blocked_extensions":
			if m.rejectOversizedExtensionList(c, p) {
				return true
			}
			// 内置黑名单不可通过配置撤销，所以把它写进 extra_allowed 注定不生效：
			// 值存进去了、effective_value 却不显示它、上传照样被拒，而没有任何
			// 地方解释原因。在写侧拒绝是唯一能让操作者当场知道的位置。
			if p.def.Key == "extra_allowed_extensions" && m.rejectBuiltinBlocked(c, p) {
				return true
			}
		}
	}
	return m.rejectInconsistentSizeOrdering(c, plans)
}

// rejectOversizedExtensionList 挡住原始长度 / 条数 / 单项长度超限的 CSV。
//
// 三个界缺一不可：条数与单项长度约束解析后的结果，而**原始字符串长度**约束真正
// 落进 TEXT 列、并在每次快照读时被重新 split 的东西 —— ParseFileExtensionCSV
// 会静默丢弃非法 token，所以几万个畸形 token 解析出 0 项、能过条数检查，
// 而 p.value 原样入库。上限声称要防的正是列大小与每请求解析开销，那由原始
// 长度决定，不由解析后的条数决定。
func (m *Manager) rejectOversizedExtensionList(c *wkhttp.Context, p preparedSetting) bool {
	if len(p.value) > fileExtensionListMaxBytes {
		// 单独的 code：说清楚超的是**原始长度**，而不是条数或单项字符数 ——
		// 一个 5000 字节里放 20 个合法项的载荷，用共享消息会被告知一组它并未
		// 触碰的上限。
		httperr.ResponseErrorL(c, errcode.ErrFileExtensionListTooLong, i18n.Params{
			"max_bytes": strconv.Itoa(fileExtensionListMaxBytes),
		}, i18n.Details{
			"max_bytes": strconv.Itoa(fileExtensionListMaxBytes),
			"got_bytes": strconv.Itoa(len(p.value)),
		})
		return true
	}
	exts := ParseFileExtensionCSV(p.value)
	if len(exts) > fileExtensionListMaxEntries {
		return m.respondExtensionListTooLarge(c, i18n.Details{
			"max_entries": strconv.Itoa(fileExtensionListMaxEntries),
			"got":         strconv.Itoa(len(exts)),
		})
	}
	for _, ext := range exts {
		if utf8.RuneCountInString(ext) > fileExtensionMaxLength {
			return m.respondExtensionListTooLarge(c, i18n.Details{
				"max_entries": strconv.Itoa(fileExtensionListMaxEntries),
				// 按 rune 截断：NormalizeFileExtension 允许非 ASCII（只挡控制字符、
				// 空白、路径分隔符与连续点），按字节切会切碎多字节字符，
				// 让 detail 里出现非法 UTF-8。
				"extension": string([]rune(ext)[:fileExtensionMaxLength]),
			})
		}
	}
	return false
}

// respondExtensionListTooLarge 统一回应超限。
//
// 两个上限既走 **params**（消息模板要插值）又走 **details**（客户端要结构化值）。
// params 与 details 不互通：httperr 把它们放进 ErrorSpec 的不同字段，只有
// params 会喂给 go-i18n 当 TemplateData。只给 details 的话，模板里的
// {{.max_entries}} 会被 text/template 解析成零值并渲染成 "<no value>" —— 运维
// 看到的是一条断串。见 TestManagerSystemSetting_ExtensionListErrorRendersLimits。
func (m *Manager) respondExtensionListTooLarge(c *wkhttp.Context, details i18n.Details) bool {
	httperr.ResponseErrorL(c, errcode.ErrFileExtensionListTooLarge, i18n.Params{
		"max_entries": strconv.Itoa(fileExtensionListMaxEntries),
		"max_length":  strconv.Itoa(fileExtensionMaxLength),
	}, details)
	return true
}

// rejectBuiltinBlocked 挡住「把内置黑名单项写进 extra_allowed」。
func (m *Manager) rejectBuiltinBlocked(c *wkhttp.Context, p preparedSetting) bool {
	for _, ext := range ParseFileExtensionCSV(p.value) {
		if IsBuiltinBlockedFileExtension(ext) {
			httperr.ResponseErrorL(c, errcode.ErrFileExtensionNotAllowlistable, nil, i18n.Details{
				"extension": ext,
			})
			return true
		}
	}
	return false
}

// rejectInconsistentSizeOrdering 挡住「全局上限低于贴纸上限」的组合（D6）。
//
// 校验 merge(当前快照, 入参)，不是任一半：上传校验里全局大小门在前、贴纸门在
// 后，所以全局上限一旦低于贴纸上限，贴纸就永远传不上去，而两个键各自看都完全
// 合法 —— 运营调大贴纸上限后看不到任何效果，也拿不到任何报错。
//
// 刻意对两个 category 都触发：只在 batch 碰到 file.* 时校验，会让运维从 sticker
// 那一侧把上限抬过全局上限，绕开这个守卫。
func (m *Manager) rejectInconsistentSizeOrdering(c *wkhttp.Context, plans []preparedSetting) bool {
	incoming := map[string]string{}
	for _, p := range plans {
		switch {
		case p.def.Category == "file" && p.def.Key == "max_size_kb",
			p.def.Category == "sticker" && p.def.Key == "upload_max_size_kb":
			incoming[p.def.Category+"."+p.def.Key] = p.value
		}
	}
	if len(incoming) == 0 {
		return false
	}
	prospective := ApplyFileStickerSizeOverlay(m.systemSettings.FileStickerSizeOrdering(), incoming)
	if !ViolatesFileStickerSizeOrdering(prospective) {
		return false
	}
	httperr.ResponseErrorL(c, errcode.ErrFileUploadSizeOrdering, nil, i18n.Details{
		"file_max_size_kb":    strconv.Itoa(prospective.FileMaxSizeKB),
		"sticker_max_size_kb": strconv.Itoa(prospective.StickerMaxSizeKB),
	})
	return true
}

// collectSettingAudits 在写入**前**从当前快照捕获旧值，供提交后落审计日志。
// 加密类型的前后值一律脱敏 —— 审计要回答的是「谁在什么时候改了哪个键」，
// 不是把密文/明文抄进日志。
func (m *Manager) collectSettingAudits(plans []preparedSetting) []settingAuditEntry {
	audits := make([]settingAuditEntry, 0, len(plans))
	for _, p := range plans {
		if p.skip {
			continue
		}
		before, _ := m.systemSettings.lookup(p.def.Category, p.def.Key)
		after := p.value
		if p.def.Type == settingTypeEncrypted {
			before, after = maskIfSet(before), maskIfSet(after)
		}
		audits = append(audits, settingAuditEntry{
			key:    schemaKey(p.def.Category, p.def.Key),
			before: before,
			after:  after,
		})
	}
	return audits
}

// settingAuditEntry 是一条待落盘的配置变更审计记录。
type settingAuditEntry struct {
	key    string
	before string
	after  string
}

// settingAuditFields 组装审计日志字段。抽成函数而不是内联，是为了让字段名与
// 脱敏行为可被单测钉住 —— 审计是安全相关的输出，不该只靠 code review 守。
//
// applied 一并记录：配置已落库但本实例 reload 失败时，这条审计要能说明
// 「改动记下了，但本实例当时还没生效」。
func settingAuditFields(a settingAuditEntry, operator, traceID string, applied bool) []zap.Field {
	return []zap.Field{
		zap.String("trace_id", traceID),
		zap.String("operator", operator),
		zap.String("setting", a.key),
		zap.String("before", a.before),
		zap.String("after", a.after),
		zap.Bool("applied_on_this_instance", applied),
	}
}

// maskIfSet 把非空的敏感值折叠成掩码，空值保持空（"未配置" 与 "已配置" 的区分
// 对审计有意义，具体内容没有）。
func maskIfSet(v string) string {
	if v == "" {
		return ""
	}
	return secretMask
}

// preparedSetting 是一条通过校验、等待写入的设置。提到包级是为了让上面这些
// 校验函数能从 updateSystemSettings 里抽出来。
type preparedSetting struct {
	def            *settingDef
	value          string
	effectiveValue string
	skip           bool
}
