package user

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	common2 "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	wkutil "github.com/Mininglamp-OSS/octo-server/pkg/util"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	rd "github.com/go-redis/redis"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

const (
	managerLoginRateLimitTag      = "manager_login"
	managerLoginRateLimitRPS      = 10.0 / 60
	managerLoginRateLimitBurst    = 5
	managerLoginRateLimitPoolSize = 10
)

// Manager 用户管理
type Manager struct {
	ctx *config.Context
	log.Log
	db            *managerDB
	userDB        *DB
	userSettingDB *SettingDB
	deviceDB      *deviceDB
	friendDB      *friendDB
	onlineService IOnlineService
	commonService common2.IService
	roleService   *RoleService
	sessionStore  userSessionStore
	loginLog      *LoginLog
}

// NewManager NewManager
func NewManager(ctx *config.Context) *Manager {
	m := &Manager{
		ctx:           ctx,
		Log:           log.NewTLog("userManager"),
		db:            newManagerDB(ctx),
		deviceDB:      newDeviceDB(ctx),
		friendDB:      newFriendDB(ctx),
		userDB:        NewDB(ctx),
		userSettingDB: NewSettingDB(ctx.DB()),
		onlineService: NewOnlineService(ctx),
		commonService: common2.NewService(ctx),
		roleService:   NewRoleService(NewDB(ctx), ctx.Cache()),
		sessionStore:  auth.SessionStoreForContext(ctx),
		loginLog:      NewLoginLog(ctx),
	}
	m.createManagerAccount()
	return m
}

// Route 配置路由规则
func (m *Manager) Route(r *wkhttp.WKHttp) {
	// manager login is an unauthenticated, high-value endpoint. Every failed
	// attempt also writes an audit row, so the broad global DDoS floor is not
	// sufficient protection against brute force or database write amplification.
	managerLoginRedis := octoredis.NewInstrumentedClient(m.ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = managerLoginRateLimitPoolSize
	})
	managerLoginLimit := r.StrictIPRateLimitMiddleware(
		context.Background(),
		managerLoginRedis,
		managerLoginRateLimitTag,
		managerLoginRateLimitRPS,
		managerLoginRateLimitBurst,
	)
	user := r.Group("/v1/manager")
	{
		user.POST("/login", managerLoginLimit, m.login) // 账号登录
	}
	auth := r.Group("/v1/manager", m.ctx.AuthMiddleware(r))
	{
		auth.GET("/me", m.me)                                 // 当前登录管理员的身份 + 能力图谱（前端据此渲染权限）
		auth.POST("/user/admin", m.addAdminUser)              // 添加一个管理员
		auth.GET("/user/admin", m.getAdminUsers)              // 查询管理员用户
		auth.DELETE("/user/admin", m.deleteAdminUsers)        // 删除管理员用户
		auth.POST("/user/add", m.addUser)                     // 添加一个用户
		auth.POST("/user/resetpassword", m.resetUserPassword) // 重置用户密码
		auth.GET("/user/list", m.list)                        // 用户列表
		auth.GET("/user/friends", m.friends)                  // 某个用户的好友
		auth.GET("/user/blacklist", m.blacklist)              // 用户黑名单列表
		auth.GET("/user/disablelist", m.disableUsers)         // 封禁用户列表
		auth.GET("user/online", m.online)                     // 在线设备信息
		auth.PUT("/user/liftban/:uid/:status", m.liftBanUser) // 解禁或封禁用户
		auth.POST("/user/updatepassword", m.updatePwd)        // 修改用户密码
		auth.GET("/user/devices", m.devices)                  // 查看某用户设备列表
		// 手机号影子列回填：一次性数据迁移任务，带游标反复调用直到 done。
		// superAdmin 专属；见 db_phone_backfill.go。
		//
		// 挂 SharedUIDRateLimiter（须在 AuthMiddleware 之后才能读到 uid）：单次调用最多
		// maxBackfillBatchesPerCall × batchSize 次 UPDATE，是本模块写放大最大的端点，
		// 必须有 per-user 频控兜住误用/脚本失控，不能只靠调用方自觉。
		backfillLimiter := appwkhttp.SharedUIDRateLimiter(r, m.ctx)
		auth.POST("/user/phone_shadow_backfill", backfillLimiter, m.phoneShadowBackfill)
		auth.GET("/user/phone_shadow_backfill", backfillLimiter, m.phoneShadowBackfillStatus)
	}
	dashboardReader := r.Group(
		"/v1/manager/user",
		m.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, m.ctx),
	)
	{
		dashboardReader.GET("/dashboard-read", m.listDashboardReaders)
		dashboardReader.PUT("/:uid/dashboard-read", m.grantDashboardRead)
		dashboardReader.DELETE("/:uid/dashboard-read", m.revokeDashboardRead)
	}
}

// managerMeResp 是管理台「我是谁 + 我能做什么」响应。capabilities 是一张后端拥有
// 的 feature→是否放行 布尔图，供前端渲染菜单/按钮（隐藏或禁用），从而不必靠撞
// 403 反推权限。
//
// 反枚举：这张图只回答「当前用户能做什么」，不暴露「某端点需要什么角色」。它由
// 本次请求解析出的权威角色（pkg/auth RoleResolver，吊销感知）计算。
type managerMeResp struct {
	UID          string          `json:"uid"`
	Name         string          `json:"name"`
	Role         string          `json:"role"`
	Capabilities map[string]bool `json:"capabilities"`
}

// me 返回当前登录管理台账号的身份与能力图谱。dashboardReader 只在这里和 Dashboard
// 读面被承认；普通管理接口仍走 octo-lib CheckLoginRole，只接受 admin∪superAdmin。
func (m *Manager) me(c *wkhttp.Context) {
	if !auth.IsManagerConsoleRole(c.GetLoginRole()) {
		respondManagerForbidden(c)
		return
	}
	role := c.GetLoginRole()
	c.Response(&managerMeResp{
		UID:          c.GetLoginUID(),
		Name:         c.GetLoginName(),
		Role:         role,
		Capabilities: managerCapabilities(role),
	})
}

