package project

import (
	"fmt"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
)

// AI 分身（用户自建的云端 Bot）在项目里的读侧原语。
//
// # 为什么这些查询在 modules/project 而不在 pkg/project
//
// 它们要读 `robot`，而 pkg/project 是一个只依赖 octo-lib 的纯谓词包——modules/group
// 正是靠这一点在不 import modules/project 的前提下拿到项目成员资格。让它去读
// robot 会给它接上 modules/robot 与 modules/botfather 的迁移依赖，把一个"两个模块
// 都能安全依赖的事实"变成"依赖了半棵树的事实"。
//
// 代价是 modules/project 的测试二进制必须装得到 `robot` 表。它靠
// project_external_test.go 引入 internal 才有——这是既有事实，但本文件把它从
// "碰巧成立"变成了"被依赖"，所以在那里显式写明。
//
// # COLLATE
//
// `robot` 与 `user` 都是未声明 COLLATE 的老表，生产库里是 utf8mb4_0900_ai_ci，而
// octo_project* 明确是 utf8mb4_general_ci。跨过去的每一次比较都要显式 COLLATE，
// 否则在生产上报 1267 而在 CI 上一路绿灯（CI 建库时指定了 general_ci）。
// P1 的 TestP1ScansSurviveCollationDrift 是这条纪律的范本。
//
// 方向的选择与 P1 的 I2/I3 扫描一致：把 COLLATE 写在**驱动侧的值**上，让被查的
// octo_project* 索引保持可用。

// agentRow 是一次分身资格判定的输入。
type agentRow struct {
	UID        string `db:"uid"`
	CreatorUID string `db:"creator_uid"`
	Hosting    string `db:"hosting"`
	Robot      int    `db:"robot"`
}

// queryAgentRowsTx 在事务内读出 uids 里"看起来像分身"的那些行。
//
// 只读事实，不做判定——判定在 service 层（eligibleAgentUIDs），因为它还要结合
// Space 席位和系统 bot 白名单，而那两样不在这张表里。
//
// 用 tx：资格判定必须与写入在同一个事务里，否则是一次会过期的读。这与 P0 对
// space_member 的处理是同一条规矩（见 requireSpaceSeatsTx 的注释）。
//
// 不加锁：`robot` 不在本模块声明的锁序里
// （space_member → space → project → group → group_member → octo_project_member），
// 给它加锁会引入一条没人分析过的新边。这里接受的窗口是"分身在判定与提交之间被
// 删除"——D14 让那条路径走 Space 移除工单，工单会关掉刚写下的项目席位，所以窗口
// 是自愈的，而不是需要锁来关闭的。
func (d *DB) queryAgentRowsTx(tx *dbr.Tx, uids []string) (map[string]agentRow, error) {
	out := make(map[string]agentRow, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	var rows []agentRow
	_, err := tx.SelectBySql(
		"SELECT u.uid AS uid, IFNULL(r.creator_uid, '') AS creator_uid, "+
			"IFNULL(r.agent_hosting, '') AS hosting, u.robot AS robot "+
			"FROM `user` u "+
			// LEFT JOIN，不是 INNER：一个 robot=1 但没有 robot 行的 uid（孤儿 bot）
			// 必须能被读到并**判为不合格**，而不是查不到就当"这个 uid 不存在"——
			// 两者在错误码上确实合并成一个（D3），但在日志里要分得开。
			// NO COLLATE: `robot` and `user` are BOTH legacy tables, so they already
			// agree. Adding one between two same-collation columns makes the
			// predicate non-sargable and gives up robot's primary key — the exact
			// mistake the P1 reconcile header warns about in the other direction.
			"LEFT JOIN `robot` r ON r.robot_id = u.uid AND r.status = 1 "+
			"WHERE u.uid IN ?",
		uids,
	).Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("project: query agent rows: %w", err)
	}
	for _, row := range rows {
		out[row.UID] = row
	}
	return out, nil
}

