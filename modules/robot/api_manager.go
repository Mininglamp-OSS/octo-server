package robot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/common"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/config"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/pkg/log"
	"github.com/TangSengDaoDao/TangSengDaoDaoServerLib/pkg/wkhttp"
	"go.uber.org/zap"
)

type Manager struct {
	ctx *config.Context
	log.Log
	db *robotDB
}

func NewManager(ctx *config.Context) *Manager {
	return &Manager{
		ctx: ctx,
		Log: log.NewTLog("robotManager"),
		db:  newBotDB(ctx),
	}
}

// 路由配置
func (m *Manager) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/manager", m.ctx.AuthMiddleware(r))
	{
		auth.GET("/robot/menus", m.list)                                 // 机器人菜单
		auth.DELETE("/robot/:robot_id/:id", m.delete)                    // 删除某个机器人菜单
		auth.PUT("/robot/status/:robot_id/:status", m.updateRobotStatus) // 修改机器人状态

		auth.GET("/robots", m.robotList)                                // 机器人列表（分页）
		auth.GET("/robots/:robot_id", m.robotDetail)                   // 机器人详情
		auth.PUT("/robots/:robot_id", m.robotUpdate)                   // 编辑机器人
		auth.DELETE("/robots/:robot_id", m.robotDelete)                // 删除机器人
		auth.POST("/robots/:robot_id/revoke_token", m.robotRevokeToken) // 重置Token
	}
}

// 查询某个机器人菜单
func (m *Manager) list(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		c.ResponseError(err)
		return
	}
	robotID := c.Query("robot_id")
	if robotID == "" {
		c.ResponseError(errors.New("机器人ID不能为空"))
		return
	}
	list, err := m.db.queryMenusWithRobotID(robotID)
	if err != nil {
		c.ResponseError(errors.New("查询机器人菜单错误"))
		return
	}
	resps := make([]*robotMenu, 0)
	if len(list) == 0 {
		c.Response(resps)
		return
	}

	for _, menu := range list {
		resps = append(resps, &robotMenu{
			Id:        menu.Id,
			CMD:       menu.CMD,
			Remark:    menu.Remark,
			Type:      menu.Type,
			RobotID:   menu.RobotID,
			CreatedAt: menu.CreatedAt.String(),
			UpdatedAt: menu.UpdatedAt.String(),
		})
	}
	c.Response(resps)
}

func (m *Manager) delete(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}
	robot_id := c.Param("robot_id")
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if robot_id == "" {
		c.ResponseError(errors.New("机器人ID不能为空"))
		return
	}
	robot, err := m.db.queryRobotWithRobtID(robot_id)
	if err != nil {
		c.ResponseError(errors.New("查询操作的机器人错误"))
		return
	}
	if robot == nil {
		c.ResponseError(errors.New("操作的机器人不存在"))
		return
	}
	tx, err := m.db.session.Begin()
	if err != nil {
		m.Error("数据库事物开启失败", zap.Error(err))
		c.ResponseError(errors.New("数据库事物开启失败"))
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			panic(err)
		}
	}()
	err = m.db.deleteMenuWithID(robot_id, id, tx)
	if err != nil {
		tx.Rollback()
		m.Error("删除机器人菜单失败", zap.Error(err))
		c.ResponseError(errors.New("删除机器人菜单失败"))
		return
	}
	robot.Version = m.ctx.GenSeq(common.RobotSeqKey)
	err = m.db.updateRobotTx(robot, tx)
	if err != nil {
		tx.Rollback()
		m.Error("修改机器人版本号错误", zap.Error(err))
		c.ResponseError(errors.New("修改机器人版本号错误"))
		return
	}
	err = tx.Commit()
	if err != nil {
		tx.RollbackUnlessCommitted()
		m.Error("数据库事物提交失败", zap.Error(err))
		c.ResponseError(errors.New("数据库事物提交失败"))
		return
	}
	c.ResponseOK()
}

// 启用或禁用机器人
func (m *Manager) updateRobotStatus(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}
	robot_id := c.Param("robot_id")
	status, _ := strconv.ParseInt(c.Param("status"), 10, 64)

	if robot_id == "" {
		c.ResponseError(errors.New("机器人ID不能为空"))
		return
	}
	robot, err := m.db.queryRobotWithRobtID(robot_id)
	if err != nil {
		c.ResponseError(errors.New("查询操作的机器人错误"))
		return
	}
	robot.Status = int(status)
	if robot == nil {
		c.ResponseError(errors.New("操作的机器人不存在"))
		return
	}
	err = m.db.updateRobot(robot)
	if err != nil {
		c.ResponseError(errors.New("修改机器人状态信息错误"))
		return
	}
	c.ResponseOK()
}

