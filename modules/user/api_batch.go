package user

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"go.uber.org/zap"
)

// maxBatchUserUIDs caps a single POST /v1/users/batch request. Requests above
// this size are rejected as an invalid parameter rather than silently
// truncated, so callers always know they exceeded the limit.
const maxBatchUserUIDs = 200

// batchUserReq is the request body for POST /v1/users/batch.
type batchUserReq struct {
	UIDs []string `json:"uids"`
}

// batchUserItem is the whitelist projection returned by POST /v1/users/batch.
// Only uid / name / avatar are exposed. The service-layer Resp also carries
// sensitive fields (Phone / Email) which MUST NOT leak through this endpoint,
// so we copy the three public fields by hand instead of serializing Resp.
type batchUserItem struct {
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

// batchGetUsers batch-fetches users by uid and returns only the public
// projection (uid / name / avatar). It reuses Service.GetUsers; that method's
// IN-query returns only matched rows, so unknown uids are silently skipped and
// a partial result is not treated as an error. An empty uids list yields an
// empty array (GetUsers short-circuits to nil,nil on an empty slice).
func (u *User) batchGetUsers(c *wkhttp.Context) {
	var req batchUserReq
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "uids")
		return
	}
	if len(req.UIDs) > maxBatchUserUIDs {
		respondUserRequestInvalid(c, "uids")
		return
	}

	resps, err := u.userService.GetUsers(req.UIDs)
	if err != nil {
		u.Error("批量获取用户失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}

	items := make([]*batchUserItem, 0, len(resps))
	for _, resp := range resps {
		// Whitelist projection: copy ONLY uid / name / avatar. Phone / Email on
		// Resp are intentionally dropped. Resp has no avatar-URL field of its
		// own (IsUploadAvatar is an internal flag), so avatar is rendered as the
		// stable per-uid avatar URL, matching the `avatar` shape other user
		// endpoints emit.
		items = append(items, &batchUserItem{
			UID:    resp.UID,
			Name:   resp.Name,
			Avatar: u.ctx.GetConfig().GetAvatarPath(resp.UID),
		})
	}
	c.JSON(http.StatusOK, items)
}
