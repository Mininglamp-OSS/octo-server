package category

// Blank imports to ensure all dependent modules register their SQL migrations
// during tests. Mirrors internal/modules.go to ensure correct migration order.
import (
	_ "github.com/Mininglamp-OSS/octo-server/modules/backup"
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	_ "github.com/Mininglamp-OSS/octo-server/modules/botfather"
	_ "github.com/Mininglamp-OSS/octo-server/modules/channel"
	_ "github.com/Mininglamp-OSS/octo-server/modules/common"
	_ "github.com/Mininglamp-OSS/octo-server/modules/file"
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/message"
	_ "github.com/Mininglamp-OSS/octo-server/modules/qrcode"
	_ "github.com/Mininglamp-OSS/octo-server/modules/report"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/statistics"
	_ "github.com/Mininglamp-OSS/octo-server/modules/thread"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
	_ "github.com/Mininglamp-OSS/octo-server/modules/voice_adapter"
	_ "github.com/Mininglamp-OSS/octo-server/modules/webhook"
	_ "github.com/Mininglamp-OSS/octo-server/modules/workplace"
)
