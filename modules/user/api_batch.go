package user

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"go.uber.org/zap"
)

// batchGet handles POST /v1/users/batch: resolve a list of uids to the minimal,
// PII-free batch DTO in one round trip. It replaces the per-uid GET /v1/users/:uid
// fan-out callers used for bulk identity resolution (anti-ghost-member checks),
// which otherwise saturates the per-endpoint rate limit on large groups.
//
// Enabled-only filtering and request-order preservation live in
// BuildBatchUsersResponse; a non-enabled (disabled / blacklisted) user is
// reported in missing_uids, matching the single-user get gate.
func (u *User) batchGet(c *wkhttp.Context) {
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
