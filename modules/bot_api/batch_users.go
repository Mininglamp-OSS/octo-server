package bot_api

import (
	"errors"
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// maxBatchRequestBodyBytes bounds the POST /v1/bot/users/batch request body.
// There is no global body-size fallback, so the handler caps its own body —
// mirrors modules/internal_resolve and modules/bot_mention. 32 KiB comfortably
// fits MaxBatchUserUIDs uids (each ≤ MaxBatchUIDLength) plus JSON framing.
const maxBatchRequestBodyBytes = 32 * 1024

// batchUsers handles POST /v1/bot/users/batch: resolve a list of uids to the
// minimal, PII-free batch DTO in one call. It is the bot-surface counterpart of
// POST /v1/users/batch and shares the request/DTO contract and projection with
// the human endpoint (modules/user.BuildBatchUsersResponse). Mounted under the
// /v1/bot group, it inherits bot auth, identity assertion, and the per-bot
// business rate limiter.
//
// Liveness filtering is mandatory here: a disabled, blacklisted, or destroyed
// account is reported in missing_uids, never present, so a bot cannot use this
// endpoint to confirm a ghost member's identity. This matches the bot single-user
// read GET /v1/bot/user/info (GetUser, which rejects any non-enabled account).
//
// Space-prefix normalization: callers on the migration path this endpoint targets
// pass space-scoped uids of the form s<digits>_<baseUID>. The sibling single-user
// read strips that prefix via stripSpacePrefix before resolving; the batch leg
// does the same per uid so a space-prefixed uid resolves against its base user
// row instead of silently landing in missing_uids. The caller's ORIGINAL string
// is preserved as the missing_uids key (the response echoes exactly what was
// sent); resolved users carry the base uid in `uid`, matching the single-user
// read. Stripping can collapse two distinct originals (e.g. s1_x and s2_x, or
// s1_x and x) onto the same base — that surfaces as ErrBatchUIDDuplicate (400)
// on the re-validated stripped list rather than silently coalescing, so a caller
// cannot request the same base identity twice under different space scopes in one
// batch.
//
// Space-scoping contract (deliberate): robotID is asserted for presence
// (authorization gate) but does NOT constrain which uids resolve — resolution is
// global across the user table, identical to the single-user GET /v1/bot/user/info
// contract. This is the intended contract for any provisioned bot token: the
// batch endpoint discloses only the minimal, PII-free identity DTO (uid / status /
// name / robot), never profile data or Space membership, so global resolution
// leaks nothing a bot could not already learn from the single-user read. It is
// deliberately NOT scoped to the bot's Spaces/groups the way membership-bearing
// endpoints (GET /v1/bot/space/members, /v1/bot/resolve/targets) are, because
// those return Space-relative rosters whereas this returns Space-independent
// identity. If a future requirement needs Space-scoped identity resolution, add a
// separate scoped endpoint rather than narrowing this one.
//
// DB-only resolution: like the human leg, this reads the user table via GetUsers
// and does not replicate the single-read synthetic fast paths (app_*_bot registry,
// iwh_ webhook senders) — see BuildBatchUsersResponse for that decision.
func (ba *BotAPI) batchUsers(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		ba.respondBotAPIIdentityMissing(c)
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBatchRequestBodyBytes)

	var req user.BatchUsersRequest
	if err := c.BindJSON(&req); err != nil {
		respondBotAPIRequestInvalid(c, "uids")
		return
	}

	// Normalize space-prefixed uids to their base form before resolving, keeping
	// each caller-supplied original so missing_uids echoes what was sent. Re-validate
	// the stripped list (stripping can create duplicates / empties).
	stripped := make([]string, len(req.UIDs))
	origByBase := make(map[string]string, len(req.UIDs))
	for i, uid := range req.UIDs {
		base := stripSpacePrefix(uid)
		stripped[i] = base
		origByBase[base] = uid
	}
	if err := user.ValidateBatchUIDs(stripped); err != nil {
		if errors.Is(err, user.ErrBatchUIDsTooMany) {
			respondBotAPILimitExceeded(c, "uids", user.MaxBatchUserUIDs)
			return
		}
		respondBotAPIRequestInvalid(c, "uids")
		return
	}

	resps, err := ba.userService.GetUsers(stripped)
	if err != nil {
		ba.Error("batch users query failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSharedInternal, nil, nil)
		return
	}

	resp := user.BuildBatchUsersResponse(stripped, resps)
	// Echo the caller's original (possibly space-prefixed) string for missing uids
	// so the response is keyed by what was sent. Resolved users keep the base uid.
	for i, base := range resp.MissingUIDs {
		if orig, ok := origByBase[base]; ok {
			resp.MissingUIDs[i] = orig
		}
	}

	c.Response(resp)
}
