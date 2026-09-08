package project

import (
	"errors"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
)

// AI 分身进出项目的资格规则（D2 / D15）。
//
// 弹窗上的那句话是「仅可带入自己的分身，确认后直接加入」。它在建项目那一刻的含义
// 是显然的，在建项目**之后**的含义原本是空白的——`members/add` 今天对目标是不是
// bot、bot 归谁没有任何规则，任何管理员都能把任何持有 Space 席位的 bot 加进项目，
// 而普通成员反而没有入口带自己的分身。D15 把弹窗的承诺延伸成一条持续成立的规则。

var (
	// errAgentNotEligible 是 D3 / D15 的**唯一**分身拒绝理由。
	//
	// 六种真实原因合并成一个：不是你的 / 不是 bot / 是本地分身 / 不在本 Space /
	// 是系统 bot / 不存在。区分开就等于把加人接口变成一个探测器——拿着一个 uid
	// 就能问出"它存不存在、是不是 bot、归谁"。真正的原因进日志。
	//
	// 与 P1 建项目群那个 ErrGroupProjectUnavailable 是同一条反探测口径。
	errAgentNotEligible = errors.New("project: agent is not eligible for this project")
)

// agentEligibility 是一次资格判定的结果，逐 uid 给出。
type agentEligibility struct {
	// OwnerUID 是这个分身的主人；仅在 OK 为真时有意义。
	OwnerUID string
	// OK 为假表示这个 uid 不能作为分身进入项目。
	OK bool
	// Reason 是给日志用的具体原因，永远不上线。
	Reason string
}

// 具体原因，只进日志（见 errAgentNotEligible 的注释）。
const (
	agentReasonNotFound      = "not_found"
	agentReasonNotBot        = "not_a_bot"
	agentReasonNoRobotRow    = "no_active_robot_row"
	agentReasonSelfHosted    = "self_hosted"
	agentReasonSystemBot     = "system_bot"
	agentReasonNotOwned      = "not_owned_by_actor"
	agentReasonNoSpaceSeat   = "no_space_seat"
	agentHostingSelfHosted   = "self_hosted"
	agentReasonOKPlaceholder = ""
)

// classifyAgentsTx 判定一批 uid 是否可以作为 ownerUID 的分身进入 spaceID 下的项目。
//
// 判定在**事务内**，且依赖两件事：
//   - `robot` / `user` 的行（queryAgentRowsTx）；
//   - Space 席位，由调用方在同一个事务里用 lockSeatsTx 锁好后以 held 传入。
//
// 为什么 Space 席位由调用方传：分身和人走**同一条**席位检查
// （lockSpaceSeatsTx 一条语句锁全部 uid），而那条语句必须在项目行锁之前发出，
// 顺序是 P0 定死的（见 createProjectOnce 里那段关于 1213 的论证）。在这里再查一次
// 就会在项目行锁之后再碰 space_member，把已经分析过的锁序推翻。
//
// hosting 的口径与通讯录（GET /v1/space/directory）一致：排除 self_hosted。
// 这是**产品一致性**，不是安全边界——agent_hosting 是客户端自报值，服务端只校验
// 形状不校验取值（见 modules/botfather/sql 的那条迁移）。排除它的理由是：用户在
// 选择器里看不到的分身，不该能从接口带进来。
func (p *Project) classifyAgentsTx(
	tx *dbr.Tx, ownerUID string, uids []string, held map[string]bool,
) (map[string]agentEligibility, error) {
	out := make(map[string]agentEligibility, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	rows, err := p.db.queryAgentRowsTx(tx, uids)
	if err != nil {
		return nil, err
	}
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if spacepkg.IsSystemBot(uid) {
			// 系统 bot 在 I2 里是被豁免的（不需要项目席位就能进项目群），
			// 所以给它一个席位既无意义也会让豁免与席位两套机制互相干扰。
			out[uid] = agentEligibility{Reason: agentReasonSystemBot}
			continue
		}
		row, ok := rows[uid]
		if !ok {
			out[uid] = agentEligibility{Reason: agentReasonNotFound}
			continue
		}
		if row.Robot != 1 {
			out[uid] = agentEligibility{Reason: agentReasonNotBot}
			continue
		}
		if row.CreatorUID == "" {
			// robot 行缺失或已禁用（LEFT JOIN 带 status=1）。孤儿 bot 不算任何人
			// 的分身，与群侧 QueryBotsInvitedByUIDTx 那条 INNER JOIN 口径一致。
			out[uid] = agentEligibility{Reason: agentReasonNoRobotRow}
			continue
		}
		if row.CreatorUID != ownerUID {
			out[uid] = agentEligibility{Reason: agentReasonNotOwned}
			continue
		}
		if row.Hosting == agentHostingSelfHosted {
			out[uid] = agentEligibility{Reason: agentReasonSelfHosted}
			continue
		}
		if !held[uid] {
			// I1：分身也必须是本 Space 的活跃成员。botfather 建 bot 时会写
			// space_member，所以正常路径下这一条总是成立；不成立意味着这个 bot
			// 属于别的 Space，或者它的席位已经被 D14 的删除路径关掉了。
			out[uid] = agentEligibility{Reason: agentReasonNoSpaceSeat}
			continue
		}
		out[uid] = agentEligibility{OwnerUID: row.CreatorUID, OK: true, Reason: agentReasonOKPlaceholder}
	}
	return out, nil
}

