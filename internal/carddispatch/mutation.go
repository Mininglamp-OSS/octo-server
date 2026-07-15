package carddispatch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	liblog "github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardrevision"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

var (
	ErrCardMutationInvalid   = errors.New("carddispatch: invalid card mutation")
	ErrCardMutationNotFound  = errors.New("carddispatch: card mutation target not found")
	ErrCardMutationForbidden = errors.New("carddispatch: card mutation sender mismatch")
	ErrCardMutationConflict  = errors.New("carddispatch: stale card mutation")
)

const cardMutationCASMaxAttempts = 5

type CardMutationRequest struct {
	SenderUID   string
	MessageID   string
	MessageSeq  uint32
	ChannelID   string
	ChannelType uint8
	ContentEdit string
}

type CardMutationResult struct {
	Applied bool
	Replay  bool
}

type CardMutationCASRequest struct {
	MessageID   string
	MessageSeq  uint32
	ChannelID   string
	ChannelType uint8
	ContentEdit string
	ContentHash string
	EditedAt    int
	CardSeq     int64
	// StorageChannel is true when ChannelID is already the persisted fake DM
	// channel. The existing Bot API resolves that before entering CAS.
	StorageChannel bool
}

type storedCardMessage struct {
	MessageID  string
	MessageSeq uint32
	FromUID    string
	Payload    []byte
}

type cardMutationWrite struct {
	SenderUID      string
	MessageID      string
	MessageSeq     uint32
	ChannelID      string
	ChannelType    uint8
	ContentEdit    string
	ContentHash    string
	EditedAt       int
	CardSeq        int64
	StorageChannel bool
}

type cardMutationBackend interface {
	Lookup(context.Context, CardMutationRequest) (storedCardMessage, error)
	Lifecycle(messageID string) (revoked bool, deleted bool, err error)
	ContentHashExists(messageID, hash string) (bool, error)
	CASWrite(cardMutationWrite) (conflict bool, replay bool, err error)
	AppendRevision(cardMutationWrite) error
	Sync(cardMutationWrite) error
}

type CardMutator struct {
	backend cardMutationBackend
	logger  interface{ Error(string, ...zap.Field) }
}

func NewCardMutator(ctx *config.Context) *CardMutator {
	mutator := newCardMutator(newProductionMutationBackend(ctx))
	mutator.logger = liblog.NewTLog("CardMutation")
	return mutator
}

func newCardMutator(backend cardMutationBackend) *CardMutator {
	return &CardMutator{backend: backend}
}

func (m *CardMutator) Mutate(ctx context.Context, request CardMutationRequest) (CardMutationResult, error) {
	if m == nil || m.backend == nil || ctx == nil || request.SenderUID == "" || request.MessageID == "" ||
		request.ChannelID == "" || request.ContentEdit == "" {
		return CardMutationResult{}, ErrCardMutationInvalid
	}
	message, err := m.backend.Lookup(ctx, request)
	if err != nil {
		return CardMutationResult{}, err
	}
	if message.MessageID != request.MessageID || message.FromUID != request.SenderUID {
		return CardMutationResult{}, ErrCardMutationForbidden
	}
	if !cardmsg.IsCardRawPayload(message.Payload) || !cardmsg.IsCardContentEdit(request.ContentEdit) {
		return CardMutationResult{}, ErrCardMutationInvalid
	}
	revoked, deleted, err := m.backend.Lifecycle(request.MessageID)
	if err != nil {
		return CardMutationResult{}, fmt.Errorf("carddispatch: query card lifecycle: %w", err)
	}
	if revoked || deleted {
		return CardMutationResult{}, ErrCardMutationNotFound
	}
	normalized, err := cardmsg.NormalizeContentEdit(request.ContentEdit)
	if err != nil {
		return CardMutationResult{}, fmt.Errorf("%w: %v", ErrCardMutationInvalid, err)
	}
	cardSeq, hasCardSeq := cardmsg.CardSeqFromContentEdit(normalized)
	if !hasCardSeq || cardSeq <= 0 {
		return CardMutationResult{}, ErrCardMutationInvalid
	}
	hash := util.MD5(normalized)
	exists, err := m.backend.ContentHashExists(request.MessageID, hash)
	if err != nil {
		return CardMutationResult{}, fmt.Errorf("carddispatch: query mutation hash: %w", err)
	}
	if exists {
		return CardMutationResult{Replay: true}, nil
	}
	write := cardMutationWrite{
		SenderUID: request.SenderUID, MessageID: request.MessageID, MessageSeq: message.MessageSeq,
		ChannelID: request.ChannelID, ChannelType: request.ChannelType, ContentEdit: normalized,
		ContentHash: hash, EditedAt: int(time.Now().Unix()), CardSeq: cardSeq,
	}
	conflict, replay, err := m.backend.CASWrite(write)
	if err != nil {
		return CardMutationResult{}, fmt.Errorf("carddispatch: persist mutation: %w", err)
	}
	if conflict {
		return CardMutationResult{}, ErrCardMutationConflict
	}
	if replay {
		return CardMutationResult{Replay: true}, nil
	}
	// Revision history and CMD fanout are secondary to the authoritative
	// content_edit, matching the existing Bot API semantics.
	if err := m.backend.AppendRevision(write); err != nil && m.logger != nil {
		m.logger.Error("card mutation revision append failed", zap.Error(err), zap.String("message_id", request.MessageID))
	}
	if err := m.backend.Sync(write); err != nil && m.logger != nil {
		m.logger.Error("card mutation CMD sync failed", zap.Error(err), zap.String("message_id", request.MessageID))
	}
	return CardMutationResult{Applied: true}, nil
}

