package user

import (
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
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	wkutil "github.com/Mininglamp-OSS/octo-server/pkg/util"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/pkg/errors"
	"go.uber.org/zap"
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
	}
	m.createManagerAccount()
	return m
}

// Route 配置路由规则
func (m *Manager) Route(r *wkhttp.WKHttp) {
	user := r.Group("/v1/manager")
	{
		user.POST("/login", m.login) // 账号登录
	}
	auth := r.Group("/v1/manager", m.ctx.AuthMiddleware(r))
	{
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
	}
}

func (m *Manager) devices(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		c.ResponseError(err)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		c.ResponseError(errors.New("请求用户uid不能为空"))
		return
	}
	devices, err := m.deviceDB.queryDeviceWithUID(uid)
	if err != nil {
		m.Error("查询用户设备列表错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户设备列表错误"))
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
		c.ResponseError(err)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		c.ResponseError(errors.New("请求用户uid不能为空"))
		return
	}
	list, err := m.db.queryUserOnline(uid)
	if err != nil {
		m.Error("查询用户在线设备信息错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户在线设备信息错误"))
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
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if err := req.Check(); err != nil {
		c.ResponseError(err)
		return
	}
	userInfo, err := m.db.queryUserInfoWithNameAndPwd(req.Username)
	if err != nil {
		m.Error("登录错误", zap.Error(err))
		c.ResponseError(errors.New("登录错误！"))
		return
	}
	if userInfo == nil || userInfo.UID == "" {
		c.ResponseError(errors.New("登录用户不存在"))
		return
	}
	matched, needsMigration := CheckPassword(req.Password, userInfo.Password)
	if !matched {
		c.ResponseError(errors.New("用户名或密码错误"))
		return
	}
	// 自动将旧 MD5 密码迁移到 bcrypt
	if needsMigration {
		if newHash, hashErr := HashPassword(req.Password); hashErr == nil {
			_ = m.userDB.updatePassword(newHash, userInfo.UID)
		}
	}
	if userInfo.Role != string(wkhttp.Admin) && userInfo.Role != string(wkhttp.SuperAdmin) {
		c.ResponseError(errors.New("登录账号未开通管理权限"))
		return
	}
	token := util.GenerUUID()
	// 将token设置到缓存
	tokenPayload, err := auth.Encode(auth.TokenInfo{UID: userInfo.UID, Name: userInfo.Name, Role: userInfo.Role})
	if err != nil {
		m.Error("编码token缓存失败！", zap.Error(err))
		c.ResponseError(errors.New("设置token缓存失败！"))
		return
	}
	err = m.ctx.Cache().SetAndExpire(m.ctx.GetConfig().Cache.TokenCachePrefix+token, tokenPayload, m.ctx.GetConfig().Cache.TokenExpire)
	if err != nil {
		m.Error("设置token缓存失败！", zap.Error(err))
		c.ResponseError(errors.New("设置token缓存失败！"))
		return
	}

	err = m.ctx.Cache().SetAndExpire(fmt.Sprintf("%s%d%s", m.ctx.GetConfig().Cache.UIDTokenCachePrefix, config.Web, userInfo.UID), token, m.ctx.GetConfig().Cache.TokenExpire)
	if err != nil {
		m.Error("设置uidtoken缓存失败！", zap.Error(err))
		c.ResponseError(errors.New("设置token缓存失败！"))
		return
	}

	c.Response(&managerLoginResp{
		UID:   userInfo.UID,
		Token: token,
		Name:  userInfo.Name,
		Role:  userInfo.Role,
	})
}

// 重置用户密码
func (m *Manager) resetUserPassword(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}

	type reqRUP struct {
		NewPassword              string `json:"new_password"`
		NewPassswordConfirmation string `json:"new_password_confirmation"`
		Uid                      string `json:"uid"`
	}
	var req reqRUP
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if len(req.NewPassword) < 6 {
		c.ResponseError(errors.New("密码长度必须大于6位"))
		return
	}
	if req.NewPassword != req.NewPassswordConfirmation {
		c.ResponseError(errors.New("两次密码不一致！"))
		return
	}
	if req.Uid == "" {
		c.ResponseError(errors.New("用户uid不能为空！"))
		return
	}
	user, err := m.userDB.QueryByUID(req.Uid)
	if err != nil {
		m.Error("查询用户信息错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户信息错误"))
		return
	}
	if user == nil {
		c.ResponseError(errors.New("操作用户不存在"))
		return
	}

	newHash, hashErr := HashPassword(req.NewPassword)
	if hashErr != nil {
		m.Error("密码哈希失败", zap.Error(hashErr))
		c.ResponseError(errors.New("密码处理失败"))
		return
	}
	err = m.userDB.UpdateUsersWithField("password", newHash, req.Uid)
	if err != nil {
		m.Error("重置用户密码错误", zap.Error(err))
		c.Response("重置用户密码错误")
		return
	}
	c.ResponseOK()
}

