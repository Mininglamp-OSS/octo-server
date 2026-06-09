package user

import (
	"embed"
	"hash/crc32"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

//go:embed assets/bot_default_avatar/*.png
var botDefaultAvatarFS embed.FS

var botDefaultAvatarFiles = []string{
	"assets/bot_default_avatar/01-blue.png",
	"assets/bot_default_avatar/02-wathet.png",
	"assets/bot_default_avatar/03-turquoise.png",
	"assets/bot_default_avatar/04-green.png",
	"assets/bot_default_avatar/05-lime.png",
	"assets/bot_default_avatar/06-yellow.png",
	"assets/bot_default_avatar/07-sunflower.png",
	"assets/bot_default_avatar/08-orange.png",
	"assets/bot_default_avatar/09-red.png",
	"assets/bot_default_avatar/10-carmine.png",
	"assets/bot_default_avatar/11-violet.png",
	"assets/bot_default_avatar/12-purple.png",
	"assets/bot_default_avatar/13-indigo.png",
}

func shouldUseBotDefaultAvatar(uid string, userInfo *Model) bool {
	if userInfo == nil {
		return false
	}
	return userInfo.Robot == 1 && !spacepkg.IsSystemBot(uid)
}

func botDefaultAvatarIndex(uid string) int {
	return int(crc32.ChecksumIEEE([]byte(uid)) % uint32(len(botDefaultAvatarFiles)))
}

func readBotDefaultAvatar(uid string) ([]byte, error) {
	return botDefaultAvatarFS.ReadFile(botDefaultAvatarFiles[botDefaultAvatarIndex(uid)])
}
