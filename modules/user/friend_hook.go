package user

// BotFriendApplyHook 机器人好友申请通知回调
// botfather 模块注册这个回调来接收通知，避免循环依赖
// spaceID: 申请来源 Space，用于隔离通知到正确的 Space
type BotFriendApplyHook func(applyUID, applyName, robotID, remark, token, spaceID string)

var botFriendApplyHook BotFriendApplyHook

// RegisterBotFriendApplyHook 注册机器人好友申请通知回调
func RegisterBotFriendApplyHook(hook BotFriendApplyHook) {
	botFriendApplyHook = hook
}
