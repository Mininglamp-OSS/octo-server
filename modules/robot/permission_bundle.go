package robot

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/permissionbundle"
)

// ConfigurePermissionBundle binds reviewed current-authority and supported
// release adapters at composition time. The pinned repository has neither
// adapter; it must not synthesize them from a pool, startup nonce or candidate.
func (rb *Robot) ConfigurePermissionBundle(store *permissionbundle.Store) {
	rb.permissionBundle = store
}
func (rb *Robot) permissionBundleHandler(operation string) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		// Keep the existing creator-only/not-found boundary before exposing any
		// body details; the store independently locks current native scope.
		if !rb.checkMCPPolicyOwner(c) {
			return
		}
		rb.permissionBundle.Serve(c, c.Param("robot_id"), c.GetLoginUID(), operation)
	}
}
