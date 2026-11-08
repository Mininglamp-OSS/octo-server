package bot_api

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/permissionbundle"
)

func (ba *BotAPI) ConfigurePermissionBundle(store *permissionbundle.Store) {
	ba.permissionBundle = store
}
func (ba *BotAPI) permissionBundleHandler(operation string) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		c.Header("Cache-Control", "private, no-store")
		// Existing Bot auth accepts legacy tokens; this new private surface
		// explicitly requires the authenticated bf_ User Bot's own exact scope.
		if getBotKindFromContext(c) != BotKindUser || !strings.HasPrefix(extractBotToken(c), "bf_") {
			permissionbundle.RespondError(c, permissionbundle.ErrForbidden)
			return
		}
		ba.permissionBundle.Serve(c, getRobotIDFromContext(c), "", operation)
	}
}
