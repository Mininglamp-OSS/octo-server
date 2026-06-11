package user

import "embed"

//go:embed assets/system_bot_avatar/*.png
var systemBotAvatarFS embed.FS

// systemBotAvatarFiles 把「有品牌化专属头像」的系统 Bot uid 映射到其头像文件。
//
// 这些系统 Bot 的头像是固定静态图，既不走 13 色随机默认头像
// （shouldUseBotDefaultAvatar 用 !IsSystemBot 已把系统 Bot 排除在外），
// 也不走 #346 引入的昵称首字母渲染——否则像 botfather 会被渲染成昵称后两字
// （"BotFather" → "er"），观感不专业。
//
// 键必须是 pkg/space.SystemBots 里的系统 Bot（由 TestSystemBotAvatarAssets 约束）。
// 未配专属图的系统 Bot（如 notification）不在此表，仍回退到默认头像逻辑——
// 后续补充设计稿后在此追加映射即可。
var systemBotAvatarFiles = map[string]string{
	"botfather": "assets/system_bot_avatar/botfather.png",
}

// systemBotAvatar 返回系统 Bot 的专属头像字节；uid 未配专属图（或资源读取失败）
// 时返回 ok=false，调用方据此回退到默认头像逻辑。
func systemBotAvatar(uid string) ([]byte, bool) {
	name, ok := systemBotAvatarFiles[uid]
	if !ok {
		return nil, false
	}
	data, err := systemBotAvatarFS.ReadFile(name)
	if err != nil {
		return nil, false
	}
	return data, true
}
