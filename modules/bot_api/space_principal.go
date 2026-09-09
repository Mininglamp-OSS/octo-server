package bot_api

import (
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

const (
	principalTypeHuman   = "human"
	principalTypeUserBot = "user_bot"
)

var errSpacePrincipalNotFound = errors.New("space principal not found")

type spacePrincipal struct {
	UID           string `json:"uid"`
	PrincipalType string `json:"principal_type"`
}

type spacePrincipalStore interface {
	lookupEligibleSpacePrincipal(callerUID, callerKind, spaceID, targetUID string) (*spacePrincipal, error)
}

// botSpacePrincipal resolves one principal without exposing why an identity is
// absent or ineligible. The exact Space is mandatory and is never inferred from
// another membership or from a client-controlled default.
func (ba *BotAPI) botSpacePrincipal(c *wkhttp.Context) {
	callerUID := getRobotIDFromContext(c)
	callerKind := getBotKindFromContext(c)
	if callerKind == BotKindApp {
		// This integration surface is deliberately User-Bot-only. Reject from the
		// authenticated kind context before validation or any principal DB lookup.
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIAppBotUnsupported, nil, nil)
		return
	}
	if callerUID == "" || callerKind != BotKindUser {
		ba.respondBotAPIIdentityMissing(c)
		return
	}

	spaceID := strings.TrimSpace(c.Query("space_id"))
	targetUID := strings.TrimSpace(c.Param("uid"))
	if spaceID == "" {
		respondBotAPIRequestInvalid(c, "space_id")
		return
	}
	if targetUID == "" {
		respondBotAPIRequestInvalid(c, "uid")
		return
	}

	store := ba.principalStoreOverride
	if store == nil {
		store = ba.db
	}
	principal, err := store.lookupEligibleSpacePrincipal(callerUID, callerKind, spaceID, targetUID)
	if errors.Is(err, errSpacePrincipalNotFound) || (err == nil && principal == nil) {
		// Every absent/inactive/unauthorized condition deliberately collapses to
		// the same response so this exact lookup cannot enumerate identities,
		// membership, bot state, ownership, or Space state.
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIUserNotFound, nil, nil)
		return
	}
	if err != nil {
		ba.Error("space principal eligibility query failed", zap.Error(err))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIQueryFailed, nil, nil)
		return
	}
	c.Response(principal)
}
