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
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	pkgutil "github.com/Mininglamp-OSS/octo-server/pkg/util"
	"go.uber.org/zap"
)

type Manager struct {
	ctx *config.Context
	log.Log
	db           *robotDB
	groupService group.IService
	// closeSeatsFn 关闭被删除 Bot 在**全部** Space 的席位（D14），可注入。
	//
	// 与 botfather 两个删除入口同一个座位（同名字段、同签名）：这三处是本仓库
	// 全部的 Bot 删除入口（另有一条创建失败的补偿路径，已登记豁免）。
	// 普查见 modules/space/bot_deletion_census_test.go。
	closeSeatsFn func(ctx *config.Context, uid, operatorUID, reason string) ([]string, error)
}

func NewManager(ctx *config.Context) *Manager {
	return &Manager{
		ctx:          ctx,
		Log:          log.NewTLog("robotManager"),
		db:           newBotDB(ctx),
		groupService: group.NewService(ctx),
		closeSeatsFn: spacemod.CloseAllSpaceSeats,
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
	robot, err := m.db.queryRobotWithRobtID(robotID)
	if err != nil {
		m.Error("查询待删除机器人失败", zap.String("robotID", robotID), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotQueryFailed, nil, nil)
		return
	}
	if robot == nil {
		httperr.ResponseErrorL(c, errcode.ErrRobotNotFound, nil, nil)
		return
	}
	if err := m.groupService.RemoveUserFromGroupsForLifecycleCleanup(robotID); err != nil {
		m.Error("清理机器人群成员失败", zap.String("robotID", robotID), zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}

	// 先清理 IM 连接和缓存，再做软删除
	if err := m.cleanupBotConnection(robotID); err != nil {
		m.Error("清理机器人连接失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrRobotStoreFailed, nil, nil)
		return
	}

	// Space 席位走**移除工单**，不是裸 UPDATE，也不是什么都不做。D14。
	//
	// 这是 Bot 的第三个删除入口，也是第十轮 review 才被数出来的那一个。
	// 第七轮补上了 REST 的 /v1/user/bots/:bot_id，靠的是一次「谁**写**
	// space_member」的普查——那个问法结构上就找不到「删 Bot 却**不写**
	// space_member」的入口，于是这一条又漏了一轮。正确的问法是「什么能删 Bot」，
	// 答案是三处，普查现在钉在 bot_deletion_census_test.go 上。
	//
	// 不关席位留下的终态，与另外两个入口的裸 UPDATE 不同、而且更糟：
	// space_member.status 仍是 1 → octo_project_member.status 仍是 1，可上面
	// RemoveUserFromGroupsForLifecycleCleanup 已经把它从**每一个**群里摘掉了，
	// 包括全员群。这正好是 I4 扫描 B 的违规形态（有项目席位、不在全员群里），
	// 而扫描 B 只报不修；D13 也回收不了它（要求 robot.status=1，这里马上变 0）；
	// 管理员想靠重新添加来修复也不行——addOneMemberOnce 会以 agent_not_eligible
	// 拒绝一个已停用的 robot 行。一个可达的管理动作造出一个永久且修不了的
	// 不变量违规。
	//
	// operatorUID 传**发起删除的超管**，不是 Bot 自己：它会流进
	// deactivateSeatForCascade 的审计与日志归因，传 botID 会让审计记录读作
	// "这个 Bot 把自己从每个项目里移除了"——一个不存在的行为者。命令入口
	// （botfather/command.go）已经改过同一处归因错误。
	//
	// 失败就**中止删除**，与另外两个入口同一条规则：此刻 robot 行还是 status=1，
	// 超管可以重试；再往下一步（deleteRobotSoft）之后就再也选不到这个 bot 了。
	// 中止时的残留是「已被移出所有群、但席位还在」，与上面那个终态同形，
	// 区别在于它是可修的：重试这次删除就会把席位关掉。
	if closed, closeErr := m.closeSeatsFn(
		m.ctx, robotID, c.GetLoginUID(), spacemod.MemberRemoveReasonBotDeleted,
	); closeErr != nil {
		m.Error("关闭机器人的Space席位失败，中止删除（robot 行保持可选，可重试）",
			zap.String("robotID", robotID), zap.Strings("closedSpaces", closed),
			zap.Error(closeErr))
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
	eventKey := botevent.QueueKey(robotID)
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
