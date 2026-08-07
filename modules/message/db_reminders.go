package message

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
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

// topicChannelSeparator 是子区频道 ID 里父群编号与短 ID 的分隔符：
// channel_id == "<groupNo>____<shortID>"（见 modules/group/api.go 的
// threadSeparator，以及 20260711 迁移里的 CONCAT(t.group_no,'____',t.short_id)）。
const topicChannelSeparator = "____"

// channelLevelVisibility 构造「频道级（uid=”）提醒对本调用方可见」的 SQL 谓词。
//
// 为什么需要它：uid=” 按建表注释表示「提醒项为整个频道内的成员」，也就是说这条
// 提醒的收件人是**该频道的成员**。此前 sync 对这一支没有任何成员校验 —— channelIDs
// 为空时连 channel_id 约束都没有，于是任意登录用户都能把全表的频道级提醒拉走，
// 拿到自己从未加入的频道的 channel_id / publisher / message_id / message_seq。
// channelIDs 由客户端提供，只能用来**收窄**，永远不能充当授权依据。
//
// 覆盖范围只有 Group 与 CommunityTopic 两种频道类型，这是刻意的：
//
//	类型 1 Person / 3 CustomerService / 4 Community / 6 Info 在 octo-server 侧
//	没有可用的成员关系表（会话在 WuKongIM，conversation_extra 只是元数据），
//	一律要求 group_member 会把这些类型的合法提醒静默丢掉 —— 那是功能回归，不是
//	安全收益。而 hasMention 不判频道类型，理论上这些类型也能产生频道级提醒，
//	所以这里是一处**知情保留的残留**，由 TestChannelLevelReminderChannelTypes
//	守卫：一旦有新的写入路径让这些类型真的产生频道级提醒，该测试转红。
//
// memberGroupNos 必须来自成员关系反推（group.IService.ActiveMemberGroupNos），
// 口径 is_deleted=0 AND status=Normal，不可用客户端传入的集合替代。
//
// 实现上用绑定参数做 IN 而不是 JOIN/EXISTS 到 group_member：reminders 已被
// 20260711 迁移强制转成 utf8mb4_general_ci，而 group_member 建表未声明 CHARSET，
// 继承建库默认 —— 两者在部分部署上会落到不同 collation，列对列比较会抛 Error 1267，
// 而用 COLLATE pin 绕开又会让索引失效退化成全表扫（同迁移的注释已记录这个陷阱）。
// 与字面量比较不存在该问题：字面量按列的 collation 强制转换。
func channelLevelVisibility(memberGroupNos []string) (string, []interface{}) {
	grp := common.ChannelTypeGroup.Uint8()
	topic := common.ChannelTypeCommunityTopic.Uint8()
	if len(memberGroupNos) == 0 {
		// 不是任何群的活跃成员：类型 2/5 一条都不可见。不能生成 `IN ()`——那不是
		// 合法 SQL，而 dbr 对空切片的展开行为也不该被依赖。
		return "reminders.channel_type not in (?,?)", []interface{}{grp, topic}
	}
	return "(reminders.channel_type not in (?,?)" +
			" or (reminders.channel_type=? and reminders.channel_id in ?)" +
			" or (reminders.channel_type=? and substring_index(reminders.channel_id,?,1) in ?))",
		[]interface{}{grp, topic, grp, memberGroupNos, topic, topicChannelSeparator, memberGroupNos}
}

/*
*
同步提醒项
@param uid 当前登录用户的uid
@param version 以uid为key的增量版本号
@param limit 数据限制
@param channelIDs 频道集合 查询以频道为目标的提醒项（客户端提供，只收窄不扩权）
@param memberGroupNos 调用方作为活跃成员所属的全部群编号，授权依据，见
channelLevelVisibility

YUJ-1377: the predicate `NOT (uid=” AND publisher=?)` excludes
channel-level broadcasts authored by the viewer itself, so the
sender of `@所有人` does not see their own red-dot. The filter must
live in SQL (not post-filtered in Go) so the LIMIT/version cursor
keeps advancing past hidden self-broadcasts — otherwise a page
fully consumed by the viewer's own broadcasts would return [] and
stall the client's incremental sync.

同理，成员可见性谓词也必须留在 SQL 里：在 Go 侧后置过滤会让游标停在被隐藏的
行上，客户端反复请求同一 version 却永远拿不到新数据。
*/
func (r *remindersDB) sync(uid string, version int64, limit uint64, channelIDs []string, memberGroupNos []string) ([]*remindersDetailModel, error) {
	var models []*remindersDetailModel

	visSQL, visArgs := channelLevelVisibility(memberGroupNos)

	// 频道级分支：先按客户端给的 channelIDs 收窄（可选），再过成员可见性（强制）。
	channelLevel := "reminders.uid='' and " + visSQL
	args := []interface{}{uid}
	if len(channelIDs) > 0 {
		channelLevel = "reminders.uid='' and reminders.channel_id in ? and " + visSQL
		args = append(args, channelIDs)
	}
	args = append(args, visArgs...)
	args = append(args, uid, version)

	where := "(reminders.uid=? or (" + channelLevel + "))" +
		" and not (reminders.uid='' and reminders.publisher=?)" +
		" and reminders.version>?"
	if version == 0 {
		where += " and reminder_done.id is null"
	}

	builder := r.session.
		Select("reminders.*,IF(reminder_done.id is null and reminders.is_deleted=0,0,1) done").
		From("reminders").
		LeftJoin("reminder_done", dbr.Expr("reminders.id=reminder_done.reminder_id and reminder_done.uid=?", uid))
	_, err := builder.Where(where, args...).OrderAsc("version").Limit(limit).Load(&models)
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
