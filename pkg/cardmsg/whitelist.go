package cardmsg

// 卡片元素白名单的**单一权威**（card-message-interaction D12.2 / P3-3）。
//
// displayElements（octo/v1 展示元素，两档共用）与 inputElements（octo/v2 交互输入，
// octo/v1 携带越级拒）是校验器（validate.go）、inputs 采集（inputs.go 的 isInputElement）、
// D12 能力清单（GET /v1/bot/card/profile 下发 elements/inputs）三处的共同来源 —— 绝不在
// 各处重抄字面量，否则白名单变更时对外清单会与校验器静默漂移（清单漂移 = 对 producer /
// J3 gate 谎报能力）。新增元素只在此追加一处，三方自动同步。
//
// 二者都是 additive-only（同 event_data / D12 wire 演进规则）：只增不改名/删除。

var displayElements = []string{
	"TextBlock", "Image", "Container", "ColumnSet", "Column", "FactSet",
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
