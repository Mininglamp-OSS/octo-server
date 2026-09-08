package group

import "strings"

// 本文件处理一件事：把**用户可控的展示名**放进系统消息的 `extra` 之前，
// 先掐掉它影响渲染结果的两条途径。
//
// 背景：本仓库的群系统消息把名字放在结构化的 `extra[i].name` 里，content 用
// `{0}`/`{1}` 占位符，由客户端替换。这个形状消除了 JSON 注入面 —— 但它**没有**
// 消除渲染面，而此前的注释把两者当成了一回事。
//
// 名字的来源是 group_member.remark，成员可以给自己设（api.go 的备注接口在
// loginUID == memberUID 时放行，且不校验内容），群管理员也能给别人设。而这些
// 消息是 NoPersist:0 的持久消息，写进群历史就留在那里，后来入群的人也读得到。

// bidiControls 是要从展示名里剥掉的双向文本控制符。
//
// 只剥这一类，不剥所有 Unicode 格式字符（Cf）：U+200D(ZWJ) 是 emoji 序列的连接符，
// 剥掉会把「👨‍👩‍👧」这样的合法昵称拆成三个字符。危害与破坏程度不成比例。
//
// 这些字符能重排周围文本的视觉顺序，于是一个名字可以让整句系统消息读起来像在说
// 另一件事，而字节层面完全"正常"。
var bidiControls = []rune{
	'‪', // LEFT-TO-RIGHT EMBEDDING
	'‫', // RIGHT-TO-LEFT EMBEDDING
	'‬', // POP DIRECTIONAL FORMATTING
	'‭', // LEFT-TO-RIGHT OVERRIDE
	'‮', // RIGHT-TO-LEFT OVERRIDE
	'⁦', // LEFT-TO-RIGHT ISOLATE
	'⁧', // RIGHT-TO-LEFT ISOLATE
	'⁨', // FIRST STRONG ISOLATE
	'⁩', // POP DIRECTIONAL ISOLATE
	'‎', // LEFT-TO-RIGHT MARK
	'‏', // RIGHT-TO-LEFT MARK
	'؜', // ARABIC LETTER MARK（U+061C，与 LRM/RLM 同类，首版漏了）
}

// sanitizeSystemMessageName 把展示名处理成可以安全放进系统消息 extra 的形式。
//
// 两件事，各自对应一条真实的渲染途径：
//
//  1. **占位符再展开**。三端客户端都是拿 content 里的 `{N}` 依次替换 extra[N].name，
//     替换是在**同一个字符串上逐次进行**的，替换进去的文本会被后续几轮重新扫描。
//     所以一个名字里写字面量 `{1}`，在替换 `{0}` 时被放进句子，下一轮替换 `{1}` 时
//     就会被当成占位符展开成继任者的名字 —— 攻击者由此在一条系统消息里凭空造出
//     半句话。iOS 的实现用 replacingOccurrences（替换**所有**匹配）会更进一步，
//     把两个槽位渲染成同一个名字。
//
//     这里把 `{` `}` 换成全角 `｛` `｝`：视觉上几乎一样（名字仍然认得出），但不再
//     匹配任何客户端的占位符正则。选择替换而不是删除，是为了让「名字里本来就有
//     花括号」的正常用户仍然看到一个像自己名字的东西。
//
//  2. **双向文本覆写**。见 bidiControls。直接剥掉，没有保留的理由 —— 展示名里
//     出现方向覆写符不存在正常用例。
//
// 这**不是**一个通用的输出转义函数，别拿它当那个用。它只针对「进入 extra、由客户端
// 做占位符替换」这一条路径。HTML/JSON 各有各的转义，和这里无关。
//
// 已知未覆盖：octo-lib 的 1007（扫码入群）走同样的两占位符形状、名字同样用户可控，
// 但它的发送在 octo-lib，本函数够不着。那需要在上游做同样的处理。
func sanitizeSystemMessageName(name string) string {
	if name == "" {
		return name
	}
	replaced := strings.NewReplacer("{", "｛", "}", "｝").Replace(name)
	return strings.Map(func(r rune) rune {
		for _, bad := range bidiControls {
			if r == bad {
				return -1
			}
		}
		return r
	}, replaced)
}
