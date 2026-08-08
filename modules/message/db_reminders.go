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
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
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

// topicChannelSeparator 取自 modules/thread —— 子区频道 ID 的所有者包，
// channel_id == "<groupNo>____<shortID>"。不在本包另立常量：仓库里已经有好几处
// 手写副本，再加一处只会让它们更容易漂移（PR#717 review P2-5）。
const topicChannelSeparator = thread.ChannelIDSeparator

// unscopedChannelTypes 是「没有成员来源、按改动前行为原样放行」的频道类型白名单。
// 逐条理由见 channelLevelVisibility 的说明。
//
// 元素类型是 int 而非 uint8：Go 里 []uint8 就是 []byte，dbr 会把它当二进制串插值，
// 生成的不是 IN 列表。这个坑不会报错，只会静默产出一个匹配不上任何行的谓词。
// 类型 1 Person **不在**此列：它有可达的生产者，且当事人可判定，见
// channelLevelVisibility 里的 Person 分支。
var unscopedChannelTypes = []int{
	int(common.ChannelTypeCustomerService.Uint8()), // 3：全仓无生产者
	int(common.ChannelTypeCommunity.Uint8()),       // 4：全仓无生产者
	int(common.ChannelTypeInfo.Uint8()),            // 6：全仓无生产者
}

// channelLevelVisibility 构造「频道级（uid=”）提醒对本调用方可见」的 SQL 谓词。
//
// 为什么需要它：uid=” 按建表注释表示「提醒项为整个频道内的成员」，也就是说这条
// 提醒的收件人是**该频道的成员**。此前 sync 对这一支没有任何成员校验 —— channelIDs
// 为空时连 channel_id 约束都没有，于是任意登录用户都能把全表的频道级提醒拉走，
// 拿到自己从未加入的频道的 channel_id / publisher / message_id / message_seq。
// channelIDs 由客户端提供，只能用来**收窄**，永远不能充当授权依据。
//
// 授权判定覆盖 Group / CommunityTopic（成员关系）与 Person（当事人关系）；
// 只有 3/4/6 走 unscopedChannelTypes 白名单原样放行 —— 它们在全仓无生产者，且没有
// 可用的成员来源，硬套 group_member 只会在它们将来变成真实数据时静默丢弃合法提醒。
//
// **用白名单而不是 `not in (2,5)` 黑名单**，是 PR#717 review P1 的整改：黑名单下
// 「2/5 之外的任何值」都放行，包括将来往 common.ChannelType 里新增的枚举值 ——
// 而 hasMention 不判频道类型，新类型会立刻产生全局可读的频道级行。reviewer 用
// channel_type=99 实测过。改成白名单后未知类型默认**拒绝**：新增枚举值的失败模式
// 从「悄悄泄露」变成「悄悄不可见」，后者才是安全的方向，且要让它可见必须有人显式
// 加进上面的白名单 —— 那是一次留痕的决策，不是默认行为。
//
// memberGroupNos 必须来自成员关系反推（group.IService.ActiveMemberGroupNos），
// 口径 is_deleted=0 AND status=Normal，不可用客户端传入的集合替代。
//
// 实现上用绑定参数做 IN 而不是 JOIN/EXISTS 到 group_member：reminders 已被
// 20260711 迁移强制转成 utf8mb4_general_ci，而 group_member 建表未声明 CHARSET，
// 继承建库默认 —— 两者在部分部署上会落到不同 collation，列对列比较会抛 Error 1267，
// 而用 COLLATE pin 绕开又会让索引失效退化成全表扫（同迁移的注释已记录这个陷阱）。
// 与字面量比较不存在该问题：字面量按列的 collation 强制转换。
func channelLevelVisibility(uid string, memberGroupNos []string) (string, []interface{}) {
	grp := common.ChannelTypeGroup.Uint8()
	topic := common.ChannelTypeCommunityTopic.Uint8()
	person := common.ChannelTypePerson.Uint8()

	// Person 分支：调用方必须是这条 DM 的**收件人**。
	//
	// Person 消息在 webhook 落库前，message.ChannelID 就是收件人 uid —— 它要与
	// FromUID 合成才得到 fakeChannelID（modules/webhook/api.go:291-293），而喂给
	// 监听器的 confMessages 用的是 toConfigMessageResp()，取的是**替换前**的原值。
	// 所以频道级 Person 行的形状是 (channel_id=收件人uid, publisher=发送者uid)。
	//
	// 为什么必须加这一支：DM 发送路径接受 ChannelTypePerson 且不清洗 mention.humans，
	// 所以客户端可以构造 {"mention":{"humans":1}} 的 DM，落出一条频道级 Person 行。
	// 此前它走白名单放行，等于把「收件人是谁」公开给所有登录用户 —— 正是本 PR 要关的
	// 那类社交图谱泄露，只是换了个频道类型（PR#717 review，两位 reviewer 独立发现）。
	//
	// 之前把它归入残留的理由是「硬套 group_member 会误伤合法提醒」，但那个理由**对
	// 类型 1 根本不适用**：不需要任何成员表，收件人 uid 就是 channel_id，发送者也已被
	// 现有的 NOT (uid='' AND publisher=?) 排除。理由与结论对不上，是我的论证错误。
	personVisible := "(reminders.channel_type=? and reminders.channel_id=?)"
	personArgs := []interface{}{person, uid}

	if len(memberGroupNos) == 0 {
		// 不是任何群的活跃成员：类型 2/5 一条都不可见，Person 仍按当事人判定，
		// 其余按白名单放行。unscopedChannelTypes 恒非空，不会生成非法的 `IN ()`。
		return "(reminders.channel_type in ? or " + personVisible + ")",
			append([]interface{}{unscopedChannelTypes}, personArgs...)
	}
	// 子区那一支的分隔符计数条件（`… = 1`）是为了与 thread.ParseChannelID 口径一致：
	// 它用 strings.Split 要求**恰好**两段，"g____a____b" 直接判非法；而单靠
	// substring_index 只取第一段，会把这种畸形 ID 当成属于 g（PR#717 review）。
	//
	// 两侧都用 length()（字节）：除数绑的是 Go 的 len(sep)，也是字节。改动前这里是
	// char_length()（字符），只因分隔符恰好是 ASCII 才等价 —— 一个授权谓词里不该
	// 埋着「只要分隔符一直是 ASCII 就成立」这种隐含前提（PR#717 review P2-2）。
	return "(reminders.channel_type in ?" +
			" or " + personVisible +
			" or (reminders.channel_type=? and reminders.channel_id in ?)" +
			" or (reminders.channel_type=? and substring_index(reminders.channel_id,?,1) in ?" +
			" and (length(reminders.channel_id)-length(replace(reminders.channel_id,?,'')))/?=1))",
		append(append([]interface{}{unscopedChannelTypes}, personArgs...),
			grp, memberGroupNos,
			topic, topicChannelSeparator, memberGroupNos,
			topicChannelSeparator, len(topicChannelSeparator),
		)
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

	// 与 reminderSync 的同名守卫成对存在。空 uid 会让下面的 reminders.uid=? 退化成
	// reminders.uid=''，直接命中全部频道级行且完全跳过成员校验。handler 已经挡了一道，
	// 这里再挡一道：本函数是授权边界，将来多一个调用方就多一次绕过机会，而这种绕过
	// 不会以报错的形式暴露 —— 它只是安静地多返回数据。
	if uid == "" {
		return nil, errors.New("reminders sync: empty caller uid")
	}

	visSQL, visArgs := channelLevelVisibility(uid, memberGroupNos)

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
