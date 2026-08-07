package carddispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	liblog "github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/internal/msgextraseq"
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

	// ErrCardMutationTooLarge 是「帧合法但存不下」——它刻意 wrap ErrCardMutationInvalid，
	// 这样既给调用方/测试一个可区分的哨兵，又让既有的 ErrCardMutationInvalid 映射
	// （bot_api → ErrBotAPICardInvalid）继续成立，不必新增对外错误码。
	ErrCardMutationTooLarge = fmt.Errorf("%w: frame exceeds the persistence column", ErrCardMutationInvalid)
)

// maxContentEditBytes 是卡片帧落库的字节上限，等于承接它的两列的物理宽度：
// message_extra.content_edit（MySQL TEXT，modules/message/sql/20220414000001）与
// octo_message_card_revision.content（TEXT，20250708000002）。TEXT 是 65,535
// **字节**，与字符集无关；utf8mb4 下一个 CJK 字符占 3 字节，一个被 JSON 转义的
// `<` 占 6 字节，所以码点级的 schema 上限换算不出这个数。
//
// 为什么闸设在这里，而不是靠调低各模板的展示上限：帧的字节数由「模板固定骨架 +
// 调用方填的自由字符串 + Finalize 追加的 plain 副本」三项相加，压任何单一字段都压不住
// 它（实测 ai.reasoning-process@0.4.0 把 thought 压到 1 码点，最坏帧仍是列宽的 107%，
// 主导项是 13 条 action 的 tool/detail 加 plain）。而且 @0.3.0 的字节已冻结、根本改不动，
// 它在自身上限下就已经是 121%。所以唯一能同时覆盖既有版本、当前版本和未来模板的位置，
// 就是写入前的这一道边界。
//
// 闸的作用是把 MySQL 的 `Data too long`（严格模式 → 500）或静默截断成非法 JSON
// （非严格模式 → 客户端渲染不出的坏帧）换成一个确定的、带字节数的拒绝。
const maxContentEditBytes = 65535

// NormalizeFrameForPersistence 是「一帧卡片能否落库」的唯一判据，所有在写入前重新校验
// 帧的路径都必须走它：CardMutator.Mutate、以及 pkg/cardtmpl 的 CardUpdater.Append /
// ReplaceView。返回的是 canonical 字节（cardmsg 已重算权威 plain），可直接写库。
//
// 为什么要有这个函数而不是各调用点自己拼：PR#712 的 P0 就是各点自己拼出来的 ——
// 渲染侧和回写侧对「同一帧是否合法」给出了不同答案，于是卡片发得出去、编辑必失败。
// 校验规则一旦有两份，它们就会漂移；这里让它只有一份。
func NormalizeFrameForPersistence(contentEdit string) (string, error) {
	normalized, err := cardmsg.NormalizeContentEdit(contentEdit)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCardMutationInvalid, err)
	}
	// 列宽闸。normalized 是 message_extra.content_edit 与
	// octo_message_card_revision.content 共同写入的那一份字节（已含 Finalize 追加的
	// plain —— 它能把帧再撑大一半，量 cardmsg.Validate 之前的渲染结果会显著低估），
	// 所以在这里量一次就覆盖两张表。见 maxContentEditBytes 的说明。
	if len(normalized) > maxContentEditBytes {
		return "", fmt.Errorf("%w: %d B > %d B",
			ErrCardMutationTooLarge, len(normalized), maxContentEditBytes)
	}
	return normalized, nil
}

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

// CardMutationTarget identifies an existing message using only authoritative
// server-side values. MessageSeq is an optional lookup optimization.
type CardMutationTarget struct {
	SenderUID   string
	MessageID   string
	MessageSeq  uint32
	ChannelID   string
	ChannelType uint8
}

