package space

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// spaceRoleAdmin is the minimum role allowed to manage space-scoped resources.
const spaceRoleAdmin = 1

// maxMemberSearchPageIndex 兜住 page_index 上限：page_size≤200 时 offset≤2e7，
// 远超任何空间成员数，且避免 (pageIndex-1)*pageSize 在转 uint64 前整型溢出。
const maxMemberSearchPageIndex = 100000

func (s *Space) searchMembers(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceId := c.Param("space_id")

	// 空间必须处于激活态。封禁(status=2)只更新 space 行、不会把 space_member 置为
	// 非激活,因此仅靠成员行 status=1 的 gate 会让封禁/解散空间的 admin 仍能搜出成员
	// (含掩码 email/phone),绕过封禁冻结。显式校验 space.status=1 统一挡掉封禁与解散。
	if s.checkSpaceActive(c, spaceId) {
		return
	}

	member, err := s.db.queryMember(spaceId, loginUID)
	if err != nil {
		s.Error("查询空间成员权限失败", zap.Error(err), zap.String("operator_uid", loginUID), zap.String("space_id", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if member == nil {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotMember, nil, nil)
		return
	}
	if member.Role < spaceRoleAdmin {
		httperr.ResponseErrorL(c, errcode.ErrSpacePermissionDenied, nil, nil)
		return
	}

	pageIndex, pageSize := clampPage(c.GetPage())
	if pageIndex > maxMemberSearchPageIndex {
		pageIndex = maxMemberSearchPageIndex
	}
	// keyword 跨列匹配 name/username/email(明文)/uid；phone 仅匹配后 4 位
	// （见 memberSearchActiveWhere）——前端按手机号检索时只需传后 4 位。
	keyword := strings.TrimSpace(c.Query("keyword"))
	count, err := s.db.countSearchMembers(spaceId, keyword)
	if err != nil {
		s.Error("统计空间成员搜索结果失败", zap.Error(err), zap.String("operator_uid", loginUID), zap.String("space_id", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	if count == 0 {
		c.Response(map[string]interface{}{"count": int64(0), "list": []memberSearchResp{}})
		return
	}
	members, err := s.db.searchMembers(spaceId, keyword, pageIndex, pageSize)
	if err != nil {
		s.Error("查询空间成员搜索结果失败", zap.Error(err), zap.String("operator_uid", loginUID), zap.String("space_id", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}

	resps := make([]memberSearchResp, 0, len(members))
	for _, m := range members {
		resps = append(resps, memberSearchResp{
			UID:       m.UID,
			Name:      m.Name,
			Username:  m.Username,
			Email:     m.Email,
			Phone:     maskPhone(m.Phone),
			Role:      m.Role,
			Robot:     m.Robot,
			CreatedAt: m.CreatedAt.String(),
		})
	}

	s.Info("空间成员搜索", zap.String("operator_uid", loginUID), zap.String("space_id", spaceId), zap.Int64("count", count))
	c.Response(map[string]interface{}{
		"count": count,
		"list":  resps,
	})
}

// maskPhone 将手机号掩码为「前 3 + **** + 后 4」（如 138****5678）。后 4 位也是
// memberSearchActiveWhere 唯一可检索的部分，使「可检索粒度 == 可见粒度」，避免
// admin 通过子串查询逐位探测/重建完整号码。用 rune 切片，避免多字节输入被字节
// 索引切成无效 UTF-8。空值保持空串。
func maskPhone(p string) string {
	r := []rune(p)
	if len(r) == 0 {
		return ""
	}
	if len(r) < 7 {
		return "***"
	}
	return string(r[:3]) + "****" + string(r[len(r)-4:])
}