// 删除管理员用户
func (m *Manager) deleteAdminUsers(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		c.ResponseError(errors.New("删除用户uid不能为空"))
		return
	}
	user, err := m.userDB.QueryByUID(uid)
	if err != nil {
		m.Error("查询管理员用户错误", zap.Error(err))
		c.ResponseError(errors.New("查询管理员用户错误"))
		return
	}
	if user == nil || len(user.UID) == 0 {
		c.ResponseError(errors.New("该用户不存在"))
		return
	}
	if user.Role == "" {
		c.ResponseError(errors.New("该用户不是管理员账号不能删除"))
		return
	}
	if user.Role == string(wkhttp.SuperAdmin) {
		c.ResponseError(errors.New("超级管理员账号不能删除"))
		return
	}
	err = m.db.deleteUserWithUIDAndRole(uid, string(wkhttp.Admin))
	if err != nil {
		m.Error("删除管理员错误", zap.Error(err))
		c.ResponseError(errors.New("删除管理员错误"))
		return
	}
	oldToken, err := m.ctx.Cache().Get(fmt.Sprintf("%s%d%s", m.ctx.GetConfig().Cache.UIDTokenCachePrefix, config.Web, user.UID))
	if err != nil {
		m.Error("获取旧token错误", zap.Error(err))
		c.ResponseError(errors.New("获取旧token错误"))
		return
	}
	if oldToken != "" {
		err = m.ctx.Cache().Delete(m.ctx.GetConfig().Cache.TokenCachePrefix + oldToken)
		if err != nil {
			m.Error("清除旧token数据错误", zap.Error(err))
			c.ResponseError(errors.New("清除旧token数据错误"))
			return
		}
	}
	c.ResponseOK()
}

