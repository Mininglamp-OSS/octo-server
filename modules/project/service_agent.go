package project

import (
	"errors"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
)

// AI 分身进出项目的资格规则（D2 / D15）。
//
// 建项目时，弹窗上的「仅可带入自己的分身，确认后直接加入」仍然只适用于
// 创建者带入自己的 agent_uids。项目创建后的 members/add 则是人员管理入口：
// 只有 Owner/Admin 能调用，目标 bot 必须同时满足组织通讯录可见条件和本模块的
// agent 事实校验；不再把“目标必须属于调用方”作为额外限制，也不恢复普通成员
// 添加自己 bot 的人员管理例外。

var (
	// errAgentNotEligible 是 D3 / D15 的**唯一**分身拒绝理由。
	//
	// 建项目的 own-agent 路径与 members/add 的目录路径共享这个 envelope；具体
	// 原因（不是 bot、孤儿/无活跃 robot、本地分身、不在本 Space、系统 bot、账号
	// 不可用或目录 owner 不合格）合并返回，避免接口成为身份探测器。创建路径还会
	// 记录“不是创建者自己的 agent”，而 members/add 按目标真实 creator 分组复用
	// 基础事实，不额外套 actor-only 限制。
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
	agentReasonNotFound    = "not_found"
	agentReasonNotBot      = "not_a_bot"
	agentReasonNoRobotRow  = "no_active_robot_row"
	agentReasonSelfHosted  = "self_hosted"
	agentReasonSystemBot   = "system_bot"
	agentReasonNotOwned    = "not_owned_by_actor"
	agentReasonNoSpaceSeat = "no_space_seat"
	// agentReasonAccountUnusable：账号已停用或已销毁。与通讯录选择器同口径
	// （u.status = 1 AND COALESCE(u.is_destroy, 0) <> 2）——D2 说资格口径与通讯录
	// 一致，而这一半原本漏了。
	agentReasonAccountUnusable = "account_unusable"
	agentHostingSelfHosted     = "self_hosted"
	agentReasonOKPlaceholder   = ""
)

// classifyAgentsTx 判定一批 uid 是否可以作为指定 ownerUID 的分身进入项目。
//
// ownerUID 表示期望的 robot.creator_uid，而不是“当前 HTTP 调用者”。创建路径
// 传入项目创建者；members/add 按每个目标 bot 的真实 creator 分组调用它，因而
// 复用相同的 bot/account/Space-seat 基础事实，却不凭空要求 creator 必须是 actor。
// members/add 另外在 service.go 叠加组织通讯录的 octo_hosted、human-owner
// 条件；创建路径则保留自己的 agent_uids 规则。
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
// classifier 本身拒绝 self_hosted；members/add 的目录层还要求精确的 octo_hosted
// 展示值。agent_hosting 是客户端自报字段，服务端的资格判断只把它作为既有
// 目录展示规则的一部分，不把它当作 Owner/权限信号。
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
			// System bots are platform-managed identities, not user-owned Project
			// agent seats. Rejecting them here also keeps the agent API from
			// exposing the platform bot whitelist as a Project roster.
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
		if !row.AccountUsable {
			// 已停用 / 已销毁的账号。通讯录里看不到它，所以接口也不该接受它。
			out[uid] = agentEligibility{Reason: agentReasonAccountUnusable}
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