// managerCapabilities 把当前各管理端点的角色档位映射为 feature→是否放行。
// admin 档位项只对 admin∪superAdmin 开放；dashboardReader 仅开放 dashboard.read；
// superAdmin 专属项只对 superAdmin 开放。键名是与前端的稳定约定。
//
// 读/写分档：每个能力键按该组操作的真实 gate 取值——admin 可达的读/写恒 true，
// superAdmin 专属的写/销毁取 isSuper——既避免前端拿单一粗标志去渲染会被后端 403 的
// 写按钮（confused-deputy UI），也避免反过来把 admin 实际能做的操作藏掉。注意有的域
// 是三档（读=admin、写=admin、销毁=superAdmin，如 space），不要漏掉 admin 写这一档。
// 当前已按各端点真实 gate 拆分：
//   - users.read   = list/friends/blacklist/disableUsers/devices/online（CheckLoginRole）
//   - users.write  = resetUserPassword/addUser/liftBanUser/updatePwd（CheckLoginRoleIsSuperAdmin）
//   - users.manage_admin = get/add/delete 管理员账号（CheckLoginRoleIsSuperAdmin）
//   - groups.read  = list/disablelist/members/blacklist（CheckLoginRole）
//   - groups.write = leftbangroup/forbidden/removeMember（CheckLoginRoleIsSuperAdmin）
//   - space.read        = 空间/成员/邀请/入群申请 列表查询（requireAdmin）
//   - space.write       = 建空间/改资料/加成员/邀请增改禁用/通过拒绝入群申请（requireAdmin，故恒 true）
//   - space.destructive = 强制解散/封禁/强制移除/改成员角色（requireSuperAdmin）
//   - skill.read        = 系统 Skill 列表/详情查看（requireSuperAdmin）
//   - skill.write       = 系统 Skill 创建/编辑/删除/分类管理（requireSuperAdmin）
//   - mcp.read          = 系统 MCP 列表/详情查看（requireSuperAdmin）
//   - mcp.write         = 系统 MCP 创建/编辑/删除（requireSuperAdmin）
//
// TODO(#366 Part 2): 目前这张表按各端点当前档位手工维护；集中式 authz 策略表落地
// 后，应改为由同一份 route→role 真源派生，彻底消除前后端漂移。
func managerCapabilities(role string) map[string]bool {
	isSuper := role == string(wkhttp.SuperAdmin)
	isAdmin := isSuper || role == string(wkhttp.Admin)
	return map[string]bool{
		// superAdmin 专属
		"system_setting":     isSuper, // 系统配置：读写均超管
		"backup":             isSuper, // 备份管理：读写均超管
		"appversion.write":   isSuper, // 版本发布 / 下载源设置
		"dashboard.trigger":  isSuper, // 手动触发 ETL
		"space.destructive":  isSuper, // 强制解散/封禁/强制移除/改成员角色
		"users.write":        isSuper, // 重置密码 / 新增用户 / 解封 / 改密
		"users.manage_admin": isSuper, // 管理员账号 增/查/删
		"groups.write":       isSuper, // 解散封禁群 / 强制移除成员
		"skill.read":         isSuper, // 系统 Skill 列表/详情（marketplace admin surface）
		"skill.write":        isSuper, // 系统 Skill 创建/编辑/删除/分类管理（marketplace admin surface）
		"mcp.read":           isSuper, // 系统 MCP 列表/详情（marketplace admin surface 只认共享 X-Admin-Token 不分 role，此处收窄到超管以缩小页面暴露面）
		"mcp.write":          isSuper, // 系统 MCP 创建/编辑/删除（同上）
		// admin ∪ superAdmin；dashboardReader 仅有 dashboard.read。
		"appversion.read": isAdmin,                            // 版本列表
		"dashboard.read":  auth.CanReadManagerDashboard(role), // 运营看板查看
		"users.read":      isAdmin,                            // 用户列表 / 好友 / 黑名单 / 禁用 / 设备 / 在线
		"groups.read":     isAdmin,                            // 群组列表 / 禁用群 / 群成员 / 群黑名单
		"space.read":      isAdmin,                            // 空间查看 / 列表
		"space.write":     isAdmin,                            // 建空间 / 改资料 / 加成员 / 邀请增改禁用 / 通过拒绝入群申请（requireAdmin）
	}
}

