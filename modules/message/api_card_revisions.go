package message

// card-message-interaction P2 D10.5：卡片修订历史查询。
// GET /v1/message/card/revisions?message_id=&channel_id=&channel_type=&limit=&full=
// —— AuthMiddleware → SharedUIDRateLimiter → Space（挂在 /v1/message 组）+ 与
// card/action 同一门禁栈（authorizeCardChannelMember：anti-IDOR 频道绑定 + 存储
// 频道成员资格）+ 生命周期兜底（isCardMessageWithdrawn）+ canonical 可见性
// （cardCanonicalVisibleToViewer：visibles / 过期 / 偏移）—— 可见性 ≥ card/action、
// 与单条读 respondSingleMessage 同口径（PR#549 review B1：修订历史披露多于单次动作，
// 门禁不得更宽）。默认返回摘要列表；full=1 每条非墓碑行附完整可渲染帧信封。墓碑行
// （清除审计）也出现在列表里。

import (
	"encoding/json"
	"strconv"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// getCardRevisions handles GET /v1/message/card/revisions.
func (m *Message) getCardRevisions(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	messageID := c.Query("message_id")
	channelID := c.Query("channel_id")
	if messageID == "" || channelID == "" {
		respondMessageRequestInvalid(c, "message_id")
		return
	}
	channelTypeI, err := strconv.ParseUint(c.Query("channel_type"), 10, 8)
	if err != nil || channelTypeI == 0 {
		respondMessageRequestInvalid(c, "channel_type")
		return
	}
	limit := 0
	if s := c.Query("limit"); s != "" {
		if v, perr := strconv.Atoi(s); perr == nil {
			limit = v
		}
	}
	full := c.Query("full") == "1"

	// D10.5 门禁：与 card/action 同口径（anti-IDOR 绑定 + 存储频道成员）。非卡片/
	// 不存在/频道不匹配 → invalid；非成员 → denied（防枚举）。
	msgM, handled := m.authorizeCardChannelMember(c, loginUID, messageID, channelID, uint8(channelTypeI),
		errcode.ErrMessageCardRevisionInvalid, errcode.ErrMessageCardRevisionDenied)
	if handled {
		return
	}

	// 撤回/删除可见性兜底（PR-C verify P1）：已撤回 / 全局删除 / 操作者本地删除的卡片
	// 无可查询内容历史（D10「revoked message leaves no queryable content history」）。
	// 与 card/action 同口径,且不依赖 revoke 时 best-effort 删除是否成功 —— 双层防御:
	// 即便修订行仍在（删除失败 / is_deleted / user-local-delete 本就不触发删除），
	// 查询也返回空列表。
	if withdrawn, werr := m.isCardMessageWithdrawn(messageID, loginUID); werr != nil {
		m.Error("查询卡片撤回/删除状态失败", zap.Error(werr), zap.String("messageID", messageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	} else if withdrawn {
		c.Response(map[string]interface{}{"revisions": []map[string]interface{}{}})
		return
	}

	// canonical 可见性对齐（PR#549 review B1）：修订历史披露的内容比单次动作触发更多，
	// 门禁必须 ≥ card/action。成员+生命周期之外补第四类可见性 —— visibles 白名单 /
	// 消息过期 / 用户清理偏移 / 频道偏移（cardCanonicalVisibleToViewer，与 card/action
	// 共用）。被 visibles 排除 / 偏移截断 / 已过期的成员看不到卡片本身，也就看不到它的
	// 历史帧：不可见 → 空列表（与 withdrawn 同处理，不泄漏存在性）；查询失败 fail-closed。
	if visible, verr := m.cardCanonicalVisibleToViewer(msgM, loginUID); verr != nil {
		m.Error("查询卡片修订可见性失败", zap.Error(verr), zap.String("messageID", messageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	} else if !visible {
		c.Response(map[string]interface{}{"revisions": []map[string]interface{}{}})
		return
	}

	revs, err := m.cardRevisions.Query(messageID, limit)
	if err != nil {
		m.Error("查询卡片修订历史失败", zap.Error(err), zap.String("messageID", messageID))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}

	items := make([]map[string]interface{}, 0, len(revs))
	for _, r := range revs {
		if r.IsTombstone == 1 {
			items = append(items, map[string]interface{}{
				"tombstone":  true,
				"cleared":    r.ClearedCount,
				"editor_uid": r.EditorUID,
				"edited_at":  r.EditedAt,
			})
			continue
		}
		item := map[string]interface{}{
			"plain":      r.Plain,
			"editor_uid": r.EditorUID,
			"edited_at":  r.EditedAt,
		}
		if r.CardSeq.Valid {
			item["card_seq"] = r.CardSeq.Int64
		}
		if full && r.Content.Valid && r.Content.String != "" {
			// 完整帧信封逐字返回（写入时已过 cardmsg.Validate/Finalize）。
			item["card"] = json.RawMessage(r.Content.String)
		}
		items = append(items, item)
	}
	c.Response(map[string]interface{}{"revisions": items})
}
