package bot_api

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// batchUsers handles POST /v1/bot/users/batch: resolve a list of uids to the
// minimal, PII-free batch DTO in one call. It is the bot-surface counterpart of
// POST /v1/users/batch and shares the request/DTO contract and projection with
// the human endpoint (modules/user.BuildBatchUsersResponse). Mounted under the
// /v1/bot group, it inherits bot auth, identity assertion, and the per-bot
// business rate limiter.
//
// Enabled-only filtering is mandatory here: a disabled or blacklisted account is
// reported in missing_uids, never present, so a bot cannot use this endpoint to
// confirm a ghost member's identity.
func (ba *BotAPI) batchUsers(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}

	var req user.BatchUsersRequest
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "uids")
		return
	}
	if err := user.ValidateBatchUIDs(req.UIDs); err != nil {
		if errors.Is(err, user.ErrBatchUIDsTooMany) {
			respondBotAPILimitExceeded(c, "uids", user.MaxBatchUserUIDs)
			return
		}
		respondBotAPIRequestInvalid(c, "uids")
		return
	}

	resps, err := ba.userService.GetUsers(req.UIDs)
	if err != nil {
		ba.Error("batch users query failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSharedInternal, nil, nil)
		return
	}

	c.Response(user.BuildBatchUsersResponse(req.UIDs, resps))
}