func (m *Manager) devices(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}
	devices, err := m.deviceDB.queryDeviceWithUID(uid)
	if err != nil {
		m.Error("查询用户设备列表错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	list := make([]*managerDeviceResp, 0)
	if len(devices) == 0 {
		c.Response(list)
		return
	}
	for _, device := range devices {
		list = append(list, &managerDeviceResp{
			ID:          device.Id,
			DeviceID:    device.DeviceID,
			DeviceName:  device.DeviceName,
			DeviceModel: device.DeviceModel,
			LastLogin:   util.ToyyyyMMddHHmm(time.Unix(device.LastLogin, 0)),
		})
	}
	c.Response(list)
}

func (m *Manager) online(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}
	list, err := m.db.queryUserOnline(uid)
	if err != nil {
		m.Error("查询用户在线设备信息错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	result := make([]*userOnlineResp, 0)
	if len(list) > 0 {
		for _, user := range list {
			result = append(result, &userOnlineResp{
				Online:      user.Online,
				DeviceFlag:  user.DeviceFlag,
				LastOnline:  user.LastOffline,
				LastOffline: user.LastOffline,
				UID:         user.UID,
			})
		}
	}
	c.Response(result)
}

// 用户登录(管理后台)。
//
// 故意不受 login.local_off 守卫,与 /v1/user/* 的本地登录入口区分对待。
// 理由(PR #104 P1 from yujiawei):
//   - login.local_off 的 SSO 安全回退只兜得住"配置错误"(env 缺失/非法),
//     兜不住 SSO 运行时故障(IdP 宕机、client_secret 过期、callback URL 被
//     反代意外屏蔽等)。这类场景里普通用户全员锁死可接受 —— 等运维修;
//     但运维自己也通过 SSO 进不来就成了死锁,只能从 DB 或重启回退。
//   - 保留 /v1/manager/login 的本地账密入口给 SuperAdmin 当紧急通道,
//     即使生产部署设了 local_off=1 也能登进管理面把开关关掉。
//   - 与上游 octo-server 的安全模型一致:manager 路由本来就要求 admin/
//     SuperAdmin 角色 + 独立速率限制,攻击面是 IdP 入口的子集而非超集。
//
// 如果未来要让 SSO 也接管管理面,正确做法是给管理面单独的 SSO 流程(可能
// 接同一个 IdP 但用不同 client / 不同 scope),而不是把 local_off 扩到这
// 里 —— 否则会把"屏蔽用户本地登录"和"屏蔽运维紧急入口"绑死,降低可
// 运维性。
func (m *Manager) login(c *wkhttp.Context) {
	var req managerLoginReq
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	if err := req.Check(); err != nil {
		// managerLoginReq.Check returns "用户名/密码不能为空"; both are pure
		// client-side input gaps with no field tagging (matches /v1/user login).
		respondUserRequestInvalid(c, "")
		return
	}
	userInfo, err := m.db.queryUserInfoWithNameAndPwd(req.Username)
	if err != nil {
		m.Error("登录错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	publicIP := util.GetClientPublicIP(c.Request)
	// 登录失败统一返回 ErrUserInvalidCredentials，避免攻击者通过"用户不存在"
	// 与"密码错误"的响应差异枚举有效管理账号（与 /v1/user login 反枚举一致）。
	if userInfo == nil || userInfo.UID == "" {
		m.loginLog.recordFailure(req.Username, publicIP, "manager")
		respondUserError(c, errcode.ErrUserInvalidCredentials)
		return
	}
	issueFence, err := beginUserSessionIssue(c.Request.Context(), m.sessionStore, userInfo.UID)
	if err != nil {
		m.Error("初始化管理端登录会话栅栏失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserTokenCacheFailed)
		return
	}
	fencedUID := userInfo.UID
	userInfo, err = m.db.queryUserInfoWithNameAndPwd(req.Username)
	if err != nil {
		m.Error("会话栅栏后复核管理端用户失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if userInfo == nil || userInfo.UID != fencedUID ||
		userInfo.Status == int(common.UserDisable) || userInfo.IsDestroy == IsDestroyDone {
		m.loginLog.recordFailure(req.Username, publicIP, "manager")
		respondUserError(c, errcode.ErrUserInvalidCredentials)
		return
	}
	matched, needsMigration := CheckPassword(req.Password, userInfo.Password)
	if !matched {
		m.loginLog.recordFailure(req.Username, publicIP, "manager")
		respondUserError(c, errcode.ErrUserInvalidCredentials)
		return
	}
	// 自动将旧 MD5 密码迁移到 bcrypt
	if needsMigration {
		if newHash, hashErr := HashPassword(req.Password); hashErr == nil {
			_ = m.userDB.updatePassword(newHash, userInfo.UID)
		}
	}
	// 角色判定用上游的 IsManagerConsoleRole（放行 admin∪superAdmin∪dashboardReader，
	// 见 575185b0），被拒同样计入登录失败日志。
	if !auth.IsManagerConsoleRole(userInfo.Role) {
		m.loginLog.recordFailure(req.Username, publicIP, "manager")
		respondUserError(c, errcode.ErrUserManagerPermissionRequired)
		return
	}
	token := util.GenerUUID()
	sessionInfo := auth.TokenInfo{
		UID:        userInfo.UID,
		Name:       userInfo.Name,
		Role:       userInfo.Role,
		Language:   userInfo.Language,
		DeviceFlag: int(config.Web),
	}
	err = issueUserSession(c.Request.Context(), m.sessionStore, token, sessionInfo, issueFence)
	if err != nil {
		m.Error("设置管理端token缓存失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserTokenCacheFailed)
		return
	}

	c.Response(&managerLoginResp{
		UID:   userInfo.UID,
		Token: token,
		Name:  userInfo.Name,
		Role:  userInfo.Role,
	})
	m.loginLog.recordSuccess(userInfo.UID, userInfo.Username, publicIP, "manager")
}

// 重置用户密码
func (m *Manager) resetUserPassword(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}

	type reqRUP struct {
		NewPassword              string `json:"new_password"`
		NewPassswordConfirmation string `json:"new_password_confirmation"`
		Uid                      string `json:"uid"`
	}
	var req reqRUP
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	if err := ValidatePasswordStrength(req.NewPassword); err != nil {
		respondPasswordStrengthError(c, err)
		return
	}
	if req.NewPassword != req.NewPassswordConfirmation {
		respondUserError(c, errcode.ErrUserPasswordMismatch)
		return
	}
	if req.Uid == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}
	user, err := m.userDB.QueryByUID(req.Uid)
	if err != nil {
		m.Error("查询用户信息错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if user == nil {
		respondUserError(c, errcode.ErrUserNotFound)
		return
	}

	newHash, hashErr := HashPassword(req.NewPassword)
	if hashErr != nil {
		m.Error("密码哈希失败", zap.Error(hashErr))
		respondUserError(c, errcode.ErrUserPasswordProcessFailed)
		return
	}
	err = updatePasswordAndRevokeSessions(c.Request.Context(), m.userDB, m.sessionStore, m.ctx.QuitUserDevice, req.Uid, newHash, "password_reset")
	if err != nil {
		m.Error("重置用户密码错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserLoginPwdUpdateFailed)
		return
	}
	c.ResponseOK()
}

// grantDashboardRead assigns the temporary dashboardReader role to an existing
// non-SuperAdmin account. The role is exclusive: assigning it to an admin is a
// deliberate downgrade to Dashboard-only access.
func (m *Manager) grantDashboardRead(c *wkhttp.Context) {
	m.setDashboardReaderRole(c, true)
}

// revokeDashboardRead removes only the temporary dashboardReader role. It does
// not remove dashboard access inherited from admin or SuperAdmin.
func (m *Manager) revokeDashboardRead(c *wkhttp.Context) {
	m.setDashboardReaderRole(c, false)
}

func (m *Manager) setDashboardReaderRole(c *wkhttp.Context, grant bool) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		respondManagerForbidden(c)
		return
	}

	target, ok := m.dashboardReaderTarget(c)
	if !ok {
		return
	}
	nextRole, changed, allowed := dashboardReaderRoleTransition(target.Role, grant)
	if !allowed {
		respondManagerForbidden(c)
		return
	}
	if grant && !dashboardReaderGrantEligible(target) {
		respondUserError(c, errcode.ErrUserDashboardReaderTargetIneligible)
		return
	}
	if !changed {
		m.finishDashboardReaderRoleRequest(c, target.UID, grant, false)
		return
	}

	updated, err := m.db.updateUserRole(target.UID, target.Role, nextRole)
	if err != nil {
		m.Error("update dashboard reader role failed",
			zap.Error(err),
			zap.String("actor_uid", c.GetLoginUID()),
			zap.String("target_uid", target.UID),
			zap.Bool("grant", grant))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	if !updated {
		// The role changed after QueryByUID. Fail closed instead of overwriting a
		// concurrent promotion (especially to SuperAdmin).
		m.Info("dashboard reader role update lost compare-and-set",
			zap.String("actor_uid", c.GetLoginUID()),
			zap.String("target_uid", target.UID),
			zap.String("expected_role", target.Role),
			zap.Bool("grant", grant))
		respondUserError(c, errcode.ErrUserManagerRoleChanged)
		return
	}
	m.finishDashboardReaderRoleRequest(c, target.UID, grant, true)
}

func (m *Manager) dashboardReaderTarget(c *wkhttp.Context) (*Model, bool) {
	targetUID := strings.TrimSpace(c.Param("uid"))
	if targetUID == "" {
		respondUserRequestInvalid(c, "uid")
		return nil, false
	}
	target, err := m.userDB.QueryByUID(targetUID)
	if err != nil {
		m.Error("query dashboard reader target failed", zap.Error(err), zap.String("target_uid", targetUID))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return nil, false
	}
	if target == nil || target.UID == "" {
		respondUserError(c, errcode.ErrUserNotFound)
		return nil, false
	}
	return target, true
}

func dashboardReaderGrantEligible(target *Model) bool {
	return target != nil && target.Robot == 0 &&
		target.Status == StatusEnable.Int() && target.IsDestroy == IsDestroyNo
}

func (m *Manager) finishDashboardReaderRoleRequest(c *wkhttp.Context, targetUID string, grant, changed bool) {
	if err := m.roleService.Invalidate(targetUID); err != nil {
		m.Error("invalidate dashboard reader role cache failed",
			zap.Error(err),
			zap.String("actor_uid", c.GetLoginUID()),
			zap.String("target_uid", targetUID),
			zap.Bool("grant", grant),
			zap.Bool("role_changed", changed))
		respondUserError(c, errcode.ErrUserRoleCacheFailed)
		return
	}
	message := "dashboard reader role already in requested state"
	if changed {
		message = "dashboard reader role changed"
	}
	m.Info(message,
		zap.String("actor_uid", c.GetLoginUID()),
		zap.String("target_uid", targetUID),
		zap.Bool("granted", grant))
	c.ResponseOK()
}

func (m *Manager) listDashboardReaders(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		respondManagerForbidden(c)
		return
	}
	users, err := m.db.queryUsersWithRole(auth.ManagerRoleDashboardReader)
	if err != nil {
		m.Error("query dashboard reader list failed", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	list := make([]*dashboardReaderResp, 0, len(users))
	for _, user := range users {
		list = append(list, &dashboardReaderResp{
			UID: user.UID, Name: user.Name, Username: user.Username,
			RegisterTime: user.CreatedAt.String(),
		})
	}
	c.Response(list)
}

// dashboardReaderRoleTransition is the complete temporary-role policy. The
// boolean results are (changed, allowed). Admin may be deliberately downgraded
// to dashboardReader; inherited admin/SuperAdmin dashboard access cannot be
// revoked through this fixed-role endpoint.
func dashboardReaderRoleTransition(currentRole string, grant bool) (nextRole string, changed, allowed bool) {
	if grant {
		switch currentRole {
		case "", string(wkhttp.Admin):
			return auth.ManagerRoleDashboardReader, true, true
		case auth.ManagerRoleDashboardReader:
			return auth.ManagerRoleDashboardReader, false, true
		default:
			return "", false, false
		}
	}

	switch currentRole {
	case "":
		return "", false, true
	case auth.ManagerRoleDashboardReader:
		return "", true, true
	default:
		return "", false, false
	}
}

// 删除管理员用户
func (m *Manager) deleteAdminUsers(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}
	user, err := m.userDB.QueryByUID(uid)
	if err != nil {
		m.Error("查询管理员用户错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if user == nil || len(user.UID) == 0 {
		respondUserError(c, errcode.ErrUserNotFound)
		return
	}
	if user.Role != string(wkhttp.Admin) && user.Role != string(wkhttp.SuperAdmin) {
		respondUserError(c, errcode.ErrUserNotAdminAccount)
		return
	}
	if user.Role == string(wkhttp.SuperAdmin) {
		respondUserError(c, errcode.ErrUserCannotDeleteSuperAdmin)
		return
	}
	err = m.deleteAdminUser(c.Request.Context(), user.UID)
	if err != nil {
		m.Error("删除管理员错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	// 撤销该管理员在所有设备端（APP/Web/PC）的登录态，而不只是 Web。尽力删除每个端
	// （不在首个错误就中断），最大化吊销面；任一失败仍向调用方报错以便排查。
	if !sessionRevocationActive(m.sessionStore) {
		// user 行已被硬删除，且兼容模式下没有持久化的 revocation intent 兜底：
		// 这是唯一一次撤销机会。必须脱离请求 context，否则客户端断开即永久跳过，
		// 运维和 runtime 都没有收敛路径。
		securityCtx, cancel := postCommitSecurityContext(c.Request.Context())
		defer cancel()
		if err := m.revokeAllDeviceTokens(securityCtx, user.UID); err != nil {
			m.Error("清除管理员token数据错误", zap.Error(err), zap.String("uid", user.UID))
			respondUserError(c, errcode.ErrUserTokenCacheFailed)
			return
		}
	}
	c.ResponseOK()
}

func (m *Manager) deleteAdminUser(ctx context.Context, uid string) error {
	if sessionRevocationActive(m.sessionStore) {
		intent, err := m.userDB.deleteAdminWithSessionRevocation(ctx, uid, string(wkhttp.Admin))
		if err != nil {
			return err
		}
		// The transaction has committed: invalidate authorization state before
		// attempting Redis revocation so an apply failure cannot preserve an
		// admin role cache alongside a residual bearer.
		m.roleService.Invalidate(uid)
		return applyAndMarkSessionRevocation(ctx, m.userDB, m.sessionStore, intent)
	}
	if err := m.db.deleteUserWithUIDAndRole(uid, string(wkhttp.Admin)); err != nil {
		return err
	}
	m.roleService.Invalidate(uid)
	return nil
}

// deviceTokenRevokeFailure pairs a device flag with the error from trying to
// revoke its session, so the caller can log per-device outcomes.
type deviceTokenRevokeFailure struct {
	flag config.DeviceFlag
	err  error
}

// revokeDeviceTokens routes every credential mutation through Session Store,
// which owns compare-delete and v3 bounded-index cleanup. It remains
// best-effort across device flags so one Redis error does not strand all other
// known sessions.
func revokeDeviceTokens(ctx context.Context, store userSessionStore, uid string) []deviceTokenRevokeFailure {
	var failures []deviceTokenRevokeFailure
	for _, flag := range []config.DeviceFlag{config.APP, config.Web, config.PC} {
		token, err := store.DeviceToken(ctx, uid, int(flag))
		if err != nil {
			failures = append(failures, deviceTokenRevokeFailure{flag, err})
			continue
		}
		if token == "" {
			continue
		}
		if err := revokeCurrentUserSession(ctx, store, token, uid, int(flag)); err != nil {
			failures = append(failures, deviceTokenRevokeFailure{flag, err})
		}
	}
	return failures
}

// revokeAllDeviceTokens 清除某 uid 在所有设备端（APP/Web/PC）的登录态：既删
// token:{token} 让旧 token 立即失效，也删 UIDToken:{flag}{uid} 反查映射。
// best-effort（见 revokeDeviceTokens），逐端记日志，返回首个错误供调用方报错。
func (m *Manager) revokeAllDeviceTokens(ctx context.Context, uid string) error {
	failures := revokeDeviceTokens(ctx, m.sessionStore, uid)
	var firstErr error
	for _, f := range failures {
		m.Error("撤销设备token失败", zap.Error(f.err), zap.String("uid", uid), zap.Uint8("device_flag", f.flag.Uint8()))
		if firstErr == nil {
			firstErr = f.err
		}
	}
	return firstErr
}

// 查询管理员列表
func (m *Manager) getAdminUsers(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	users, err := m.db.queryUsersWithRole(string(wkhttp.Admin))
	if err != nil {
		m.Error("查询管理员用户错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	list := make([]*adminUserResp, 0)
	if len(users) > 0 {
		for _, user := range users {
			list = append(list, &adminUserResp{
				UID:          user.UID,
				Name:         user.Name,
				Username:     user.Username,
				RegisterTime: user.CreatedAt.String(),
			})
		}
	}
	c.Response(list)
}

// 添加一个管理员
func (m *Manager) addAdminUser(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	type reqVO struct {
		LoginName string `json:"login_name"`
		Name      string `json:"name"`
		Password  string `json:"password"`
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	if req.LoginName == "" {
		respondUserRequestInvalid(c, "login_name")
		return
	}
	if req.Name == "" {
		respondUserRequestInvalid(c, "name")
		return
	}
	if err := ValidateName(req.Name); err != nil {
		respondUserRequestInvalid(c, "name")
		return
	}
	if req.Password == "" {
		respondUserRequestInvalid(c, "password")
		return
	}
	if err := ValidatePasswordStrength(req.Password); err != nil {
		respondPasswordStrengthError(c, err)
		return
	}
	user, err := m.db.queryUserWithNameAndRole(req.LoginName, string(wkhttp.Admin))
	if err != nil {
		m.Error("查询用户是否存在错误", zap.String("username", req.LoginName))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if user != nil && len(user.UID) > 0 {
		respondUserError(c, errcode.ErrUserAlreadyExists)
		return
	}
	userModel := &Model{}
	userModel.UID = util.GenerUUID()
	userModel.Name = req.Name
	userModel.Vercode = fmt.Sprintf("%s@%d", util.GenerUUID(), common.User)
	userModel.QRVercode = fmt.Sprintf("%s@%d", util.GenerUUID(), common.QRCode)
	userModel.Phone = ""
	userModel.Username = req.LoginName
	userModel.Zone = ""
	userModel.Role = string(wkhttp.Admin)
	hashedPassword, err := HashPassword(req.Password)
	if err != nil {
		m.Error("密码哈希失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserPasswordProcessFailed)
		return
	}
	userModel.Password = hashedPassword
	userModel.ShortNo = util.Ten2Hex(time.Now().UnixNano())
	userModel.IsUploadAvatar = 0
	userModel.NewMsgNotice = 0
	userModel.MsgShowDetail = 0
	userModel.SearchByPhone = 0
	userModel.SearchByShort = 0
	userModel.VoiceOn = 0
	userModel.ShockOn = 0
	userModel.Sex = 1
	userModel.Status = int(common.UserAvailable)
	err = m.userDB.Insert(userModel)
	if err != nil {
		m.Error("添加管理员错误", zap.String("username", req.Name), zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	c.ResponseOK()
}

// 添加一个用户
func (m *Manager) addUser(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	var req managerAddUserReq
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	if err := req.checkAddUserReq(); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	if err := ValidatePasswordStrength(req.Password); err != nil {
		respondPasswordStrengthError(c, err)
		return
	}
	userInfo, err := m.userDB.QueryByUsername(fmt.Sprintf("%s%s", req.Zone, req.Phone))
	if err != nil {
		m.Error("查询用户信息失败！", zap.String("username", req.Phone), zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if userInfo != nil {
		respondUserError(c, errcode.ErrUserAlreadyExists)
		return
	}
	uid := util.GenerUUID()
	var shortNo = ""
	var shortNumStatus = 0
	if m.ctx.GetConfig().ShortNo.NumOn {
		shortNo, err = m.commonService.GetShortno()
		if err != nil {
			m.Error("获取短编号失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserShortNoGenFailed)
			return
		}
	} else {
		shortNo = util.Ten2Hex(time.Now().UnixNano())
	}
	if m.ctx.GetConfig().ShortNo.EditOff {
		shortNumStatus = 1
	}
	tx, err := m.db.session.Begin()
	if err != nil {
		m.Error("开启事物错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	userModel := &Model{}
	userModel.UID = uid
	userModel.Name = req.Name
	userModel.Vercode = fmt.Sprintf("%s@%d", util.GenerUUID(), common.User)
	userModel.QRVercode = fmt.Sprintf("%s@%d", util.GenerUUID(), common.QRCode)
	userModel.Phone = req.Phone
	userModel.Username = fmt.Sprintf("%s%s", req.Zone, req.Phone)
	userModel.Zone = req.Zone
	hashedPassword, err := HashPassword(req.Password)
	if err != nil {
		tx.Rollback()
		m.Error("密码哈希失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserPasswordProcessFailed)
		return
	}
	userModel.Password = hashedPassword
	userModel.ShortNo = shortNo
	userModel.IsUploadAvatar = 0
	userModel.NewMsgNotice = 1
	userModel.MsgShowDetail = 1
	userModel.SearchByPhone = 1
	userModel.ShortStatus = shortNumStatus
	userModel.SearchByShort = 1
	userModel.VoiceOn = 1
	userModel.ShockOn = 1
	userModel.Sex = req.Sex
	userModel.Status = int(common.UserAvailable)
	err = m.userDB.insertTx(userModel, tx)
	if err != nil {
		tx.Rollback()
		m.Error("添加用户错误", zap.String("username", req.Phone), zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}

	err = m.addSystemFriend(uid)
	if err != nil {
		tx.Rollback()
		m.Error("添加后台生成用户和系统账号为好友关系失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	err = m.addFileHelperFriend(uid)
	if err != nil {
		tx.Rollback()
		m.Error("添加后台生成用户和文件助手为好友关系失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	//发送用户注册事件
	eventID, err := m.ctx.EventBegin(&wkevent.Data{
		Event: event.EventUserRegister,
		Type:  wkevent.Message,
		Data: map[string]interface{}{
			"uid": uid,
		},
	}, tx)
	if err != nil {
		tx.RollbackUnlessCommitted()
		m.Error("开启事件失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		m.Error("数据库事物提交失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	m.ctx.EventCommit(eventID)
	c.ResponseOK()
}

// 用户列表
func (m *Manager) list(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	keyword := c.Query("keyword")
	onlineStr := c.Query("online")

	var online int64 = -1
	if strings.TrimSpace(onlineStr) != "" {
		online = wkutil.ParseInt64OrDefault(onlineStr, -1)
	}
	// 默认不过滤，确保兼容旧前端；前端只在需要时显式传 1。
	filter := userListFilter{
		OnlineStatus:  int(online),
		ExcludeBot:    c.Query("exclude_bot") == "1",
		ExcludeSystem: c.Query("exclude_system") == "1",
		BotOnly:       c.Query("bot_only") == "1",
		SystemOnly:    c.Query("system_only") == "1",
	}
	// 互斥校验：bot_only 与 exclude_bot、system_only 与 exclude_system 同时存在
	// 是逻辑矛盾，会得到空结果集。返回 400 让前端及早发现 query 构造 bug，
	// 比静默返回空更好。允许 bot_only + system_only（交集语义清晰）。
	if filter.BotOnly && filter.ExcludeBot {
		respondUserListFilterConflict(c, "bot_only", "exclude_bot")
		return
	}
	if filter.SystemOnly && filter.ExcludeSystem {
		respondUserListFilterConflict(c, "system_only", "exclude_system")
		return
	}
	pageIndex, pageSize := c.GetPage()
	var userList []*managerUserModel
	var count int64
	if keyword == "" {
		userList, err = m.db.queryUserListWithPage(uint64(pageSize), uint64(pageIndex), filter)
		if err != nil {
			m.Error("查询用户列表报错", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}

		count, err = m.db.queryUserCount(filter)
		if err != nil {
			m.Error("查询用户数量错误", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}
	} else {
		userList, err = m.db.queryUserListWithPageAndKeyword(keyword, uint64(pageSize), uint64(pageIndex), filter)
		if err != nil {
			m.Error("查询用户列表报错", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}

		count, err = m.db.queryUserCountWithKeyWord(keyword, filter)
		if err != nil {
			m.Error("查询用户数量错误", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}
	}

	result := make([]*managerUserResp, 0)
	if len(userList) > 0 {
		uids := make([]string, 0)
		for _, user := range userList {
			uids = append(uids, user.UID)
		}
		resps, err := m.onlineService.GetUserLastOnlineStatus(uids)
		respsdata := map[string]*config.OnlinestatusResp{}
		if len(resps) > 0 {
			for _, v := range resps {
				respsdata[v.UID] = v
			}
		}
		if err != nil {
			m.Error("查询用户在线状态失败", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}
		devices, err := m.deviceDB.queryDeviceLastLoginWithUids(uids)
		if err != nil {
			m.Error("查询用户最后一次登录设备信息错误", zap.Error(err))
			respondUserError(c, errcode.ErrUserQueryFailed)
			return
		}
		var i = 0
		for _, user := range userList {
			var device *deviceModel
			if len(devices) > 0 {
				for _, model := range devices {
					if model.UID == user.UID {
						device = model
						break
					}
				}
			}
			var lastLoginTime string
			var deviceName string = ""
			var deviceModel string = ""
			var online int
			var lastOnlineTime string = ""
			if device != nil {
				deviceModel = device.DeviceModel
				deviceName = device.DeviceName
				lastLoginTime = util.ToyyyyMMddHHmm(time.Unix(device.LastLogin, 0))
			}
			/* if i < len(resps) {
				online = resps[i].Online
				lastOnlineTime = util.ToyyyyMMddHHmm(time.Unix(int64(resps[i].LastOffline), 0))
			} */
			if respsdata[user.UID] != nil {
				online = respsdata[user.UID].Online
				lastOnlineTime = util.ToyyyyMMddHHmm(time.Unix(int64(respsdata[user.UID].LastOffline), 0))
			}
			showPhone := getShowPhoneNum(user.Phone)
			isSystem := 0
			if spacepkg.SystemBots[user.UID] {
				isSystem = 1
			}
			result = append(result, &managerUserResp{
				UID:      user.UID,
				Username: user.Username,
				Name:     user.Name,
				// Email 不脱敏:管理后台需要据此识别 SSO 邮箱登录用户(username 可能为空)。
				Email:          user.Email,
				Phone:          showPhone,
				Sex:            user.Sex,
				ShortNo:        user.ShortNo,
				LastLoginTime:  lastLoginTime,
				DeviceName:     deviceName,
				DeviceModel:    deviceModel,
				Online:         online,
				LastOnlineTime: lastOnlineTime,
				RegisterTime:   user.CreatedAt.String(),
				Status:         user.Status,
				IsDestroy:      user.IsDestroy,
				GiteeUID:       user.GiteeUID,
				GithubUID:      user.GithubUID,
				WXOpenid:       user.WXOpenid,
				IsBot:          user.Robot,
				IsSystem:       isSystem,
			})
			i++
		}
	}
	c.Response(map[string]interface{}{
		"list":  result,
		"count": count,
	})
}

// 查询某个用户的好友
func (m *Manager) friends(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}
	sortType := c.Query("sort_type")
	if sortType == "" {
		sortType = "1"
	}
	sortTypeInt := wkutil.AtoiOrDefault(sortType, 0)
	list, err := m.friendDB.QueryFriends(uid)
	if err != nil {
		m.Error("查询用户好友错误", zap.String("uid", uid), zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	result := make([]*managerFriendResp, 0)
	if len(list) == 0 {
		c.Response(result)
		return
	}
	if sortTypeInt == 0 {
		for _, friend := range list {
			result = append(result, &managerFriendResp{
				UID:              friend.ToUID,
				Remark:           friend.Remark,
				Name:             friend.ToName,
				RelationshipTime: friend.CreatedAt.String(),
			})
		}
		c.Response(result)
		return
	}
	// 查询最近会话
	conversations, err := m.ctx.IMSyncUserConversation(uid, 0, 1, "", nil)
	if err != nil {
		m.Error("同步离线后的最近会话失败！", zap.Error(err), zap.String("loginUID", uid))
		respondUserError(c, errcode.ErrUserIMCallFailed)
		return
	}
	if len(conversations) == 0 {
		for _, friend := range list {
			result = append(result, &managerFriendResp{
				UID:              friend.ToUID,
				Remark:           friend.Remark,
				Name:             friend.ToName,
				RelationshipTime: friend.CreatedAt.String(),
			})
		}
		c.Response(result)
		return
	}
	sort.SliceStable(conversations, func(i, j int) bool {
		return conversations[i].Timestamp > conversations[j].Timestamp
	})
	for _, conv := range conversations {
		if conv.ChannelType != common.ChannelTypePerson.Uint8() {
			continue
		}
		var f *DetailModel
		for _, friend := range list {
			if friend.ToUID == conv.ChannelID {
				f = friend
				break
			}
		}
		if f != nil {
			result = append(result, &managerFriendResp{
				UID:              f.ToUID,
				Remark:           f.Remark,
				Name:             f.ToName,
				RelationshipTime: f.CreatedAt.String(),
			})
		}
	}
	for _, f := range list {
		isAdd := true
		for _, r := range result {
			if r.UID == f.ToUID {
				isAdd = false
				break
			}
		}
		if isAdd {
			result = append(result, &managerFriendResp{
				UID:              f.ToUID,
				Remark:           f.Remark,
				Name:             f.ToName,
				RelationshipTime: f.CreatedAt.String(),
			})
		}
	}
	c.Response(result)
}

// 查询某个用户的黑名单
func (m *Manager) blacklist(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}
	list, err := m.db.queryUserBlacklists(uid)
	if err != nil {
		m.Error("查询黑名单列表失败！", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	blacklists := []*managerBlackUserResp{}
	for _, result := range list {
		blacklists = append(blacklists, &managerBlackUserResp{
			UID:      result.UID,
			Name:     result.Name,
			CreateAt: result.UpdatedAt.String(),
		})
	}
	c.Response(blacklists)
}

// 查看封禁用户列表
func (m *Manager) disableUsers(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	pageIndex, pageSize := c.GetPage()
	list, err := m.db.queryUserListWithStatus(int(common.UserDisable), uint64(pageSize), uint64(pageIndex))
	if err != nil {
		m.Error("通过状态查询用户列表错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	count, err := m.db.queryUserCountWithStatus(int(common.UserDisable))
	if err != nil {
		m.Error("查询用户数量错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	result := make([]*managerDisableUserResp, 0)
	if len(list) > 0 {
		for _, user := range list {
			showPhone := getShowPhoneNum(user.Phone)
			result = append(result, &managerDisableUserResp{
				Name:         user.Name,
				ShortNo:      user.ShortNo,
				Phone:        showPhone,
				UID:          user.UID,
				ClosureTime:  user.UpdatedAt.String(),
				RegisterTime: user.CreatedAt.String(),
			})
		}
	}
	c.Response(map[string]interface{}{
		"list":  result,
		"count": count,
	})
}

// 封禁或解禁用户
func (m *Manager) liftBanUser(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	uid := c.Param("uid")
	status := c.Param("status")
	if uid == "" {
		respondUserRequestInvalid(c, "uid")
		return
	}
	if status == "" {
		respondUserRequestInvalid(c, "status")
		return
	}
	userStatus := wkutil.AtoiOrDefault(status, 0)
	if userStatus != int(common.UserAvailable) && userStatus != int(common.UserDisable) {
		respondUserRequestInvalid(c, "status")
		return
	}
	userInfo, err := m.userDB.QueryByUID(uid)
	if err != nil {
		m.Error("查询用户信息失败！", zap.String("uid", uid), zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if userInfo == nil {
		respondUserError(c, errcode.ErrUserNotFound)
		return
	}
	alreadyInState := userInfo.Status == userStatus
	disabling := userStatus == int(common.UserDisable)
	var securityErr error
	var mutationErr error
	committed := false
	if disabling {
		if alreadyInState {
			committed = true
			if sessionRevocationActive(m.sessionStore) {
				retryCtx, cancel := postCommitSecurityContext(c.Request.Context())
				pending, pendingErr := m.userDB.pendingSessionRevocation(retryCtx, uid, "account_disable")
				if pendingErr != nil {
					mutationErr = pendingErr
				} else if pending != nil {
					mutationErr = applyAndMarkSessionRevocation(retryCtx, m.userDB, m.sessionStore, pending)
				}
				cancel()
			}
		} else {
			committed, mutationErr = updateUserFieldAndRevokeSessionsWithResult(c.Request.Context(), m.userDB, m.sessionStore, uid, "status", status, "account_disable")
			if !committed {
				m.Error("修改用户状态错误", zap.Error(mutationErr))
				respondUserError(c, errcode.ErrUserStoreFailed)
				return
			}
		}
	} else if !alreadyInState {
		if err = m.userDB.UpdateUsersWithField("status", status, uid); err != nil {
			m.Error("修改用户状态错误", zap.Error(err))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
	}

	ban := 0
	if disabling {
		ban = 1
	}

	channelErr := m.ctx.IMCreateOrUpdateChannelInfo(&config.ChannelInfoCreateReq{
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Ban:         ban,
	})
	if channelErr != nil {
		m.Error("更新WebIM的token失败！", zap.Error(channelErr))
	}
	var quitErr error
	if disabling {
		securityErr = finishCommittedUserSecurityMutation(c.Request.Context(), m.sessionStore, uid, committed, mutationErr, m.ctx.QuitUserDevice)
	} else {
		quitErr = m.ctx.QuitUserDevice(userInfo.UID, -1)
		if quitErr != nil {
			m.Error("下线用户所有登录设备错误", zap.Error(quitErr), zap.String("uid", uid))
		}
	}
	if securityErr != nil {
		m.Error("封禁用户会话撤销未同步完成", zap.Error(securityErr), zap.String("uid", uid))
		respondUserError(c, errcode.ErrUserStoreFailed)
		return
	}
	if channelErr != nil || quitErr != nil {
		respondUserError(c, errcode.ErrUserIMCallFailed)
		return
	}
	c.ResponseOK()
}

// 修改登录密码
func (m *Manager) updatePwd(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		respondManagerForbidden(c)
		return
	}
	loginUID := c.GetLoginUID()
	type updatePwdReq struct {
		Password    string `json:"password"`
		NewPassword string `json:"new_password"`
	}
	var req updatePwdReq
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	if req.Password == "" || req.NewPassword == "" {
		respondUserRequestInvalid(c, "password")
		return
	}
	user, err := m.userDB.QueryByUID(loginUID)
	if err != nil {
		m.Error("查询用户信息错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if user == nil {
		respondUserError(c, errcode.ErrUserNotFound)
		return
	}
	matched, _ := CheckPassword(req.Password, user.Password)
	if !matched {
		respondUserError(c, errcode.ErrUserOldPasswordIncorrect)
		return
	}
	if err := ValidatePasswordStrength(req.NewPassword); err != nil {
		respondPasswordStrengthError(c, err)
		return
	}
	if req.Password == req.NewPassword {
		respondUserError(c, errcode.ErrUserNewPasswordSameAsOld)
		return
	}
	newHashedPassword, err := HashPassword(req.NewPassword)
	if err != nil {
		m.Error("密码哈希失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserPasswordProcessFailed)
		return
	}
	err = updatePasswordAndRevokeSessions(c.Request.Context(), m.userDB, m.sessionStore, m.ctx.QuitUserDevice, loginUID, newHashedPassword, "password_change")
	if err != nil {
		m.Error("修改用户密码错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserLoginPwdUpdateFailed)
		return
	}
	c.ResponseOK()
}
func (r managerAddUserReq) checkAddUserReq() error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("用户名不能为空！")
	}
	if err := ValidateName(r.Name); err != nil {
		return err
	}
	if strings.TrimSpace(r.Password) == "" {
		return errors.New("密码不能为空！")
	}
	if strings.TrimSpace(r.Phone) == "" {
		return errors.New("手机号不能为空！")
	}

	return nil
}
func (r managerLoginReq) Check() error {
	if strings.TrimSpace(r.Username) == "" {
		return errors.New("用户名不能为空！")
	}
	if strings.TrimSpace(r.Password) == "" {
		return errors.New("密码不能为空！")
	}
	return nil
}

// 处理注册用户和文件助手互为好友
func (m *Manager) addFileHelperFriend(uid string) error {
	if uid == "" {
		m.Error("用户ID不能为空")
		return errors.New("用户ID不能为空")
	}
	isFriend, err := m.friendDB.IsFriend(uid, m.ctx.GetConfig().Account.FileHelperUID)
	if err != nil {
		m.Error("查询用户关系失败")
		return err
	}
	if !isFriend {
		version, err := m.ctx.GenSeq(common.FriendSeqKey)
		if err != nil {
			m.Error("GenSeq failed", zap.Error(err))
			return err
		}
		err = m.friendDB.Insert(&FriendModel{
			UID:     uid,
			ToUID:   m.ctx.GetConfig().Account.FileHelperUID,
			Version: version,
		})
		if err != nil {
			m.Error("注册用户和文件助手成为好友失败")
			return err
		}
	}
	return nil
}

// addSystemFriend 处理注册用户和系统账号互为好友
func (m *Manager) addSystemFriend(uid string) error {

	if uid == "" {
		m.Error("用户ID不能为空")
		return errors.New("用户ID不能为空")
	}
	isFriend, err := m.friendDB.IsFriend(uid, m.ctx.GetConfig().Account.SystemUID)
	if err != nil {
		m.Error("查询用户关系失败")
		return err
	}
	tx, err := m.friendDB.session.Begin()
	if err != nil {
		m.Error("开启事物错误", zap.Error(err))
		return errors.New("开启事物错误")
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	if !isFriend {
		version, err := m.ctx.GenSeq(common.FriendSeqKey)
		if err != nil {
			m.Error("GenSeq failed", zap.Error(err))
			return err
		}
		err = m.friendDB.InsertTx(&FriendModel{
			UID:     uid,
			ToUID:   m.ctx.GetConfig().Account.SystemUID,
			Version: version,
		}, tx)
		if err != nil {
			m.Error("注册用户和系统账号成为好友失败")
			tx.Rollback()
			return err
		}
	}
	// systemIsFriend, err := u.friendDB.IsFriend(u.ctx.GetConfig().SystemUID, uid)
	// if err != nil {
	// 	u.Error("查询系统账号和注册用户关系失败")
	// 	tx.Rollback()
	// 	return err
	// }
	// if !systemIsFriend {
	// 	version := u.ctx.GenSeq(common.FriendSeqKey)
	// 	err := u.friendDB.InsertTx(&FriendModel{
	// 		UID:     u.ctx.GetConfig().SystemUID,
	// 		ToUID:   uid,
	// 		Version: version,
	// 	}, tx)
	// 	if err != nil {
	// 		u.Error("系统账号和注册用户成为好友失败")
	// 		tx.Rollback()
	// 		return err
	// 	}
	// }
	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		m.Error("用户注册数据库事物提交失败", zap.Error(err))
		return err
	}
	return nil
}

// 创建一个系统管理账户
func (m *Manager) createManagerAccount() {
	user, err := m.userDB.QueryByUID(m.ctx.GetConfig().Account.AdminUID)
	if err != nil {
		m.Error("查询系统管理账号错误", zap.Error(err))
		return
	}
	if (user != nil && user.UID != "") || m.ctx.GetConfig().AdminPwd == "" {
		return
	}

	var pwd = m.ctx.GetConfig().AdminPwd
	// 超管播种密码同样必须过口令复杂度策略。这里 fail-closed（不建账号）而不是降级
	// 建一个弱口令超管：上面的 early return 保证只有"超管尚不存在"时才会走到这里，
	// 所以拒绝创建只影响全新部署，不会把已有环境锁在外面；而一个弱口令的超级管理员
	// 正是审计和真实攻击面上最贵的那个洞。
	//
	// 只记错误类型不记密码本身。
	if err := ValidatePasswordStrength(pwd); err != nil {
		m.Error("拒绝创建超级管理员：AdminPwd 不满足口令复杂度策略"+
			"（≥8 个字符、≤72 字节，且含大小写字母/数字/特殊字符中的至少 2 类）。"+
			"请修正配置后重启以完成超管账号创建。",
			zap.Error(err))
		return
	}
	hashedPwd, hashErr := HashPassword(pwd)
	if hashErr != nil {
		m.Error("密码哈希失败", zap.Error(hashErr))
		return
	}
	err = m.userDB.Insert(newManagerSeedModel(m.ctx.GetConfig().Account.AdminUID, hashedPwd))
	if err != nil {
		m.Error("新增系统管理员错误", zap.Error(err))
		return
	}
}

// newManagerSeedModel 构造首次启动的超管账号。超管通过 username 登录，
// 固定虚构手机号没有业务用途，却会让 bootstrap 不必要地依赖
// OCTO_PII_ENCRYPTION_SECRET。保持 phone 为空可以在 PII 密钥配置前先建立
// 管理入口，且不会因降级而在超管行上留下一个明文的虚构号码
// （缺密钥时的降级行为见 DB.syncPhoneShadow）。
func newManagerSeedModel(adminUID, hashedPwd string) *Model {
	return &Model{
		UID:      adminUID,
		Name:     "超级管理员",
		ShortNo:  "30000",
		Category: "system",
		Role:     string(wkhttp.SuperAdmin),
		Username: string(wkhttp.SuperAdmin),
		Status:   1,
		Password: hashedPwd,
	}
}

func getShowPhoneNum(mobile string) string {
	if len(mobile) <= 3 {
		return mobile
	}
	phone := mobile[:3]
	var length = len(mobile) - 3
	if length > 4 {
		length = 4
	}
	for i := 0; i < length; i++ {
		phone = fmt.Sprintf("%s*", phone)
	}
	var index = 3 + length
	if index > 0 && index < len(mobile) {
		return phone + mobile[index:]
	}
	return phone
}

type managerLoginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type managerLoginResp struct {
	UID   string `json:"uid"`
	Token string `json:"token"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}
type managerAddUserReq struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Phone    string `json:"phone"`
	Zone     string `json:"zone"`
	Sex      int    `json:"sex"`
}
type managerBlackUserResp struct {
	Name     string `json:"name"`
	UID      string `json:"uid"`
	CreateAt string `json:"create_at"`
}
type adminUserResp struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Username     string `json:"username"`
	RegisterTime string `json:"register_time"`
}
type dashboardReaderResp struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Username     string `json:"username"`
	RegisterTime string `json:"register_time"`
}
type managerUserResp struct {
	Name           string `json:"name"`
	UID            string `json:"uid"`
	Phone          string `json:"phone"`
	Username       string `json:"username"`
	Email          string `json:"email"`
	ShortNo        string `json:"short_no"`
	Sex            int    `json:"sex"`
	RegisterTime   string `json:"register_time"`
	LastLoginTime  string `json:"last_login_time"`
	DeviceName     string `json:"device_name"`
	DeviceModel    string `json:"device_model"`
	Online         int    `json:"online"`
	LastOnlineTime string `json:"last_online_time"`
	Status         int    `json:"status"`
	IsDestroy      int    `json:"is_destroy"`
	WXOpenid       string `json:"wx_openid"`  // 微信openid
	GiteeUID       string `json:"gitee_uid"`  // gitee uid
	GithubUID      string `json:"github_uid"` // github uid
	IsBot          int    `json:"is_bot"`     // 0.否 1.是；来源 user.robot，与 /v1/robot/space_bots 一致
	IsSystem       int    `json:"is_system"`  // 0.否 1.是；来源 pkg/space.SystemBots（botfather/u_10000/fileHelper/notification）
}

type managerFriendResp struct {
	Name             string `json:"name"`
	UID              string `json:"uid"`
	Remark           string `json:"remark"`
	RelationshipTime string `json:"relationship_time"`
}

type managerDisableUserResp struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	ShortNo      string `json:"short_no"`
	Sex          int    `json:"sex"`
	RegisterTime string `json:"register_time"`
	Phone        string `json:"phone"`
	ClosureTime  string `json:"closure_time"`
}

type managerDeviceResp struct {
	ID          int64  `json:"id"`
	DeviceID    string `json:"device_id"`    // 设备ID
	DeviceName  string `json:"device_name"`  // 设备名称
	DeviceModel string `json:"device_model"` // 设备型号
	LastLogin   string `json:"last_login"`   // 设备最后一次登录时间
	Self        int    `json:"self"`         // 是否是本机
}

type userOnlineResp struct {
	UID         string `json:"uid"`
	DeviceFlag  uint8  `json:"device_flag"`
	LastOnline  int    `json:"last_online"`
	LastOffline int    `json:"last_offline"`
	Online      int    `json:"online"`
}

func newUserOnlineResp(m *onlineStatusWeightModel) *userOnlineResp {

	return &userOnlineResp{
		UID:         m.UID,
		DeviceFlag:  m.DeviceFlag,
		LastOnline:  m.LastOnline,
		LastOffline: m.LastOffline,
		Online:      m.Online,
	}
}

// phoneShadowBackfillReq 回填请求。cursor 由上一次响应的 next_cursor 透传回来，
// 首次调用传 0。
type phoneShadowBackfillReq struct {
	Cursor     int64 `json:"cursor"`
	BatchSize  int   `json:"batch_size"`
	IntervalMS int   `json:"interval_ms"`
}

// phoneShadowBackfill 触发一批手机号影子列回填。
//
// 刻意做成"带游标的多次调用"而不是一次长跑请求：单次调用有批数上限（见
// db_phone_backfill.go），运维按响应里的 next_cursor 反复 POST 直到 done=true，
// 这样既不会产生长事务/超时，也能随时中断续跑（任务本身幂等）。
func (m *Manager) phoneShadowBackfill(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		respondManagerForbidden(c)
		return
	}
	var req phoneShadowBackfillReq
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	if req.Cursor < 0 {
		respondUserRequestInvalid(c, "cursor")
		return
	}
	result, err := m.userDB.BackfillPhoneShadow(
		c.Request.Context(), req.Cursor, req.BatchSize, time.Duration(req.IntervalMS)*time.Millisecond)
	if err != nil {
		// 主密钥未配置也走这里：属于部署配置问题，对客户端是内部错误。
		m.Error("手机号影子列回填失败", zap.Error(err))
		respondUserServiceError(c)
		return
	}
	c.Response(result)
}

// phoneShadowBackfillStatus 只读进度：仍待回填的行数。用于确认回填是否收敛，
// 以及后续把空间成员后四位检索从明文兜底切成纯 phone_last4 的判断依据。
func (m *Manager) phoneShadowBackfillStatus(c *wkhttp.Context) {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		respondManagerForbidden(c)
		return
	}
	remaining, err := m.userDB.CountPhoneShadowPending()
	if err != nil {
		m.Error("统计手机号影子列待回填行数失败", zap.Error(err))
		respondUserServiceError(c)
		return
	}
	c.Response(map[string]interface{}{"remaining": remaining})
}
