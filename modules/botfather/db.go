package botfather

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/gocraft/dbr/v2"
)

type botfatherDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newBotfatherDB(ctx *config.Context) *botfatherDB {
	return &botfatherDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

type robotModel struct {
	AppID        string
	RobotID      string
	Username     string
	InlineOn     int
	Placeholder  string
	Token        string
	Version      int64
	Status       int
	CreatorUID   string
	Description  string
	BotToken     string
	IMTokenCache string
	BotCommands  string
	AutoApprove   int    // 0=需要审批 1=自动通过
	AccessMode    int    // 0=需要审批 1=自动通过 2=禁止申请
	AgentPlatform string // AI Agent 平台名称（如 OpenClaw）
	AgentVersion  string // Agent 平台版本号
	PluginVersion string // Octo 插件版本号
	// BoundAgentRef 占用方不透明标签（如 octopush:agent_xxx）；空=空闲。
	BoundAgentRef string
	// BoundAt 占用时间；timestamp NULL，未占用时无效。用 NullTime 承接 NULL，
	// 否则 Select("*") 把 NULL bound_at 扫进 string 会报错，殃及所有 robot 查询。
	BoundAt dbr.NullTime
	db.BaseModel
}

// queryRobotByBotToken 通过BotToken查询机器人
func (d *botfatherDB) queryRobotByBotToken(botToken string) (*robotModel, error) {
	if botToken == "" {
		return nil, nil
	}
	var m *robotModel
	_, err := d.session.Select("*").From("robot").Where("bot_token=? and bot_token!='' and status=1", botToken).Load(&m)
	return m, err
}

// queryRobotByRobotID 通过RobotID查询机器人
func (d *botfatherDB) queryRobotByRobotID(robotID string) (*robotModel, error) {
	var m *robotModel
	_, err := d.session.Select("*").From("robot").Where("robot_id=?", robotID).Load(&m)
	return m, err
}

// queryRobotsByCreatorUID 查询某个用户创建的所有机器人
func (d *botfatherDB) queryRobotsByCreatorUID(creatorUID string) ([]*robotModel, error) {
	var list []*robotModel
	_, err := d.session.Select("*").From("robot").Where("creator_uid=? and status=1", creatorUID).Load(&list)
	return list, err
}

// queryRobotsByCreatorUIDAndSpaceID 查询某用户在指定 Space 下创建的机器人
func (d *botfatherDB) queryRobotsByCreatorUIDAndSpaceID(creatorUID, spaceID string) ([]*robotModel, error) {
	var list []*robotModel
	_, err := d.session.SelectBySql(
		"SELECT r.* FROM robot r INNER JOIN space_member sm ON sm.uid=r.robot_id AND sm.space_id=? AND sm.status=1 WHERE r.creator_uid=? AND r.status=1",
		spaceID, creatorUID,
	).Load(&list)
	return list, err
}

// insertRobotTx 插入机器人（事务）
func (d *botfatherDB) insertRobotTx(m *robotModel, tx *dbr.Tx) error {
	_, err := tx.InsertInto("robot").Columns(
		"app_id", "robot_id", "username", "token", "version", "status",
		"creator_uid", "description", "bot_token", "im_token_cache", "bot_commands",
		"auto_approve",
	).Values(
		m.AppID, m.RobotID, m.Username, m.Token, m.Version, m.Status,
		m.CreatorUID, m.Description, m.BotToken, m.IMTokenCache, m.BotCommands,
		m.AutoApprove,
	).Exec()
	return err
}

// updateRobotIMTokenCache 更新机器人的IM Token缓存
func (d *botfatherDB) updateRobotIMTokenCache(robotID string, imToken string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"im_token_cache": imToken,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// updateRobotBotToken 重置机器人的Bot Token
func (d *botfatherDB) updateRobotBotToken(robotID string, newToken string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"bot_token": newToken,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// updateRobotName 更新机器人名称（需要同时更新user表）
func (d *botfatherDB) updateRobotDescription(robotID string, description string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"description": description,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// updateRobotAgentInfo 更新机器人的 Agent 运行时信息
func (d *botfatherDB) updateRobotAgentInfo(robotID, agentPlatform, agentVersion, pluginVersion string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"agent_platform": agentPlatform,
		"agent_version":  agentVersion,
		"plugin_version": pluginVersion,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// deleteRobot 软删除机器人（status=0）并清除 username 以释放标识符供复用。
func (d *botfatherDB) deleteRobot(robotID string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"status":   0,
		"username": "",
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// queryRobotList 分页查询机器人列表（后台管理用）
func (d *botfatherDB) queryRobotList(pageIndex, pageSize int) ([]*robotModel, error) {
	var list []*robotModel
	_, err := d.session.Select("*").From("robot").
		Where("status=1").
		OrderDir("created_at", false).
		Limit(uint64(pageSize)).
		Offset(uint64(pageIndex * pageSize)).
		Load(&list)
	return list, err
}

// queryRobotCount 查询机器人总数
func (d *botfatherDB) queryRobotCount() (int64, error) {
	var count int64
	err := d.session.Select("count(*)").From("robot").Where("status=1").LoadOne(&count)
	return count, err
}

// queryRobotCountByCreator 查询某用户创建的机器人数量
func (d *botfatherDB) queryRobotCountByCreator(creatorUID string) (int64, error) {
	var count int64
	err := d.session.Select("count(*)").From("robot").Where("creator_uid=? and status=1", creatorUID).LoadOne(&count)
	return count, err
}

// updateBotCommands 更新机器人命令列表
func (d *botfatherDB) updateBotCommands(robotID string, botCommands string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"bot_commands": botCommands,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// existRobotByUsername 检查用户名是否被现存记录占用。
// 已删除的 Bot 会在 deleteRobot 中清空 username，因此不会阻止标识符复用。
func (d *botfatherDB) existRobotByUsername(username string) (bool, error) {
	var count int
	err := d.session.Select("count(*)").From("robot").Where("username=?", username).LoadOne(&count)
	return count > 0, err
}

func (d *botfatherDB) queryAllActiveRobots() ([]*robotModel, error) {
	var models []*robotModel
	_, err := d.session.Select("*").From("robot").Where("status=1 AND bot_token != ''").Load(&models)
	return models, err
}

// ========== User API Key ==========

// queryActiveUserAPIKeyByKey 通过明文 API Key 查询 active（status=1）记录（鉴权链路）。
// 当前 PR 无生产路径把 key 置为 revoked（status=0）；status=1 过滤是为 PR2 的撤销
// 能力预留的前瞻防御，届时被撤销的 key 在此即时失效。存量行经迁移 DEFAULT 1 回填，
// 行为不变。
func (d *botfatherDB) queryActiveUserAPIKeyByKey(apiKey string) (*userAPIKeyModel, error) {
	if apiKey == "" {
		return nil, nil
	}
	var m *userAPIKeyModel
	_, err := d.session.Select("*").From("user_api_key").
		Where("api_key=? AND status=?", apiKey, userAPIKeyStatusActive).Load(&m)
	return m, err
}

// queryActiveUserAPIKey 按 (uid, space_id, client_id) 三元组查询未撤销的 key。
// spaceID="" 对应无 Space 绑定的 legacy 行（client_id 由迁移 DEFAULT 回填）。
func (d *botfatherDB) queryActiveUserAPIKey(uid, spaceID, clientID string) (*userAPIKeyModel, error) {
	var m *userAPIKeyModel
	_, err := d.session.Select("*").From("user_api_key").
		Where("uid=? AND space_id=? AND client_id=? AND status=?", uid, spaceID, clientID, userAPIKeyStatusActive).
		Load(&m)
	return m, err
}

// insertUserAPIKey 插入用户API Key（含绑定的 Space ID 与 client_id 维度）。
func (d *botfatherDB) insertUserAPIKey(uid, apiKey, spaceID, clientID string) error {
	_, err := d.session.InsertInto("user_api_key").
		Columns("uid", "api_key", "space_id", "client_id").
		Values(uid, apiKey, spaceID, clientID).Exec()
	return err
}

// querySpaceNameByID 查询Space名称
func (d *botfatherDB) querySpaceNameByID(spaceID string) (string, error) {
	var name string
	err := d.session.SelectBySql("SELECT name FROM space WHERE id=? AND status=1", spaceID).LoadOne(&name)
	return name, err
}

// queryRobotByRobotIDAndCreator 查询指定创建者的Bot
func (d *botfatherDB) queryRobotByRobotIDAndCreator(robotID, creatorUID string) (*robotModel, error) {
	var m *robotModel
	_, err := d.session.Select("*").From("robot").Where("robot_id=? AND creator_uid=? AND status=1", robotID, creatorUID).Load(&m)
	return m, err
}

// queryRobotByUsernameActive 查询活跃的Bot（用于用户名冲突检测）
func (d *botfatherDB) queryRobotByUsernameActive(username string) (*robotModel, error) {
	var m *robotModel
	_, err := d.session.Select("*").From("robot").Where("username=? AND status=1", username).Load(&m)
	return m, err
}

// isBotInSpace 检查 bot 是否属于指定 Space
func (d *botfatherDB) isBotInSpace(robotID string, spaceID string) (bool, error) {
	var count int
	_, err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM space_member sm "+
			"INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1 "+
			"WHERE sm.uid=? AND sm.space_id=? AND sm.status=1",
		robotID, spaceID,
	).Load(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// querySpaceIDByRobotID returns the active Space ID for the given bot.
// Checks both space_member.status=1 and space.status=1.
//
// Mininglamp-OSS/octo-server#36: deterministic ORDER BY (Option C). When the
// bot is a member of multiple active Spaces, the earliest joined wins, with
// `space_id` as a tie-breaker. This makes the result stable across calls
// instead of engine-dependent. Voice context resolution (`api_voice.go`) is
// the only caller in this package and accepts the first match — keeping the
// SQL signature unchanged means no caller code needs to move.
func (d *botfatherDB) querySpaceIDByRobotID(robotID string) (string, error) {
	var spaceID string
	err := d.session.SelectBySql(
		"SELECT sm.space_id FROM space_member sm INNER JOIN space s ON s.space_id = sm.space_id WHERE sm.uid=? AND sm.status=1 AND s.status=1 ORDER BY sm.created_at ASC, sm.space_id ASC LIMIT 1",
		robotID,
	).LoadOne(&spaceID)
	return spaceID, err
}

// ========== Bot 占用 / 绑定 ==========

// bindRobotCAS 行级 CAS 占用 Bot：仅当 Bot 空闲（bound_agent_ref='')或已被
// 同一 agentRef 占用（幂等）时才写入。返回受影响行数：1=占用成功，0=未命中
// （不存在 / 非本 creator / 已被他人占用），由调用方复查区分。
//
// 互斥完全由这条 UPDATE 的 WHERE 在 DB 层保证，无需额外锁——多个 Agent 并发抢
// 同一空闲 Bot 时只有一条能把 bound_agent_ref 从 '' 改走，其余 affected=0。
func (d *botfatherDB) bindRobotCAS(robotID, creatorUID, agentRef string) (int64, error) {
	res, err := d.session.UpdateBySql(
		"UPDATE robot SET bound_agent_ref=?, bound_at=NOW() "+
			"WHERE robot_id=? AND creator_uid=? AND status=1 "+
			"AND (bound_agent_ref='' OR bound_agent_ref=?)",
		agentRef, robotID, creatorUID, agentRef,
	).Exec()
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// unbindRobotCAS 行级 CAS 释放占用：仅当 Bot 已空闲（幂等）或当前占用方正是
// agentRef 时才清空。返回受影响行数；affected=0 时由调用方复查区分「已空闲（幂等
// 成功）」「被他人占用（拒绝）」「不存在 / 非本 creator」。
//
// 归属校验是互斥不变量的关键一环：uk_ 按 (uid, space, client) 维度，同一用户同一
// client 的所有 Agent 共享一把 key，若释放不校验 agent_ref，Agent B 就能用同一把
// key 把 Agent A 的占用清掉再自占——绕过 bind 的抢占互斥。故释放与占用对称地用
// agent_ref 做 CAS。
func (d *botfatherDB) unbindRobotCAS(robotID, creatorUID, agentRef string) (int64, error) {
	res, err := d.session.UpdateBySql(
		"UPDATE robot SET bound_agent_ref='', bound_at=NULL "+
			"WHERE robot_id=? AND creator_uid=? AND status=1 "+
			"AND (bound_agent_ref='' OR bound_agent_ref=?)",
		robotID, creatorUID, agentRef,
	).Exec()
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
