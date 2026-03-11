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
	AutoApprove  int // 0=需要审批 1=自动通过
	AccessMode   int // 0=需要审批 1=自动通过 2=禁止申请
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

// deleteRobot 删除机器人（软删除 - 设置status=0）
func (d *botfatherDB) deleteRobot(robotID string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"status": 0,
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

// existRobotByUsername 检查用户名是否已被占用（包括已软删除的记录）。
// 设计意图：移除 status=1 过滤条件，防止新 Bot 复用已删除 Bot 的 username，
// 避免因 username 唯一约束导致的数据冲突和潜在的消息路由混乱。
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
