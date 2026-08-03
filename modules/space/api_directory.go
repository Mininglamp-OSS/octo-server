package space

import (
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

const (
	// directoryDefaultLimit 是不传 limit 时的页大小，保持保守，避免随手调用把
	// 整个 Space 拉回去。
	directoryDefaultLimit = 50
	// directoryMaxLimit 与 listMembers 的上限对齐。这个值刻意留得高：通讯录需要
	// 一次性拿到整个 Space 才能建本地拼音索引和 A-Z 字母索引，线上最大的 Space
	// 约 1700 人 + 4400 bot ≈ 6100 行，若把上限压到几百，客户端会从今天的 2 个
	// 请求变成十几个，且 OFFSET 会一路推到六千行——纯属自伤。响应始终带 total，
	// 想翻页的调用方随时可以翻。
	directoryMaxLimit = 10000
)

// listDirectory 返回 Space 通讯录目录：全部 / 只看真人 / 只看 AI。
//
// 它取代客户端「拉 space/{id}/members + 拉 robot/space_bots 再本地合并去重计数」
// 的做法。那套做法不是客户端多此一举，而是两个接口对 bot 的口径不重合所致：
// members 只返回调用者自己创建的 bot，space_bots 返回全空间的 bot，前者是后者的
// 真子集。本接口只用一套口径，见 db_directory.go 的说明。
func (s *Space) listDirectory(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := c.Param("space_id")

	// SpaceMiddleware 已按「路径参数优先」解析并校验了成员身份（见
	// pkg/space/middleware.go 的 resolveSpaceID）。这里再断言一次「中间件校验的
	// Space」与「本 handler 即将查询的 Space」是同一个：若日后中间件解析顺序被
	// 改动、或本路由漏挂中间件，请求在此 fail-closed，而不是悄悄按一个未经校验的
	// space_id 出数据。校验失败复用 not_member，不额外区分原因（反枚举）。
	if spacepkg.GetSpaceID(c) != spaceID {
		httperr.ResponseErrorL(c, errcode.ErrSpaceNotMember, nil, nil)
		return
	}

	listType := c.Query("type")
	if listType == "" {
		listType = directoryTypeAll
	}
	switch listType {
	case directoryTypeAll, directoryTypeHuman, directoryTypeBot:
	default:
		// 不静默回落到 all：静默回落会让客户端的拼写错误表现为「筛选没生效」，
		// 比直接报错更难查。
		respondSpaceRequestInvalid(c, "type")
		return
	}

	page, _ := strconv.ParseUint(c.Query("page"), 10, 64)
	if page <= 0 {
		page = 1
	}
	limit, _ := strconv.ParseUint(c.Query("limit"), 10, 64)
	if limit <= 0 {
		limit = directoryDefaultLimit
	}
	if limit > directoryMaxLimit {
		limit = directoryMaxLimit
	}

	total, err := s.db.countDirectory(spaceID, listType)
	if err != nil {
		s.Error("查询空间目录总数失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}

	rows, err := s.db.queryDirectory(spaceID, listType, page, limit)
	if err != nil {
		s.Error("查询空间目录失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}

	// relation 只在本页确有 bot 行时才查。
	botUIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.isBot() {
			botUIDs = append(botUIDs, row.UID)
		}
	}
	var relations map[string]string
	if len(botUIDs) > 0 {
		relations, err = s.db.queryBotRelations(loginUID, botUIDs)
		if err != nil {
			// 不做 fail-soft：关系态查不到就整体报错，而不是把所有 bot 一律标成
			// not_added——后者会让用户看到「已添加的 AI 显示成未添加」的错数据。
			s.Error("查询空间目录 Bot 关系态失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
			return
		}
	}

	items := make([]directoryItemResp, 0, len(rows))
	for _, row := range rows {
		item := directoryItemResp{
			UID:       row.UID,
			Name:      row.DisplayName(),
			Kind:      directoryKindHuman,
			Role:      row.Role,
			CreatedAt: row.CreatedAt.String(),
		}
		if row.isBot() {
			relation := relations[row.UID]
			if relation == "" {
				relation = directoryRelationNotAdded
			}
			item.Kind = directoryKindBot
			item.Bot = &directoryBotResp{Relation: relation}
		}
		items = append(items, item)
	}

	c.Response(directoryResp{
		Total: int(total),
		Page:  int(page),
		Limit: int(limit),
		Items: items,
	})
}
