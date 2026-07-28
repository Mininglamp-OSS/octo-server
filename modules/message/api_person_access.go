package message

// personAccessDeps 是 personChannelAccessAllowed 需要的最小依赖面。
// user.IService 天然满足它，单测可用三方法 fake 替换，无需起 DB。
type personAccessDeps interface {
	// IsFriend 查询 uid → toUID 的好友关系（friend 表口径）。
	IsFriend(uid string, toUID string) (bool, error)
	// QueryPeerRobotInfo 返回目标用户是否为 bot 及其创建者 UID。
	QueryPeerRobotInfo(peerUID string) (isRobot bool, creatorUID string, err error)
	// AreSpaceMembers 校验两个 uid 是否同属一个在籍 Space。
	AreSpaceMembers(spaceID, uid1, uid2 string) (bool, error)
}

// personChannelAccessAllowed 判定 loginUID 是否有权操作与 peerUID 的私聊(DM)频道。
//
// 口径与 modules/user/api_pinned.go::validateChannelAccess 逐字对齐（另见
// modules/messages_search/authz.go::checkP2PAccess），按序判定，满足其一即放行：
//
//  1. peerUID == loginUID —— 给自己的「备忘」频道，好友/bot/Space 判定都无意义；
//  2. 双方是好友 —— 个人空间（非 Space）部署下 friend 表是权威口径；
//  3. 对方是 bot —— 本人创建的 bot 放行；他人创建的 bot 必须先加好友，与 robot
//     模块的转发规则一致（modules/robot/event.go："用户与Bot非好友关系，拒绝转发消息"）。
//     bot 判定必须在 Space 判定之前收口：bot 没有 space_member 行，落到第 4 步恒为 false，
//     会把「自己的 bot」误拒；
//  4. 对方是真人且与调用者同属 spaceID 这个在籍 Space —— 企业通讯录部署下 friend 表
//     近乎为空（只有系统 bot），私聊可达性由 Space 在籍关系决定，与写消息路径
//     （api.go sendMsg 的 Person/Space 分支 CheckBothMembers）同语义。
//
// 只有好友口径会让企业部署下同事之间的 DM 一律 403（channel_access_denied）——
// 这正是本函数存在的原因。
//
// spaceID 只取请求上 SpaceMiddleware 已校验的 space_id（spacepkg.GetSpaceID）。
// 请求没带 space_id 时不去解析调用者的默认 Space（决策 D1），判定退回好友口径 ——
// 与本次改动前的行为一致，且无 Space 的请求不会多一次查询。
//
// 任何一次查询出错都 fail-closed（返回 false + error），由调用方按
// ErrMessageQueryFailed 归并，绝不因 DB 抖动放行。
func personChannelAccessAllowed(deps personAccessDeps, loginUID, peerUID, spaceID string) (bool, error) {
	if peerUID == "" {
		return false, nil
	}
	if peerUID == loginUID {
		return true, nil
	}
	isFriend, err := deps.IsFriend(loginUID, peerUID)
	if err != nil {
		return false, err
	}
	if isFriend {
		return true, nil
	}
	isRobot, creatorUID, err := deps.QueryPeerRobotInfo(peerUID)
	if err != nil {
		return false, err
	}
	if isRobot {
		return creatorUID == loginUID, nil
	}
	if spaceID == "" {
		return false, nil
	}
	return deps.AreSpaceMembers(spaceID, loginUID, peerUID)
}
