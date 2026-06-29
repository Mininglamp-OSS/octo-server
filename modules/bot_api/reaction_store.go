package bot_api

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

// reactionStore is the bot_api-local data access for message reactions.
//
// #111: reactions for bot actors reuse the SAME `reaction_users` table the
// user-facing reaction API writes to (modules/message). We deliberately do NOT
// import message's private messageReactionDB (keeping module DB access private
// is the octo-server convention); instead we own a small, single-file SQL
// surface here. The bot is recorded as the reaction actor via uid=robotID, with
// the bot's display name in the name column so user clients render it normally.
type reactionStore struct {
	session *dbr.Session
	ctx     *config.Context
}

func newReactionStore(ctx *config.Context) *reactionStore {
	return &reactionStore{ctx: ctx, session: ctx.DB()}
}

// reactionRow mirrors the columns of reaction_users that the bot path touches.
// (Same physical table as message.reactionModel; redeclared locally so we don't
// reach into another module's private type.)
type reactionRow struct {
	MessageID   string
	Seq         int64
	ChannelID   string
	ChannelType uint8
	UID         string
	Name        string
	Emoji       string
	IsDeleted   int
	db.BaseModel
}

// queryByActorAndMessage returns the bot's existing reaction row for a message,
// or nil when none exists. (uid, message_id) is effectively unique — the user
// path enforces "one reaction row per actor per message", which we preserve.
//
// Concurrency: this is query-then-write with no row lock and no DB unique
// constraint on (uid, message_id) — the same pattern the user-facing path
// (modules/message addOrCancelReaction) has always used. Two concurrent adds
// for the same (bot, message) could in principle insert two rows. For a
// normally-serial bot actor this is low-probability and no worse than the
// existing user-device race; hardening both paths would mean adding a
// UNIQUE(uid, message_id) index in a later migration (it must also dedupe any
// pre-existing duplicate rows), which is out of scope for this change.
func (s *reactionStore) queryByActorAndMessage(uid, messageID string) (*reactionRow, error) {
	var m *reactionRow
	_, err := s.session.Select("*").
		From("reaction_users").
		Where("uid=? and message_id=?", uid, messageID).
		Load(&m)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *reactionStore) insert(m *reactionRow) error {
	_, err := s.session.InsertInto("reaction_users").
		Columns(util.AttrToUnderscore(m)...).
		Record(m).
		Exec()
	return err
}

func (s *reactionStore) update(m *reactionRow) error {
	_, err := s.session.Update("reaction_users").
		SetMap(map[string]interface{}{
			"seq":        m.Seq,
			"emoji":      m.Emoji,
			"is_deleted": m.IsDeleted,
			"name":       m.Name,
		}).
		Where("id=?", m.Id).
		Exec()
	return err
}

// genSeq mints the next reaction seq for a stored channel id, matching the
// user-facing path (modules/message genMessageReactionSeq) so bot and user
// reactions share one monotonic stream per channel.
func (s *reactionStore) genSeq(storeChannelID string) (int64, error) {
	return s.ctx.GenSeq(common.MessageReactionSeqKey + ":" + storeChannelID)
}
