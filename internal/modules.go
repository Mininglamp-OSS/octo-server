// Module import ordering notes:
//
// Migration execution order is determined by SQL filename timestamps
// (`YYYYMMDD<NNNNNN>_<module>_*.sql`), not by the blank-import order here —
// sql-migrate pools every module's SQL into one slice and sorts by
// VersionInt across the whole set, so a cross-module dependency like
// "botfather ALTERs robot" is honoured by virtue of the botfather file
// being timestamped later than the robot CREATE.
//
// Likewise Go init() order is determined by the package dependency graph
// (a package always inits after every package it imports). Blank-import
// order here does NOT influence it.
//
// We still keep the historical orderings below — `robot` before `botfather`
// and `bot_api` before `app_bot` — because (1) they match how original
// authors thought about the dependencies and (2) the inline grouping makes
// the migration / Go-package relationships easy to scan. They are
// belt-and-braces, not load-bearing (PR #21 review I4 by Jerry-Xin + yujiawei).

package modules

// 引入模块
import (
	_ "github.com/Mininglamp-OSS/octo-server/modules/backup"
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"

	// `robot` before `botfather`: botfather migrations ALTER the robot table
	// (历史顺序，非 load-bearing —— 真正排序由 SQL 文件时间戳决定)。
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"

	_ "github.com/Mininglamp-OSS/octo-server/modules/botfather"

	_ "github.com/Mininglamp-OSS/octo-server/modules/category"
	_ "github.com/Mininglamp-OSS/octo-server/modules/channel"
	_ "github.com/Mininglamp-OSS/octo-server/modules/common"
	_ "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	_ "github.com/Mininglamp-OSS/octo-server/modules/file"
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
	_ "github.com/Mininglamp-OSS/octo-server/modules/integration"
	_ "github.com/Mininglamp-OSS/octo-server/modules/message"
	_ "github.com/Mininglamp-OSS/octo-server/modules/messages_search"
	_ "github.com/Mininglamp-OSS/octo-server/modules/notify"
	_ "github.com/Mininglamp-OSS/octo-server/modules/oidc"
	_ "github.com/Mininglamp-OSS/octo-server/modules/opanalytics"
	_ "github.com/Mininglamp-OSS/octo-server/modules/openapi"
	_ "github.com/Mininglamp-OSS/octo-server/modules/qrcode"
	_ "github.com/Mininglamp-OSS/octo-server/modules/report"
	// PR-C: modules/runtime removed; runtime/bot orchestration owned by
	// octo-fleet (separate service). The migration table rows for the
	// runtime SQL files stay in gorp_migrations harmlessly — sql-migrate
	// only complains about unknown DOWNGRADE rows when downgrading.
	_ "github.com/Mininglamp-OSS/octo-server/modules/search"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/statistics"
	_ "github.com/Mininglamp-OSS/octo-server/modules/sticker"
	_ "github.com/Mininglamp-OSS/octo-server/modules/thread"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
	// usersecret 提供用户外部密钥别名表 + write-only CRUD + resolve;resolve
	// 鉴权按 bf_ bot token 反查 robot.creator_uid,运行期查 robot 表(非 import 依赖)。
	_ "github.com/Mininglamp-OSS/octo-server/modules/usersecret"
	// app_bot and bot_api query user/robot tables at runtime; app_bot
	// also imports bot_api at the Go package level, so register bot_api
	// before app_bot（历史顺序，非 load-bearing —— Go init() 顺序由依赖图决定）.
	_ "github.com/Mininglamp-OSS/octo-server/modules/bot_api"

	_ "github.com/Mininglamp-OSS/octo-server/modules/app_bot"
	// PR-A: cross-service JWT issuer + bot endpoints (mint / token lookup).
	_ "github.com/Mininglamp-OSS/octo-server/modules/bot_provision"
	_ "github.com/Mininglamp-OSS/octo-server/modules/voice_adapter"
	_ "github.com/Mininglamp-OSS/octo-server/modules/webhook"
	_ "github.com/Mininglamp-OSS/octo-server/modules/workplace"
)
