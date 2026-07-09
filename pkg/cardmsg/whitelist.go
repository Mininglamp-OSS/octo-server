package cardmsg

// 卡片元素白名单（card-message-interaction D12.2 / P3-3）。二者对外都是 D12 能力清单
// （GET /v1/bot/card/profile 下发 elements/inputs）的来源，绝不在各处重抄字面量；但它们
// 与校验器的绑定强度**不同**，注意区分（PR#556 review：勿把 displayElements 也说成三方
// 结构性单一权威）：
//
//   - inputElements（octo/v2 交互输入，octo/v1 携带越级拒）是**真正的三方结构性单一权威**：
//     校验器（validate.go element() 的 default 分支经 isInputElement）、inputs 采集
//     （inputs.go collectInputSpecs 经 isInputElement）、D12 清单 inputs 都从它派生 —— 新增
//     输入元素只改这一处，三方自动同步、不可能漂移。
//   - displayElements（octo/v1 展示元素，两档共用）只是 **D12 清单 elements 的来源**，且只列
//     **顶层可放置**的展示元素。校验器对展示元素是**逐类型手写 case**（TextBlock 查 markdown、
//     Image 查 url、Container 递归 items、ColumnSet 递归 columns、FactSet 查 facts —— 各自校验体
//     不同，无法像输入那样并成一个 isInputElement 分支）。因此 displayElements 与校验器接受集的
//     一致性**不是结构性保证，而是由 TestDisplayElementsAuthority 逐个按顶层 body 元素 Validate
//     守卫**：往这里加展示元素必须同时给校验器加顶层 case 并让该测试通过，否则清单会广播一个
//     校验器拒绝的元素（漂移 = 谎报能力）。Column 刻意**不**在其中——它非独立元素、只作 ColumnSet
//     的子列（element() 无顶层 Column case，顶层 Column 会被拒），由 ColumnSet 结构性涵盖
//     （PR#556 review）。
//
// 二者都是 additive-only（同 event_data / D12 wire 演进规则）：只增不改名/删除。

var displayElements = []string{
	"TextBlock", "RichTextBlock", "Image", "ImageSet",
	"Container", "ColumnSet", "FactSet",
	"Table", "ActionSet",
}

var inputElements = []string{
	"Input.Text", "Input.Toggle", "Input.ChoiceSet",
	"Input.Number", "Input.Date", "Input.Time",
}

// DisplayElements 返回 octo/v1 展示元素白名单副本（D12 清单据此下发 elements；单一权威，
// 调用方 MUST 用它而非重抄字面量）。每次返回新切片，调用方改不到内部状态。
func DisplayElements() []string { return append([]string(nil), displayElements...) }

// InputElements 返回 octo/v2 交互输入白名单副本（D12 清单据此下发 inputs；同上纪律）。
func InputElements() []string { return append([]string(nil), inputElements...) }

// isInputElement 报告 t 是否为 octo/v2 交互输入元素（成员属于 inputElements）。校验器
// （validate.go element 派发）与 inputs 采集（collectInputSpecsFromElements）共用它，确保
// 「发送期放行集」「提交期声明采集集」「D12 清单 inputs」恒等。
func isInputElement(t string) bool {
	for _, e := range inputElements {
		if t == e {
			return true
		}
	}
	return false
}
