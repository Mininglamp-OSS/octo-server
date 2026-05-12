// Module import ordering notes:
//
// Migration execution order is determined by SQL filename timestamps
// (`YYYYMMDD<NNNNNN>_<module>_*.sql`), not by the import order here —
// sql-migrate pools every module's SQL into one slice and sorts by
// VersionInt across the whole set, so a cross-module dependency like
// "botfather ALTERs robot" is honoured by virtue of the botfather file
// being timestamped later than the robot CREATE.
//
// The import order in this file still matters for Go-level concerns:
//
//   - `bot_api` must appear BEFORE `app_bot` because `app_bot` imports
//     `bot_api` at the Go package level.
//
//   - Both `bot_api` and `app_bot` appear AFTER `user` and `robot` since
//     they query those tables at runtime (Go-init ordering).

package modules

// 引入模块
import (
	_ "github.com/Mininglamp-OSS/octo-server/modules/backup"
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	// `robot` before `botfather`: botfather migrations ALTER the robot table.
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/botfather"
	_ "github.com/Mininglamp-OSS/octo-server/modules/category"
	_ "github.com/Mininglamp-OSS/octo-server/modules/channel"
	_ "github.com/Mininglamp-OSS/octo-server/modules/common"
	_ "github.com/Mininglamp-OSS/octo-server/modules/file"
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/message"
	_ "github.com/Mininglamp-OSS/octo-server/modules/notify"
	_ "github.com/Mininglamp-OSS/octo-server/modules/oidc"
	_ "github.com/Mininglamp-OSS/octo-server/modules/openapi"
	_ "github.com/Mininglamp-OSS/octo-server/modules/qrcode"
	_ "github.com/Mininglamp-OSS/octo-server/modules/report"
	_ "github.com/Mininglamp-OSS/octo-server/modules/search"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/statistics"
	_ "github.com/Mininglamp-OSS/octo-server/modules/thread"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
	// app_bot and bot_api query user/robot tables at runtime; app_bot
	// also imports bot_api, so register bot_api before app_bot.
	_ "github.com/Mininglamp-OSS/octo-server/modules/bot_api"
	_ "github.com/Mininglamp-OSS/octo-server/modules/app_bot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/voice"
	_ "github.com/Mininglamp-OSS/octo-server/modules/webhook"
	_ "github.com/Mininglamp-OSS/octo-server/modules/workplace"
)
