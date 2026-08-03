package space

// 空间目录（directory）查询。
//
// 目录把「成员花名册」与「Bot 目录」合成一份：一行 = 一条 space_member。相对
// queryMembers 有两处刻意的差异：
//
//  1. 不按 robot.creator_uid 过滤。queryMembers 只让调用者看见自己创建的 bot，
//     导致「别人创建的 bot」在成员列表里完全隐形，客户端不得不再打一次
//     /v1/robot/space_bots 补全。目录的可见范围仅由 Space 成员身份决定——这与
//     space_bots 今天已经返回的集合一致，不是放宽；而本接口额外要求成员身份，
//     所以净效果是收紧。
//  2. robot 表 JOIN 带 status=1，且「是 bot 账号但未启用」的行整行排除。
//     queryMembers 的 JOIN 不带 status 而投影列带，二者口径不一致，会让调用者
//     自己创建的停用 bot 以 robot=0 返回、被前端当成真人渲染。
const (
	directoryTypeAll   = "all"
	directoryTypeHuman = "human"
	directoryTypeBot   = "bot"
)

// directoryExcludedUID 是平台内置的 bot 管理账号，与 space_bots 一致地排除，
// 避免它出现在任何用户可见的目录里。
const directoryExcludedUID = "botfather"

// directoryFromJoin 是 count 与分页查询共用的 FROM/JOIN 片段。
// 三个 LEFT JOIN 全部命中唯一键（user.uid、user_verification 的主键 user_id、
// robot.robot_id 唯一索引），因此不会扇出，一个 uid 恒对应一行。
const directoryFromJoin = `
	FROM space_member sm
	LEFT JOIN user u ON u.uid = sm.uid
	LEFT JOIN user_verification uv ON uv.user_id = sm.uid
	LEFT JOIN robot r ON r.robot_id = sm.uid AND r.status = 1`

// directoryWhereClause 生成目录查询的 WHERE 片段与参数。
//
// count 与分页查询共用同一个构造函数，避免两处条件漂移——那会让 total 与 items
// 对不上，客户端翻页时表现为「永远差几条」这类难查的问题。
func directoryWhereClause(spaceID, listType string) (string, []interface{}) {
	clause := `sm.space_id = ? AND sm.status = 1 AND sm.uid <> ?`
	args := []interface{}{spaceID, directoryExcludedUID}

	// 停用 bot（user.robot=1，但 robot 表没有 status=1 的行）整行排除：它既不该
	// 出现在 bot 里，也不该退化成 human。
	clause += ` AND (IFNULL(u.robot, 0) = 0 OR r.robot_id IS NOT NULL)`

	switch listType {
	case directoryTypeHuman:
		clause += ` AND IFNULL(u.robot, 0) = 0`
	case directoryTypeBot:
		// 上面的排除条件已保证「是 bot 账号」蕴含「已启用」，这里只需筛账号类型。
		clause += ` AND IFNULL(u.robot, 0) = 1`
	}
	return clause, args
}

// directoryOrderBy 是目录的全序。
//
// sm.uid 这个 tiebreaker 是必需的而非装饰：role + created_at 并非唯一，批量导入
// 或同批建号的成员会共享同一组值，缺少唯一末位排序键时 LIMIT/OFFSET 翻页可能
// 漏行或重复行。queryMembers 目前就缺这一项（searchMembers 有）。
const directoryOrderBy = ` ORDER BY sm.role DESC, sm.created_at ASC, sm.uid ASC`

// queryDirectory 返回目录的一页。
func (d *DB) queryDirectory(spaceID, listType string, page, limit uint64) ([]*directoryRowModel, error) {
	where, args := directoryWhereClause(spaceID, listType)
	args = append(args, limit, (page-1)*limit)

	var models []*directoryRowModel
	_, err := d.session.SelectBySql(`
		SELECT sm.*,
			IFNULL(u.name, '') as name,
			IFNULL(uv.real_name, '') as real_name,
			IFNULL(u.robot, 0) as robot,
			CASE WHEN r.robot_id IS NOT NULL THEN 1 ELSE 0 END as active_bot`+
		directoryFromJoin+` WHERE `+where+directoryOrderBy+` LIMIT ? OFFSET ?`,
		args...).Load(&models)
	return models, err
}

// countDirectory 返回过滤后、分页前的总行数，用于客户端的 tab 计数与翻页。
func (d *DB) countDirectory(spaceID, listType string) (int64, error) {
	where, args := directoryWhereClause(spaceID, listType)
	var count int64
	_, err := d.session.SelectBySql(
		`SELECT COUNT(*)`+directoryFromJoin+` WHERE `+where, args...).Load(&count)
	return count, err
}

// queryBotRelations 查询 loginUID 对给定 bot 的好友关系态：friend 命中为 added，
// 否则存在未处理的好友申请为 pending，都没有则由调用方兜底为 not_added。
//
// 只在当前页确有 bot 行时调用——type=human 恒无 bot，整段跳过，不做无谓的 IN 查询。
//
// pending 的取数与 /v1/robot/space_bots 不同，是刻意纠正而非口径漂移。
// space_bots（modules/robot/api.go:1541）查的是
// `SELECT to_uid, status FROM friend_apply WHERE uid = ? AND to_uid IN ?`，两处都错：
//
//  1. 表名。全仓库不存在 friend_apply 表，只有 friend_apply_record
//     （modules/user/sql/20231127000001_user_legacy01.sql）。
//  2. 方向。申请记录以「uid = 被申请方，to_uid = 申请人」存储
//     （见 modules/user/api_friend.go:473 的写入：UID=req.ToUID, ToUID=fromUID），
//     space_bots 的 WHERE 把两者用反了。
//
// 该查询在 space_bots 里以 `_, _ =` 吞掉了错误，所以线上 status 实际只会返回
// added / not_added，pending 是一条走不到的分支。修正 space_bots 会改变它的响应，
// 属于 robot-space-bots-space-isolation 之外的另一个任务；本接口作为新契约，
// 从一开始就按正确的表和方向实现。
func (d *DB) queryBotRelations(loginUID string, botUIDs []string) (map[string]string, error) {
	relations := make(map[string]string, len(botUIDs))
	if loginUID == "" || len(botUIDs) == 0 {
		return relations, nil
	}

	type friendRow struct {
		ToUID string `db:"to_uid"`
	}
	var friends []friendRow
	if _, err := d.session.SelectBySql(
		"SELECT to_uid FROM friend WHERE uid = ? AND to_uid IN ? AND is_deleted = 0",
		loginUID, botUIDs,
	).Load(&friends); err != nil {
		return nil, err
	}
	for _, f := range friends {
		relations[f.ToUID] = directoryRelationAdded
	}

	// status=0 即「未处理」；1 通过 / 2 拒绝都不算 pending。
	type applyRow struct {
		UID string `db:"uid"`
	}
	var applies []applyRow
	if _, err := d.session.SelectBySql(
		"SELECT uid FROM friend_apply_record WHERE to_uid = ? AND uid IN ? AND status = 0",
		loginUID, botUIDs,
	).Load(&applies); err != nil {
		return nil, err
	}
	// 已是好友的不被申请记录覆盖：added 强于 pending。
	for _, a := range applies {
		if _, ok := relations[a.UID]; !ok {
			relations[a.UID] = directoryRelationPending
		}
	}
	return relations, nil
}