// 查询管理员列表
func (m *Manager) getAdminUsers(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}
	users, err := m.db.queryUsersWithRole(string(wkhttp.Admin))
	if err != nil {
		m.Error("查询管理员用户错误", zap.Error(err))
		c.ResponseError(errors.New("查询管理员用户错误"))
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
		c.ResponseError(err)
		return
	}
	type reqVO struct {
		LoginName string `json:"login_name"`
		Name      string `json:"name"`
		Password  string `json:"password"`
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if req.LoginName == "" {
		c.ResponseError(errors.New("登录用户名不能为空"))
		return
	}
	if req.Name == "" {
		c.ResponseError(errors.New("用户名不能为空"))
		return
	}
	if err := ValidateName(req.Name); err != nil {
		c.ResponseError(err)
		return
	}
	if req.Password == "" {
		c.ResponseError(errors.New("密码不能为空"))
		return
	}
	user, err := m.db.queryUserWithNameAndRole(req.LoginName, string(wkhttp.Admin))
	if err != nil {
		m.Error("查询用户是否存在错误", zap.String("username", req.LoginName))
		c.ResponseError(errors.New("查询用户是否存在错误"))
		return
	}
	if user != nil && len(user.UID) > 0 {
		c.ResponseError(errors.New("该用户名已存在"))
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
		c.ResponseError(errors.New("密码处理失败"))
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
		m.Error("添加管理员错误", zap.String("username", req.Name))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// 添加一个用户
func (m *Manager) addUser(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}
	var req managerAddUserReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if err := req.checkAddUserReq(); err != nil {
		c.ResponseError(err)
		return
	}
	userInfo, err := m.userDB.QueryByUsername(fmt.Sprintf("%s%s", req.Zone, req.Phone))
	if err != nil {
		m.Error("查询用户信息失败！", zap.String("username", req.Phone))
		c.ResponseError(err)
		return
	}
	if userInfo != nil {
		c.ResponseError(errors.New("该用户已存在"))
		return
	}
	uid := util.GenerUUID()
	var shortNo = ""
	var shortNumStatus = 0
	if m.ctx.GetConfig().ShortNo.NumOn {
		shortNo, err = m.commonService.GetShortno()
		if err != nil {
			m.Error("获取短编号失败！", zap.Error(err))
			c.ResponseError(errors.New("获取短编号失败！"))
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
		c.ResponseError(errors.New("开启事物错误"))
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
		m.Error("密码哈希失败", zap.Error(err))
		c.ResponseError(errors.New("密码处理失败"))
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
		m.Error("添加用户错误", zap.String("username", req.Phone))
		c.ResponseError(err)
		return
	}

	err = m.addSystemFriend(uid)
	if err != nil {
		tx.Rollback()
		c.ResponseError(errors.New("添加后台生成用户和系统账号为好友关系失败"))
		return
	}
	err = m.addFileHelperFriend(uid)
	if err != nil {
		tx.Rollback()
		c.ResponseError(errors.New("添加后台生成用户和文件助手为好友关系失败"))
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
		c.ResponseError(errors.New("开启事件失败！"))
		return
	}
	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		m.Error("数据库事物提交失败", zap.Error(err))
		c.ResponseError(errors.New("数据库事物提交失败"))
		return
	}
	m.ctx.EventCommit(eventID)
	c.ResponseOK()
}

// 用户列表
func (m *Manager) list(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		c.ResponseError(err)
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
		c.ResponseError(errors.New("bot_only 与 exclude_bot 互斥"))
		return
	}
	if filter.SystemOnly && filter.ExcludeSystem {
		c.ResponseError(errors.New("system_only 与 exclude_system 互斥"))
		return
	}
	pageIndex, pageSize := c.GetPage()
	var userList []*managerUserModel
	var count int64
	if keyword == "" {
		userList, err = m.db.queryUserListWithPage(uint64(pageSize), uint64(pageIndex), filter)
		if err != nil {
			m.Error("查询用户列表报错", zap.Error(err))
			c.ResponseError(err)
			return
		}

		count, err = m.db.queryUserCount(filter)
		if err != nil {
			m.Error("查询用户数量错误", zap.Error(err))
			c.ResponseError(errors.New("查询用户数量错误"))
			return
		}
	} else {
		userList, err = m.db.queryUserListWithPageAndKeyword(keyword, uint64(pageSize), uint64(pageIndex), filter)
		if err != nil {
			m.Error("查询用户列表报错", zap.Error(err))
			c.ResponseError(err)
			return
		}

		count, err = m.db.queryUserCountWithKeyWord(keyword, filter)
		if err != nil {
			m.Error("查询用户数量错误", zap.Error(err))
			c.ResponseError(errors.New("查询用户数量错误"))
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
			c.ResponseError(errors.New("查询用户在线状态失败"))
			return
		}
		devices, err := m.deviceDB.queryDeviceLastLoginWithUids(uids)
		if err != nil {
			m.Error("查询用户最后一次登录设备信息错误", zap.Error(err))
			c.ResponseError(errors.New("查询用户最后一次登录设备信息错误"))
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
		c.ResponseError(err)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		c.ResponseError(errors.New("查询用户ID不能为空"))
		return
	}
	sortType := c.Query("sort_type")
	if sortType == "" {
		sortType = "1"
	}
	sortTypeInt := wkutil.AtoiOrDefault(sortType, 0)
	list, err := m.friendDB.QueryFriends(uid)
	if err != nil {
		m.Error("查询用户好友错误", zap.String("uid", uid))
		c.ResponseError(err)
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
		c.ResponseError(errors.New("同步离线后的最近会话失败！"))
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
		c.ResponseError(err)
		return
	}
	uid := c.Query("uid")
	if uid == "" {
		c.ResponseError(errors.New("查询用户ID不能为空"))
		return
	}
	list, err := m.db.queryUserBlacklists(uid)
	if err != nil {
		m.Error("查询黑名单列表失败！", zap.Error(err))
		c.ResponseError(errors.New("查询黑名单列表失败！"))
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
		c.ResponseError(err)
		return
	}
	pageIndex, pageSize := c.GetPage()
	list, err := m.db.queryUserListWithStatus(int(common.UserDisable), uint64(pageSize), uint64(pageIndex))
	if err != nil {
		m.Error("通过状态查询用户列表错误", zap.Error(err))
		c.ResponseError(errors.New("通过状态查询用户列表错误"))
		return
	}
	count, err := m.db.queryUserCountWithStatus(int(common.UserDisable))
	if err != nil {
		m.Error("查询用户数量错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户数量错误"))
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
		c.ResponseError(err)
		return
	}
	uid := c.Param("uid")
	status := c.Param("status")
	if uid == "" {
		c.ResponseError(errors.New("操作用户id不能为空"))
		return
	}
	if status == "" {
		c.ResponseError(errors.New("修改状态类型不能为空"))
		return
	}
	userStatus := wkutil.AtoiOrDefault(status, 0)
	if userStatus != int(common.UserAvailable) && userStatus != int(common.UserDisable) {
		c.ResponseError(errors.New("修改状态类型不匹配"))
		return
	}
	userInfo, err := m.userDB.QueryByUID(uid)
	if err != nil {
		m.Error("查询用户信息失败！", zap.String("uid", uid))
		c.ResponseError(errors.New("查询用户信息错误"))
		return
	}
	if userInfo == nil {
		c.ResponseError(errors.New("操作用户不存在"))
		return
	}
	if userInfo.Status == userStatus {
		c.ResponseOK()
		return
	}
	err = m.userDB.UpdateUsersWithField("status", status, uid)
	if err != nil {
		m.Error("修改用户状态错误", zap.Error(err))
		c.ResponseError(errors.New("修改用户状态错误"))
		return
	}

	ban := 0
	if userStatus == int(common.UserDisable) {
		ban = 1
	}

	err = m.ctx.IMCreateOrUpdateChannelInfo(&config.ChannelInfoCreateReq{
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Ban:         ban,
	})
	if err != nil {
		m.Error("更新WebIM的token失败！", zap.Error(err))
		c.ResponseError(errors.New("更新IM的token失败！"))
		return
	}
	err = m.ctx.QuitUserDevice(userInfo.UID, -1)
	if err != nil {
		m.Error("下线用户所有登录设备错误", zap.Error(err), zap.String("uid", uid))
		c.ResponseError(errors.New("下线用户所有登录设备错误"))
		return
	}
	c.ResponseOK()
}