// queryOwnedAgentSeatsTx 读出 ownerUID 名下、当前在这个项目里有活跃席位的分身。
//
// D13「分身跟人走」的输入：一个人的项目席位关闭时，他名下的分身席位一并关闭。
//
// 判定字段是 robot.creator_uid，与群侧 QueryBotsInvitedByUIDTx 同源
// （modules/group/db.go，#354「bot 永远跟随其主人，无角色例外」）。同源不是巧合而是
// 要求：群侧已经在移除一个人时按这个字段带走他的 bot，项目侧若按别的字段判断
// （比如 invite_uid），两边就会对"谁的分身"给出不同答案，于是出现"群里被带走了、
// 项目席位还在"的行——正是 I4 要防的那种。
//
// r.status = 1：没有活跃 robot 行的 bot（孤儿 / 已禁用）不算任何人的分身，与群侧
// 那条 INNER JOIN 的口径一致。它的席位由 I1 / I4 对账报出，不在这里静默处理。
func (d *DB) queryOwnedAgentSeatsTx(tx *dbr.Tx, projectID, ownerUID string) ([]string, error) {
	if projectID == "" || ownerUID == "" {
		return nil, nil
	}
	var uids []string
	_, err := tx.SelectBySql(
		"SELECT pm.uid FROM `octo_project_member` pm "+
			// COLLATE 在**驱动侧的值**上：pm 是 pinned 的 general_ci，robot 是老表，
			// 两者跨 schema，必须显式。写在 pm.uid 上让 robot 的主键仍然可用——
			// 与 P1 对账扫描同一条规则。
			"INNER JOIN `robot` r ON r.robot_id = pm.uid COLLATE utf8mb4_general_ci "+
			"WHERE pm.project_id = ? AND pm.status = ? AND pm.removing = 0 "+
			// creator_uid 比的是一个**字面量**，不是另一张表的列。字面量是可强制
			// 转换的，不会报 1267，所以这里不加 COLLATE——加了只会让 idx_robot_creator_uid
			// 失效，换不到任何安全性。
			"  AND r.creator_uid = ? AND r.status = 1 "+
			// 稳定顺序：级联会逐个开事务处理，固定顺序让并发的两次移除以同样的
			// 顺序碰这些行，少一种死锁形状。
			"ORDER BY pm.uid "+
			// **加锁读，且只锁 pm。**
			//
			// 这次读直接授权紧随其后的写（把这些席位置为 removing=1）。非加锁读
			// answers from the snapshot：本事务的读视图在第一条语句就打开了，于是
			// 一个在那之后提交的新分身席位对这次读不可见，它会被漏掉——人走了、
			// 他的分身席位还活着，正是 D13 要防的那个终局，而且没有任何东西会回来
			// 补上。TestNoWriteAuthorisingAggregateIsANonLockingRead 钉住这条规则，
			// 并且是它先发现了这里的漏洞。
			//
			// FOR UPDATE OF pm 而不是裸 FOR UPDATE：不锁 `robot`。robot 不在本模块
			// 声明的锁序里（space_member → space → project → group → group_member →
			// octo_project_member），锁它等于凭空加一条没人分析过的边。
			// lockSpaceSeatsTx 用 FOR SHARE OF sm 是同一个手法。
			"FOR UPDATE OF pm",
		projectID, MemberStatusActive, ownerUID,
	).Load(&uids)
	if err != nil {
		return nil, fmt.Errorf("project: query owned agent seats: %w", err)
	}
	return uids, nil
}

// queryAgentOwnerTx 读一个 uid 的分身所有者。返回 "" 表示它不是一个活跃的分身
// （不是 bot、没有 robot 行、robot 已禁用）。
//
// D15 用它回答"这个加人请求的目标是不是调用方自己的分身"。
func (d *DB) queryAgentOwnerTx(tx *dbr.Tx, uid string) (string, error) {
	if uid == "" || spacepkg.IsSystemBot(uid) {
		return "", nil
	}
	var owners []string
	_, err := tx.SelectBySql(
		"SELECT IFNULL(r.creator_uid, '') FROM `robot` r "+
			"INNER JOIN `user` u ON u.uid = r.robot_id AND u.robot = 1 "+
			"WHERE r.robot_id = ? AND r.status = 1",
		uid,
	).Load(&owners)
	if err != nil {
		return "", fmt.Errorf("project: query agent owner: %w", err)
	}
	if len(owners) == 0 {
		return "", nil
	}
	return owners[0], nil
}

// countActiveSeatsByKind 分别数活跃席位里的人和分身（D16）。
//
// 一条语句而不是两条：两条 COUNT 之间可以插进一次成员变化，于是
// member_count + agent_count 会不等于配额所数的席位总数，而客户端会拿这两个数去
// 减。条件聚合让两个数出自同一次扫描、同一个读视图。
//
// 只有会话版，没有事务版。它服务的是**响应渲染**，不授权任何写入；一个事务内的
// 版本会被 TestNoWriteAuthorisingAggregateIsANonLockingRead 要求成为加锁读，
// 而为一个纯展示用的计数在成员表上取锁是没有理由的。
//
// LEFT JOIN `user`：没有 user 行的成员必须仍被计入（与 listMembers 的 LEFT JOIN
// 同一个理由——名册和计数不能各说各话），此时 robot 读作 0，计为人。
func (d *DB) countActiveSeatsByKind(projectID string) (humans, agents int, err error) {
	var row struct {
		Humans int `db:"humans"`
		Agents int `db:"agents"`
	}
	err = d.session.SelectBySql(
		// COALESCE around each SUM: SUM over an EMPTY set is NULL, not 0, and
		// scanning NULL into an int errors. An empty active roster is reachable —
		// P0's Space cascade can close the last seat — so without this a project in
		// that state makes every detail read log a warning and fall back.
		"SELECT "+
			"COALESCE(SUM(IF(IFNULL(u.robot, 0) = 1, 0, 1)), 0) AS humans, "+
			"COALESCE(SUM(IF(IFNULL(u.robot, 0) = 1, 1, 0)), 0) AS agents "+
			"FROM `octo_project_member` pm "+
			"LEFT JOIN `user` u ON u.uid = pm.uid COLLATE utf8mb4_general_ci "+
			"WHERE pm.project_id = ? AND pm.status = ? AND pm.removing = 0",
		projectID, MemberStatusActive,
	).LoadOne(&row)
	if err != nil {
		return 0, 0, fmt.Errorf("project: count seats by kind: %w", err)
	}
	return row.Humans, row.Agents, nil
}
