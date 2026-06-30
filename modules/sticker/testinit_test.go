package sticker

// Blank imports so every dependent module registers its SQL migrations during
// tests. This MUST mirror internal/modules.go (minus the sticker self-import):
// the shared test DB's gorp_migrations table is populated by whatever module
// set the test binary registers, and an omitted module that other suites apply
// trips sql-migrate's "unknown migration in database" guard on a reused DB
// (PR #508 review by Jerry-Xin).
import (
	_ "github.com/Mininglamp-OSS/octo-server/modules/backup"
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"

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
	_ "github.com/Mininglamp-OSS/octo-server/modules/search"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/statistics"
	_ "github.com/Mininglamp-OSS/octo-server/modules/thread"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
	_ "github.com/Mininglamp-OSS/octo-server/modules/usersecret"

	_ "github.com/Mininglamp-OSS/octo-server/modules/bot_api"

	_ "github.com/Mininglamp-OSS/octo-server/modules/app_bot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/bot_provision"
	_ "github.com/Mininglamp-OSS/octo-server/modules/voice_adapter"
	_ "github.com/Mininglamp-OSS/octo-server/modules/webhook"
	_ "github.com/Mininglamp-OSS/octo-server/modules/workplace"
)