// 修改登录密码
func (m *Manager) updatePwd(c *wkhttp.Context) {
	err := c.CheckLoginRoleIsSuperAdmin()
	if err != nil {
		c.ResponseError(err)
		return
	}
	loginUID := c.GetLoginUID()
	type updatePwdReq struct {
		Password    string `json:"password"`
		NewPassword string `json:"new_password"`
	}
	var req updatePwdReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if req.Password == "" || req.NewPassword == "" {
		c.ResponseError(errors.New("密码不能为空"))
		return
	}
	user, err := m.userDB.QueryByUID(loginUID)
	if err != nil {
		m.Error("查询用户信息错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户信息错误"))
		return
	}
	if user == nil {
		c.ResponseError(errors.New("操作用户不存在"))
		return
	}
	matched, _ := CheckPassword(req.Password, user.Password)
	if !matched {
		c.ResponseError(errors.New("原密码错误"))
		return
	}
	if len(req.NewPassword) < 6 {
		c.ResponseError(errors.New("密码长度必须大于6位"))
		return
	}
	if req.Password == req.NewPassword {
		c.ResponseError(errors.New("新密码不能和旧密码一样"))
		return
	}
	newHashedPassword, err := HashPassword(req.NewPassword)
	if err != nil {
		m.Error("密码哈希失败", zap.Error(err))
		c.ResponseError(errors.New("密码处理失败"))
		return
	}
	err = m.userDB.UpdateUsersWithField("password", newHashedPassword, loginUID)
	if err != nil {
		m.Error("修改用户密码错误", zap.Error(err))
		c.Response("修改用户密码错误")
		return
	}
	// 清除token缓存
	oldToken, err := m.ctx.Cache().Get(fmt.Sprintf("%s%d%s", m.ctx.GetConfig().Cache.UIDTokenCachePrefix, config.Web, user.UID))
	if err != nil {
		m.Error("获取旧token错误", zap.Error(err))
		c.ResponseError(errors.New("获取旧token错误"))
		return
	}
	if oldToken != "" {
		err = m.ctx.Cache().Delete(m.ctx.GetConfig().Cache.TokenCachePrefix + oldToken)
		if err != nil {
			m.Error("清除旧token数据错误", zap.Error(err))
			c.ResponseError(errors.New("清除旧token数据错误"))
			return
		}
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

	username := string(wkhttp.SuperAdmin)
	role := string(wkhttp.SuperAdmin)
	var pwd = m.ctx.GetConfig().AdminPwd
	hashedPwd, hashErr := HashPassword(pwd)
	if hashErr != nil {
		m.Error("密码哈希失败", zap.Error(hashErr))
		return
	}
	err = m.userDB.Insert(&Model{
		UID:      m.ctx.GetConfig().Account.AdminUID,
		Name:     "超级管理员",
		ShortNo:  "30000",
		Category: "system",
		Role:     role,
		Username: username,
		Zone:     "0086",
		Phone:    "13000000002",
		Status:   1,
		Password: hashedPwd,
	})
	if err != nil {
		m.Error("新增系统管理员错误", zap.Error(err))
		return
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