type robotMenu struct {
	Id        int64  `json:"id"`
	CMD       string `json:"cmd"`
	Remark    string `json:"remark"`
	Type      string `json:"type"`
	RobotID   string `json:"robot_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ========== 机器人管理端点 ==========

// 机器人列表（分页）
func (m *Manager) robotList(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		c.ResponseError(err)
		return
	}
	pageIndex, _ := strconv.Atoi(c.Query("page_index"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}

	list, err := m.db.queryRobotListPaged(pageIndex, pageSize)
	if err != nil {
		m.Error("查询机器人列表失败", zap.Error(err))
		c.ResponseError(errors.New("查询机器人列表失败"))
		return
	}
	count, err := m.db.queryRobotTotalCount()
	if err != nil {
		m.Error("查询机器人总数失败", zap.Error(err))
		c.ResponseError(errors.New("查询机器人总数失败"))
		return
	}

	resps := make([]*robotListResp, 0, len(list))
	for _, r := range list {
		resps = append(resps, &robotListResp{
			RobotID:     r.RobotID,
			Username:    r.Username,
			Status:      r.Status,
			CreatorUID:  r.CreatorUID,
			Description: r.Description,
			CreatedAt:   r.CreatedAt.String(),
			UpdatedAt:   r.UpdatedAt.String(),
		})
	}
	c.Response(map[string]interface{}{
		"count": count,
		"list":  resps,
	})
}

// 机器人详情
func (m *Manager) robotDetail(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		c.ResponseError(err)
		return
	}
	robotID := c.Param("robot_id")
	if robotID == "" {
		c.ResponseError(errors.New("机器人ID不能为空"))
		return
	}
	r, err := m.db.queryRobotWithRobtID(robotID)
	if err != nil {
		m.Error("查询机器人详情失败", zap.Error(err))
		c.ResponseError(errors.New("查询机器人详情失败"))
		return
	}
	if r == nil {
		c.ResponseError(errors.New("机器人不存在"))
		return
	}
	c.Response(&robotDetailResp{
		RobotID:     r.RobotID,
		Username:    r.Username,
		Status:      r.Status,
		CreatorUID:  r.CreatorUID,
		Description: r.Description,
		BotToken:    r.BotToken,
		BotCommands: r.BotCommands,
		CreatedAt:   r.CreatedAt.String(),
		UpdatedAt:   r.UpdatedAt.String(),
	})
}

// 编辑机器人信息
func (m *Manager) robotUpdate(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}
	robotID := c.Param("robot_id")
	if robotID == "" {
		c.ResponseError(errors.New("机器人ID不能为空"))
		return
	}

	var req robotUpdateReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("数据格式有误"))
		return
	}

	fields := make(map[string]interface{})
	if req.Description != nil {
		fields["description"] = *req.Description
	}
	if req.Status != nil {
		fields["status"] = *req.Status
	}

	if len(fields) == 0 {
		c.ResponseError(errors.New("没有需要更新的字段"))
		return
	}

	err = m.db.updateRobotInfo(robotID, fields)
	if err != nil {
		m.Error("更新机器人信息失败", zap.Error(err))
		c.ResponseError(errors.New("更新机器人信息失败"))
		return
	}
	c.ResponseOK()
}

// 删除机器人
func (m *Manager) robotDelete(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}
	robotID := c.Param("robot_id")
	if robotID == "" {
		c.ResponseError(errors.New("机器人ID不能为空"))
		return
	}
	err = m.db.deleteRobotSoft(robotID)
	if err != nil {
		m.Error("删除机器人失败", zap.Error(err))
		c.ResponseError(errors.New("删除机器人失败"))
		return
	}
	c.ResponseOK()
}

// 重置机器人Token
func (m *Manager) robotRevokeToken(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}
	robotID := c.Param("robot_id")
	if robotID == "" {
		c.ResponseError(errors.New("机器人ID不能为空"))
		return
	}

	newToken := "bf_" + randomHexStr(16)
	err = m.db.updateRobotBotToken(robotID, newToken)
	if err != nil {
		m.Error("重置Token失败", zap.Error(err))
		c.ResponseError(errors.New("重置Token失败"))
		return
	}
	c.Response(map[string]interface{}{
		"bot_token": newToken,
	})
}

func randomHexStr(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type robotListResp struct {
	RobotID     string `json:"robot_id"`
	Username    string `json:"username"`
	Status      int    `json:"status"`
	CreatorUID  string `json:"creator_uid"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type robotDetailResp struct {
	RobotID     string `json:"robot_id"`
	Username    string `json:"username"`
	Status      int    `json:"status"`
	CreatorUID  string `json:"creator_uid"`
	Description string `json:"description"`
	BotToken    string `json:"bot_token"`
	BotCommands string `json:"bot_commands"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type robotUpdateReq struct {
	Description *string `json:"description"`
	Status      *int    `json:"status"`
}