func (m *CardMutator) WriteCAS(request CardMutationCASRequest) (bool, error) {
	if m == nil || m.backend == nil {
		return false, ErrCardMutationInvalid
	}
	conflict, _, err := m.backend.CASWrite(cardMutationWrite{
		MessageID: request.MessageID, MessageSeq: request.MessageSeq, ChannelID: request.ChannelID,
		ChannelType: request.ChannelType, ContentEdit: request.ContentEdit, ContentHash: request.ContentHash,
		EditedAt: request.EditedAt, CardSeq: request.CardSeq, StorageChannel: request.StorageChannel,
	})
	return conflict, err
}

type productionMutationBackend struct {
	ctx       *config.Context
	revisions *cardrevision.Store
}

func newProductionMutationBackend(ctx *config.Context) *productionMutationBackend {
	if ctx == nil {
		return nil
	}
	return &productionMutationBackend{ctx: ctx, revisions: cardrevision.NewStore(ctx.DB())}
}

func (b *productionMutationBackend) Lookup(_ context.Context, request CardMutationRequest) (storedCardMessage, error) {
	if b == nil || b.ctx == nil {
		return storedCardMessage{}, ErrCardMutationInvalid
	}
	if request.MessageSeq > 0 {
		response, err := b.ctx.IMGetWithChannelAndSeqs(request.ChannelID, request.ChannelType, request.SenderUID, []uint32{request.MessageSeq})
		if err != nil {
			return storedCardMessage{}, err
		}
		if response == nil || len(response.Messages) == 0 {
			return storedCardMessage{}, ErrCardMutationNotFound
		}
		message := response.Messages[0]
		return storedCardMessage{
			MessageID: strconv.FormatInt(message.MessageID, 10), MessageSeq: message.MessageSeq,
			FromUID: message.FromUID, Payload: message.Payload,
		}, nil
	}
	messageID, err := strconv.ParseInt(request.MessageID, 10, 64)
	if err != nil {
		return storedCardMessage{}, ErrCardMutationInvalid
	}
	response, err := b.ctx.IMSearchMessages(&config.MsgSearchReq{
		ChannelID: request.ChannelID, ChannelType: request.ChannelType,
		MessageIds: []int64{messageID}, LoginUID: request.SenderUID,
	})
	if err != nil {
		return storedCardMessage{}, err
	}
	if response == nil || len(response.Messages) == 0 {
		return storedCardMessage{}, ErrCardMutationNotFound
	}
	message := response.Messages[0]
	if message.MessageSeq == 0 {
		return storedCardMessage{}, ErrCardMutationNotFound
	}
	return storedCardMessage{
		MessageID: strconv.FormatInt(message.MessageID, 10), MessageSeq: message.MessageSeq,
		FromUID: message.FromUID, Payload: message.Payload,
	}, nil
}

