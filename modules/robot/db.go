package robot

import (
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/config"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/pkg/db"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

type robotDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newBotDB(ctx *config.Context) *robotDB {
	return &robotDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}
func (d *robotDB) queryRobotWithRobtID(robotID string) (*robot, error) {
	var m *robot
	_, err := d.session.Select("*").From("robot").Where("robot_id=?", robotID).Load(&m)
	return m, err
}
func (d *robotDB) queryVaildRobotWithRobtID(robotID string) (*robot, error) {
	var m *robot
	_, err := d.session.Select("*").From("robot").Where("robot_id=? and status=1", robotID).Load(&m)
	return m, err
}

func (d *robotDB) exist(robotID string) (bool, error) {
	var cn int
	err := d.session.Select("count(*)").From("robot").Where("robot_id=? and status=1", robotID).LoadOne(&cn)
	return cn > 0, err
}

func (d *robotDB) insert(m *robot) error {
	_, err := d.session.InsertInto("robot").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

func (d *robotDB) insertTx(m *robot, tx *dbr.Tx) error {
	_, err := tx.InsertInto("robot").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}
func (d *robotDB) insertMenuTx(m *menu, tx *dbr.Tx) error {
	_, err := tx.InsertInto("robot_menu").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

func (d *robotDB) queryWithIDs(robotIDs []string) ([]*robot, error) {
	var list []*robot
	_, err := d.session.Select("*").From("robot").Where("robot_id in ?", robotIDs).Load(&list)
	return list, err
}
func (d *robotDB) queryWithUsernames(usernames []string) ([]*robot, error) {
	var list []*robot
	_, err := d.session.Select("*").From("robot").Where("username in ?", usernames).Load(&list)
	return list, err
}
func (d *robotDB) queryWithUsername(username string) (*robot, error) {
	var rb *robot
	_, err := d.session.Select("*").From("robot").Where("username = ?", username).Load(&rb)
	return rb, err
}

func (d *robotDB) queryVaildRobotIDs(robotIDs []string) ([]string, error) {
	var vaildRobotIDs []string
	_, err := d.session.Select("robot_id").From("robot").Where("robot_id in ?", robotIDs).Load(&vaildRobotIDs)
	return vaildRobotIDs, err
}

// 同步机器人菜单
func (d *robotDB) queryMenusWithRobotIDs(uids []string) ([]*menu, error) {
	var menus []*menu
	_, err := d.session.Select("*").From("robot_menu").Where("robot_id in ?", uids).OrderDir("created_at", false).Load(&menus)
	return menus, err
}

// 修改机器人信息
func (d *robotDB) updateRobotTx(m *robot, tx *dbr.Tx) error {
	_, err := tx.Update("robot").SetMap(map[string]interface{}{
		"version": m.Version,
	}).Where("robot_id=?", m.RobotID).Exec()
	return err
}
func (d *robotDB) updateRobot(m *robot) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"version": m.Version,
		"status":  m.Status,
	}).Where("robot_id=?", m.RobotID).Exec()
	return err
}
func (d *robotDB) queryMenusWithRobotID(robotID string) ([]*menu, error) {
	var menus []*menu
	_, err := d.session.Select("*").From("robot_menu").Where("robot_id=?", robotID).OrderDir("created_at", false).Load(&menus)
	return menus, err
}
func (d *robotDB) deleteMenuWithID(robotID string, id int64, tx *dbr.Tx) error {
	_, err := tx.DeleteFrom("robot_menu").Where("robot_id=? and id=?", robotID, id).Exec()
	return err
}

// queryRobotListPaged 分页查询机器人列表
func (d *robotDB) queryRobotListPaged(pageIndex, pageSize int) ([]*robot, error) {
	var list []*robot
	_, err := d.session.Select("*").From("robot").
		Where("status=1").
		OrderDir("created_at", false).
		Limit(uint64(pageSize)).
		Offset(uint64(pageIndex * pageSize)).
		Load(&list)
	return list, err
}

// queryRobotTotalCount 查询机器人总数
func (d *robotDB) queryRobotTotalCount() (int64, error) {
	var count int64
	err := d.session.Select("count(*)").From("robot").Where("status=1").LoadOne(&count)
	return count, err
}

// queryRobotByBotToken 通过Bot Token查询机器人
func (d *robotDB) queryRobotByBotToken(botToken string) (*robot, error) {
	if botToken == "" {
		return nil, nil
	}
	var m *robot
	_, err := d.session.Select("*").From("robot").Where("bot_token=? and bot_token!='' and status=1", botToken).Load(&m)
	return m, err
}

// updateRobotBotToken 重置机器人的Bot Token
func (d *robotDB) updateRobotBotToken(robotID string, newToken string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"bot_token": newToken,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

// updateRobotInfo 更新机器人信息（管理后台用）
func (d *robotDB) updateRobotInfo(robotID string, fields map[string]interface{}) error {
	_, err := d.session.Update("robot").SetMap(fields).Where("robot_id=?", robotID).Exec()
	return err
}

// deleteRobotSoft 软删除机器人
func (d *robotDB) deleteRobotSoft(robotID string) error {
	_, err := d.session.Update("robot").SetMap(map[string]interface{}{
		"status": 0,
	}).Where("robot_id=?", robotID).Exec()
	return err
}

type menu struct {
	RobotID string // 机器人ID
	CMD     string // 命令
	Remark  string // 命令说明
	Type    string // 命令类型
	db.BaseModel
}
type robot struct {
	AppID        string
	RobotID      string // 机器人唯一ID
	Username     string // 机器人用户名
	InlineOn     int    // 是否开启行内搜索
	Placeholder  string // 输入框占位符，开启行内搜索有效
	Token        string
	Version      int64
	Status       int
	CreatorUID   string // 创建者UID
	Description  string // 机器人描述
	BotToken     string // Bot认证Token
	IMTokenCache string // 缓存的IM Token
	BotCommands  string // 机器人命令列表JSON
	db.BaseModel
}
