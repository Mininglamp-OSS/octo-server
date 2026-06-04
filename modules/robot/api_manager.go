package robot

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	pkgutil "github.com/Mininglamp-OSS/octo-server/pkg/util"
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
		auth.GET("/robots/:robot_id", m.robotDetail)                    // 机器人详情
		auth.PUT("/robots/:robot_id", m.robotUpdate)                    // 编辑机器人
		auth.DELETE("/robots/:robot_id", m.robotDelete)                 // 删除机器人
		auth.POST("/robots/:robot_id/revoke_token", m.robotRevokeToken) // 重置Token
	}
}

// 查询某个机器人菜单
func (m *Manager) list(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	robotID := c.Query("robot_id")
	if robotID == "" {
		respondRobotRequestInvalid(c, "robot_id")
		return
	}
	list, err := m.db.queryMenusWithRobotID(robotID)
	if err != nil {
		m.Error("查询机器人菜单失败", zap.Error(err), zap.String("robot_id", robotID))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
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
		respondManagerForbidden(c)
		return
	}
	robot_id := c.Param("robot_id")
	id := pkgutil.ParseInt64OrDefault(c.Param("id"), 0)
	if robot_id == "" {
		respondRobotRequestInvalid(c, "robot_id")
		return
	}
	robot, err := m.db.queryRobotWithRobtID(robot_id)
	if err != nil {
		m.Error("查询操作的机器人失败", zap.Error(err), zap.String("robot_id", robot_id))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if robot == nil {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	tx, err := m.db.session.Begin()
	if err != nil {
		m.Error("数据库事物开启失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	err = m.db.deleteMenuWithID(robot_id, id, tx)
	if err != nil {
		tx.Rollback()
		m.Error("删除机器人菜单失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	genSeqVal, err := m.ctx.GenSeq(common.RobotSeqKey)
	if err != nil {
		m.Error("生成机器人版本号失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	robot.Version = genSeqVal
	err = m.db.updateRobotTx(robot, tx)
	if err != nil {
		tx.Rollback()
		m.Error("修改机器人版本号错误", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	err = tx.Commit()
	if err != nil {
		tx.RollbackUnlessCommitted()
		m.Error("数据库事物提交失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// 启用或禁用机器人
func (m *Manager) updateRobotStatus(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	robot_id := c.Param("robot_id")
	status := pkgutil.ParseInt64OrDefault(c.Param("status"), 0)

	if robot_id == "" {
		respondRobotRequestInvalid(c, "robot_id")
		return
	}
	robot, err := m.db.queryRobotWithRobtID(robot_id)
	if err != nil {
		m.Error("查询操作的机器人失败", zap.Error(err), zap.String("robot_id", robot_id))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if robot == nil {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	robot.Status = int(status)
	err = m.db.updateRobot(robot)
	if err != nil {
		m.Error("修改机器人状态信息失败", zap.Error(err), zap.String("robot_id", robot_id))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
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
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	pageIndex := pkgutil.AtoiOrDefault(c.Query("page_index"), 1)
	pageSize := pkgutil.AtoiOrDefault(c.Query("page_size"), 20)
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	if pageIndex < 0 {
		pageIndex = 0
	}

	list, err := m.db.queryRobotListPaged(pageIndex, pageSize)
	if err != nil {
		m.Error("查询机器人列表失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	count, err := m.db.queryRobotTotalCount()
	if err != nil {
		m.Error("查询机器人总数失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
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
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	robotID := c.Param("robot_id")
	if robotID == "" {
		respondRobotRequestInvalid(c, "robot_id")
		return
	}
	r, err := m.db.queryRobotWithRobtID(robotID)
	if err != nil {
		m.Error("查询机器人详情失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if r == nil {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
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
		respondManagerForbidden(c)
		return
	}
	robotID := c.Param("robot_id")
	if robotID == "" {
		respondRobotRequestInvalid(c, "robot_id")
		return
	}

	var req robotUpdateReq
	if err := c.BindJSON(&req); err != nil {
		respondRobotRequestInvalid(c, "")
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
		httperr.ResponseErrorL(c, errcode.ErrRobotNoFieldsToUpdate, nil, nil)
		return
	}

	err = m.db.updateRobotInfo(robotID, fields)
	if err != nil {
		m.Error("更新机器人信息失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// 删除机器人
func (m *Manager) robotDelete(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	robotID := c.Param("robot_id")
	if robotID == "" {
		respondRobotRequestInvalid(c, "robot_id")
		return
	}

	// 先清理 IM 连接和缓存，再做软删除
	if err := m.cleanupBotConnection(robotID); err != nil {
		m.Error("清理机器人连接失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}

	err = m.db.deleteRobotSoft(robotID)
	if err != nil {
		m.Error("删除机器人失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}
	c.ResponseOK()
}

// cleanupBotConnection 清理机器人的IM连接、缓存和事件队列
func (m *Manager) cleanupBotConnection(robotID string) error {
	// 1. 更新 IM Token，旧连接立即失效
	newIMToken := util.GenerUUID()
	_, err := m.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         robotID,
		Token:       newIMToken,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if err != nil {
		return fmt.Errorf("更新IM Token失败: %w", err)
	}

	// 2. 清空缓存的 IM Token
	m.db.updateRobotIMTokenCache(robotID, "")

	// 3. 清除心跳 Redis key
	heartbeatKey := fmt.Sprintf("bot:heartbeat:%s", robotID)
	m.ctx.GetRedisConn().Del(heartbeatKey)

	// 4. 清除事件队列 Redis key
	eventKey := fmt.Sprintf("robotEvent:%s", robotID)
	m.ctx.GetRedisConn().Del(eventKey)

	return nil
}

// 重置机器人Token
func (m *Manager) robotRevokeToken(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	robotID := c.Param("robot_id")
	if robotID == "" {
		respondRobotRequestInvalid(c, "robot_id")
		return
	}

	newToken, err := m.generateUniqueBotToken()
	if err != nil {
		m.Error("生成Token失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotTokenGenFailed, nil, nil)
		return
	}
	err = m.db.updateRobotBotToken(robotID, newToken)
	if err != nil {
		m.Error("重置Token失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}

	// 撤销旧 IM Token，踢掉现有连接
	if err := m.cleanupBotConnection(robotID); err != nil {
		m.Error("清理机器人连接失败", zap.Error(err))
		// bot_token 已更新，连接清理失败不阻塞返回，但记录错误
	}

	c.Response(map[string]interface{}{
		"bot_token": newToken,
	})
}

func randomHexStr(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand.Read failed: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// generateUniqueBotToken 生成唯一的Bot Token（最多重试3次）
func (m *Manager) generateUniqueBotToken() (string, error) {
	for i := 0; i < 3; i++ {
		hexStr, err := randomHexStr(16)
		if err != nil {
			return "", fmt.Errorf("生成随机Token失败: %w", err)
		}
		token := "bf_" + hexStr
		existing, err := m.db.queryRobotByBotToken(token)
		if err != nil {
			return "", fmt.Errorf("检查Token唯一性失败: %w", err)
		}
		if existing == nil {
			return token, nil
		}
	}
	return "", fmt.Errorf("生成唯一Token失败，已重试3次")
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
