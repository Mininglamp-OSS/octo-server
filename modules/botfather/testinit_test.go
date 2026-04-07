package botfather

// Blank imports to ensure all dependent modules register their SQL migrations
// during tests. Without these, tables like `robot` and `app` won't be created.
import (
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)
