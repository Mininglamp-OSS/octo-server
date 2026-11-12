package bot_api

import (
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/permissionbundle"
)

func (ba *BotAPI) permissionFacts(c *wkhttp.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Vary", "Authorization, token")
	if getBotKindFromContext(c) != BotKindUser || !strings.HasPrefix(extractBotToken(c), "bf_") {
		permissionbundle.RespondError(c, permissionbundle.ErrForbidden)
		return
	}
	space, e := permissionbundle.ExplicitSpace(c.Request)
	if e != nil {
		permissionbundle.RespondError(c, e)
		return
	}
	if c.GetHeader("Content-Encoding") != "" {
		permissionbundle.RespondError(c, permissionbundle.ErrInvalid)
		return
	}
	r := permissionbundle.RuntimeFor(ba.ctx)
	if r == nil {
		permissionbundle.RespondError(c, permissionbundle.ErrUnavailable)
		return
	}
	out, e := r.Facts(c.Request.Context(), permissionbundle.Key{Origin: r.Store().Origin, BotUID: getRobotIDFromContext(c), SpaceID: space}, c.Request.Body)
	if e != nil {
		permissionbundle.RespondError(c, e)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}

func (ba *BotAPI) permissionBindDelivery(c *wkhttp.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Vary", "Authorization, token")
	if getBotKindFromContext(c) != BotKindUser || !strings.HasPrefix(extractBotToken(c), "bf_") {
		permissionbundle.RespondError(c, permissionbundle.ErrForbidden)
		return
	}
	space, e := permissionbundle.ExplicitSpace(c.Request)
	if e != nil {
		permissionbundle.RespondError(c, e)
		return
	}
	if c.GetHeader("Content-Encoding") != "" {
		permissionbundle.RespondError(c, permissionbundle.ErrInvalid)
		return
	}
	r := permissionbundle.RuntimeFor(ba.ctx)
	if r == nil || !r.HasDeliveryBinding() {
		permissionbundle.RespondError(c, permissionbundle.ErrUnavailable)
		return
	}
	out, e := r.BindDeliveredMessage(c.Request.Context(), permissionbundle.Key{Origin: r.Store().Origin, BotUID: getRobotIDFromContext(c), SpaceID: space}, c.Request.Body)
	if e != nil {
		permissionbundle.RespondError(c, e)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", out)
}
