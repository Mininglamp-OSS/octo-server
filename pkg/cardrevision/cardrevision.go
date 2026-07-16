// Package cardrevision owns the D10 card revision history side table
// (octo_message_card_revision): the queryable "这张卡以前是什么" surface that
// the D6 full-frame-replacement model (latest-frame-only content_edit) cannot
// answer on its own.
//
// card-message-interaction P2 D10（spec: .octospec/tasks/card-message-interaction/
// brief.md；执行 brief: .octospec/tasks/card-message-p2-revision-history/brief.md）。
//
// 共享表模式（同 message_extra）：写入方是 modules/bot_api（bot 编辑追加帧 /
// 清除写墓碑），读取方是 modules/message（GET 查询 / 撤回删除）。两侧都用本 Store
// 经 ctx.DB() 的 *dbr.Session 访问，schema 知识集中在此，避免跨模块 raw SQL 漂移。
package cardrevision

import (
	"github.com/gocraft/dbr/v2"
)

const table = "octo_message_card_revision"

// CapFrames 每消息保留的非墓碑帧上限（D10.3）。墓碑行是审计标记，不计入 cap，
// 也不被裁剪。
const CapFrames = 20

// MaxQueryLimit 查询接口的返回条数硬上限（防一次拉全表）。
const MaxQueryLimit = 100

// maxPlainRunes 摘要列 plain 的字符上限（对齐 VARCHAR(512)，rune-safe 截断防溢出）。
const maxPlainRunes = 512

// Revision 是一条卡片修订记录：非墓碑帧（IsTombstone=0，带 Content/Plain/CardSeq）
// 或清除墓碑（IsTombstone=1，Content 为 NULL，ClearedCount>0）。字段名经 dbr 默认
// camelCase→snake_case 映射到列（与 messageExtraModel 同约定，无需 db tag）。
type Revision struct {
	ID           int64
	MessageID    string
	ChannelID    string
	ChannelType  uint8
	CardSeq      dbr.NullInt64
	Content      dbr.NullString
	Plain        string
	IsTombstone  int
	ClearedCount int
	EditorUID    string
	EditedAt     int64
	dbBaseCreatedUpdated
}

// dbBaseCreatedUpdated 占位 created_at/updated_at 两列，Load "SELECT *" 时不至于因
// 多出的列报错（dbr 对未知列宽容，但保留字段更明确）。
type dbBaseCreatedUpdated struct {
	CreatedAt dbr.NullTime `db:"created_at"`
	UpdatedAt dbr.NullTime `db:"updated_at"`
}

// Store 是修订表的数据访问层，包裹一个 *dbr.Session（来自 ctx.DB()）。
type Store struct {
	session *dbr.Session
}

// NewStore 构造 Store。
func NewStore(session *dbr.Session) *Store {
	return &Store{session: session}
}

// AppendFrame 追加一条非墓碑帧并把该消息的非墓碑帧裁剪到 CapFrames（保留最新）。
// 由 bot 编辑路径在 content_edit 落库成功后调用（仅非 transient 帧）；返回的 err
// 供调用方记日志——history 是次级面，append 失败不得阻断卡片更新（content_edit 才
// 是权威状态）。
func (s *Store) AppendFrame(rev Revision) error {
	if r := []rune(rev.Plain); len(r) > maxPlainRunes {
		rev.Plain = string(r[:maxPlainRunes]) // rune-safe 截断，避免 VARCHAR(512) 溢出
	}
	if _, err := s.session.InsertBySql(
		"INSERT INTO "+table+" (message_id,channel_id,channel_type,card_seq,content,plain,is_tombstone,cleared_count,editor_uid,edited_at) VALUES (?,?,?,?,?,?,0,0,?,?)",
		rev.MessageID, rev.ChannelID, rev.ChannelType, rev.CardSeq, rev.Content, rev.Plain, rev.EditorUID, rev.EditedAt,
	).Exec(); err != nil {
		return err
	}
	// 裁剪：删除超出 cap 的最旧非墓碑帧（墓碑行不参与）。子查询套派生表以绕过
	// MySQL「不能 DELETE 正在子查询里引用的同表」限制。
	_, err := s.session.DeleteBySql(
		"DELETE FROM "+table+" WHERE message_id=? AND is_tombstone=0 AND id NOT IN "+
			"(SELECT id FROM (SELECT id FROM "+table+" WHERE message_id=? AND is_tombstone=0 ORDER BY id DESC LIMIT ?) t)",
		rev.MessageID, rev.MessageID, CapFrames,
	).Exec()
	return err
}

// Query 返回该消息的修订记录（含墓碑），按 id 倒序（最新在前），最多 limit 条。
func (s *Store) Query(messageID string, limit int) ([]*Revision, error) {
	if limit <= 0 || limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}
	var list []*Revision
	_, err := s.session.SelectBySql(
		"SELECT * FROM "+table+" WHERE message_id=? ORDER BY id DESC LIMIT ?", messageID, limit,
	).Load(&list)
	return list, err
}

// Clear 删除该消息的所有非墓碑帧并写入一条墓碑行（审计：editor+时间+清除帧数）。
// 在单事务内 count→delete→insert，返回被清除的帧数。由 bot 清除端点调用（属主校验
// 在调用方完成）。
func (s *Store) Clear(messageID, channelID string, channelType uint8, editorUID string, editedAt int64) (int, error) {
	tx, err := s.session.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.RollbackUnlessCommitted()

	var cleared int
	if err := tx.SelectBySql("SELECT count(*) FROM "+table+" WHERE message_id=? AND is_tombstone=0", messageID).LoadOne(&cleared); err != nil {
		return 0, err
	}
	if _, err := tx.DeleteBySql("DELETE FROM "+table+" WHERE message_id=? AND is_tombstone=0", messageID).Exec(); err != nil {
		return 0, err
	}
	if _, err := tx.InsertBySql(
		"INSERT INTO "+table+" (message_id,channel_id,channel_type,plain,is_tombstone,cleared_count,editor_uid,edited_at) VALUES (?,?,?,'',1,?,?,?)",
		messageID, channelID, channelType, cleared, editorUID, editedAt,
	).Exec(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return cleared, nil
}

// DeleteByMessageID 删除该消息的全部修订行（含墓碑）。撤回消息时调用（best-effort，
// 撤回不得因此失败）。
func (s *Store) DeleteByMessageID(messageID string) error {
	_, err := s.session.DeleteBySql("DELETE FROM "+table+" WHERE message_id=?", messageID).Exec()
	return err
}
