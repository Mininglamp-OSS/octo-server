package cardmsg

// 内联图标：为什么是「精确字节白名单」而不是「内容过滤器」。
//
// 卡片模板需要一两个矢量图标（如折叠箭头）。三条路都不好走：外链把一个第三方 host
// 变成每张卡的运行时依赖；相对路径服务端没有静态资源路由；而放开 `data:image/svg+xml`
// 让服务端去过滤 SVG 内容，等于要在 `cardmsg` 里写一个 SVG 消毒器 —— SVG 是 XSS 的
// 经典载体，而服务端无法知道客户端是用 <img src> 渲染（浏览器沙箱化，脚本不执行）还是
// 内联进 DOM（脚本会执行），所以必须按后者的最坏假设过滤。
//
// 那个消毒器写不对。第一版是「禁止 <script/<use/href/url(/@import/on*=」的子串
// 黑名单，PR#712 review 当场给出五个绕过：命名空间前缀（`<s:script>`）、CSS 标识符
// 转义（`fill:\75 rl(...)`、`@\69 mport`）、SVG 1.2 Tiny 的 `<handler>` 元素。子串
// 黑名单对 XML + CSS 两层可转义语法本质上是漏的。
//
// 所以这里换一个不需要消毒器的问题定义：**只接受本仓库审过的那几串确定字节**。
// 白名单是精确全串匹配，不解码、不解析、不过滤。由此得到四个性质：
//
//   - 校验不依赖调用位置。没有「可信调用点」这种档位，也就不存在某个回写路径忘记开
//     档位、导致服务端自己渲染的帧在落库前被自己拒掉（PR#712 的 P0 正是如此：render
//     侧开了档位，carddispatch/updater 的三个回写点没开，V4 卡片发得出去、编辑必失败）。
//   - 消毒器的漏洞不再可达。任意 SVG 一律不匹配，五个绕过连同未来第六个都进不来。
//   - 模板里的 `Image.url` 必须是字面量。`${field}` 插值出来的串不可能等于常量，
//     所以「bot 填的数据字段变成 data: 注入口」这条路被构造性堵死，不需要另加编译期规则。
//   - 新增图标是一次人工评审。往这里加字节是显式动作，评审的对象就是要内联进每个客户端
//     DOM 的那几百字节本身 —— 这正是应该被人看一眼的东西。
//
// 代价：模板作者不能自由用图标。这是有意的。

// chevronDownIcon / chevronUpIcon 是 ai.reasoning-process@0.4.0 折叠/展开箭头的
// 内联字节（Lucide chevron-down / chevron-up，stroke 取 #a1a6ab，1em 方形）。
// 与 handoff 模板里的字面量逐字节一致 —— TestInlineImageAllowlistCoversTemplates
// 会遍历所有已注册模板，任何一个不在本白名单里的 data: URL 都会让该测试失败。
const (
	chevronDownIcon = "data:image/svg+xml,%3Csvg%20xmlns%3D%22http%3A%2F%2Fwww.w3.org%2F2000%2Fsvg%22%20width%3D%221em%22%20height%3D%221em%22%20viewBox%3D%220%200%2024%2024%22%3E%3Cpath%20fill%3D%22none%22%20stroke%3D%22%23a1a6ab%22%20stroke-linecap%3D%22round%22%20stroke-linejoin%3D%22round%22%20stroke-width%3D%222%22%20d%3D%22m6%209l6%206l6-6%22%2F%3E%3C%2Fsvg%3E"
	chevronUpIcon   = "data:image/svg+xml,%3Csvg%20xmlns%3D%22http%3A%2F%2Fwww.w3.org%2F2000%2Fsvg%22%20width%3D%221em%22%20height%3D%221em%22%20viewBox%3D%220%200%2024%2024%22%3E%3Cpath%20fill%3D%22none%22%20stroke%3D%22%23a1a6ab%22%20stroke-linecap%3D%22round%22%20stroke-linejoin%3D%22round%22%20stroke-width%3D%222%22%20d%3D%22m18%2015l-6-6l-6%206%22%2F%3E%3C%2Fsvg%3E"
)

// vettedInlineImages 是允许出现在图片类 URL 字段上的内联图，按完整字节索引。
// 只读，进程生命周期内不变 —— 没有注册接口，加图标必须改这份源码。
var vettedInlineImages = map[string]struct{}{
	chevronDownIcon: {},
	chevronUpIcon:   {},
}

// isVettedInlineImage 报告 raw 是否与某个审过的内联图逐字节相同。
func isVettedInlineImage(raw string) bool {
	_, ok := vettedInlineImages[raw]
	return ok
}

// checkImageURL 校验图片类字段（Image.url / ImageSet.images[].url）的 URL：审过的
// 内联图放行，其余一切仍走 checkURL 的正向 allowlist（只接受绝对 http/https，
// 于是 javascript:、其他 data:、相对路径继续被拒）。
//
// 刻意不 TrimSpace 后再比对：带空白的变体不是审过的那串字节，落回 checkURL 由它
// 按 URL 规则处置。白名单只认它审过的东西，不做规范化 —— 规范化是又一处可以出岔子的
// 语义，而它换不来任何东西。
func checkImageURL(raw string) error {
	if isVettedInlineImage(raw) {
		return nil
	}
	return checkURL(raw)
}
