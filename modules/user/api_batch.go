package user

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"go.uber.org/zap"
)

// maxBatchRequestBodyBytes bounds the POST /v1/users/batch request body. The
// global per-IP limiter is a frequency floor, not a size cap, and there is no
// global body-size fallback, so each batch handler caps its own body — mirrors
// modules/internal_resolve and modules/bot_mention. 32 KiB comfortably fits
// MaxBatchUserUIDs uids (each ≤ MaxBatchUIDLength) plus JSON framing.
const maxBatchRequestBodyBytes = 32 * 1024

// batchGet handles POST /v1/users/batch: resolve a list of uids to the minimal,
// PII-free batch DTO in one round trip. It replaces the per-uid GET /v1/users/:uid
// fan-out callers used for bulk identity resolution (anti-ghost-member checks),
// which otherwise saturates the per-endpoint rate limit on large groups.
//
// Liveness filtering (Status == StatusEnable AND IsDestroy == IsDestroyNo) and
// request-order preservation live in BuildBatchUsersResponse; a non-live
// (disabled / blacklisted / destroyed) account is reported in missing_uids. Note
// this gate is STRICTER than the human single-user read GET /v1/users/:uid
// (u.get → GetUserDetail), which applies no status/destroy filter — see
// BuildBatchUsersResponse for the full rationale.
//
// Error taxonomy (deliberate, see P2 decision): every request-validation failure
// — malformed body, empty/too-many/duplicate/over-length uids — collapses to a
// single ErrUserRequestInvalid("uids") 400. Unlike the bot leg, the human leg
// does NOT branch cap-overflow into a distinct limit-exceeded envelope carrying
// the cap. This endpoint is internal-facing (docs-backend controls the request
// shape and already knows MaxBatchUserUIDs); a per-reason code would add i18n
// surface for no client benefit. The cap is documented in the API contract, not
// discovered from the error body.
func (u *User) batchGet(c *wkhttp.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBatchRequestBodyBytes)

	var req BatchUsersRequest
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "uids")
		return
	}
	if err := ValidateBatchUIDs(req.UIDs); err != nil {
		respondUserRequestInvalid(c, "uids")
		return
	}

	resps, err := u.userService.GetUsers(req.UIDs)
	if err != nil {
		u.Error("批量获取用户信息失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}

	c.Response(BuildBatchUsersResponse(req.UIDs, resps))
}
