package robot

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

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
