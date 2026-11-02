package conversation_ext

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

type FoldedChat struct {
	ChannelID   string `json:"channel_id" db:"channel_id"`
	ChannelType uint8  `json:"channel_type" db:"channel_type"`
}

type foldStore interface {
	ListFolded(uid, spaceID string) ([]FoldedChat, error)
	SetFolded(uid, spaceID string, chat FoldedChat, folded bool) error
}

func (d *DB) ListFolded(uid, spaceID string) ([]FoldedChat, error) {
	rows := make([]FoldedChat, 0)
	_, err := d.session.SelectBySql("SELECT channel_id, channel_type FROM user_folded_chat WHERE uid=? AND space_id=?", uid, spaceID).Load(&rows)
	return rows, err
}

func (d *DB) SetFolded(uid, spaceID string, chat FoldedChat, folded bool) error {
	if !folded {
		_, err := d.session.DeleteFrom("user_folded_chat").Where("uid=? AND space_id=? AND channel_type=? AND channel_id=?", uid, spaceID, chat.ChannelType, chat.ChannelID).Exec()
		return err
	}
	_, err := d.session.InsertBySql("INSERT IGNORE INTO user_folded_chat (uid, space_id, channel_type, channel_id) VALUES (?, ?, ?, ?)", uid, spaceID, chat.ChannelType, chat.ChannelID).Exec()
	return err
}

// Reuse access checks only; folding must never mutate follow membership.
func (s *Service) authorizeFold(uid, spaceID string, chat FoldedChat) error {
	if chat.ChannelType == targetTypeGroup {
		checker := s.getChannelAuthChecker()
		if checker == nil {
			return ErrDefaultFollowedGuardNotConfigured
		}
		return checker.AuthorizeChannelFollow(uid, spaceID, chat.ChannelID)
	}
	groupNo, shortID, err := parseThreadChannelID(chat.ChannelID)
	if err != nil {
		return err
	}
	checker := s.getThreadAuthChecker()
	if checker == nil {
		return ErrDefaultFollowedGuardNotConfigured
	}
	return checker.AuthorizeThreadFollow(uid, spaceID, groupNo, shortID)
}

type foldedAPI struct {
	db        foldStore
	authorize func(uid, spaceID string, chat FoldedChat) error
}

func foldForbidden(c *wkhttp.Context) {
	code, ok := codes.Lookup("err.shared.auth.forbidden")
	if !ok {
		panic("missing shared forbidden code")
	}
	httperr.ResponseErrorL(c, code, nil, nil)
}

func foldIdentity(c *wkhttp.Context) (string, string, bool) {
	uid, spaceID := c.GetLoginUID(), spacepkg.GetSpaceID(c)
	if uid == "" || spaceID == "" {
		foldForbidden(c)
		return "", "", false
	}
	return uid, spaceID, true
}

func (f *foldedAPI) list(c *wkhttp.Context) {
	uid, spaceID, ok := foldIdentity(c)
	if !ok {
		return
	}
	rows, err := f.db.ListFolded(uid, spaceID)
	if err != nil {
		log.NewTLog("FoldedChats").Error("list folded chats", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
		return
	}
	c.JSON(http.StatusOK, rows)
}

func (f *foldedAPI) set(c *wkhttp.Context) {
	uid, spaceID, ok := foldIdentity(c)
	if !ok {
		return
	}
	var req struct {
		FoldedChat
		Folded *bool `json:"folded"`
	}
	if err := c.BindJSON(&req); err != nil || req.Folded == nil || strings.TrimSpace(req.ChannelID) == "" || len(req.ChannelID) > 100 || (req.ChannelType != targetTypeGroup && req.ChannelType != targetTypeThread) {
		httperr.ResponseErrorL(c, errcode.ErrMessageRequestInvalid, nil, nil)
		return
	}
	// Unfold removes only this caller's preference, including a departed/deleted chat.
	if *req.Folded {
		if req.ChannelType == targetTypeThread {
			if _, _, err := parseThreadChannelID(req.ChannelID); err != nil {
				httperr.ResponseErrorL(c, errcode.ErrMessageRequestInvalid, nil, nil)
				return
			}
		}
		if err := f.authorize(uid, spaceID, req.FoldedChat); err != nil {
			if errors.Is(err, ErrChannelForbidden) || errors.Is(err, ErrThreadForbidden) {
				foldForbidden(c)
			} else {
				log.NewTLog("FoldedChats").Error("authorize folded chat", zap.Error(err))
				httperr.ResponseErrorL(c, errcode.ErrMessageQueryFailed, nil, nil)
			}
			return
		}
	}
	if err := f.db.SetFolded(uid, spaceID, req.FoldedChat, *req.Folded); err != nil {
		log.NewTLog("FoldedChats").Error("save folded chat", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrMessageStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}