// ineligibleAgentUIDs 按输入顺序返回不合格的 uid，供错误响应的 details 使用。
// 顺序稳定是为了让同一个请求两次得到同样的响应，测试才能断言。
func ineligibleAgentUIDs(uids []string, verdicts map[string]agentEligibility) []string {
	bad := make([]string, 0)
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if v, ok := verdicts[uid]; !ok || !v.OK {
			bad = append(bad, uid)
		}
	}
	return bad
}

// ineligibleAgentReasons 返回 uid → 具体原因，只用于日志。
func ineligibleAgentReasons(uids []string, verdicts map[string]agentEligibility) map[string]string {
	reasons := make(map[string]string)
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		v, ok := verdicts[uid]
		if !ok {
			reasons[uid] = agentReasonNotFound
			continue
		}
		if !v.OK {
			reasons[uid] = v.Reason
		}
	}
	return reasons
}

// canManageOwnAgents 是 D15(b) 的窄能力：任何活跃项目成员都可以带自己的分身进来、
// 把自己的分身移出去，不需要 canManageMembers。
//
// 单独成为一个能力位而不是让客户端从 role 推导，与 capabilitiesFor 的整体口径一致：
// 客户端一旦自己推导权限矩阵，它就会在矩阵第一次变化时与服务端分叉。
func canManageOwnAgents(projectRole int) bool { return isProjectMember(projectRole) }

// agentNotEligibleError carries the ineligible SUBSET out to the handler.
//
// D3 rejects the whole request when any one agent is ineligible, but the refusal
// still has to name WHICH uids were bad, or the picker cannot highlight them. The
// first version echoed every uid the caller submitted, which is a different claim:
// with one bad uid in a batch of ten it tells the client all ten were refused, and
// the user's only recovery is to re-pick from scratch.
//
// It carries uids and nothing else. The REASONS stay in the log — see
// errAgentNotEligible for why splitting them would build an oracle.
type agentNotEligibleError struct {
	// UIDs is the ineligible subset, in the caller's submission order.
	UIDs []string
}

func (e *agentNotEligibleError) Error() string { return errAgentNotEligible.Error() }

// Unwrap keeps errors.Is(err, errAgentNotEligible) true, so every existing arm that
// tests the sentinel — including the per-uid members/add path, which does not need
// the subset — keeps working unchanged.
func (e *agentNotEligibleError) Unwrap() error { return errAgentNotEligible }
