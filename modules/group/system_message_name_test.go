package group

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSanitizeSystemMessageName 钉住展示名进入系统消息 extra 前必须被掐掉的两条
// 渲染途径。
//
// 这两条都**不是** JSON 注入 —— 名字放在结构化的 extra[i].name 里，JSON 层面是
// 干净的。此前的注释把"结构化"当成了"安全"，那只买到了一半。真正剩下的是**渲染**：
// content 里的 `{N}` 由客户端在同一个字符串上逐次替换，替换进去的文本会被后续几轮
// 重新扫描。
func TestSanitizeSystemMessageName(t *testing.T) {
	t.Run("花括号被换成全角，不再匹配占位符", func(t *testing.T) {
		// 攻击形状：把自己的群备注设成含字面量 {1}。渲染 {0} 时它被放进句子，
		// 下一轮渲染 {1} 时就被当占位符展开成继任者的名字 —— 一条系统消息里
		// 凭空多出半句话，且 NoPersist:0 永久留在群历史里。
		got := sanitizeSystemMessageName("张三{1}已被移出")
		assert.NotContains(t, got, "{", "半角左花括号必须消失，否则仍会被当占位符")
		assert.NotContains(t, got, "}", "半角右花括号必须消失")
		assert.Contains(t, got, "张三", "名字的可读部分必须保留")
		assert.Contains(t, got, "已被移出", "只处理花括号，不吃掉其它字符")
	})

	t.Run("双向控制符被剥掉", func(t *testing.T) {
		// RIGHT-TO-LEFT OVERRIDE 能把后面的文本视觉上反转，
		// 让整句系统消息读起来像在说另一件事，而字节层面"正常"。
		got := sanitizeSystemMessageName("张三‮李四")
		assert.NotContains(t, got, "‮", "RLO 必须被剥掉")
		assert.Equal(t, "张三李四", got, "剥掉控制符后其余字符原样保留")
	})

	t.Run("所有 bidi 控制符都覆盖到", func(t *testing.T) {
		// 逐个验，避免"改了实现只剥其中几个"这种半吊子修复溜过去。
		for _, r := range []rune{
			'‪', '‫', '‬', '‭', '‮',
			'⁦', '⁧', '⁨', '⁩',
			'‎', '‏',
		} {
			got := sanitizeSystemMessageName("a" + string(r) + "b")
			assert.Equal(t, "ab", got, "U+%04X 必须被剥掉", r)
		}
	})

	t.Run("emoji 连接符必须保留", func(t *testing.T) {
		// ZWJ(U+200D) 是 emoji 序列的连接符。一并剥掉会把合法昵称拆成散字，
		// 危害与破坏程度不成比例 —— 所以只剥 bidi，不剥所有格式字符。
		family := "👨‍👩‍👧"
		assert.Equal(t, family, sanitizeSystemMessageName(family),
			"ZWJ 必须保留，否则 emoji 昵称会被拆散")
	})

	t.Run("正常名字原样返回", func(t *testing.T) {
		for _, name := range []string{"张三", "Alice", "user_01", "李 四", "🐱"} {
			assert.Equal(t, name, sanitizeSystemMessageName(name),
				"不含花括号和 bidi 的名字不该被改动")
		}
	})

	t.Run("空串安全", func(t *testing.T) {
		assert.Equal(t, "", sanitizeSystemMessageName(""))
	})
}
