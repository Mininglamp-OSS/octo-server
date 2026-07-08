package cardmsg

// 卡片 profile 能力档位的**单一权威**（card-message-interaction D2 / D12）。
//
// 校验器 interactiveByProfile（validate.go：判定某 profile 是否放行 Action.Submit /
// Input.* 交互元素）与 D12 能力清单 AcceptedProfiles（GET /v1/bot/card/profile 据此
// 下发 profiles）都从 acceptedProfiles 派生 —— 两者不可能漂移。新增 profile（未来
// octo/v3）只改这一处：校验接受集与对外清单自动一致，无需在 handler 里重抄字面量。

// profileSpec 描述一个被接受的 profile 及其能力档位。
type profileSpec struct {
	name string
	// interactive octo/v2 放行 Action.Submit + Input.*（P2 D1）；octo/v1 恒 false（展示档）。
	interactive bool
}

// acceptedProfiles 是本 build 接受的 profile 有序集合（v1→v2）—— 校验器接受集与 D12
// 对外清单的唯一来源。
var acceptedProfiles = []profileSpec{
	{name: ProfileV1, interactive: false}, // P1 Decision 10：展示档
	{name: ProfileV2, interactive: true},  // P2 D1/D2：交互档
}

// AcceptedProfiles 返回本 build 接受的 profile 值（有序，与校验器 interactiveByProfile
// 的接受集恒一致）。D12 能力清单据此下发 profiles；调用方 MUST 用它、而非重抄字面量
// （单一权威，见 acceptedProfiles）。每次调用返回新切片，调用方改不到内部状态。
func AcceptedProfiles() []string {
	out := make([]string, len(acceptedProfiles))
	for i := range acceptedProfiles {
		out[i] = acceptedProfiles[i].name
	}
	return out
}
