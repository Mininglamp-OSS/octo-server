package bot_api

import (
	"errors"
	"net/http"
	"strings"

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
// /v1/bot group, it inherits bot auth and identity assertion.
//
// Rate bound: the route carries its OWN always-on per-IP strict bucket
// (StrictIPRateLimitMiddleware tag "bot_users_batch", mounted in bot_api.go),
// which is the endpoint's real default-config rate bound. The per-bot business
// limiter the /v1/bot group also runs is default-off (enabled=false / dry-run=true,
// see modules/common/system_settings.go defaultBotRateLimit*) — it is a hot-tunable
// shadow lever, NOT a bound you can rely on out of the box. The always-on IP bucket
// exists precisely so this identity-resolution endpoint (each call resolves up to
// MaxBatchUserUIDs uids) is bounded independently of that switch, mirroring the
// bot_register / bot_heartbeat always-on IP buckets.
//
// Liveness filtering is mandatory here: a disabled, blacklisted, or terminally
// destroyed (is_destroy == IsDestroyDone) account is reported in missing_uids,
// never present, so a bot cannot use this endpoint to confirm a ghost member's
// identity. A cooling-off account (is_destroy == IsDestroyApplying) is still live
// and stays present — see BuildBatchUsersResponse. This matches the bot single-user
// read GET /v1/bot/user/info (GetUser, which rejects any non-enabled account).
//
// Space-prefix normalization: callers on the migration path this endpoint targets
// pass space-scoped uids of the form s<digits>_<baseUID>. The sibling single-user
// read strips that prefix via stripSpacePrefix before resolving; the batch leg
// does the same per uid so a space-prefixed uid resolves against its base user
// row instead of silently landing in missing_uids. The caller's ORIGINAL string
// is preserved as the response key for BOTH slices — resolved users and missing
// uids alike echo exactly what was sent, so every request uid appears in exactly
// one slice keyed by the string the caller used (the shared contract in
// modules/user.BuildBatchUsersResponse). Stripping can collapse two distinct
// originals (e.g. s1_x and s2_x, or s1_x and x) onto the same base — that surfaces
// as ErrBatchUIDDuplicate (400) on the re-validated stripped list rather than
// silently coalescing, so a caller cannot request the same base identity twice
// under different space scopes in one batch.
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

	// Fail-fast on the count cap before allocating the per-uid working slices.
	// Stripping the space prefix is 1:1 (it never changes the count), so the cap
	// is decidable on the raw request; ValidateBatchUIDs re-checks it on the
	// stripped list as the authoritative gate. Checking it here just avoids sizing
	// two slices to a pathological count.
	if len(req.UIDs) > user.MaxBatchUserUIDs {
		respondBotAPILimitExceeded(c, "uids", user.MaxBatchUserUIDs)
		return
	}
	if len(req.UIDs) == 0 {
		respondBotAPIRequestInvalid(c, "uids")
		return
	}

	// Normalize each caller uid, keeping its original string as the response key:
	//   1. TrimSpace — align with the single read (getUserInfo trims c.Query("uid")).
	//   2. bound the ORIGINAL length before stripping — the space prefix (s<n>_)
	//      adds to it, and ValidateBatchUIDs only sees the shorter stripped base,
	//      so an over-long caller string would otherwise slip the length cap.
	//   3. strip the space prefix (s<digits>_<baseUID>) to the base user row form.
	// origByBase maps each stripped base back to the caller's original key so both
	// resolved users and missing_uids echo exactly what was sent — every request
	// key lands in exactly one of the two slices under the key the caller used.
	// Re-validate the stripped list (stripping can create duplicates / empties).
	stripped := make([]string, len(req.UIDs))
	origByBase := make(map[string]string, len(req.UIDs))
	for i, uid := range req.UIDs {
		uid = strings.TrimSpace(uid)
		if len(uid) > user.MaxBatchUIDLength {
			respondBotAPIRequestInvalid(c, "uids")
			return
		}
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

	// BuildBatchUsersResponse keys its output by the (stripped, base) strings it was
	// given; map every entry — resolved users AND missing uids — back to the caller's
	// original key so the response echoes exactly what was sent. Without this, a
	// space-prefixed uid that resolved would come back under its bare base uid and
	// the caller (indexing by the original s<n>_ key) would find it in neither slice,
	// misjudging a live member as a ghost.
	resp := user.BuildBatchUsersResponse(stripped, resps)
	for i := range resp.Users {
		if orig, ok := origByBase[resp.Users[i].UID]; ok {
			resp.Users[i].UID = orig
		}
	}
	for i, base := range resp.MissingUIDs {
		if orig, ok := origByBase[base]; ok {
			resp.MissingUIDs[i] = orig
		}
	}

	c.Response(resp)
}
