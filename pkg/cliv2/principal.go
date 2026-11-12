package cliv2

import (
	"codex.mlamp.cn/dmwork/octo-auth-sdk/contract"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// Principal contains the Token owner, its bound Space, and the optionally
// validated User Bot binding. It is attached to the request after SDK
// authentication and identity validation succeed.
type Principal struct {
	UserID   string
	UserName string
	BotName  string
	SpaceID  string
	BotID    string
}

type requestAuthContextKey struct{}

type requestAuthContext struct {
	principal   Principal
	permissions []contract.PermissionGroup
}

// PrincipalFrom returns the trusted request principal, or the zero value when
// called outside an authenticated cliv2 route.
func PrincipalFrom(c *wkhttp.Context) Principal {
	if c == nil || c.Request == nil {
		return Principal{}
	}
	auth, _ := c.Request.Context().Value(requestAuthContextKey{}).(requestAuthContext)
	return auth.principal
}

func trustedPrincipal(access contract.AccessContext, userOnly bool) (Principal, string, string, bool) {
	upstream := access.Principal
	if upstream.TokenID == "" || upstream.OwnerID == "" || upstream.SpaceID == "" ||
		upstream.SubjectType != "user" || upstream.SubjectID != upstream.OwnerID ||
		access.Permissions == nil || access.BotBindings == nil || len(access.BotBindings) > 1 {
		return Principal{}, "", "", false
	}

	principal := Principal{
		UserID:   upstream.OwnerID,
		UserName: upstream.OwnerName,
		SpaceID:  upstream.SpaceID,
	}
	actorID, actorName := principal.UserID, principal.UserName
	if len(access.BotBindings) == 1 {
		binding := access.BotBindings[0]
		if binding.BotID == "" {
			return Principal{}, "", "", false
		}
		principal.BotID = binding.BotID
		principal.BotName = binding.BotName
		if !userOnly {
			actorID, actorName = binding.BotID, binding.BotName
		}
	}
	return principal, actorID, actorName, true
}
