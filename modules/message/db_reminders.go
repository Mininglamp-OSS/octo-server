package message

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

type remindersDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newRemindersDB(ctx *config.Context) *remindersDB {
	return &remindersDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

func (r *remindersDB) inserts(models []*remindersModel) error {
	tx, err := r.session.Begin()
	if err != nil {
		return errors.New("开启事物错误")
	}
	defer func() {
		if err := recover(); err != nil {
			tx.RollbackUnlessCommitted()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	for _, m := range models {
		_, err := tx.InsertInto("reminders").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (r *remindersDB) deleteWithChannel(channelID string, channelType uint8, messageID int64, version int64) error {
	_, err := r.session.Update("reminders").Set("is_deleted", 1).Set("version", version).Where("channel_id=? and channel_type=? and message_id=?", channelID, channelType, messageID).Exec()
	return err
}

func (r *remindersDB) deleteWithChannelAndUIDTx(channelID string, channelType uint8, uid string, messageID int64, version int64, tx *dbr.Tx) error {
	_, err := tx.Update("reminders").Set("is_deleted", 1).Set("version", version).Where("channel_id=? and channel_type=? and uid=? and message_id=?", channelID, channelType, uid, messageID).Exec()
	return err
}
func (r *remindersDB) queryWithUIDAndChannel(uid string, channelID string, channelType uint8, messageSeq uint32) ([]*remindersDetailModel, error) {
	var list []*remindersDetailModel
	builder := r.session.Select("reminders.*,IF(reminder_done.id is null and reminders.is_deleted=0,0,1) done").From("reminders").LeftJoin("reminder_done", dbr.Expr("reminders.id=reminder_done.reminder_id and reminder_done.uid=?", uid))
	// YUJ-1377: same publisher exclusion as sync() — channel-level
	// (uid='') reminders authored by the viewer themselves must not
	// be returned. Per-uid rows (uid=?) are unaffected.
	_, err := builder.Where("(reminders.uid=?  or  ( reminders.uid='' and reminders.channel_id=? and reminders.channel_type=?))  and not (reminders.uid='' and reminders.publisher=?)  and reminders.message_seq<=? and reminder_done.id is null", uid, channelID, channelType, uid, messageSeq).Load(&list)
	return list, err
}

/*
*
同步提醒项
@param uid 当前登录用户的uid
@param version 以uid为key的增量版本号
@param limit 数据限制
@param channelIDs 频道集合 查询以频道为目标的提醒项

YUJ-1377: the predicate `NOT (uid=” AND publisher=?)` excludes
channel-level broadcasts authored by the viewer itself, so the
sender of `@所有人` does not see their own red-dot. The filter must
live in SQL (not post-filtered in Go) so the LIMIT/version cursor
keeps advancing past hidden self-broadcasts — otherwise a page
fully consumed by the viewer's own broadcasts would return [] and
stall the client's incremental sync.
*/
func (r *remindersDB) sync(uid string, version int64, limit uint64, channelIDs []string) ([]*remindersDetailModel, error) {
	var models []*remindersDetailModel
	var err error
	if version == 0 {
		builder := r.session.Select("reminders.*,IF(reminder_done.id is null and reminders.is_deleted=0,0,1) done").From("reminders").LeftJoin("reminder_done", dbr.Expr("reminders.id=reminder_done.reminder_id and reminder_done.uid=?", uid))

		if len(channelIDs) == 0 {
			_, err = builder.Where("(reminders.uid=?  or   reminders.uid='')  and not (reminders.uid='' and reminders.publisher=?)  and reminders.version>? and reminder_done.id is null", uid, uid, version).OrderAsc("version").Limit(limit).Load(&models)
		} else {
			_, err = builder.Where("(reminders.uid=?  or  ( reminders.uid='' and reminders.channel_id in ?))  and not (reminders.uid='' and reminders.publisher=?)  and reminders.version>? and reminder_done.id is null", uid, channelIDs, uid, version).OrderAsc("version").Limit(limit).Load(&models)
		}
	} else {
		build := r.session.Select("reminders.*,IF(reminder_done.id is null and reminders.is_deleted=0,0,1) done").From("reminders").LeftJoin("reminder_done", dbr.Expr("reminders.id=reminder_done.reminder_id and reminder_done.uid=?", uid))
		if len(channelIDs) == 0 {
			_, err = build.Where("(reminders.uid=?  or  reminders.uid='')  and not (reminders.uid='' and reminders.publisher=?)  and reminders.version>?", uid, uid, version).OrderAsc("version").Limit(limit).Load(&models)
		} else {
			_, err = build.Where("(reminders.uid=?  or  ( reminders.uid='' and reminders.channel_id in ?))  and not (reminders.uid='' and reminders.publisher=?)  and reminders.version>?", uid, channelIDs, uid, version).OrderAsc("version").Limit(limit).Load(&models)
		}

	}
	return models, err
}

func (r *remindersDB) insertDonesTx(ids []int64, uid string, tx *dbr.Tx) error {
	if len(ids) == 0 {
		return nil
	}

	// 对 reminder_id 进行排序，确保事务按相同顺序获取锁，避免死锁
	sortedIds := make([]int64, len(ids))
	copy(sortedIds, ids)
	sort.Slice(sortedIds, func(i, j int) bool {
		return sortedIds[i] < sortedIds[j]
	})

	// 使用批量插入来减少锁持有时间
	if len(sortedIds) > 1 {
		return r.batchInsertDonesTx(sortedIds, uid, tx)
	}

	// 单个插入
	_, err := tx.InsertBySql("insert ignore into reminder_done(reminder_id,uid) values(?,?)", sortedIds[0], uid).Exec()
	if err != nil {
		r.ctx.Error("insertDonesTx failed", zap.Error(err), zap.Int64("reminder_id", sortedIds[0]), zap.String("uid", uid))
		return err
	}
	return nil
}

// 批量插入方法，减少锁持有时间
func (r *remindersDB) batchInsertDonesTx(sortedIds []int64, uid string, tx *dbr.Tx) error {
	if len(sortedIds) == 0 {
		return nil
	}

	// 使用 strings.Builder 一次性构建 SQL 占位符，避免循环中的字符串拼接
	var placeholders strings.Builder
	valueArgs := make([]any, 0, len(sortedIds)*2)

	for i, id := range sortedIds {
		if i > 0 {
			placeholders.WriteString(",")
		}
		placeholders.WriteString("(?,?)")
		valueArgs = append(valueArgs, id, uid)
	}

	sql := "INSERT IGNORE INTO reminder_done(reminder_id,uid) VALUES " + placeholders.String()
	_, err := tx.InsertBySql(sql, valueArgs...).Exec()
	if err != nil {
		r.ctx.Error("batchInsertDonesTx failed", zap.Error(err), zap.String("uid", uid), zap.Int("count", len(sortedIds)))
		return err
	}
	return nil
}

func (r *remindersDB) updateVersionTx(version int64, id int64, tx *dbr.Tx) error {
	_, err := tx.Update("reminders").Set("version", version).Where("id=?", id).Exec()
	return err
}

type remindersDetailModel struct {
	Done int
	remindersModel
}

type remindersModel struct {
	ChannelID    string
	ChannelType  uint8
	ClientMsgNo  string
	MessageSeq   uint32
	MessageID    string
	ReminderType int
	Publisher    string
	UID          string
	Text         string
	Data         string
	IsLocate     int
	Version      int64
	IsDeleted    int
	db.BaseModel
}