// CardMutationSnapshot is the current effective type-17 frame. Envelope is
// content_edit when present, otherwise the immutable original payload.
type CardMutationSnapshot struct {
	Envelope json.RawMessage
	CardSeq  int64
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
	EffectiveContent(messageID string) (content string, cardSeq int64, found bool, err error)
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

// Snapshot returns the active effective card after applying the same sender and
// lifecycle gates as Mutate. It exists for read-modify-write operations such as
// CardUpdater.Append; the subsequent Mutate still performs all checks again and
// owns the CAS.
func (m *CardMutator) Snapshot(ctx context.Context, target CardMutationTarget) (CardMutationSnapshot, error) {
	if m == nil || m.backend == nil || ctx == nil || target.SenderUID == "" || target.MessageID == "" || target.ChannelID == "" {
		return CardMutationSnapshot{}, ErrCardMutationInvalid
	}
	request := CardMutationRequest{
		SenderUID: target.SenderUID, MessageID: target.MessageID, MessageSeq: target.MessageSeq,
		ChannelID: target.ChannelID, ChannelType: target.ChannelType,
	}
	message, err := m.backend.Lookup(ctx, request)
	if err != nil {
		return CardMutationSnapshot{}, err
	}
	if message.MessageID != target.MessageID || message.FromUID != target.SenderUID {
		return CardMutationSnapshot{}, ErrCardMutationForbidden
	}
	if !cardmsg.IsCardRawPayload(message.Payload) {
		return CardMutationSnapshot{}, ErrCardMutationInvalid
	}
	revoked, deleted, err := m.backend.Lifecycle(target.MessageID)
	if err != nil {
		return CardMutationSnapshot{}, fmt.Errorf("carddispatch: query card lifecycle: %w", err)
	}
	if revoked || deleted {
		return CardMutationSnapshot{}, ErrCardMutationNotFound
	}
	content, seq, found, err := m.backend.EffectiveContent(target.MessageID)
	if err != nil {
		return CardMutationSnapshot{}, fmt.Errorf("carddispatch: query effective card: %w", err)
	}
	if !found {
		return CardMutationSnapshot{Envelope: append(json.RawMessage(nil), message.Payload...)}, nil
	}
	if !cardmsg.IsCardContentEdit(content) {
		return CardMutationSnapshot{}, ErrCardMutationInvalid
	}
	parsedSeq, ok := cardmsg.CardSeqFromContentEdit(content)
	if !ok || parsedSeq <= 0 || parsedSeq != seq {
		return CardMutationSnapshot{}, ErrCardMutationInvalid
	}
	return CardMutationSnapshot{Envelope: json.RawMessage(content), CardSeq: seq}, nil
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
	normalized, err := NormalizeFrameForPersistence(request.ContentEdit)
	if err != nil {
		if errors.Is(err, ErrCardMutationTooLarge) && m.logger != nil {
			m.logger.Error("card mutation frame exceeds the persistence column",
				zap.String("message_id", request.MessageID),
				zap.String("sender_uid", request.SenderUID),
				zap.Int("frame_bytes", len(request.ContentEdit)),
				zap.Int("column_bytes", maxContentEditBytes))
		}
		return CardMutationResult{}, err
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
	if !cardmsg.TransientFromContentEdit(normalized) {
		if err := m.backend.AppendRevision(write); err != nil && m.logger != nil {
			m.logger.Error("card mutation revision append failed", zap.Error(err), zap.String("message_id", request.MessageID))
		}
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
	seq       *msgextraseq.Store
}

func newProductionMutationBackend(ctx *config.Context) *productionMutationBackend {
	if ctx == nil {
		return nil
	}
	return &productionMutationBackend{ctx: ctx, revisions: cardrevision.NewStore(ctx.DB()), seq: msgextraseq.New(ctx)}
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

func (b *productionMutationBackend) EffectiveContent(messageID string) (string, int64, bool, error) {
	var current struct {
		Content dbr.NullString `db:"content_edit"`
		CardSeq dbr.NullInt64  `db:"card_seq"`
	}
	err := b.ctx.DB().Select("content_edit", "card_seq").From("message_extra").Where("message_id=?", messageID).LoadOne(&current)
	if err == dbr.ErrNotFound {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	if !current.Content.Valid || current.Content.String == "" {
		return "", 0, false, nil
	}
	if !current.CardSeq.Valid {
		return current.Content.String, 0, true, nil
	}
	return current.Content.String, current.CardSeq.Int64, true, nil
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
		// Whole-transaction retry on a retriable lock error (1213/1205): each
		// casWriteOnce opens a fresh tx, so re-running is safe. Report the retry
		// on the shared allocator metric (reserve_total result=retry). With the
		// #627 D4 mode-aware lock order this loop is a backstop (1205 lock-wait
		// under load / any unforeseen source) — the transactional card-vs-seq
		// same-message deadlock is removed at the root, not merely retried here.
		b.seq.ObserveReserveRetry()
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

	fakeChannelID := write.ChannelID
	if write.ChannelType == common.ChannelTypePerson.Uint8() && !write.StorageChannel {
		fakeChannelID = common.GetFakeChannelIDWith(write.SenderUID, write.ChannelID)
	}

	// #627 D4: pick the lock order by allocator mode so the GLOBAL order is uniform
	// across every message_extra writer and cannot deadlock.
	//   - transactional: reserve the channel sequence FIRST, then take the
	//     message-row lock (state → channel-seq → message-row). This matches the
	//     non-card writers (all seq-first), so no AB-BA cycle exists. Version order
	//     == commit order gives monotonicity; a stale-frame CAS reject rolls the tx
	//     back, reverting the sequence advance, so no version is consumed.
	//   - legacy: the version comes from GenSeq (no serializing lock), so the
	//     message-row lock must come FIRST to preserve single-process version
	//     monotonicity (PR#548, TestBotCardEditMixedFrameVersionMonotonicIM).
	mode, err := b.seq.Mode(tx)
	if err != nil {
		return false, false, err
	}

	var version int64
	if mode == msgextraseq.ModeTransactional {
		versions, rerr := b.seq.ReserveTx(tx, fakeChannelID, write.ChannelType, 1)
		if rerr != nil {
			return false, false, rerr
		}
		version = versions[0]
		if conflict, replay, done, cerr := b.casGate(tx, write); cerr != nil || done {
			return conflict, replay, cerr
		}
	} else {
		if conflict, replay, done, cerr := b.casGate(tx, write); cerr != nil || done {
			return conflict, replay, cerr
		}
		versions, rerr := b.seq.ReserveTx(tx, fakeChannelID, write.ChannelType, 1)
		if rerr != nil {
			return false, false, rerr
		}
		version = versions[0]
	}

	if _, err := tx.InsertBySql(
		"INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,content_edit,content_edit_hash,edited_at,version,card_seq) VALUES (?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE content_edit=VALUES(content_edit),content_edit_hash=VALUES(content_edit_hash),edited_at=VALUES(edited_at),version=VALUES(version),card_seq=VALUES(card_seq)",
		write.MessageID, write.MessageSeq, fakeChannelID, write.ChannelType, write.ContentEdit, write.ContentHash, write.EditedAt, version, write.CardSeq,
	).Exec(); err != nil {
		return false, false, err
	}
	return false, false, tx.Commit()
}

// casGate takes the message-row lock FOR UPDATE and evaluates the card_seq CAS.
// done=true means the caller must stop before writing — either a stale-frame
// conflict (conflict=true) or an idempotent replay of the same frame
// (replay=true). done=false means the frame is accepted and the caller writes.
// Only an accepted frame ever reaches the INSERT, so a rejected frame consumes no
// version (in transactional mode the caller's rollback reverts the seq advance).
//
// Pre-existing hazard (predates #627, not a regression): the FOR UPDATE below
// targets message_id, which for a brand-new card message does not yet exist, so
// under REPEATABLE READ it takes a gap lock. Two first-frame writes on different
// channels whose message_ids are index-adjacent can gap-lock-deadlock (1213) on
// their subsequent INSERTs. This is masked by the outer CASWrite bounded retry
// (a fresh tx re-reads an existing row → record lock, no gap), so it self-heals;
// eliminating it (upsert-first, à la msgextraseq.lockSequenceRow) is a follow-up.
func (b *productionMutationBackend) casGate(tx *dbr.Tx, write cardMutationWrite) (conflict, replay, done bool, err error) {
	var stored struct {
		CardSeq dbr.NullInt64 `db:"card_seq"`
		Hash    string        `db:"content_edit_hash"`
	}
	if serr := tx.SelectBySql("SELECT card_seq, content_edit_hash FROM message_extra WHERE message_id=? FOR UPDATE", write.MessageID).LoadOne(&stored); serr != nil && serr != dbr.ErrNotFound {
		return false, false, true, serr
	}
	if stored.CardSeq.Valid && stored.CardSeq.Int64 >= write.CardSeq {
		if stored.CardSeq.Int64 == write.CardSeq && stored.Hash == write.ContentHash {
			return false, true, true, nil
		}
		return true, false, true, nil
	}
	return false, false, false, nil
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