func (b *productionMutationBackend) Lifecycle(messageID string) (bool, bool, error) {
	var lifecycle struct {
		Revoke    int `db:"revoke"`
		IsDeleted int `db:"is_deleted"`
	}
	err := b.ctx.DB().SelectBySql("SELECT `revoke`, is_deleted FROM message_extra WHERE message_id=?", messageID).LoadOne(&lifecycle)
	if err == dbr.ErrNotFound {
		return false, false, nil
	}
	return lifecycle.Revoke == 1, lifecycle.IsDeleted == 1, err
}

func (b *productionMutationBackend) ContentHashExists(messageID, hash string) (bool, error) {
	var count int
	err := b.ctx.DB().Select("count(*)").From("message_extra").Where("message_id=? and content_edit_hash=?", messageID, hash).LoadOne(&count)
	return count > 0, err
}

func (b *productionMutationBackend) CASWrite(write cardMutationWrite) (bool, bool, error) {
	var conflict, replay bool
	var err error
	for attempt := 0; attempt < cardMutationCASMaxAttempts; attempt++ {
		conflict, replay, err = b.casWriteOnce(write)
		if err == nil || !isMutationRetriableLockError(err) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 3 * time.Millisecond)
	}
	return conflict, replay, err
}

func (b *productionMutationBackend) casWriteOnce(write cardMutationWrite) (bool, bool, error) {
	tx, err := b.ctx.DB().Begin()
	if err != nil {
		return false, false, err
	}
	defer tx.RollbackUnlessCommitted()

	var stored struct {
		CardSeq dbr.NullInt64 `db:"card_seq"`
		Hash    string        `db:"content_edit_hash"`
	}
	if err := tx.SelectBySql("SELECT card_seq, content_edit_hash FROM message_extra WHERE message_id=? FOR UPDATE", write.MessageID).LoadOne(&stored); err != nil && err != dbr.ErrNotFound {
		return false, false, err
	}
	if stored.CardSeq.Valid && stored.CardSeq.Int64 >= write.CardSeq {
		if stored.CardSeq.Int64 == write.CardSeq && stored.Hash == write.ContentHash {
			return false, true, nil
		}
		return true, false, nil
	}
	fakeChannelID := write.ChannelID
	if write.ChannelType == common.ChannelTypePerson.Uint8() && !write.StorageChannel {
		fakeChannelID = common.GetFakeChannelIDWith(write.SenderUID, write.ChannelID)
	}
	version, err := b.ctx.GenSeq(fmt.Sprintf("%s:%s", common.MessageExtraSeqKey, fakeChannelID))
	if err != nil {
		return false, false, err
	}
	if _, err := tx.InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version,card_seq) VALUES (?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE content_edit=VALUES(content_edit),content_edit_hash=VALUES(content_edit_hash),edited_at=VALUES(edited_at),version=VALUES(version),card_seq=VALUES(card_seq)",
		write.MessageID, write.MessageSeq, fakeChannelID, write.ChannelType, write.ContentEdit, write.ContentHash, write.EditedAt, version, write.CardSeq,
	).Exec(); err != nil {
		return false, false, err
	}
	return false, false, tx.Commit()
}

func (b *productionMutationBackend) AppendRevision(write cardMutationWrite) error {
	fakeChannelID := write.ChannelID
	if write.ChannelType == common.ChannelTypePerson.Uint8() && !write.StorageChannel {
		fakeChannelID = common.GetFakeChannelIDWith(write.SenderUID, write.ChannelID)
	}
	return b.revisions.AppendFrame(cardrevision.Revision{
		MessageID: write.MessageID, ChannelID: fakeChannelID, ChannelType: write.ChannelType,
		Content: dbr.NewNullString(write.ContentEdit), Plain: cardmsg.PlainFromContentEdit(write.ContentEdit),
		CardSeq: dbr.NewNullInt64(write.CardSeq), EditorUID: write.SenderUID, EditedAt: int64(write.EditedAt),
	})
}

func (b *productionMutationBackend) Sync(write cardMutationWrite) error {
	return b.ctx.SendCMD(config.MsgCMDReq{
		NoPersist: true, ChannelID: write.ChannelID, ChannelType: write.ChannelType,
		FromUID: write.SenderUID, CMD: common.CMDSyncMessageExtra,
	})
}

func isMutationRetriableLockError(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && (mysqlErr.Number == 1213 || mysqlErr.Number == 1205)
}
