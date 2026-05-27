package user

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/source"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	rd "github.com/go-redis/redis"
	"github.com/gocraft/dbr/v2"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/network"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	common2 "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

var (
	ErrUserNeedVerification = errors.New("user need verification") // 用户需要验证
)

// qrcodeChanMap stores channels for QR code login long-polling.
// Concurrency safety is ensured by qrcodeChanLock:
// - SendQRCodeInfo: holds lock during both map read AND channel send (no TOCTOU)
// - removeQRCodeChan: holds lock during map delete AND channel close
// - getQRCodeModelChan: holds lock during map write
// The channel is buffered (size 1) to prevent message loss between
// getQRCodeModelChan return and the caller's select/receive.
// See: #294, #345 for race condition fixes.
var qrcodeChanMap = map[string]chan *common.QRCodeModel{}
var qrcodeChanLock sync.RWMutex

// User 用户相关API
type User struct {
	db            *DB
	friendDB      *friendDB
	deviceDB      *deviceDB
	smsServie     commonapi.ISMSService
	fileService   file.IService
	settingDB     *SettingDB
	onlineDB      *onlineDB
	userService   IService
	onlineService *OnlineService
	giteeDB       *giteeDB
	githubDB      *githubDB
	pinnedDB       *PinnedDB
	pinned         *Pinned
	spaceSettingDB *SpaceSettingDB

	setting *Setting
	log.Log
	ctx                      *config.Context
	userDeviceTokenPrefix    string
	loginUUIDPrefix          string
	openapiAuthcodePrefix    string
	openapiAccessTokenPrefix string
	loginLog                 *LoginLog
	identitieDB              *identitieDB
	onetimePrekeysDB         *onetimePrekeysDB
	maillistDB               *maillistDB
	commonService            common2.IService
	deviceFlagDB             *deviceFlagDB
	deviceFlagsCache         []*deviceFlagModel
	deviceFlagsOnce          sync.Once
	deviceFlagsErr           error
	appService               app.IService
	loginGuard               *LoginGuard
	verificationDB           *verificationDB
}

// New New
func New(ctx *config.Context) *User {
	u := &User{
		ctx:                      ctx,
		db:                       NewDB(ctx),
		deviceDB:                 newDeviceDB(ctx),
		friendDB:                 newFriendDB(ctx),
		smsServie:                commonapi.NewSMSService(ctx),
		settingDB:                NewSettingDB(ctx.DB()),
		setting:                  NewSetting(ctx),
		userDeviceTokenPrefix:    common.UserDeviceTokenPrefix,
		loginUUIDPrefix:          "loginUUID:",
		openapiAuthcodePrefix:    "openapi:authcodePrefix:",
		openapiAccessTokenPrefix: "openapi:accessTokenPrefix:",
		onlineDB:                 newOnlineDB(ctx),
		onlineService:            NewOnlineService(ctx),
		Log:                      log.NewTLog("User"),
		fileService:              file.NewService(ctx),
		userService:              NewService(ctx),
		loginLog:                 NewLoginLog(ctx),
		identitieDB:              newIdentitieDB(ctx),
		onetimePrekeysDB:         newOnetimePrekeysDB(ctx),
		maillistDB:               newMaillistDB(ctx),
		deviceFlagDB:             newDeviceFlagDB(ctx),
		giteeDB:                  newGiteeDB(ctx),
		githubDB:                 newGithubDB(ctx),
		commonService:            common2.NewService(ctx),
		appService:               app.NewService(ctx),
		loginGuard:               NewLoginGuard(ctx.GetRedisConn(), loginGuardThresholdFromEnv(), loginGuardWindowFromEnv()),
		pinnedDB:                 NewPinnedDB(ctx),
		spaceSettingDB:           NewSpaceSettingDB(ctx.DB()),
		verificationDB:           newVerificationDB(ctx),
	}
	u.pinned = NewPinned(u.pinnedDB, u.friendDB)
	InitGlobalPinnedDB(ctx) // 初始化全局 PinnedDB 供其他模块调用
	u.updateSystemUserToken()
	source.SetUserProvider(u)
	// 注入外部 IdP 登录 handler:Service 通过 IService 暴露 LoginByExternalIdentity,
	// 但实际逻辑落在 *User 上（依赖 execLogin / createUserWithRespAndTx 等私有方法）。
	if svc, ok := u.userService.(*Service); ok {
		svc.SetExternalLoginHandler(u)
		// 同款反向注入:VerifyPasswordByUID / Send|VerifyOIDCBindSMS 都依赖
		// *User 私有的 loginGuard / smsServie / db.QueryByUID,Service 持不到。
		svc.SetOIDCBindHandler(u)
	}

	return u
}

// Route 路由配置
func (u *User) Route(r *wkhttp.WKHttp) {
	// 端点级严格 per-IP 限流：防暴力破解 / 撞库 / 手机号枚举 / SMS 费用 DoS
	// 同类端点共享一个限流器实例，使同一 IP 的总配额受控，避免攻击者跨端点分散
	rlCtx := context.Background()
	// 限流状态存 Redis，多副本共享配额；生命周期跟随进程，与 main.go 的做法一致
	// PoolSize 显式设 10：理由同 main.go——限流 Lua 脚本短事务，不需要大池。
	rlRedis := rd.NewClient(octoredis.MustBuildOptions(u.ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	}))
	// burst 取小值：人类正常重试容忍 + 不给攻击者初始白嫖窗口
	// tag 用稳定字符串分离 keyspace；注意 register 和 sms 参数相同但语义不同，必须分开
	loginLimit := r.StrictIPRateLimitMiddleware(rlCtx, rlRedis, "login", 10.0/60, 5)       // 10 req/min, burst 5
	verifyLimit := r.StrictIPRateLimitMiddleware(rlCtx, rlRedis, "verify", 1000.0/60, 100) // 1000 req/min, burst 100 (Gateway traffic)
	registerLimit := r.StrictIPRateLimitMiddleware(rlCtx, rlRedis, "register", 5.0/60, 3)  // 5 req/min, burst 3
	smsLimit := r.StrictIPRateLimitMiddleware(rlCtx, rlRedis, "sms", 5.0/60, 3)            // 5 req/min, burst 3
	searchLimit := r.StrictIPRateLimitMiddleware(rlCtx, rlRedis, "search", 30.0/60, 15)    // 30 req/min, burst 15

	auth := r.Group("/v1", u.ctx.AuthMiddleware(r))
	{

		auth.GET("/users/:uid", u.get) // 根据uid查询用户信息
		// 获取用户的会话信息
		// auth.GET("/users/:uid/conversation", u.userConversationInfoGet)

		auth.GET("/user/search", searchLimit, u.search)
		auth.POST("/users/:uid/avatar", u.uploadAvatar)              //上传用户头像
		auth.PUT("/users/:uid/setting", u.setting.userSettingUpdate) // 更新用户设置
	}

	user := r.Group("/v1/user", u.ctx.AuthMiddleware(r))
	{
		user.POST("/device_token", u.registerUserDeviceToken)      // 注册用户设备
		user.DELETE("/device_token", u.unregisterUserDeviceToken)  // 卸载用户设备
		user.POST("/device_badge", u.registerUserDeviceBadge)      // 上传设备红点数量
		user.GET("/grant_login", u.grantLogin)                     // 授权登录
		user.GET("/current", u.currentUser)                        // 获取当前登录用户信息（含 self 实名字段）
		user.PUT("/current", u.userUpdateWithField)                //修改用户信息
		user.GET("/qrcode", u.qrcodeMy)                            // 我的二维码
		user.PUT("/my/setting", u.userUpdateSetting)               // 更新我的设置
		user.POST("/blacklist/:uid", u.addBlacklist)               //添加黑名单
		user.DELETE("/blacklist/:uid", u.removeBlacklist)          //移除黑名单
		user.GET("/blacklists", u.blacklists)                      //黑名单列表
		user.POST("/chatpwd", u.setChatPwd)                        //设置聊天密码
		user.POST("/lockscreenpwd", u.setLockScreenPwd)            //设置锁屏密码
		user.PUT("/lock_after_minute", u.lockScreenAfterMinuteSet) // 设置多久后锁屏
		user.DELETE("/lockscreenpwd", u.closeLockScreenPwd)        //关闭锁屏密码
		user.GET("/customerservices", u.customerservices)          //客服列表
		user.DELETE("/destroy/:code", u.destroyAccount)            // 注销用户（即时，已废弃但保留兼容）
		user.POST("/sms/destroy", u.sendDestroyCode)               //获取注销账号短信验证码
		user.POST("/destroy/apply", u.destroyApply)                // 申请注销（进入冷静期）
		user.POST("/destroy/cancel", u.destroyCancel)              // 撤销注销申请
		user.GET("/destroy/status", u.destroyStatus)               // 查询注销状态
		user.PUT("/updatepassword", u.updatePwd)                   // 修改登录密码
		user.POST("/web3publickey", u.uploadWeb3PublicKey)         // 上传web3公钥
		user.POST("/quit", u.quit)                                 // 退出登录
		// #################### 登录设备管理 ####################
		user.GET("/devices", u.deviceList)                 // 用户登录设备
		user.DELETE("/devices/:device_id", u.deviceDelete) // 删除登录设备
		user.GET("/devices/:device_id", u.getDevice)       // 查询某个登录设备
		user.GET("/online", u.onlineList)                  // 用户在线列表（我的设备和我的好友）
		user.POST("/online", u.onlinelistWithUIDs)         // 获取指定的uid在线状态
		user.POST("/pc/quit", u.pcQuit)                    // 退出pc登录

		// #################### 用户通讯录 ####################
		user.POST("/maillist", u.addMaillist)
		user.GET("/maillist", u.getMailList)

		// #################### 用户红点 ####################
		user.GET("/reddot/:category", u.getRedDot)      // 获取用户红点
		user.DELETE("/reddot/:category", u.clearRedDot) // 清除红点
	}

	// #################### 用户置顶频道（需要 Space 隔离）####################
	pinned := r.Group("/v1/user/pinned", u.ctx.AuthMiddleware(r), spacepkg.SpaceMiddleware(u.ctx))
	{
		pinned.POST("", u.pinned.Add)            // 添加置顶
		pinned.DELETE("", u.pinned.Remove)       // 移除置顶
		pinned.GET("", u.pinned.List)            // 获取置顶列表
		pinned.PUT("/sort", u.pinned.UpdateSort) // 更新排序
	}

	// #################### Space 级用户设置 ####################
	spaceSetting := r.Group("/v1/user/space", u.ctx.AuthMiddleware(r), spacepkg.SpaceMiddleware(u.ctx))
	{
		spaceSetting.GET("/setting", u.getSpaceSetting)
		spaceSetting.PUT("/setting", u.updateSpaceSetting)
	}
	v := r.Group("/v1")
	{

		v.POST("/user/register", registerLimit, u.register)                 //用户注册
		v.POST("/user/login", loginLimit, u.login)                          // 用户登录
		v.POST("/user/usernamelogin", loginLimit, u.usernameLogin)          // 用户名登录
		v.POST("/user/usernameregister", registerLimit, u.usernameRegister) // 用户名注册
		v.POST("/user/emaillogin", loginLimit, u.emailLogin)                // 邮箱登录
		v.POST("/user/emailregister", registerLimit, u.emailRegister)       // 邮箱注册
		v.POST("/user/email/sendcode", smsLimit, u.emailSendCode)           // 发送邮箱验证码
		v.POST("/user/email/forgetpwd", loginLimit, u.emailForgetPwd)       // 邮箱忘记密码

		v.POST("/user/pwdforget_web3", u.resetPwdWithWeb3PublicKey) // 通过web3公钥重置密码
		v.GET("/user/web3verifytext", u.getVerifyText)              // 获取验证字符串
		v.POST("/user/web3verifysign", u.web3verifySignature)       // 验证签名
		//v.POST("user/wxlogin", u.wxLogin)
		v.POST("/user/sms/forgetpwd", smsLimit, u.getForgetPwdSMS) //获取忘记密码验证码
		v.POST("/user/pwdforget", loginLimit, u.pwdforget)         //重置登录密码
		v.GET("/users/:uid/avatar", u.UserAvatar)                  // 用户头像
		v.GET("/users/:uid/im", u.userIM)                          // 获取用户所在IM节点信息
		v.GET("/user/loginuuid", u.getLoginUUID)                   // 获取扫描用的登录uuid
		v.GET("/user/loginstatus", u.getloginStatus)
		v.POST("/user/sms/registercode", smsLimit, u.sendRegisterCode)             //获取注册短信验证码
		v.POST("/user/login_authcode/:auth_code", loginLimit, u.loginWithAuthCode) // 通过认证码登录
		v.POST("/user/sms/login_check_phone", smsLimit, u.sendLoginCheckPhoneCode) //发送登录设备验证验证码
		v.POST("/user/login/check_phone", loginLimit, u.loginCheckPhone)           //登录验证设备手机号

		// #################### Token / Bot 认证验证（供 Gateway 调用） ####################
		v.POST("/auth/verify", verifyLimit, u.authVerifyToken)   // 验证用户 token
		v.POST("/auth/verify-bot", verifyLimit, u.authVerifyBot) // 验证 Bot API Key
		// ↑ Verify endpoints are rate-limited (1000 req/min/IP). For production,
		// restrict access at network level (nginx allow internal IPs only) or
		// add X-Internal-Key header validation.

		// #################### 第三方授权 ####################
		v.GET("/user/thirdlogin/authcode", u.thirdAuthcode)     // 第三方授权码获取
		v.GET("/user/thirdlogin/authstatus", u.thirdAuthStatus) // github认证页面
		// github
		v.GET("/user/github", u.github)            // github认证页面
		v.GET("/user/oauth/github", u.githubOAuth) // github登录
		// gitee
		v.GET("/user/gitee", u.gitee)            // gitee认证页面
		v.GET("/user/oauth/gitee", u.giteeOAuth) // gitee登录

	}

	// /v1/internal/verify-token —— Aegis OIDC Phase 2d 翻译层 (YUJ-394)
	//
	// 老的 verify-service HMAC 回调 /v1/internal/verification/complete 与 5 分钟 JWT
	// 签发 /v1/internal/verify-token 已随 Aegis OIDC 直切(YUJ-382 / Aegis OIDC Phase 1)
	// 全部废弃,新链路走 oidc callback 直接写 user_verification。
	// /verification/complete 彻底删除:合法客户端只有 verify-service 自己,该服务已下线。
	//
	// 但已发布的老 App 仍会调用 /v1/internal/verify-token 来获取一个"跳转 URL"去做实名。
	// Phase 1 临时改成 410 Gone,会让老 App 点去认证就报错 —— 用户体验不可接受。
	// Phase 2d 恢复该接口为"翻译层":认证后直接返回 Aegis 账户页 URL,不再签任何
	// HMAC/JWT,只是代理返回一个稳定 URL。
	// 保留 AuthMiddleware —— 不能让未登录用户拿到携带 return_to 的认证跳转。
	internal := r.Group("/v1/internal", u.ctx.AuthMiddleware(r))
	{
		internal.GET("/verify-token", u.verifyTokenAegisRedirect)
		internal.POST("/verify-token", u.verifyTokenAegisRedirect)
	}

	u.ctx.AddOnlineStatusListener(u.onlineService.listenOnlineStatus) // 监听在线状态
	u.ctx.AddOnlineStatusListener(u.handleOnlineStatus)               // 需要放在listenOnlineStatus之后
	u.ctx.Schedule(time.Minute*5, u.onlineStatusCheck)                // 在线状态定时检查
	u.ctx.Schedule(time.Minute*5, u.checkDestroyExpired)              // 注销冷静期到期扫描

}

// app退出登录
func (u *User) quit(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	err := u.ctx.QuitUserDevice(loginUID, int(config.Web)) // 退出web
	if err != nil {
		u.Error("退出web设备失败", zap.Error(err))
		c.ResponseError(errors.New("退出web设备失败"))
		return
	}

	err = u.ctx.QuitUserDevice(loginUID, int(config.PC))
	if err != nil {
		u.Error("退出PC设备失败", zap.Error(err))
		c.ResponseError(errors.New("退出PC设备失败"))
		return
	}

	err = u.ctx.GetRedisConn().Del(fmt.Sprintf("%s%s", u.userDeviceTokenPrefix, loginUID))
	if err != nil {
		u.Error("删除设备token失败！", zap.Error(err))
		c.ResponseError(errors.New("删除设备token失败！"))
		return
	}
	c.ResponseOK()
}

// 清除红点
func (u *User) clearRedDot(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	category := c.Param("category")
	if category == "" {
		c.ResponseError(errors.New("分类不能为空"))
		return
	}
	userRedDot, err := u.db.queryUserRedDot(loginUID, category)
	if err != nil {
		u.Error("查询用户红点错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户红点错误"))
		return
	}
	if userRedDot != nil {
		userRedDot.Count = 0
		err = u.db.updateUserRedDot(userRedDot)
		if err != nil {
			u.Error("修改用户红点错误", zap.Error(err))
			c.ResponseError(errors.New("查询用户红点错误"))
			return
		}
	}
	c.ResponseOK()
}

// 获取用户红点
func (u *User) getRedDot(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	category := c.Param("category")
	if category == "" {
		c.ResponseError(errors.New("分类不能为空"))
		return
	}
	userRedDot, err := u.db.queryUserRedDot(loginUID, UserRedDotCategoryFriendApply)
	if err != nil {
		u.Error("查询用户红点错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户红点错误"))
		return
	}
	count := 0
	isDot := 0
	if userRedDot != nil {
		count = userRedDot.Count
		isDot = userRedDot.IsDot
	}
	c.Response(map[string]interface{}{
		"count":  count,
		"is_dot": isDot,
	})
}

// updateSystemUserToken 更新系统账号token
func (u *User) updateSystemUserToken() {
	_, err := u.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         u.ctx.GetConfig().Account.SystemUID,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
		Token:       util.GenerUUID(),
	})
	if err != nil {
		u.Error("更新IM的token失败！", zap.Error(err))
	}

	_, err = u.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         u.ctx.GetConfig().Account.FileHelperUID,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
		Token:       util.GenerUUID(),
	})
	if err != nil {
		u.Error("更新IM的token失败！", zap.Error(err))
	}

	// 系统管理员
	_, err = u.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         u.ctx.GetConfig().Account.AdminUID,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
		Token:       util.GenerUUID(),
	})
	if err != nil {
		u.Error("更新IM的token失败！", zap.Error(err))
	}

}

// UserAvatar 用户头像
func (u *User) UserAvatar(c *wkhttp.Context) {
	uid := c.Param("uid")
	v := c.Query("v")
	if u.ctx.GetConfig().IsVisitor(uid) {
		c.Header("Content-Type", "image/jpeg")
		avatarBytes, err := os.ReadFile("assets/assets/visitor.png")
		if err != nil {
			u.Error("头像读取失败！", zap.Error(err))
			c.Writer.WriteHeader(http.StatusNotFound)
			return
		}
		c.Writer.Write(avatarBytes)
		return
	}
	if uid == u.ctx.GetConfig().Account.SystemUID {
		c.Header("Content-Type", "image/jpeg")
		avatarBytes, err := os.ReadFile("assets/assets/u_10000.png")
		if err != nil {
			u.Error("系统用户头像读取失败！", zap.Error(err))
			c.Writer.WriteHeader(http.StatusNotFound)
			return
		}
		c.Writer.Write(avatarBytes)
		return
	}
	if uid == u.ctx.GetConfig().Account.FileHelperUID {
		c.Header("Content-Type", "image/jpeg")
		avatarBytes, err := os.ReadFile("assets/assets/fileHelper.jpeg")
		if err != nil {
			u.Error("文件传输助手头像读取失败！", zap.Error(err))
			c.Writer.WriteHeader(http.StatusNotFound)
			return
		}
		c.Writer.Write(avatarBytes)
		return
	}

	userInfo, err := u.db.QueryByUID(uid)
	if err != nil {
		u.Error("查询用户信息错误", zap.Error(err))
		c.Writer.WriteHeader(http.StatusNotFound)
		return
	}
	if userInfo == nil {
		u.Error("用户不存在", zap.Error(err))
		c.Writer.WriteHeader(http.StatusNotFound)
		return
	}
	ph := ""
	downloadUrl := ""
	if userInfo.IsUploadAvatar == 1 {
		avatarID := crc32.ChecksumIEEE([]byte(uid)) % uint32(u.ctx.GetConfig().Avatar.Partition)
		ph = fmt.Sprintf("/avatar/%d/%s.png", avatarID, uid)
	} else {
		// 配置使用本地默认头像
		if u.ctx.GetConfig().Avatar.Default != "" && strings.TrimSpace(u.ctx.GetConfig().Avatar.DefaultBaseURL) == "" {
			// 读取配置的头像文件
			avatarPath := u.ctx.GetConfig().Avatar.Default
			imageData, err := os.ReadFile(avatarPath)
			if err != nil {
				u.Error("打开本地头像文件失败", zap.Error(err))
			} else {
				c.Header("Content-Type", "image/png")
				c.Header("Content-Disposition", "inline; filename=avatar.png")
				c.Header("Cache-Control", "public, max-age=86400")
				c.Data(http.StatusOK, "image/png", imageData)
				return
			}
		}

		if strings.TrimSpace(u.ctx.GetConfig().Avatar.DefaultBaseURL) != "" {
			avatarID := crc32.ChecksumIEEE([]byte(uid)) % uint32(u.ctx.GetConfig().Avatar.DefaultCount)
			ph = fmt.Sprintf("/avatar/default/test (%d).jpg", avatarID)
			downloadUrl = strings.ReplaceAll(u.ctx.GetConfig().Avatar.DefaultBaseURL, "{avatar}", fmt.Sprintf("%d", avatarID))
		} else {
			// 本地生成基于 UID 的彩色圆形默认头像
			imageData, genErr := generateDefaultAvatar(uid)
			if genErr != nil {
				u.Error("生成默认头像失败", zap.Error(genErr))
				c.Writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			c.Header("Content-Type", "image/png")
			c.Header("Content-Disposition", "inline; filename=avatar.png")
			c.Header("Cache-Control", "public, max-age=86400")
			c.Data(http.StatusOK, "image/png", imageData)
			return
		}
	}
	if downloadUrl == "" {
		downloadUrl, err = u.fileService.DownloadURL(ph, "")
		if err != nil {
			u.Error("获取文件下载地址失败", zap.Error(err))
			c.Writer.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	if strings.Contains(downloadUrl, "?") {
		c.Redirect(http.StatusFound, fmt.Sprintf("%s&v=%s", downloadUrl, v))
	} else {
		c.Redirect(http.StatusFound, fmt.Sprintf("%s?v=%s", downloadUrl, v))
	}
}

// uploadAvatar 上传用户头像
func (u *User) uploadAvatar(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	targetUID := c.Param("uid")
	if targetUID == "" {
		targetUID = loginUID
	}

	// 若 targetUID 与 loginUID 不同，需确认 loginUID 有权限修改该头像
	if targetUID != loginUID {
		var creatorUID string
		err := u.ctx.DB().Select("IFNULL(creator_uid,'')").From("robot").Where("robot_id=? and status=1", targetUID).LoadOne(&creatorUID)
		if err != nil || creatorUID != loginUID {
			// User Bot 校验失败，尝试 App Bot 权限校验
			var appBot struct {
				Scope   string `db:"scope"`
				SpaceID string `db:"space_id"`
			}
			cnt, appErr := u.ctx.DB().SelectBySql(
				"SELECT scope, IFNULL(space_id,'') as space_id FROM app_bot WHERE uid=? LIMIT 1", targetUID,
			).Load(&appBot)
			if appErr != nil || cnt == 0 {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "无权限修改该用户头像", "status": 403})
				return
			}
			switch appBot.Scope {
			case "platform":
				if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
					c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "无权限修改该用户头像", "status": 403})
					return
				}
			case "space":
				// superAdmin 可管理任何 space Bot（与 updateBot admin 路由一致）
				if saErr := c.CheckLoginRoleIsSuperAdmin(); saErr != nil {
					// 非 superAdmin，fallback 到 space_member 校验
					var member struct {
						Role int `db:"role"`
					}
					mCnt, mErr := u.ctx.DB().SelectBySql(
						"SELECT role FROM space_member WHERE space_id=? AND uid=? AND status=1 LIMIT 1", appBot.SpaceID, loginUID,
					).Load(&member)
					if mErr != nil || mCnt == 0 || member.Role < 1 {
						c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "无权限修改该用户头像", "status": 403})
						return
					}
				}
			default:
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "无权限修改该用户头像", "status": 403})
				return
			}
		}
	}

	if c.Request.MultipartForm == nil {
		err := c.Request.ParseMultipartForm(1024 * 1024 * 20) // 20M
		if err != nil {
			u.Error("数据格式不正确！", zap.Error(err))
			c.ResponseError(errors.New("数据格式不正确！"))
			return
		}
	}
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		u.Error("读取文件失败！", zap.Error(err))
		c.ResponseError(errors.New("读取文件失败！"))
		return
	}
	avatarID := crc32.ChecksumIEEE([]byte(targetUID)) % uint32(u.ctx.GetConfig().Avatar.Partition)
	_, err = u.fileService.UploadFile(fmt.Sprintf("avatar/%d/%s.png", avatarID, targetUID), "image/png", "", func(w io.Writer) error {
		_, err := io.Copy(w, file)
		return err
	})
	defer file.Close()
	if err != nil {
		u.Error("上传文件失败！", zap.Error(err))
		c.ResponseError(errors.New("上传文件失败！"))
		return
	}
	friends, err := u.friendDB.QueryFriends(targetUID)
	if err != nil {
		u.Error("查询用户好友失败")
		return
	}
	if len(friends) > 0 {
		uids := make([]string, 0)
		for _, friend := range friends {
			uids = append(uids, friend.ToUID)
		}
		// 发送头像更新命令
		err = u.ctx.SendCMD(config.MsgCMDReq{
			CMD:         common.CMDUserAvatarUpdate,
			Subscribers: uids,
			Param: map[string]interface{}{
				"uid": targetUID,
			},
		})
		if err != nil {
			u.Error("发送个人头像更新命令失败！")
			return
		}
	}
	//更改用户上传头像状态
	err = u.db.UpdateUsersWithField("is_upload_avatar", "1", targetUID)
	if err != nil {
		u.Error("修改用户是否修改头像错误！", zap.Error(err))
		c.ResponseError(errors.New("修改用户是否修改头像错误！"))
		return
	}
	c.ResponseOK()
}

// 获取用户的IM连接地址
func (u *User) userIM(c *wkhttp.Context) {
	uid := c.Param("uid")
	headers := map[string]string{}
	if mt := u.ctx.GetConfig().WuKongIM.ManagerToken; mt != "" {
		headers["token"] = mt
	}
	resp, err := network.Get(fmt.Sprintf("%s/route?uid=%s", u.ctx.GetConfig().WuKongIM.APIURL, uid), nil, headers)
	if err != nil {
		u.Error("调用IM服务失败！", zap.Error(err))
		c.ResponseError(errors.New("调用IM服务失败！"))
		return
	}
	var resultMap map[string]interface{}
	err = util.ReadJsonByByte([]byte(resp.Body), &resultMap)
	if err != nil {
		c.ResponseError(err)
		return
	}
	c.JSON(resp.StatusCode, resultMap)
}

func (u *User) qrcodeMy(c *wkhttp.Context) {
	userModel, err := u.db.QueryByUID(c.GetLoginUID())
	if err != nil {
		c.ResponseErrorf("查询当前用户信息失败！", err)
		return
	}
	if userModel == nil {
		c.ResponseError(errors.New("登录用户不存在！"))
		return
	}
	if userModel.QRVercode == "" {
		c.ResponseError(errors.New("用户没有QRVercode，非法操作！"))
		return
	}
	path := strings.ReplaceAll(u.ctx.GetConfig().QRCodeInfoURL, ":code", fmt.Sprintf("vercode_%s", userModel.QRVercode))
	c.Response(gin.H{
		"data": fmt.Sprintf("%s/%s", u.ctx.GetConfig().External.BaseURL, path),
	})
}

// currentUser 返回当前登录用户的权威 profile（含 self 实名字段）。
//
// YUJ-413：/v1/user/login 和 GET /v1/user/current 必须下发 realname_verified /
// real_name / realname_verified_at 三字段，否则 Web/Android/iOS 三端 self 实名
// 徽章和 displayName 无法渲染（friend/sync、conversation/sync 对他人已下发
// 同名字段，唯独 self 路径漏加）。
//
// 客户端调用场景：
//   - Android VerifyLandingActivity → UserModel.refreshCurrentUser()；
//   - iOS   WKRealnameVerifyManager Custom Tabs 回跳；
//   - Web   loginSuccess() 后亦可作 fallback；
//
// 语义 vs POST /v1/user/login：
//   - 结构完全对齐 loginUserDetailResp，客户端可共用 parser；
//   - token 字段回显当前请求头里的 token（不换发），保持会话稳定；
//   - realname_verified / real_name / realname_verified_at 从 user_verification
//     表读取；未实名用户 realname_verified=false，其它字段 omitempty 省略。
func (u *User) currentUser(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	if loginUID == "" {
		c.ResponseError(errors.New("未登录"))
		return
	}
	userInfo, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询当前用户信息失败", zap.Error(err), zap.String("uid", loginUID))
		c.ResponseError(errors.New("查询当前用户信息失败"))
		return
	}
	if userInfo == nil {
		c.ResponseError(errors.New("当前用户不存在"))
		return
	}
	// token 回显请求头 token：/user/current 不换发 token,避免干扰现有会话;
	// 客户端本身就用这个 token 调的接口,回填仅为结构对齐 login response。
	resp := newLoginUserDetailResp(userInfo, c.GetHeader("token"), u.ctx)
	u.applyRealnameToLoginResp(resp, userInfo.UID)
	c.Response(resp)
}

// 修改用户信息
func (u *User) userUpdateWithField(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	var reqMap map[string]interface{}
	if err := c.BindJSON(&reqMap); err != nil {
		u.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	// 查询用户信息
	users, err := u.db.QueryByUID(loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询用户信息出错！"))
		return
	}
	if users == nil {
		c.ResponseError(errors.New("用户信息不存在！"))
		return
	}

	for key, value := range reqMap {
		//是否允许更新此field
		if !allowUpdateUserField(key) {
			c.ResponseError(errors.New("不允许更新【" + key + "】"))
			return
		}
		if key == "short_no" {
			if u.ctx.GetConfig().ShortNo.EditOff {
				c.ResponseError(errors.New("不允许编辑！"))
				return
			}
			if users.ShortStatus == 1 {
				c.ResponseError(errors.New("用户短编号只能修改一次"))
				return
			}
			if len(fmt.Sprintf("%v", value)) < 6 || len(fmt.Sprintf("%v", value)) > 20 {
				c.ResponseError(errors.New("短号须以字母开头，仅支持使用6～20个字母、数字、下划线、减号自由组合"))
				return
			}
			isLetter := true
			isIncludeNum := false
			for index, r := range fmt.Sprintf("%v", value) {
				if !unicode.IsLetter(r) && index == 0 {
					isLetter = false
					break
				}
				if unicode.Is(unicode.Han, r) {
					isLetter = false
					break
				}
				if unicode.IsDigit(r) {
					isIncludeNum = true
				}
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
					isLetter = false
					break
				}
			}
			if !isLetter || !isIncludeNum {
				c.ResponseError(errors.New("短号须以字母开头，仅支持使用6～20个字母、数字、下划线、减号自由组合"))
				return
			}
			users, err = u.db.QueryUserWithOnlyShortNo(fmt.Sprintf("%v", value))
			if err != nil {
				u.Error("通过short_no查询用户失败！", zap.Error(err), zap.String("shortNo", key))
				c.ResponseError(errors.New("通过short_no查询用户失败！"))
				return
			}
			if users != nil {
				c.ResponseError(errors.New("已存在，请换一个！"))
				return
			}

			tx, err := u.db.session.Begin()
			if err != nil {
				u.Error("创建事务失败！", zap.Error(err))
				c.ResponseError(errors.New("创建事务失败！"))
				return
			}
			defer func() {
				if err := recover(); err != nil {
					tx.Rollback()
					fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
				}
			}()
			err = u.db.UpdateUsersWithFieldTx(key, fmt.Sprintf("%v", value), loginUID, tx)
			if err != nil {
				c.ResponseError(errors.New("修改用户资料失败"))
				tx.Rollback()
				return
			}
			err = u.db.UpdateUsersWithFieldTx("short_status", "1", loginUID, tx)
			if err != nil {
				u.Error("修改用户资料失败", zap.Error(err), zap.Any(key, value))
				c.ResponseError(errors.New("修改用户资料失败"))
				tx.Rollback()
				return
			}
			err = tx.Commit()
			if err != nil {
				u.Error("数据库事物提交失败", zap.Error(err))
				c.ResponseError(errors.New("数据库事物提交失败"))
				tx.Rollback()
				return
			}
			c.ResponseOK()
			return
		}
		//修改用户信息
		if key == "name" {
			nameStr := fmt.Sprintf("%s", value)
			if nameStr == "" {
				c.ResponseError(errors.New("名字不能为空！"))
				return
			}
			if err := ValidateName(nameStr); err != nil {
				c.ResponseError(err)
				return
			}
		}

		err = u.db.UpdateUsersWithField(key, fmt.Sprintf("%v", value), loginUID)
		if err != nil {
			u.Error("修改用户资料失败", zap.Error(err))
			c.ResponseError(errors.New("修改用户资料失败"))
			return
		}
		if key == "name" {
			// 将重新设置token设置到缓存（这里主要是更新登录者的name）
			payload, encErr := auth.Encode(auth.TokenInfo{UID: loginUID, Name: fmt.Sprintf("%v", value), Role: c.GetLoginRole()})
			if encErr != nil {
				u.Error("编码token缓存失败！", zap.Error(encErr))
				c.ResponseError(errors.New("重新设置token缓存失败！"))
				return
			}
			err = u.ctx.Cache().Set(u.ctx.GetConfig().Cache.TokenCachePrefix+c.GetHeader("token"), payload)
			if err != nil {
				u.Error("重新设置token缓存失败！", zap.Error(err))
				c.ResponseError(errors.New("重新设置token缓存失败！"))
				return
			}
		}
	}
	// 发送频道刚刚消息给登录好友
	friends, err := u.friendDB.QueryFriends(loginUID)
	if err != nil {
		u.Error("查询用户好友错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户好友错误"))
		return
	}
	if len(friends) > 0 {
		uids := make([]string, 0)
		for _, friend := range friends {
			uids = append(uids, friend.ToUID)
		}
		err = u.ctx.SendCMD(config.MsgCMDReq{
			CMD:         common.CMDChannelUpdate,
			Subscribers: uids,
			Param: map[string]interface{}{
				"channel_id":   loginUID,
				"channel_type": common.ChannelTypePerson,
			},
		})
		if err != nil {
			u.Error("发送频道更改消息错误！", zap.Error(err))
			c.ResponseError(errors.New("发送频道更改消息错误！"))
			return
		}
	}

	c.ResponseOK()
}

func (u *User) userUpdateSetting(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	var reqMap map[string]interface{}
	if err := c.BindJSON(&reqMap); err != nil {
		u.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	// 查询用户信息
	users, err := u.db.QueryByUID(loginUID)
	if err != nil {
		c.ResponseError(errors.New("查询用户信息出错！"))
		return
	}
	if users == nil {
		c.ResponseError(errors.New("用户信息不存在！"))
		return
	}
	for key, value := range reqMap {
		if key == "device_lock" ||
			key == "search_by_phone" ||
			key == "search_by_short" ||
			key == "new_msg_notice" ||
			key == "msg_show_detail" ||
			key == "offline_protection" ||
			key == "voice_on" ||
			key == "shock_on" ||
			key == "mute_of_app" {
			if key == "device_lock" && fmt.Sprintf("%v", value) == "1" {
				if users.Phone == "15900000002" || users.Phone == "15900000003" || users.Phone == "15900000004" || users.Phone == "15900000005" || users.Phone == "15900000006" {
					c.ResponseError(errors.New("演示账号不支持开启设备锁"))
					return
				}

			}
			err = u.db.UpdateUsersWithField(key, fmt.Sprintf("%v", value), loginUID)
			if err != nil {
				u.Error("修改用户资料失败", zap.Error(err))
				c.ResponseError(errors.New("修改用户资料失败"))
				return
			}
		}
	}
	c.ResponseOK()
}

// 获取用户详情
func (u *User) get(c *wkhttp.Context) {
	uid := c.Param("uid")
	groupNo := c.Query("group_no")
	loginUID := c.MustGet("uid").(string)

	if u.ctx.GetConfig().IsVisitor(uid) { // 访客频道
		c.Request.URL.Path = fmt.Sprintf("/v1/hotline/visitors/%s/im", uid)
		u.ctx.GetHttpRoute().HandleContext(c)
		return
	}

	userDetailResp, err := u.userService.GetUserDetail(uid, loginUID)
	if err != nil {
		u.Error("获取用户详情失败！", zap.Error(err))
		c.ResponseError(errors.New("获取用户详情失败！"))
		return
	}
	if userDetailResp == nil {
		c.ResponseError(errors.New("用户不存在！"))
		return
	}
	isShowShortNo := false
	vercode := ""
	var groupMember *model.GroupMemberResp
	if groupNo != "" {
		modules := register.GetModules(u.ctx)
		for _, m := range modules {
			if m.BussDataSource.IsShowShortNo != nil && vercode == "" {
				tempShowShortNo, tempVercode, _ := m.BussDataSource.IsShowShortNo(groupNo, uid, loginUID)
				if tempShowShortNo {
					isShowShortNo = tempShowShortNo
					vercode = tempVercode
				}
			}
			if m.BussDataSource.GetGroupMember != nil && groupMember == nil {
				groupMember, _ = m.BussDataSource.GetGroupMember(groupNo, uid)
			}
		}
	}

	if groupMember != nil && groupMember.InviteUID != "" && groupMember.IsDeleted == 0 {
		inviteJoinGroupUserInfo, err := u.userService.GetUserDetail(groupMember.InviteUID, uid)
		if err != nil {
			u.Error("获取加入群聊邀请用户详情失败！", zap.Error(err))
		}
		if inviteJoinGroupUserInfo != nil {
			var name = inviteJoinGroupUserInfo.Name
			if inviteJoinGroupUserInfo.Remark != "" {
				name = inviteJoinGroupUserInfo.Remark
			}
			userDetailResp.JoinGroupInviteUID = groupMember.InviteUID
			userDetailResp.JoinGroupTime = groupMember.CreatedAt
			userDetailResp.JoinGroupInviteName = name
		}
		userDetailResp.GroupMember = &GroupMemberResp{
			UID:                groupMember.UID,
			Name:               groupMember.Name,
			GroupNo:            groupMember.GroupNo,
			Remark:             groupMember.Remark,
			Role:               groupMember.Role,
			Status:             groupMember.Status,
			InviteUID:          groupMember.InviteUID,
			Robot:              groupMember.Role,
			ForbiddenExpirTime: groupMember.ForbiddenExpirTime,
			CreatedAt:          groupMember.CreatedAt,
		}
		// YUJ-206：补齐外部来源 / 归属 Space 视图字段（is_external /
		// source_space_id / source_space_name / home_space_id / home_space_name），
		// 供 Web/Android/iOS UserInfo 判定"同 Space 非好友 → 直接发消息" vs
		// "跨 Space 外部成员 → 仅可在群内交流"。
		// 命名与 /groups/{no}/members 的 memberDetailResp 保持一致。
		// Provider 由 group 模块在 init 阶段通过 RegisterGroupMemberExternalProvider
		// 注入；失败仅 log，不影响主响应链路（字段缺省即回落到原"is_external=0"语义，
		// 客户端会走非 Space 模式陌生人分支，属可接受的降级）。
		if provider := getGroupMemberExternalProvider(); provider != nil {
			if isExt, srcID, srcName, homeID, homeName, err := provider(groupNo, uid); err != nil {
				u.Error("查询群成员外部来源字段失败", zap.Error(err),
					zap.String("group_no", groupNo), zap.String("uid", uid))
			} else {
				userDetailResp.GroupMember.IsExternal = isExt
				userDetailResp.GroupMember.SourceSpaceID = srcID
				userDetailResp.GroupMember.SourceSpaceName = srcName
				userDetailResp.GroupMember.HomeSpaceID = homeID
				userDetailResp.GroupMember.HomeSpaceName = homeName
			}
		}
	}

	if userDetailResp.Follow == 1 || uid == loginUID {
		isShowShortNo = true
	}
	if !isShowShortNo {
		userDetailResp.ShortNo = ""
		userDetailResp.Vercode = ""
	} else {
		if groupNo != "" {
			userDetailResp.Vercode = vercode
		}
	}
	c.Response(userDetailResp)
}

//	获取用户详情
//
//	func (u *User) userConversationInfoGet(c *wkhttp.Context) {
//		uid := c.Param("uid")
//		loginUID := c.MustGet("uid").(string)
//		model, err := u.db.QueryDetailByUID(uid, loginUID)
//		if err != nil {
//			u.Error("查询用户信息失败！", zap.Error(err), zap.String("uid", uid))
//			c.ResponseError(errors.New("查询用户信息失败！"))
//			return
//		}
//		if model == nil {
//			c.ResponseError(errors.New("用户信息不存在！"))
//			return
//		}
//		userDetailResp := newUserDetailResp(model)
//		if uid == loginUID {
//			userDetailResp.Name = u.ctx.GetConfig().FileHelperName
//		}
//		c.Response(userDetailResp)
//	}
//
// 微信登录
func (u *User) wxLogin(c *wkhttp.Context) {
	type wxLoginReq struct {
		Code   string     `json:"code"`
		Flag   int        `json:"flag"`
		Device *deviceReq `json:"device"`
	}
	var req wxLoginReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if req.Code == "" {
		c.ResponseError(errors.New("微信code不能为空"))
		return
	}
	accessTokenResp, err := network.Get("https://api.weixin.qq.com/sns/oauth2/access_token", map[string]string{
		"appid":      u.ctx.GetConfig().Wechat.AppID,
		"secret":     u.ctx.GetConfig().Wechat.AppSecret,
		"code":       req.Code,
		"grant_type": "authorization_code",
	}, nil)
	if err != nil {
		u.Error("获取微信access_token错误", zap.Error(err))
		c.ResponseError(errors.New("获取微信access_token错误"))
		return
	}
	if accessTokenResp.StatusCode != http.StatusOK {
		c.ResponseErrorf("请求验证微信access_token错误", fmt.Errorf("错误代码-> %d", accessTokenResp.StatusCode))
		return
	}
	var bodyMap map[string]interface{}
	if err = util.ReadJsonByByte([]byte(accessTokenResp.Body), &bodyMap); err != nil {
		c.ResponseErrorf("解码微信access_token返回数据失败！", err)
		return
	}
	accessToken, ok := bodyMap["access_token"].(string)
	if !ok {
		c.ResponseError(errors.New("微信返回数据格式错误：缺少access_token"))
		return
	}
	openid, ok := bodyMap["openid"].(string)
	if !ok {
		c.ResponseError(errors.New("微信返回数据格式错误：缺少openid"))
		return
	}
	wxUserInfoResp, err := network.Get("https://api.weixin.qq.com/sns/userinfo", map[string]string{
		"access_token": accessToken,
		"openid":       openid,
	}, nil)
	if err != nil {
		u.Error("获取微信用户资料错误", zap.Error(err))
		c.ResponseError(errors.New("获取微信用户资料错误"))
		return
	}

	if wxUserInfoResp.StatusCode != http.StatusOK {
		c.ResponseErrorf("获取微信用户资料请求错误", fmt.Errorf("错误代码-> %d", wxUserInfoResp.StatusCode))
		return
	}

	var wxUserInfoBodyMap map[string]interface{}
	if err = util.ReadJsonByByte([]byte(wxUserInfoResp.Body), &wxUserInfoBodyMap); err != nil {
		c.ResponseErrorf("解码微信用户信息返回数据失败！", err)
		return
	}

	unionid, _ := wxUserInfoBodyMap["unionid"].(string)
	nickname, _ := wxUserInfoBodyMap["nickname"].(string)
	var sex int64
	if sexNum, ok := wxUserInfoBodyMap["sex"].(json.Number); ok {
		sex, _ = sexNum.Int64()
	}
	headimgurl, _ := wxUserInfoBodyMap["headimgurl"].(string)
	// 验证该用户是否存在
	loginSpan := u.ctx.Tracer().StartSpan(
		"login",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	loginSpanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), loginSpan)
	loginSpan.SetTag("username", nickname)
	defer loginSpan.Finish()

	userInfo, err := u.db.queryWithWXOpenIDAndWxUnionidCtx(loginSpanCtx, openid, unionid)
	if err != nil {
		u.Error("通过微信openid查询用户是否存在错误", zap.Error(err))
		c.ResponseError(errors.New("通过微信openid查询用户是否存在错误"))
		return
	}
	if userInfo != nil {
		if userInfo.IsDestroy == IsDestroyDone {
			c.ResponseError(errors.New("用户不存在"))
			return
		}
		u.execLoginAndRespose(userInfo, config.DeviceFlag(req.Flag), req.Device, loginSpanCtx, c)
	} else {
		// 创建用户
		uid := util.GenerUUID()
		var model = &createUserModel{
			UID:       uid,
			Zone:      "",
			Phone:     "",
			Password:  "",
			Sex:       int(sex),
			Name:      nickname,
			WXOpenid:  openid,
			WXUnionid: unionid,
			Flag:      req.Flag,
			Device:    req.Device,
		}
		// 下载微信用户头像并上传
		if headimgurl != "" {
			timeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			imgReader, _ := u.fileService.DownloadImage(headimgurl, timeoutCtx)
			cancel()
			if imgReader != nil {
				avatarID := crc32.ChecksumIEEE([]byte(uid)) % uint32(u.ctx.GetConfig().Avatar.Partition)
				_, err = u.fileService.UploadFile(fmt.Sprintf("avatar/%d/%s.png", avatarID, uid), "image/png", "", func(w io.Writer) error {
					_, err := io.Copy(w, imgReader)
					return err
				})
				defer imgReader.Close()
				if err == nil {
					// u.Error("上传文件失败！", zap.Error(err))
					// c.ResponseError(errors.New("上传文件失败！"))
					// return
					model.IsUploadAvatar = 1
				}
			}
		}
		u.createUser(loginSpanCtx, model, c, nil)
	}
}

// 登录
func (u *User) login(c *wkhttp.Context) {
	if common2.EnsureSystemSettings(u.ctx).LocalLoginOff() {
		c.ResponseError(errors.New("本地登录已关闭"))
		return
	}

	var req loginReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if err := req.Check(); err != nil {
		c.ResponseError(err)
		return
	}
	if err := u.loginGuard.Check(req.Username); err != nil {
		u.Warn("登录被临时锁定", zap.String("username", req.Username), zap.Error(err))
		c.ResponseError(err)
		return
	}
	loginSpan := u.ctx.Tracer().StartSpan(
		"login",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	loginSpanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), loginSpan)
	loginSpan.SetTag("username", req.Username)
	defer loginSpan.Finish()

	userInfo, err := u.db.QueryByUsernameCxt(loginSpanCtx, req.Username)
	if err != nil {
		u.Error("查询用户信息失败！", zap.String("username", req.Username))
		c.ResponseError(err)
		return
	}
	// 已注销 / 被禁用账号统一拒绝；与 emailLogin / usernameLogin 行为对齐
	if userInfo == nil || userInfo.IsDestroy == IsDestroyDone || userInfo.Status == 0 {
		u.loginGuard.RecordFailureLogged(req.Username)
		// 统一错误消息，避免攻击者通过响应差异枚举有效账号
		c.ResponseError(errors.New("用户名或密码错误"))
		return
	}
	if userInfo.Password == "" {
		// 同样走失败计数 + 通用错误消息，避免攻击者区分"账号不允许登录"与"密码错误"
		u.loginGuard.RecordFailureLogged(req.Username)
		c.ResponseError(errors.New("用户名或密码错误"))
		return
	}
	matched, needsMigration := CheckPassword(req.Password, userInfo.Password)
	if !matched {
		u.loginGuard.RecordFailureLogged(req.Username)
		c.ResponseError(errors.New("用户名或密码错误"))
		return
	}
	u.loginGuard.ResetLogged(req.Username)
	// 自动迁移 MD5 密码到 bcrypt
	if needsMigration {
		if newHash, err := HashPassword(req.Password); err == nil {
			_ = u.db.updatePassword(newHash, userInfo.UID)
		}
	}
	u.execLoginAndRespose(userInfo, config.DeviceFlag(req.Flag), req.Device, loginSpanCtx, c)
}

// 验证登录用户信息
func (u *User) execLoginAndRespose(userInfo *Model, flag config.DeviceFlag, device *deviceReq, loginSpanCtx context.Context, c *wkhttp.Context) {

	result, err := u.execLogin(userInfo, flag, device, loginSpanCtx)
	if err != nil {
		if errors.Is(err, ErrUserNeedVerification) {
			phone := ""
			if len(userInfo.Phone) > 5 {
				phone = fmt.Sprintf("%s******%s", userInfo.Phone[0:3], userInfo.Phone[len(userInfo.Phone)-2:])
			}
			c.ResponseWithStatus(http.StatusBadRequest, map[string]interface{}{
				"status": 110,
				"msg":    "需要验证手机号码！",
				"uid":    userInfo.UID,
				"phone":  phone,
			})
			return
		}
		c.ResponseError(err)
		return
	}

	c.Response(result)

	publicIP := util.GetClientPublicIP(c.Request)
	go u.sentWelcomeMsg(publicIP, userInfo.UID)
}

func (u *User) execLogin(userInfo *Model, flag config.DeviceFlag, device *deviceReq, loginSpanCtx context.Context) (*loginUserDetailResp, error) {
	if userInfo.Status == int(common.UserDisable) {
		return nil, errors.New("该用户已被禁用")
	}
	deviceLevel := config.DeviceLevelSlave
	if flag == config.APP {
		deviceLevel = config.DeviceLevelMaster
	}
	//app登录验证设备锁
	if flag == 0 && userInfo.DeviceLock == 1 {
		if device == nil {
			return nil, errors.New("登录设备信息不能为空！")
		}
		var existDevice bool
		var err error
		existDevice, err = u.deviceDB.existDeviceWithDeviceIDAndUIDCtx(loginSpanCtx, device.DeviceID, userInfo.UID)
		if err != nil {
			u.Error("查询是否存在的设备失败", zap.Error(err))
			return nil, errors.New("查询是否存在的设备失败")
		}
		if existDevice {
			err = u.deviceDB.updateDeviceLastLoginCtx(loginSpanCtx, time.Now().Unix(), device.DeviceID, userInfo.UID)
			if err != nil {
				u.Error("更新用户登录设备失败", zap.Error(err))
				return nil, errors.New("更新用户登录设备失败")
			}
		}
		if !existDevice {
			err := u.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", u.ctx.GetConfig().Cache.LoginDeviceCachePrefix, userInfo.UID), util.ToJson(device), u.ctx.GetConfig().Cache.LoginDeviceCacheExpire)
			if err != nil {
				u.Error("缓存登录设备失败！", zap.Error(err))
				return nil, errors.New("缓存登录设备失败！")
			}
			return nil, ErrUserNeedVerification
		}
	}
	//更新最后一次登录设备信息
	// flag == config.APP &&
	if device != nil {
		err := u.deviceDB.insertOrUpdateDeviceCtx(loginSpanCtx, &deviceModel{
			UID:         userInfo.UID,
			DeviceID:    device.DeviceID,
			DeviceName:  device.DeviceName,
			DeviceModel: device.DeviceModel,
			LastLogin:   time.Now().Unix(),
		})
		if err != nil {
			u.Error("更新用户登录设备失败", zap.Error(err))
			return nil, errors.New("更新用户登录设备失败")
		}

	}
	token := util.GenerUUID()
	// 将token设置到缓存
	tokenSpan, _ := u.ctx.Tracer().StartSpanFromContext(loginSpanCtx, "SetAndExpire")
	tokenSpan.SetTag("key", "token")
	// 获取老的token并清除老token数据
	oldToken, err := u.ctx.Cache().Get(fmt.Sprintf("%s%d%s", u.ctx.GetConfig().Cache.UIDTokenCachePrefix, flag, userInfo.UID))
	if err != nil {
		u.Error("获取旧token错误", zap.Error(err))
		tokenSpan.Finish()
		return nil, errors.New("获取旧token错误")
	}
	if flag == config.APP {
		if oldToken != "" {
			err = u.ctx.Cache().Delete(u.ctx.GetConfig().Cache.TokenCachePrefix + oldToken)
			if err != nil {
				u.Error("清除旧token数据错误", zap.Error(err))
				tokenSpan.Finish()
				return nil, errors.New("清除旧token数据错误")
			}
		}
	} else { // PC暂时不执行删除操作，因为PC可以同时登陆
		if strings.TrimSpace(oldToken) != "" { // 如果是web或pc类设备 因为支持多登所以这里依然使用老token
			token = oldToken
		}
	}

	tokenPayload, err := auth.Encode(auth.TokenInfo{UID: userInfo.UID, Name: userInfo.Name, Role: userInfo.Role})
	if err != nil {
		u.Error("编码token缓存失败！", zap.Error(err))
		tokenSpan.Finish()
		return nil, errors.New("设置token缓存失败！")
	}
	err = u.ctx.Cache().SetAndExpire(u.ctx.GetConfig().Cache.TokenCachePrefix+token, tokenPayload, u.ctx.GetConfig().Cache.TokenExpire)
	if err != nil {
		u.Error("设置token缓存失败！", zap.Error(err))
		tokenSpan.Finish()
		return nil, errors.New("设置token缓存失败！")
	}
	err = u.ctx.Cache().SetAndExpire(fmt.Sprintf("%s%d%s", u.ctx.GetConfig().Cache.UIDTokenCachePrefix, flag, userInfo.UID), token, u.ctx.GetConfig().Cache.TokenExpire)
	if err != nil {
		u.Error("设置uidtoken缓存失败！", zap.Error(err))
		tokenSpan.Finish()
		return nil, errors.New("设置uidtoken缓存失败！")
	}
	tokenSpan.Finish()

	updateTokenSpan, _ := u.ctx.Tracer().StartSpanFromContext(loginSpanCtx, "UpdateIMToken")

	imTokenReq := config.UpdateIMTokenReq{
		UID:         userInfo.UID,
		Token:       token,
		DeviceFlag:  config.DeviceFlag(flag),
		DeviceLevel: deviceLevel,
	}
	imResp, err := u.ctx.UpdateIMToken(imTokenReq)
	if err != nil {
		u.Error("更新IM的token失败！", zap.Error(err))
		updateTokenSpan.SetTag("err", err)
		updateTokenSpan.Finish()
		return nil, errors.New("更新IM的token失败！")
	}
	updateTokenSpan.Finish()

	if imResp.Status == config.UpdateTokenStatusBan {
		return nil, errors.New("此账号已经被封禁！")
	}

	resp := newLoginUserDetailResp(userInfo, token, u.ctx)
	u.applyRealnameToLoginResp(resp, userInfo.UID)
	return resp, nil
}

// sendWelcomeMsg 发送欢迎语
func (u *User) sentWelcomeMsg(publicIP, uid string) {
	appconfig, err := u.commonService.GetAppConfig()
	if err != nil {
		u.Error("获取应用配置错误", zap.Error(err))
	}
	if appconfig.SendWelcomeMessageOn == 0 {
		return
	}
	// 等待用户数据持久化完成（该函数在 goroutine 中调用）
	time.Sleep(500 * time.Millisecond)
	//发送登录欢迎消息
	lastLoginLog := u.loginLog.getLastLoginIP(uid)
	content := u.ctx.GetConfig().WelcomeMessage
	var sentContent string

	if appconfig != nil && appconfig.WelcomeMessage != "" {
		content = appconfig.WelcomeMessage
	}
	if lastLoginLog != nil {
		ipStr := fmt.Sprintf("上次的登录信息：%s %s\n本次登录的信息：%s %s", lastLoginLog.LoginIP, lastLoginLog.CreateAt, publicIP, util.ToyyyyMMddHHmmss(time.Now()))
		sentContent = fmt.Sprintf("%s\n%s", content, ipStr)
	} else {
		ipStr := fmt.Sprintf("本次登录的信息：%s %s", publicIP, util.ToyyyyMMddHHmmss(time.Now()))
		sentContent = fmt.Sprintf("%s\n%s", content, ipStr)
	}
	// YUJ-674 / Mininglamp-OSS#37: PERSONAL DM 走 NewPersonalMsgSendReq builder。
	// SystemUID 是平台级账户，没有 Space 上下文 → senderSpaceID = ""，builder
	// 会 fail-closed strip，与"系统欢迎消息不归属任何 Space"语义一致。
	err = u.ctx.SendMessage(config.NewPersonalMsgSendReq(
		uid,
		u.ctx.GetConfig().Account.SystemUID,
		map[string]interface{}{
			"content": sentContent,
			"type":    common.Text,
		},
		"", // SystemUID is Space-agnostic; builder strips any client-supplied space_id.
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
	))
	if err != nil {
		u.Error("发送登录消息欢迎消息失败", zap.Error(err))
	}
	//保存登录日志
	u.loginLog.add(uid, publicIP)
}

// 注册
func (u *User) register(c *wkhttp.Context) {
	var req registerReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if err := req.CheckRegister(); err != nil {
		c.ResponseError(err)
		return
	}

	if common2.EnsureSystemSettings(u.ctx).RegisterOff() {
		c.ResponseError(errors.New("注册通道暂不开放，请长按标题使用官网上演示账号登录"))
		return
	}
	// 仅中国号码闸门必须在 register 这里再判一次：sendRegisterCode 处的
	// 校验只能拦"取码"动作，但管理员把 only_china 切到 1 之前已发出去的
	// 验证码、或任何能让 smsService.Verify 通过的外部路径，都还能拿着
	// 非 0086 区号走到这里完成注册。把判断前移到 createUser 之前，
	// 闭合 time-of-check vs time-of-use 缺口。
	if common2.EnsureSystemSettings(u.ctx).RegisterOnlyChina() &&
		strings.TrimSpace(req.Zone) != "0086" {
		c.ResponseError(errors.New("仅仅支持中国大陆手机号注册！"))
		return
	}
	appConfig, err := u.commonService.GetAppConfig()
	if err != nil {
		u.Error("查询应用设置错误", zap.Error(err))
		c.ResponseError(err)
		return
	}
	var registerInviteOn = 0
	if appConfig != nil {
		registerInviteOn = appConfig.RegisterInviteOn
	}
	var invite *model.Invite
	if registerInviteOn == 1 {
		if req.InviteCode == "" {
			c.ResponseError(errors.New("邀请码不能为空"))
			return
		}
		var inviteCodeIsExist = false
		modules := register.GetModules(u.ctx)
		for _, m := range modules {
			if m.BussDataSource.GetInviteCode != nil {
				invite, _ = m.BussDataSource.GetInviteCode(req.InviteCode)
				if invite != nil && invite.Uid != "" {
					inviteCodeIsExist = true
					break
				}
			}
		}
		if !inviteCodeIsExist {
			c.ResponseError(errors.New("邀请码不存在"))
			return
		}
	}
	registerSpan := u.ctx.Tracer().StartSpan(
		"user.register",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	defer registerSpan.Finish()
	registerSpanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), registerSpan)

	registerSpan.SetTag("username", fmt.Sprintf("%s%s", req.Zone, req.Phone))
	//验证手机号是否注册
	userInfo, err := u.db.QueryByUsernameCxt(registerSpanCtx, fmt.Sprintf("%s%s", req.Zone, req.Phone))
	if err != nil {
		u.Error("查询用户信息失败！", zap.String("username", req.Phone))
		c.ResponseError(err)
		return
	}
	if userInfo != nil {
		c.ResponseError(errors.New("该用户已存在"))
		return
	}
	//测试模式（仅非 release 生效）
	if commonapi.IsTestCodeEnabled(u.ctx.GetConfig()) {
		if !commonapi.MatchTestCode(u.ctx.GetConfig(), req.Code) {
			c.ResponseError(errors.New("验证码错误"))
			return
		}
	} else {
		//线上验证短信验证码
		err = u.smsServie.Verify(registerSpanCtx, req.Zone, req.Phone, req.Code, commonapi.CodeTypeRegister)
		if err != nil {
			c.ResponseError(err)
			return
		}
	}
	uid := util.GenerUUID()
	var model = &createUserModel{
		UID:      uid,
		Sex:      1,
		Name:     req.Name,
		Zone:     req.Zone,
		Phone:    req.Phone,
		Password: req.Password,
		Flag:     int(req.Flag),
		Device:   req.Device,
	}
	u.createUser(registerSpanCtx, model, c, invite)
}

// 搜索用户
func (u *User) search(c *wkhttp.Context) {
	keyword := c.Query("keyword")
	spaceID := c.Query("space_id")
	useModel, err := u.db.QueryByKeyword(keyword)
	if err != nil {
		u.Error("查询用户信息失败！", zap.Error(err), zap.String("keyword", keyword))
		c.ResponseError(errors.New("查询用户信息失败！"))
		return
	}
	if useModel == nil {
		c.JSON(http.StatusOK, gin.H{
			"exist": 0,
		})
		return
	}
	// Space 模式：搜索结果只返回 Space 成员
	if spaceID != "" {
		isMember, err := spacepkg.CheckMembership(u.ctx.DB(), spaceID, useModel.UID)
		if err != nil {
			u.Error("校验 Space 成员错误", zap.Error(err))
			c.ResponseError(errors.New("校验成员关系错误"))
			return
		}
		if !isMember {
			c.JSON(http.StatusOK, gin.H{
				"exist": 0,
			})
			return
		}
	} else {
		// 未指定 Space：仅允许查询自己或与登录用户至少共享一个 Space 的用户，
		// 防止通过 short_no/phone/email 跨 Space 探测用户存在性。
		loginUID := c.GetLoginUID()
		if loginUID != "" && loginUID != useModel.UID {
			shared, err := spacepkg.HaveCommonSpace(u.ctx.DB(), loginUID, useModel.UID)
			if err != nil {
				u.Error("校验共同 Space 错误", zap.Error(err))
				c.ResponseError(errors.New("校验共同 Space 错误"))
				return
			}
			if !shared {
				c.JSON(http.StatusOK, gin.H{
					"exist": 0,
				})
				return
			}
		}
	}
	appconfig, _ := u.commonService.GetAppConfig()

	if keyword == useModel.Phone {
		//关闭了手机号搜索
		if useModel.SearchByPhone == 0 || (appconfig != nil && appconfig.SearchByPhone == 0) || u.ctx.GetConfig().PhoneSearchOff {
			c.JSON(http.StatusOK, gin.H{
				"exist": 0,
			})
			return
		}
	}

	if useModel.SearchByShort == 0 {
		//关闭了短编号搜索
		if strings.EqualFold(keyword, useModel.ShortNo) {
			c.JSON(http.StatusOK, gin.H{
				"exist": 0,
			})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"exist": 1,
		"data":  newUserResp(useModel),
	})
}

// 注册用户设备token
func (u *User) registerUserDeviceToken(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	var req struct {
		DeviceToken string `json:"device_token"` // 设备token
		DeviceType  string `json:"device_type"`  // 设备类型 IOS，MI，HMS
		BundleID    string `json:"bundle_id"`    // app的唯一ID标示
	}
	if err := c.BindJSON(&req); err != nil {
		u.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if strings.TrimSpace(req.DeviceToken) == "" {
		c.ResponseError(errors.New("设备token不能为空！"))
		return
	}
	if strings.TrimSpace(req.DeviceType) == "" {
		c.ResponseError(errors.New("设备类型不能为空！"))
		return
	}
	if strings.TrimSpace(req.BundleID) == "" {
		c.ResponseError(errors.New("bundleID不能为空！"))
		return
	}
	err := u.ctx.GetRedisConn().Hmset(fmt.Sprintf("%s%s", u.userDeviceTokenPrefix, loginUID), "device_type", req.DeviceType, "device_token", req.DeviceToken, "bundle_id", req.BundleID)
	if err != nil {
		u.Error("存储用户设备token失败！", zap.Error(err))
		c.ResponseError(errors.New("存储用户设备token失败！"))
		return
	}
	c.ResponseOK()
}

// 注册用户设备红点数量
func (u *User) registerUserDeviceBadge(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	var req struct {
		Badge int `json:"badge"` // 设备红点数量
	}
	if err := c.BindJSON(&req); err != nil {
		u.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	err := u.setUserBadge(loginUID, int64(req.Badge))
	if err != nil {
		u.Error("存储用户红点失败！", zap.Error(err))
		c.ResponseError(errors.New("存储用户红点失败！"))
		return
	}
	c.ResponseOK()
}

func (u *User) setUserBadge(uid string, badge int64) error {
	err := u.ctx.GetRedisConn().Hset(common.UserDeviceBadgePrefix, uid, fmt.Sprintf("%d", badge))
	if err != nil {
		return err
	}
	return nil
}

// 卸载注册设备token
func (u *User) unregisterUserDeviceToken(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)

	err := u.ctx.GetRedisConn().Del(fmt.Sprintf("%s%s", u.userDeviceTokenPrefix, loginUID))
	if err != nil {
		u.Error("删除设备token失败！", zap.Error(err))
		c.ResponseError(errors.New("删除设备token失败！"))
		return
	}
	c.ResponseOK()
}

// 获取登录的uuid（web登录）
func (u *User) getLoginUUID(c *wkhttp.Context) {
	uuid := util.GenerUUID()
	deviceId := c.Query("device_id")
	deviceName := c.Query("device_name")
	deviceModel := c.Query("device_model")
	err := u.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.QRCodeCachePrefix, uuid), util.ToJson(common.NewQRCodeModel(common.QRCodeTypeScanLogin, map[string]interface{}{
		"app_id":  "wukongchat",
		"status":  common.ScanLoginStatusWaitScan,
		"pub_key": c.Query("pub_key"),
	})), time.Minute*1)
	if err != nil {
		u.Error("设置登录uuid失败！", zap.Error(err))
		c.ResponseError(errors.New("设置登录uuid失败！"))
		return
	}
	// 缓存设备信息
	if deviceId != "" && deviceName != "" && deviceModel != "" {
		err := u.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.DeviceCacheUUIDPrefix, uuid), util.ToJson(map[string]interface{}{
			"device_id":    deviceId,
			"device_name":  deviceName,
			"device_model": deviceModel,
		}), time.Minute*2)
		if err != nil {
			u.Error("设置登录设备信息失败！", zap.Error(err))
			c.ResponseError(errors.New("设置登录设备信息失败！"))
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"uuid":   uuid,
		"qrcode": fmt.Sprintf("%s/%s", u.ctx.GetConfig().External.BaseURL, strings.ReplaceAll(u.ctx.GetConfig().QRCodeInfoURL, ":code", uuid)),
	})
}

// 通过loginUUID获取登录状态
func (u *User) getloginStatus(c *wkhttp.Context) {
	uuid := c.Query("uuid")
	qrcodeInfo, err := u.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.QRCodeCachePrefix, uuid))
	if err != nil {
		u.Error("获取uuid绑定的二维码信息失败！", zap.Error(err))
		c.ResponseError(errors.New("获取uuid绑定的二维码信息失败！"))
		return
	}
	if qrcodeInfo == "" {
		c.JSON(http.StatusOK, gin.H{
			"status": common.ScanLoginStatusExpired,
		})
		return
	}
	var qrcodeModel *common.QRCodeModel
	err = util.ReadJsonByByte([]byte(qrcodeInfo), &qrcodeModel)
	if err != nil {
		u.Error("解码二维码信息失败！", zap.Error(err))
		c.ResponseError(errors.New("解码二维码信息失败！"))
		return
	}
	if qrcodeModel == nil {
		c.JSON(http.StatusOK, gin.H{
			"status": common.ScanLoginStatusExpired,
		})
		return
	}
	qrcodeChan := u.getQRCodeModelChan(uuid)
	select {
	case qrcodeModel := <-qrcodeChan:
		u.removeQRCodeChan(uuid)
		if qrcodeModel == nil {
			break
		}
		c.JSON(http.StatusOK, qrcodeModel.Data)
		break
	case <-time.After(10 * time.Second):
		u.removeQRCodeChan(uuid)
		c.JSON(http.StatusOK, qrcodeModel.Data)
		break

	}
}

// 通过authCode登录
func (u *User) loginWithAuthCode(c *wkhttp.Context) {
	authCode := c.Param("auth_code")
	authCodeKey := fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode)
	flagI64, _ := strconv.ParseInt(c.Query("flag"), 10, 64)
	var flag config.DeviceFlag
	if flagI64 == 0 {
		flag = config.Web // loginWithAuthCode 默认为web登陆
	} else {
		flag = config.DeviceFlag(flagI64)
	}
	authInfo, err := u.ctx.GetRedisConn().GetString(authCodeKey)
	if err != nil {
		u.Error("获取授权信息失败！", zap.Error(err))
		c.ResponseError(errors.New("获取授权信息失败！"))
		return
	}
	if authInfo == "" {
		c.ResponseError(errors.New("授权码失效或不存在！"))
		return
	}
	var authInfoMap map[string]interface{}
	err = util.ReadJsonByByte([]byte(authInfo), &authInfoMap)
	if err != nil {
		u.Error("解码授权信息失败！", zap.Error(err))
		c.ResponseError(errors.New("解码授权信息失败！"))
		return
	}
	authType, ok := authInfoMap["type"].(string)
	if !ok {
		c.ResponseError(errors.New("授权信息格式错误：缺少type"))
		return
	}
	if authType != string(common.AuthCodeTypeScanLogin) {
		c.ResponseError(errors.New("授权码不是登录授权码！"))
		return
	}
	scaner, ok := authInfoMap["scaner"].(string)
	if !ok {
		c.ResponseError(errors.New("授权信息格式错误：缺少scaner"))
		return
	}
	// 获取老的token
	token, err := u.ctx.Cache().Get(fmt.Sprintf("%s%d%s", u.ctx.GetConfig().Cache.UIDTokenCachePrefix, flag, scaner))
	if err != nil {
		u.Error("获取旧token错误", zap.Error(err))
		c.ResponseError(errors.New("获取旧token错误"))
		return
	}
	if strings.TrimSpace(token) == "" {
		token = util.GenerUUID()
	}

	userModel, err := u.db.QueryByUID(scaner)
	if err != nil {
		u.Error("查询用户信息失败", zap.String("uid", scaner), zap.Error(err))
		c.ResponseError(errors.New("查询用户信息失败"))
		return
	}
	// 已注销账号拒绝授权登录；冷静期账号允许（与其他登录路径一致）
	if userModel == nil || userModel.IsDestroy == IsDestroyDone {
		c.ResponseError(errors.New("用户不存在"))
		return
	}
	// 获取缓存设备
	uuid, _ := authInfoMap["uuid"].(string)
	if uuid != "" {
		deviceCache, err := u.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.DeviceCacheUUIDPrefix, uuid))
		if err != nil {
			u.Error("获取登录设备信息失败！", zap.Error(err))
			c.ResponseError(errors.New("获取登录设备信息失败！"))
			return
		}
		if deviceCache != "" {
			var deviceInfoMap map[string]interface{}
			err = util.ReadJsonByByte([]byte(deviceCache), &deviceInfoMap)
			if err != nil {
				u.Error("解码设备信息失败！", zap.Error(err))
				c.ResponseError(errors.New("解码设备信息失败！"))
				return
			}
			deviceId, _ := deviceInfoMap["device_id"].(string)
			deviceName, _ := deviceInfoMap["device_name"].(string)
			dmodel, _ := deviceInfoMap["device_model"].(string)
			if deviceId != "" && deviceName != "" && dmodel != "" {
				span := u.ctx.Tracer().StartSpan(
					"user.authCodeLogin",
					opentracing.ChildOf(c.GetSpanContext()),
				)
				defer span.Finish()
				spanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), span)
				// 更新设备信息
				err := u.deviceDB.insertOrUpdateDeviceCtx(spanCtx, &deviceModel{
					UID:         userModel.UID,
					DeviceID:    deviceId,
					DeviceName:  deviceName,
					DeviceModel: dmodel,
					LastLogin:   time.Now().Unix(),
				})
				if err != nil {
					u.Error("更新用户登录设备失败", zap.Error(err))
					c.ResponseError(errors.New("更新用户登录设备失败"))
					return
				}
			}
		}
	}
	imResp, err := u.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         scaner,
		Token:       token,
		DeviceFlag:  flag,
		DeviceLevel: config.DeviceLevelSlave,
	})
	if err != nil {
		u.Error("更新IM的token失败！", zap.Error(err))
		c.ResponseError(errors.New("更新IM的token失败！"))
		return
	}
	if imResp.Status == config.UpdateTokenStatusBan {
		c.ResponseError(errors.New("此账号已经被封禁！"))
		return
	}

	// 将token设置到缓存
	tokenPayload, err := auth.Encode(auth.TokenInfo{UID: userModel.UID, Name: userModel.Name})
	if err != nil {
		u.Error("编码token缓存失败！", zap.Error(err))
		return
	}
	err = u.ctx.Cache().SetAndExpire(u.ctx.GetConfig().Cache.TokenCachePrefix+token, tokenPayload, u.ctx.GetConfig().Cache.TokenExpire)
	if err != nil {
		u.Error("设置token缓存失败！", zap.Error(err))
		c.ResponseError(errors.New("设置token缓存失败！"))
		return
	}
	err = u.ctx.GetRedisConn().Del(authCodeKey)
	if err != nil {
		u.Error("删除授权码失败！", zap.Error(err))
		c.ResponseError(errors.New("删除授权码失败！"))
		return
	}

	err = u.ctx.Cache().SetAndExpire(fmt.Sprintf("%s%d%s", u.ctx.GetConfig().Cache.UIDTokenCachePrefix, flag, userModel.UID), token, u.ctx.GetConfig().Cache.TokenExpire)
	if err != nil {
		u.Error("设置uidtoken缓存失败！", zap.Error(err))
		c.ResponseError(errors.New("设置uidtoken缓存失败！"))
		return
	}

	resp := map[string]interface{}{
		"app_id":     userModel.AppID,
		"name":       userModel.Name,
		"username":   userModel.Username,
		"uid":        userModel.UID,
		"token":      token,
		"short_no":   userModel.ShortNo,
		"avatar":     u.ctx.GetConfig().GetAvatarPath(userModel.UID),
		"im_pub_key": "",
	}
	// YUJ-413 R5 Blocking #2:auth-code 登录走手写 map,没经过
	// newLoginUserDetailResp,必须单独补三个实名字段 —— 否则扫码登录的客户端
	// 永远拿不到 self 实名态,和 POST /v1/user/login 契约不一致。
	u.applyRealnameToAuthCodeMap(resp, userModel.UID)
	c.Response(resp)
}

// 获取二维码数据的管道
func (u *User) getQRCodeModelChan(uuid string) <-chan *common.QRCodeModel {
	qrcodeModelChan := make(chan *common.QRCodeModel, 1) // buffered: prevent message loss between return and receive
	qrcodeChanLock.Lock()
	qrcodeChanMap[uuid] = qrcodeModelChan
	qrcodeChanLock.Unlock()
	return qrcodeModelChan
}
func (u *User) removeQRCodeChan(uuid string) {
	qrcodeChanLock.Lock()
	defer qrcodeChanLock.Unlock()
	ch, exist := qrcodeChanMap[uuid]
	if exist {
		delete(qrcodeChanMap, uuid)
		close(ch) // close channel to unblock any pending sender
	}
}

// SendQRCodeInfo 发送二维码数据
func SendQRCodeInfo(uuid string, qrcode *common.QRCodeModel) {
	qrcodeChanLock.Lock()
	defer qrcodeChanLock.Unlock()

	qrcodeChan := qrcodeChanMap[uuid]
	if qrcodeChan != nil {
		select {
		case qrcodeChan <- qrcode:
		default:
			// channel 已满或无接收者
		}
	}
}

// 授权登录
func (u *User) grantLogin(c *wkhttp.Context) {
	authCode := c.Query("auth_code")
	loginUID := c.MustGet("uid").(string)
	encrypt := c.Query("encrypt") // signal相关密钥
	if authCode == "" {
		c.ResponseError(errors.New("授权码不能为空！"))
		return
	}
	authInfo, err := u.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode))
	if err != nil {
		u.Error("获取授权信息失败！", zap.Error(err))
		c.ResponseError(errors.New("获取授权信息失败！"))
		return
	}
	if authInfo == "" {
		c.ResponseError(errors.New("授权码失效或不存在！"))
		return
	}
	var authInfoMap map[string]interface{}
	err = util.ReadJsonByByte([]byte(authInfo), &authInfoMap)
	if err != nil {
		u.Error("解码授权信息失败！", zap.Error(err))
		c.ResponseError(errors.New("解码授权信息失败！"))
		return
	}
	authType, ok := authInfoMap["type"].(string)
	if !ok {
		c.ResponseError(errors.New("授权信息格式错误：缺少type"))
		return
	}
	if authType != string(common.AuthCodeTypeScanLogin) {
		c.ResponseError(errors.New("授权码不是登录授权码！"))
		return
	}
	scaner, ok := authInfoMap["scaner"].(string)
	if !ok {
		c.ResponseError(errors.New("授权信息格式错误：缺少scaner"))
		return
	}
	if scaner != loginUID {
		c.ResponseError(errors.New("扫描者与授权者不是同一个用户！"))
		return
	}
	uuid, _ := authInfoMap["uuid"].(string)
	qrcodeInfo := common.NewQRCodeModel(common.QRCodeTypeScanLogin, map[string]interface{}{
		"app_id":    "wukongchat",
		"status":    common.ScanLoginStatusAuthed,
		"uid":       loginUID,
		"auth_code": authCode,
		"encrypt":   encrypt,
	})
	err = u.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.QRCodeCachePrefix, uuid), util.ToJson(qrcodeInfo), time.Minute*5)
	if err != nil {
		u.Error("更新二维码信息失败！", zap.Error(err))
		c.ResponseError(errors.New("更新二维码信息失败！"))
		return
	}
	SendQRCodeInfo(uuid, qrcodeInfo)
	c.ResponseOK()
}

// addBlacklist 添加黑名单
func (u *User) addBlacklist(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	uid := c.Param("uid")
	if strings.TrimSpace(uid) == "" {
		c.ResponseError(errors.New("添加黑名单的用户ID不能空！"))
		return
	}
	model, err := u.settingDB.QueryUserSettingModel(uid, loginUID)
	if err != nil {
		u.Error("查询用户设置失败", zap.Error(err))
		c.ResponseError(errors.New("查询用户设置失败！"))
		return
	}
	//如果没有设置记录先添加一条记录
	if model == nil || strings.TrimSpace(model.UID) == "" {
		userSettingModel := &SettingModel{
			UID:   loginUID,
			ToUID: uid,
		}
		err = u.settingDB.InsertUserSettingModel(userSettingModel)
		if err != nil {
			u.Error("添加用户设置失败", zap.Error(err))
			c.ResponseError(errors.New("添加用户设置失败！"))
			return
		}
	}

	//添加黑名单
	version, err := u.ctx.GenSeq(common.UserSettingSeqKey)
	if err != nil {
		c.ResponseError(err)
		return
	}
	friendVersion, err := u.ctx.GenSeq(common.FriendSeqKey)
	if err != nil {
		c.ResponseError(err)
		return
	}
	tx, err := u.ctx.DB().Begin()
	if err != nil {
		u.Error("开启事务失败！", zap.Error(err))
		c.ResponseError(errors.New("开启事务失败！"))
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	err = u.db.AddOrRemoveBlacklistTx(loginUID, uid, 1, version, tx)
	if err != nil {
		tx.Rollback()
		u.Error("添加黑名单失败！", zap.Error(err))
		c.ResponseError(errors.New("添加黑名单失败！"))
		return
	}
	err = u.friendDB.updateVersionTx(friendVersion, loginUID, uid, tx)
	if err != nil {
		tx.Rollback()
		u.Error("更新好友的版本号失败！", zap.Error(err))
		c.ResponseError(errors.New("更新好友的版本号失败！"))
		return
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		u.Error("提交数据库失败！", zap.Error(err))
		c.ResponseError(errors.New("提交数据库失败！"))
		return
	}

	// DB事务提交成功后，再请求IM服务器设置黑名单
	err = u.ctx.IMBlacklistAdd(config.ChannelBlacklistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   loginUID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{uid},
	})
	if err != nil {
		u.Error("IM设置黑名单失败，DB已提交", zap.Error(err), zap.String("loginUID", loginUID), zap.String("uid", uid))
	}

	// 发送给被拉黑的人去更新拉黑人的频道
	err = u.ctx.SendChannelUpdate(config.ChannelReq{
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
	}, config.ChannelReq{
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	})
	if err != nil {
		u.Warn("发送频道更新命令失败！", zap.Error(err))
	}

	// 发送给操作者，去更新被拉黑的人的频道
	err = u.ctx.SendChannelUpdate(config.ChannelReq{
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	}, config.ChannelReq{
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
	})
	if err != nil {
		u.Warn("发送频道更新命令失败！", zap.Error(err))
	}

	c.ResponseOK()
}

// removeBlacklist 移除黑名单
func (u *User) removeBlacklist(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	uid := c.Param("uid")
	if strings.TrimSpace(uid) == "" {
		c.ResponseError(errors.New("移除黑名单的用户ID不能空！"))
		return
	}

	version, err := u.ctx.GenSeq(common.UserSettingSeqKey)
	if err != nil {
		c.ResponseError(err)
		return
	}
	friendVersion, err := u.ctx.GenSeq(common.FriendSeqKey)
	if err != nil {
		c.ResponseError(err)
		return
	}

	tx, err := u.ctx.DB().Begin()
	if err != nil {
		u.Error("开启事务失败！", zap.Error(err))
		c.ResponseError(errors.New("开启事务失败！"))
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	err = u.db.AddOrRemoveBlacklistTx(loginUID, uid, 0, version, tx)
	if err != nil {
		tx.Rollback()
		u.Error("移除黑名单失败！", zap.Error(err))
		c.ResponseError(errors.New("移除黑名单失败！"))
		return
	}
	err = u.friendDB.updateVersionTx(friendVersion, loginUID, uid, tx)
	if err != nil {
		tx.Rollback()
		u.Error("更新好友的版本号失败！", zap.Error(err))
		c.ResponseError(errors.New("更新好友的版本号失败！"))
		return
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		u.Error("提交数据库失败！", zap.Error(err))
		c.ResponseError(errors.New("提交数据库失败！"))
		return
	}

	// DB事务提交成功后，再请求IM服务器移除黑名单
	err = u.ctx.IMBlacklistRemove(config.ChannelBlacklistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   loginUID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{uid},
	})
	if err != nil {
		u.Error("IM移除黑名单失败，DB已提交", zap.Error(err), zap.String("loginUID", loginUID), zap.String("uid", uid))
	}

	// 发送给被拉黑的人去更新拉黑人的频道
	err = u.ctx.SendChannelUpdate(config.ChannelReq{
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
	}, config.ChannelReq{
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	})
	if err != nil {
		u.Warn("发送频道更新命令失败！", zap.Error(err))
	}

	// 发送给操作者，去更新被拉黑的人的频道
	err = u.ctx.SendChannelUpdate(config.ChannelReq{
		ChannelID:   loginUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
	}, config.ChannelReq{
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
	})
	if err != nil {
		u.Warn("发送频道更新命令失败！", zap.Error(err))
	}

	c.ResponseOK()
}

// blacklists 获取黑名单列表
func (u *User) blacklists(c *wkhttp.Context) {
	loginUID := c.MustGet("uid").(string)
	list, err := u.db.Blacklists(loginUID)
	if err != nil {
		u.Error("查询黑名单列表失败！", zap.Error(err))
		c.ResponseError(errors.New("查询黑名单列表失败！"))
		return
	}
	blacklists := []*blacklistResp{}
	for _, result := range list {
		blacklists = append(blacklists, &blacklistResp{
			UID:      result.UID,
			Name:     result.Name,
			Username: result.Username,
		})
	}
	c.Response(blacklists)
}

// sendRegisterCode 发送注册短信
func (u *User) sendRegisterCode(c *wkhttp.Context) {
	if common2.EnsureSystemSettings(u.ctx).RegisterOff() {
		c.ResponseError(errors.New("注册通道暂不开放，请长按标题使用官网上演示账号登录"))
		return
	}
	var req codeReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if strings.TrimSpace(req.Zone) == "" {
		c.ResponseError(errors.New("区号不能为空！"))
		return
	}
	if strings.TrimSpace(req.Phone) == "" {
		c.ResponseError(errors.New("手机号不能为空！"))
		return
	}
	if common2.EnsureSystemSettings(u.ctx).RegisterOnlyChina() {
		if strings.TrimSpace(req.Zone) != "0086" {
			c.ResponseError(errors.New("仅仅支持中国大陆手机号注册！"))
			return
		}
	}

	span := u.ctx.Tracer().StartSpan(
		"user.sendRegisterCode",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	defer span.Finish()
	spanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), span)

	model, err := u.db.QueryByPhone(req.Zone, req.Phone)
	if err != nil {
		u.Error("查询用户信息失败！", zap.Error(err))
		c.ResponseError(errors.New("查询用户信息失败！"))
		return
	}
	if model != nil {
		c.Response(map[string]interface{}{
			"exist": 1,
		})
		return
	}
	err = u.smsServie.SendVerifyCode(spanCtx, req.Zone, req.Phone, commonapi.CodeTypeRegister)
	if err != nil {
		u.Error("发送短信验证码失败", zap.Error(err))
		c.ResponseError(errors.New("发送短信验证码失败！"))
		return
	}
	c.Response(map[string]interface{}{
		"exist": 0,
	})
}

// setChatPwd 修改用户聊天密码
func (u *User) setChatPwd(c *wkhttp.Context) {
	var req chatPwdReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if strings.TrimSpace(req.ChatPwd) == "" {
		c.ResponseError(errors.New("聊天密码不能为空"))
		return
	}
	if strings.TrimSpace(req.LoginPwd) == "" {
		c.ResponseError(errors.New("登录密码不能为空！"))
		return
	}
	loginUID := c.MustGet("uid").(string)
	user, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询用户信息失败！", zap.Error(err))
		c.ResponseError(errors.New("查询用户信息失败"))
		return
	}
	pwdMatched, _ := CheckPassword(req.LoginPwd, user.Password)
	if !pwdMatched {
		c.ResponseError(errors.New("登录密码错误"))
		return
	}
	//修改用户聊天密码
	hashedChatPwd, err := HashPassword(req.ChatPwd)
	if err != nil {
		u.Error("哈希聊天密码失败！", zap.Error(err))
		c.ResponseError(errors.New("修改聊天密码失败"))
		return
	}
	err = u.db.UpdateUsersWithField("chat_pwd", hashedChatPwd, loginUID)
	if err != nil {
		u.Error("查询用户信息失败！", zap.Error(err))
		c.ResponseError(errors.New("修改聊天密码失败"))
		return
	}
	c.ResponseOK()
}

// 设置锁屏密码
func (u *User) lockScreenAfterMinuteSet(c *wkhttp.Context) {
	var req struct {
		LockAfterMinute int `json:"lock_after_minute"` // 在几分钟后锁屏
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if req.LockAfterMinute < 0 {
		c.ResponseError(errors.New("锁屏时间不能小于0"))
		return
	}
	if req.LockAfterMinute > 60 {
		c.ResponseError(errors.New("锁屏时间不能大于60分钟"))
		return
	}
	loginUID := c.GetLoginUID()
	err := u.db.UpdateUsersWithField("lock_after_minute", strconv.FormatInt(int64(req.LockAfterMinute), 10), loginUID)
	if err != nil {
		u.Error("修改用户锁屏密码错误", zap.Error(err))
		c.ResponseError(errors.New("修改用户锁屏密码错误"))
		return
	}
	c.ResponseOK()
}

// 设置锁屏密码
func (u *User) setLockScreenPwd(c *wkhttp.Context) {
	var req struct {
		LockScreenPwd string `json:"lock_screen_pwd"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if strings.TrimSpace(req.LockScreenPwd) == "" {
		c.ResponseError(errors.New("锁屏密码不能为空"))
		return
	}

	loginUID := c.GetLoginUID()
	hashedLockPwd, err := HashPassword(req.LockScreenPwd)
	if err != nil {
		u.Error("哈希锁屏密码失败！", zap.Error(err))
		c.ResponseError(errors.New("修改锁屏密码失败"))
		return
	}
	err = u.db.UpdateUsersWithField("lock_screen_pwd", hashedLockPwd, loginUID)
	if err != nil {
		u.Error("修改用户锁屏密码错误", zap.Error(err))
		c.ResponseError(errors.New("修改用户锁屏密码错误"))
		return
	}
	c.ResponseOK()
}

// 关闭锁屏密码
func (u *User) closeLockScreenPwd(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	err := u.db.UpdateUsersWithField("lock_screen_pwd", "", loginUID)
	if err != nil {
		u.Error("修改用户锁屏密码错误", zap.Error(err))
		c.ResponseError(errors.New("修改用户锁屏密码错误"))
		return
	}
	c.ResponseOK()
}

// sendLoginCheckPhoneCode 发送登录验证短信
func (u *User) sendLoginCheckPhoneCode(c *wkhttp.Context) {
	// 设备验证短信是本地登录二阶段的一部分,local_off=1 时必须连发码也拒,
	// 否则攻击者跳过 /v1/user/login 直接走二阶段仍能拿到 token,
	// 同时还把短信通道当作免费枚举/滥发入口。
	if common2.EnsureSystemSettings(u.ctx).LocalLoginOff() {
		c.ResponseError(errors.New("本地登录已关闭"))
		return
	}
	var req struct {
		UID string `json:"uid"`
	}
	if err := c.BindJSON(&req); err != nil {
		u.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if req.UID == "" {
		c.ResponseError(errors.New("uid不能为空！"))
		return
	}

	span := u.ctx.Tracer().StartSpan(
		"user.sendLoginCheckPhoneCode",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	defer span.Finish()
	spanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), span)

	userinfo, err := u.db.QueryByUID(req.UID)
	if err != nil {
		u.Error("查询用户信息失败！", zap.Error(err))
		c.ResponseError(errors.New("修改聊天密码失败"))
		return
	}
	if userinfo == nil {
		u.Error("该用户不存在", zap.Error(err))
		c.ResponseError(errors.New("该用户不存在"))
		return
	}
	//发送短信
	// if u.ctx.GetConfig().Test {
	// 	c.ResponseOK()
	// 	return
	// }
	err = u.smsServie.SendVerifyCode(spanCtx, userinfo.Zone, userinfo.Phone, commonapi.CodeTypeCheckMobile)
	if err != nil {
		u.Error("发送短信失败", zap.Error(err))
		ext.LogError(span, err)
		c.ResponseError(errors.New("发送短信失败"))
		return
	}
	c.ResponseOK()
}

// loginCheckPhone 登录验证设备短信
func (u *User) loginCheckPhone(c *wkhttp.Context) {
	if common2.EnsureSystemSettings(u.ctx).LocalLoginOff() {
		c.ResponseError(errors.New("本地登录已关闭"))
		return
	}
	var req struct {
		UID  string `json:"uid"`
		Code string `json:"code"`
	}
	if err := c.BindJSON(&req); err != nil {
		u.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if req.UID == "" {
		c.ResponseError(errors.New("uid不能为空！"))
		return
	}
	if req.Code == "" {
		c.ResponseError(errors.New("验证码不能为空！"))
		return
	}
	span := u.ctx.Tracer().StartSpan(
		"user.loginCheckPhone",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	defer span.Finish()
	spanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), span)

	userInfo, err := u.db.QueryByUID(req.UID)
	if err != nil {
		u.Error("查询用户信息失败！", zap.Error(err))
		c.ResponseError(errors.New("修改聊天密码失败"))
		return
	}
	if userInfo == nil {
		u.Error("该用户不存在", zap.Error(err))
		c.ResponseError(errors.New("该用户不存在"))
		return
	}
	// 已注销账号拒绝设备验证登录；冷静期账号允许
	if userInfo.IsDestroy == IsDestroyDone {
		c.ResponseError(errors.New("该用户不存在"))
		return
	}
	err = u.smsServie.Verify(spanCtx, userInfo.Zone, userInfo.Phone, req.Code, commonapi.CodeTypeCheckMobile)
	if err != nil {
		u.Error("验证短信失败", zap.Error(err))
		c.ResponseError(err)
		return
	}

	loginDeviceJsonStr, err := u.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", u.ctx.GetConfig().Cache.LoginDeviceCachePrefix, req.UID))
	if err != nil {
		u.Error("获取登录设备缓存失败！", zap.Error(err))
		c.ResponseError(errors.New("获取登录设备缓存失败！"))
		return
	}
	if loginDeviceJsonStr == "" {
		c.ResponseError(errors.New("登录设备已过期，请重新登录"))
		return
	}
	var loginDeivce *deviceReq
	err = util.ReadJsonByByte([]byte(loginDeviceJsonStr), &loginDeivce)
	if err != nil {
		u.Error("解码登录设备信息失败！", zap.Error(err), zap.String("uid", req.UID))
		c.ResponseError(errors.New("解码登录设备信息失败！"))
		return
	}
	err = u.deviceDB.insertOrUpdateDeviceCtx(spanCtx, &deviceModel{
		UID:         userInfo.UID,
		DeviceID:    loginDeivce.DeviceID,
		DeviceName:  loginDeivce.DeviceName,
		DeviceModel: loginDeivce.DeviceModel,
		LastLogin:   time.Now().Unix(),
	})
	if err != nil {
		u.Error("添加或更新登录设备信息失败！", zap.Error(err))
		c.ResponseError(errors.New("添加或更新登录设备信息失败！"))
		return
	}
	token := util.GenerUUID()
	// 将token设置到缓存
	tokenPayload, err := auth.Encode(auth.TokenInfo{UID: userInfo.UID, Name: userInfo.Name})
	if err != nil {
		u.Error("编码token缓存失败！", zap.Error(err))
		c.ResponseError(errors.New("设置token缓存失败！"))
		return
	}
	err = u.ctx.Cache().SetAndExpire(u.ctx.GetConfig().Cache.TokenCachePrefix+token, tokenPayload, u.ctx.GetConfig().Cache.TokenExpire)
	if err != nil {
		u.Error("设置token缓存失败！", zap.Error(err))
		c.ResponseError(errors.New("设置token缓存失败！"))
		return
	}
	// err = u.ctx.UpdateIMToken(userInfo.UID, token, config.DeviceFlag(0), config.DeviceLevelMaster)
	imResp, err := u.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         userInfo.UID,
		Token:       token,
		DeviceFlag:  config.APP,
		DeviceLevel: config.DeviceLevelMaster,
	})
	if err != nil {
		u.Error("更新IM的token失败！", zap.Error(err))
		c.ResponseError(errors.New("更新IM的token失败！"))
		return
	}
	if imResp.Status == config.UpdateTokenStatusBan {
		c.ResponseError(errors.New("此账号已经被封禁！"))
		return
	}
	resp := newLoginUserDetailResp(userInfo, token, u.ctx)
	u.applyRealnameToLoginResp(resp, userInfo.UID)
	c.Response(resp)
}

// customerservices 客服列表
func (u *User) customerservices(c *wkhttp.Context) {
	list, err := u.db.QueryByCategory(CategoryCustomerService)
	if err != nil {
		u.Error("查询客服列表失败", zap.Error(err))
		c.ResponseError(errors.New("查询客服列表失败"))
		return
	}
	results := []*customerservicesResp{}
	if len(list) > 0 {
		for _, user := range list {
			results = append(results, &customerservicesResp{
				UID:  user.UID,
				Name: user.Name,
			})
		}
	}
	c.Response(results)
}

// 发送注销账号验证吗
func (u *User) sendDestroyCode(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	userInfo, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询登录用户信息错误", zap.Error(err))
		c.ResponseError(errors.New("查询登录用户信息错误"))
		return
	}
	if userInfo == nil {
		c.ResponseError(errors.New("登录用户不存在"))
		return
	}
	switch userInfo.IsDestroy {
	case IsDestroyApplying:
		c.ResponseError(errors.New("账号已在注销冷静期中，请使用新版客户端撤销或查询状态"))
		return
	case IsDestroyDone:
		c.ResponseError(errors.New("账号已注销"))
		return
	}
	err = u.smsServie.SendVerifyCode(c.Context, userInfo.Zone, userInfo.Phone, commonapi.CodeTypeDestroyAccount)
	if err != nil {
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// 注销账号
func (u *User) destroyAccount(c *wkhttp.Context) {
	code := c.Param("code")
	loginUID := c.GetLoginUID()
	if code == "" {
		c.ResponseError(errors.New("验证码不能为空"))
		return
	}
	userInfo, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询登录用户信息错误", zap.Error(err))
		c.ResponseError(errors.New("查询登录用户信息错误"))
		return
	}
	if userInfo == nil {
		c.ResponseError(errors.New("登录用户不存在"))
		return
	}
	switch userInfo.IsDestroy {
	case IsDestroyApplying:
		c.ResponseError(errors.New("账号已在注销冷静期中，请使用新版客户端撤销或查询状态"))
		return
	case IsDestroyDone:
		c.ResponseError(errors.New("账号已注销"))
		return
	}
	//测试模式（仅非 release 生效）
	if commonapi.IsTestCodeEnabled(u.ctx.GetConfig()) {
		if !commonapi.MatchTestCode(u.ctx.GetConfig(), code) {
			c.ResponseError(errors.New("验证码错误"))
			return
		}
	} else {
		//线上验证短信验证码
		// 校验验证码
		err = u.smsServie.Verify(c.Context, userInfo.Zone, userInfo.Phone, code, commonapi.CodeTypeDestroyAccount)
		if err != nil {
			c.ResponseError(err)
			return
		}
	}

	// 毫秒时间戳：13 位足够保证唯一（UnixNano 19 位会撑爆 varchar(40)）。
	// username 通过 anonymizeUsername 兜底防溢出（海外长手机号时回退 hash 形式）。
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	phone := fmt.Sprintf("%s@%s@delete", userInfo.Phone, stamp)
	username := anonymizeUsername(loginUID, userInfo.Zone, phone, stamp)
	err = u.db.destroyAccount(loginUID, username, phone)
	if err != nil {
		u.Error("注销账号错误", zap.Error(err))
		c.ResponseError(errors.New("注销账号错误"))
		return
	}
	err = u.ctx.QuitUserDevice(c.GetLoginUID(), -1) // 退出全部登陆设备
	if err != nil {
		u.Error("退出登陆设备失败", zap.Error(err))
		c.ResponseError(errors.New("退出登陆设备失败"))
		return
	}

	c.ResponseOK()
}

// 处理注册用户和文件助手互为好友
func (u *User) addFileHelperFriend(uid string) error {
	if uid == "" {
		u.Error("用户ID不能为空")
		return errors.New("用户ID不能为空")
	}
	isFriend, err := u.friendDB.IsFriend(uid, u.ctx.GetConfig().Account.FileHelperUID)
	if err != nil {
		u.Error("查询用户关系失败")
		return err
	}
	if !isFriend {
		version, err := u.ctx.GenSeq(common.FriendSeqKey)
		if err != nil {
			u.Error("GenSeq failed", zap.Error(err))
			return err
		}
		err = u.friendDB.Insert(&FriendModel{
			UID:     uid,
			ToUID:   u.ctx.GetConfig().Account.FileHelperUID,
			Version: version,
		})
		if err != nil {
			u.Error("注册用户和文件助手成为好友失败")
			return err
		}
	}
	return nil
}

// addBotFatherFriend 处理注册用户和BotFather互为好友（双向记录 + 白名单 + CMD同步，使用事务）
func (u *User) addBotFatherFriend(uid string) error {
	const botFatherUID = "botfather"
	if uid == "" {
		return errors.New("用户ID不能为空")
	}

	// 检查正向好友关系，若已存在则跳过
	isFriend, err := u.friendDB.IsFriend(uid, botFatherUID)
	if err != nil {
		u.Error("查询用户与BotFather关系失败", zap.Error(err))
		return err
	}
	if isFriend {
		return nil
	}

	// 使用事务保证双向好友关系的原子性
	tx, err := u.friendDB.session.Begin()
	if err != nil {
		u.Error("创建数据库事务失败", zap.Error(err))
		return errors.New("创建数据库事务失败")
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in addBotFatherFriend: %v\n%s\n", err, debug.Stack())
		}
	}()

	// 正向：uid → botfather
	version, err := u.ctx.GenSeq(common.FriendSeqKey)
	if err != nil {
		u.Error("GenSeq failed", zap.Error(err))
		tx.Rollback()
		return err
	}
	err = u.friendDB.InsertTx(&FriendModel{
		UID:     uid,
		ToUID:   botFatherUID,
		Version: version,
	}, tx)
	if err != nil {
		u.Error("注册用户和BotFather成为好友失败", zap.Error(err))
		tx.Rollback()
		return err
	}

	// 反向：botfather → uid
	version2, err := u.ctx.GenSeq(common.FriendSeqKey)
	if err != nil {
		u.Error("GenSeq failed", zap.Error(err))
		tx.Rollback()
		return err
	}
	err = u.friendDB.InsertTx(&FriendModel{
		UID:     botFatherUID,
		ToUID:   uid,
		Version: version2,
	}, tx)
	if err != nil {
		u.Error("BotFather和注册用户成为好友失败", zap.Error(err))
		tx.Rollback()
		return err
	}

	err = tx.Commit()
	if err != nil {
		u.Error("提交事务失败", zap.Error(err))
		return err
	}

	// 双向IM白名单
	err = u.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   uid,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{botFatherUID},
	})
	if err != nil {
		u.Error("添加IM白名单失败(user->botfather)", zap.Error(err))
	}
	err = u.ctx.IMWhitelistAdd(config.ChannelWhitelistReq{
		ChannelReq: config.ChannelReq{
			ChannelID:   botFatherUID,
			ChannelType: common.ChannelTypePerson.Uint8(),
		},
		UIDs: []string{uid},
	})
	if err != nil {
		u.Error("添加IM白名单失败(botfather->user)", zap.Error(err))
	}

	// 发送好友同步CMD，通知客户端更新好友列表
	err = u.ctx.SendCMD(config.MsgCMDReq{
		CMD:         common.CMDFriendAccept,
		Subscribers: []string{uid, botFatherUID},
		Param: map[string]interface{}{
			"to_uid":   uid,
			"from_uid": botFatherUID,
		},
	})
	if err != nil {
		u.Error("发送BotFather好友同步CMD失败", zap.Error(err))
	}
	return nil
}

// addSystemFriend 处理注册用户和系统账号互为好友
func (u *User) addSystemFriend(uid string) error {

	if uid == "" {
		u.Error("用户ID不能为空")
		return errors.New("用户ID不能为空")
	}
	isFriend, err := u.friendDB.IsFriend(uid, u.ctx.GetConfig().Account.SystemUID)
	if err != nil {
		u.Error("查询用户关系失败")
		return err
	}
	tx, err := u.friendDB.session.Begin()
	if err != nil {
		u.Error("创建数据库事物失败")
		return errors.New("创建数据库事物失败")
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	if !isFriend {
		version, err := u.ctx.GenSeq(common.FriendSeqKey)
		if err != nil {
			u.Error("GenSeq failed", zap.Error(err))
			return err
		}
		err = u.friendDB.InsertTx(&FriendModel{
			UID:     uid,
			ToUID:   u.ctx.GetConfig().Account.SystemUID,
			Version: version,
		}, tx)
		if err != nil {
			u.Error("注册用户和系统账号成为好友失败")
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
		u.Error("用户注册数据库事物提交失败", zap.Error(err))
		return err
	}
	return nil
}

// 重置登录密码
func (u *User) pwdforget(c *wkhttp.Context) {
	var req resetPwdReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if strings.TrimSpace(req.Zone) == "" {
		c.ResponseError(errors.New("区号不能为空！"))
		return
	}
	if strings.TrimSpace(req.Phone) == "" {
		c.ResponseError(errors.New("手机号不能为空！"))
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		c.ResponseError(errors.New("验证码不能为空！"))
		return
	}
	if strings.TrimSpace(req.Pwd) == "" {
		c.ResponseError(errors.New("密码不能为空！"))
		return
	}
	userInfo, err := u.db.QueryByPhone(req.Zone, req.Phone)
	if err != nil {
		u.Error("查询用户信息错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户信息错误"))
		return
	}
	if userInfo == nil {
		c.ResponseError(errors.New("该账号不存在"))
		return
	}
	//测试模式（仅非 release 生效）
	if commonapi.IsTestCodeEnabled(u.ctx.GetConfig()) {
		if !commonapi.MatchTestCode(u.ctx.GetConfig(), req.Code) {
			c.ResponseError(errors.New("验证码错误"))
			return
		}
	} else {
		//线上验证短信验证码
		err = u.smsServie.Verify(context.Background(), req.Zone, req.Phone, req.Code, commonapi.CodeTypeForgetLoginPWD)
		if err != nil {
			c.ResponseError(err)
			return
		}
	}

	hashedPassword, hashErr := HashPassword(req.Pwd)
	if hashErr != nil {
		u.Error("密码哈希失败", zap.Error(hashErr))
		c.ResponseError(errors.New("密码处理失败"))
		return
	}
	err = u.db.UpdateUsersWithField("password", hashedPassword, userInfo.UID)
	if err != nil {
		u.Error("修改登录密码错误", zap.Error(err))
		c.ResponseError(errors.New("修改登录密码错误"))
		return
	}
	c.ResponseOK()
}

// 获取忘记密码验证码
func (u *User) getForgetPwdSMS(c *wkhttp.Context) {
	var req codeReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if strings.TrimSpace(req.Zone) == "" {
		c.ResponseError(errors.New("区号不能为空！"))
		return
	}
	if strings.TrimSpace(req.Phone) == "" {
		c.ResponseError(errors.New("手机号不能为空！"))
		return
	}

	span := u.ctx.Tracer().StartSpan(
		"user.sendForgetPwdCode",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	defer span.Finish()
	spanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), span)

	model, err := u.db.QueryByPhone(req.Zone, req.Phone)
	if err != nil {
		u.Error("查询用户信息失败！", zap.Error(err))
		c.ResponseError(errors.New("查询用户信息失败！"))
		return
	}
	if model == nil {
		c.ResponseError(errors.New("该手机号未注册"))
		return
	}
	err = u.smsServie.SendVerifyCode(spanCtx, req.Zone, req.Phone, commonapi.CodeTypeForgetLoginPWD)
	if err != nil {
		u.Error("发送短信验证码失败", zap.Error(err))
		c.ResponseError(errors.New("发送短信验证码失败！"))
		return
	}
	c.ResponseOK()
}

// 是否允许更新
func allowUpdateUserField(field string) bool {
	allowfields := []string{"sex", "short_no", "name", "search_by_phone", "search_by_short", "new_msg_notice", "msg_show_detail", "voice_on", "shock_on", "msg_expire_second"}
	for _, allowFiled := range allowfields {
		if field == allowFiled {
			return true
		}
	}
	return false
}

func (u *User) createUser(registerSpanCtx context.Context, createUser *createUserModel, c *wkhttp.Context, invite *model.Invite) {
	tx, err := u.db.session.Begin()
	if err != nil {
		u.Error("创建数据库事物失败", zap.Error(err))
		c.ResponseError(errors.New("创建数据库事物失败"))
		return
	}
	defer func() {
		if err := recover(); err != nil {
			tx.Rollback()
			fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
		}
	}()
	publicIP := util.GetClientPublicIP(c.Request)
	resp, err := u.createUserWithRespAndTx(registerSpanCtx, createUser, publicIP, invite, tx, func() error {
		err := tx.Commit()
		if err != nil {
			tx.Rollback()
			u.Error("数据库事物提交失败", zap.Error(err))
			c.ResponseError(errors.New("数据库事物提交失败"))
			return nil
		}
		return nil
	})
	if err != nil {
		tx.Rollback()
		c.ResponseError(errors.New("注册失败！"))
		return
	}
	c.Response(resp)
}

func (u *User) createUserTx(registerSpanCtx context.Context, createUser *createUserModel, c *wkhttp.Context, commitCallback func() error, invite *model.Invite, tx *dbr.Tx) {
	publicIP := util.GetClientPublicIP(c.Request)
	resp, err := u.createUserWithRespAndTx(registerSpanCtx, createUser, publicIP, invite, tx, commitCallback)
	if err != nil {
		c.ResponseError(errors.New("注册失败！"))
		return
	}
	c.Response(resp)
}

func (u *User) createUserWithRespAndTx(registerSpanCtx context.Context, createUser *createUserModel, publicIP string, invite *model.Invite, tx *dbr.Tx, commitCallback func() error) (*loginUserDetailResp, error) {
	var (
		shortNo = ""
		err     error
	)
	if u.ctx.GetConfig().ShortNo.NumOn {
		shortNo, err = u.commonService.GetShortno()
		if err != nil {
			u.Error("获取短编号失败！", zap.Error(err))
			return nil, err
		}
	} else {
		shortNo = util.Ten2Hex(time.Now().UnixNano())
	}

	userModel := &Model{}
	userModel.UID = createUser.UID
	if createUser.Name != "" {
		userModel.Name = createUser.Name
	} else {
		appconfig, err := u.commonService.GetAppConfig()
		if err != nil {
			u.Error("获取应用配置失败！", zap.Error(err))
			return nil, err
		}
		if appconfig != nil && appconfig.RegisterUserMustCompleteInfoOn == 1 {
			userModel.Name = ""
		} else {
			userModel.Name = Names[rand.Intn(len(Names)-1)]
		}
	}
	userModel.Sex = createUser.Sex
	userModel.Vercode = fmt.Sprintf("%s@%d", util.GenerUUID(), common.User)
	userModel.QRVercode = fmt.Sprintf("%s@%d", util.GenerUUID(), common.QRCode)
	userModel.Phone = createUser.Phone
	userModel.Zone = createUser.Zone
	userModel.Email = createUser.Email
	if createUser.Phone != "" {
		userModel.Username = fmt.Sprintf("%s%s", createUser.Zone, createUser.Phone)
	}
	if createUser.Password != "" {
		hashedPwd, hashErr := HashPassword(createUser.Password)
		if hashErr != nil {
			u.Error("密码哈希失败", zap.Error(hashErr))
			return nil, hashErr
		}
		userModel.Password = hashedPwd
	}
	if createUser.Username != "" {
		userModel.Username = createUser.Username
	}

	userModel.ShortNo = shortNo
	userModel.OfflineProtection = 0
	userModel.NewMsgNotice = 1
	userModel.MsgShowDetail = 1
	userModel.SearchByPhone = 1
	userModel.SearchByShort = 1
	userModel.VoiceOn = 1
	userModel.ShockOn = 1
	userModel.IsUploadAvatar = createUser.IsUploadAvatar
	userModel.WXOpenid = createUser.WXOpenid
	userModel.WXUnionid = createUser.WXUnionid
	userModel.GiteeUID = createUser.GiteeUID
	userModel.GithubUID = createUser.GithubUID
	userModel.Status = int(common.UserAvailable)
	err = u.db.insertTx(userModel, tx)
	if err != nil {
		u.Error("注册用户失败", zap.Error(err))
		return nil, err
	}
	if createUser.Device != nil {
		err = u.deviceDB.insertOrUpdateDeviceTx(&deviceModel{
			UID:         createUser.UID,
			DeviceID:    createUser.Device.DeviceID,
			DeviceName:  createUser.Device.DeviceName,
			DeviceModel: createUser.Device.DeviceModel,
			LastLogin:   time.Now().Unix(),
		}, tx)
		if err != nil {
			u.Error("添加用户设备信息失败", zap.Error(err))
			return nil, err
		}
	}
	err = u.addSystemFriend(createUser.UID)
	if err != nil {
		u.Error("添加注册用户和系统账号为好友关系失败", zap.Error(err))
		return nil, err
	}
	err = u.addFileHelperFriend(createUser.UID)
	if err != nil {
		u.Error("添加注册用户和文件助手为好友关系失败", zap.Error(err))
		return nil, err
	}
	// Space 模式下不再自动添加 BotFather 为好友
	// Bot 通过 Space 成员关系自动可用
	// err = u.addBotFatherFriend(createUser.UID)
	// if err != nil {
	// 	u.Warn("添加注册用户和BotFather为好友关系失败", zap.Error(err))
	// }
	inviteCode := ""
	inviteUID := ""
	vercode := ""
	if invite != nil {
		inviteCode = invite.InviteCode
		inviteUID = invite.Uid
		vercode = invite.Vercode
	}
	//发送用户注册事件
	eventID, err := u.ctx.EventBegin(&wkevent.Data{
		Event: event.EventUserRegister,
		Type:  wkevent.Message,
		Data: map[string]interface{}{
			"uid":            createUser.UID,
			"invite_code":    inviteCode,
			"invite_uid":     inviteUID,
			"invite_vercode": vercode,
		},
	}, tx)
	if err != nil {
		u.Error("开启事件失败！", zap.Error(err))
		return nil, err
	}

	if commitCallback != nil {
		if err := commitCallback(); err != nil {
			return nil, err
		}
	}
	u.ctx.EventCommit(eventID)
	token := util.GenerUUID()
	// 将token设置到缓存
	tokenPayload, err := auth.Encode(auth.TokenInfo{UID: userModel.UID, Name: userModel.Name, Role: userModel.Role})
	if err != nil {
		u.Error("编码token缓存失败！", zap.Error(err))
		return nil, err
	}
	err = u.ctx.Cache().SetAndExpire(u.ctx.GetConfig().Cache.TokenCachePrefix+token, tokenPayload, u.ctx.GetConfig().Cache.TokenExpire)
	if err != nil {
		u.Error("设置token缓存失败！", zap.Error(err))
		return nil, err
	}
	_, err = u.ctx.UpdateIMToken(config.UpdateIMTokenReq{
		UID:         createUser.UID,
		Token:       token,
		DeviceFlag:  config.DeviceFlag(createUser.Flag),
		DeviceLevel: config.DeviceLevelSlave,
	})
	if err != nil {
		u.Error("更新IM的token失败！", zap.Error(err))
		return nil, err
	}
	go u.sentWelcomeMsg(publicIP, createUser.UID)

	if u.ctx.GetConfig().ShortNo.NumOn {
		err = u.commonService.SetShortnoUsed(userModel.ShortNo, "user")
		if err != nil {
			u.Error("设置短编号被使用失败！", zap.Error(err), zap.String("shortNo", userModel.ShortNo))
		}
	}

	resp := newLoginUserDetailResp(userModel, token, u.ctx)
	u.applyRealnameToLoginResp(resp, userModel.UID)
	return resp, nil
}

// ---------- vo ----------
type createUserModel struct {
	UID            string
	Name           string
	Zone           string
	Phone          string
	Email          string
	Sex            int
	Password       string
	WXOpenid       string
	WXUnionid      string
	GiteeUID       string
	GithubUID      string
	Username       string
	Flag           int
	IsUploadAvatar int
	Device         *deviceReq
}

// 重置登录密码
type resetPwdReq struct {
	Zone  string `json:"zone"`  //区号
	Phone string `json:"phone"` //手机号
	Code  string `json:"code"`  //验证码
	Pwd   string `json:"pwd"`   //密码
}
type customerservicesResp struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}
type registerReq struct {
	Name       string     `json:"name"`
	Zone       string     `json:"zone"`
	Phone      string     `json:"phone"`
	Code       string     `json:"code"`
	Password   string     `json:"password"`
	Flag       uint8      `json:"flag"`        // 注册设备的标记 0.APP 1.PC
	Device     *deviceReq `json:"device"`      //注册用户设备信息
	InviteCode string     `json:"invite_code"` // 邀请码
}

func (r registerReq) CheckRegister() error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("用户名不能为空！")
	}
	if err := ValidateName(r.Name); err != nil {
		return err
	}
	if strings.TrimSpace(r.Zone) == "" {
		return errors.New("区号不能为空！")
	}
	if strings.TrimSpace(r.Phone) == "" {
		return errors.New("手机号不能为空！")
	}
	if strings.TrimSpace(r.Code) == "" {
		return errors.New("验证码不能为空！")
	}
	if strings.TrimSpace(r.Password) == "" {
		return errors.New("密码不能为空！")
	}
	if len(r.Password) < 6 {
		return errors.New("密码长度必须大于6位！")
	}
	return nil
}

// 设置聊天密码请求
type chatPwdReq struct {
	ChatPwd  string `json:"chat_pwd"`  //聊天密码
	LoginPwd string `json:"login_pwd"` //登录密码
}

// 注册验证码请求
type codeReq struct {
	Zone  string `json:"zone"`
	Phone string `json:"phone"`
}
type loginReq struct {
	Username string     `json:"username"`
	Password string     `json:"password"`
	Flag     int        `json:"flag"`   // 设备标示 0.APP 1.PC
	Device   *deviceReq `json:"device"` //登录设备信息
}

func (r loginReq) Check() error {
	if strings.TrimSpace(r.Username) == "" {
		return errors.New("用户名不能为空！")
	}
	if strings.TrimSpace(r.Password) == "" {
		return errors.New("密码不能为空！")
	}
	return nil
}

type userResp struct {
	UID     string `json:"uid"`
	Name    string `json:"name"`
	Vercode string `json:"vercode"`
}

func newUserResp(m *Model) userResp {
	return userResp{
		UID:     m.UID,
		Name:    m.Name,
		Vercode: m.Vercode,
	}
}

type deviceReq struct {
	DeviceID    string `json:"device_id"`    //设备唯一ID
	DeviceName  string `json:"device_name"`  //设备名称
	DeviceModel string `json:"device_model"` //设备model
}

type loginUserDetailResp struct {
	UID             string  `json:"uid"`
	AppID           string  `json:"app_id"`
	Name            string  `json:"name"`
	Username        string  `json:"username"`
	Sex             int     `json:"sex"`               //性别1:男
	Category        string  `json:"category"`          //用户分类 '客服'
	ShortNo         string  `json:"short_no"`          // 用户唯一短编号
	Zone            string  `json:"zone"`              //区号
	Phone           string  `json:"phone"`             //手机号
	Token           string  `json:"token"`             //token
	ChatPwd         string  `json:"chat_pwd"`          //聊天密码
	LockScreenPwd   string  `json:"lock_screen_pwd"`   // 锁屏密码
	LockAfterMinute int     `json:"lock_after_minute"` // 在N分钟后锁屏
	Setting         setting `json:"setting"`
	RSAPublicKey    string  `json:"rsa_public_key"` // 应用公钥做一些消息验证 base64编码
	ShortStatus     int     `json:"short_status"`
	MsgExpireSecond int64   `json:"msg_expire_second"` // 消息过期时长
	// 注销状态提示：仅当账号处于冷静期（is_destroy=1）时下发
	// DestroyStatus: 0=正常 1=注销申请中
	// DestroyRemainingDays: 距到期还剩天数（向上取整，最小 0）
	DestroyStatus        int   `json:"destroy_status,omitempty"`
	DestroyRemainingDays int   `json:"destroy_remaining_days,omitempty"`
	DestroyExpireAt      int64 `json:"destroy_expire_at,omitempty"` // Unix 秒
	// YUJ-413: self 实名字段（必须下发，否则 Web/Android/iOS 三端 self 徽章和
	// displayName 全部瞎 —— friend/sync、conversation/sync 对他人已下发同名字段，
	// 这里补 self 路径）。
	// 字段语义和 UserDetailResp 对齐：
	//   RealnameVerified    - 是否已完成 OCTO 实名（user_verification 表有记录）
	//   RealName            - 已认证时的权威姓名；未认证留空
	//   RealnameVerifiedAt  - 实名完成时间(Unix 秒)；未认证为 0 并被 omitempty 剥离
	RealnameVerified   bool   `json:"realname_verified"`
	RealName           string `json:"real_name,omitempty"`
	RealnameVerifiedAt int64  `json:"realname_verified_at,omitempty"`
}

type setting struct {
	SearchByPhone     int `json:"search_by_phone"`    //是否可以通过手机号搜索0.否1.是
	SearchByShort     int `json:"search_by_short"`    //是否可以通过短编号搜索0.否1.是
	NewMsgNotice      int `json:"new_msg_notice"`     //新消息通知0.否1.是
	MsgShowDetail     int `json:"msg_show_detail"`    //显示消息通知详情0.否1.是
	VoiceOn           int `json:"voice_on"`           //声音0.否1.是
	ShockOn           int `json:"shock_on"`           //震动0.否1.是
	OfflineProtection int `json:"offline_protection"` //离线保护，断网屏保
	DeviceLock        int `json:"device_lock"`        // 设备锁
	MuteOfApp         int `json:"mute_of_app"`        // web登录 app是否静音
}

type blacklistResp struct {
	UID      string `json:"uid"`
	Name     string `json:"name"`
	Username string `json:"usename"`
}

func newLoginUserDetailResp(m *Model, token string, ctx *config.Context) *loginUserDetailResp {

	var destroyStatus, destroyRemainingDays int
	var destroyExpireAt int64
	if m.IsDestroy == IsDestroyApplying && m.DestroyExpireAt.Valid {
		destroyStatus = IsDestroyApplying
		destroyExpireAt = m.DestroyExpireAt.Time.Unix()
		destroyRemainingDays = remainingDays(m.DestroyExpireAt.Time)
	}

	return &loginUserDetailResp{
		DestroyStatus:        destroyStatus,
		DestroyRemainingDays: destroyRemainingDays,
		DestroyExpireAt:      destroyExpireAt,
		UID:                  m.UID,
		AppID:                m.AppID,
		Name:                 m.Name,
		Username:             m.Username,
		Sex:                  m.Sex,
		Category:             m.Category,
		ShortNo:              m.ShortNo,
		Zone:                 m.Zone,
		Phone:                m.Phone,
		Token:                token,
		ChatPwd:              m.ChatPwd,
		LockScreenPwd:        m.LockScreenPwd,
		LockAfterMinute:      m.LockAfterMinute,
		ShortStatus:          m.ShortStatus,
		RSAPublicKey:         base64.StdEncoding.EncodeToString([]byte(ctx.GetConfig().AppRSAPubKey)),
		MsgExpireSecond:      m.MsgExpireSecond,
		Setting: setting{
			SearchByPhone:     m.SearchByPhone,
			SearchByShort:     m.SearchByShort,
			NewMsgNotice:      m.NewMsgNotice,
			MsgShowDetail:     m.MsgShowDetail,
			VoiceOn:           m.VoiceOn,
			ShockOn:           m.ShockOn,
			OfflineProtection: m.OfflineProtection,
			DeviceLock:        m.DeviceLock,
			MuteOfApp:         m.MuteOfApp,
		},
	}
}

// applyRealnameToLoginResp 从 user_verification 表读取 self 实名字段并写入
// login / current response。YUJ-413：/v1/user/login、GET /v1/user/current 必须下发
// realname_verified / real_name / realname_verified_at 三字段，否则 Web/Android/iOS
// 三端 self 徽章和 displayName 无法渲染（friend/sync、conversation/sync 对他人已
// 下发同名字段，这里补齐 self 路径）。
//
// 语义：
//   - 未实名 / 查询失败 → realname_verified=false，其它字段保持零值（被 omitempty 剥离）；
//   - 已实名 → realname_verified=true，real_name 回填，verified_at 转 Unix 秒。
//
// 查询失败仅 warn 不阻断登录 —— 实名是增强信息，查询抖动不应让登录失败。
func (u *User) applyRealnameToLoginResp(resp *loginUserDetailResp, uid string) {
	if resp == nil || uid == "" {
		return
	}
	vr, err := u.verificationDB.QueryByUID(uid)
	if err != nil {
		u.Warn("查询 self 实名认证记录失败", zap.Error(err), zap.String("uid", uid))
		return
	}
	if vr == nil {
		return
	}
	resp.RealnameVerified = true
	resp.RealName = vr.RealName
	if !vr.VerifiedAt.IsZero() {
		resp.RealnameVerifiedAt = vr.VerifiedAt.Unix()
	}
}

// applyRealnameToAuthCodeMap 往 loginWithAuthCode 的 map[string]interface{}
// 响应里写三个实名字段（YUJ-413 R5 Blocking #2）。
//
// loginWithAuthCode 走的是手写 map（历史扫码登录协议保留了一组最小字段），
// 不经过 newLoginUserDetailResp / applyRealnameToLoginResp，之前完全没有
// 实名字段 —— 扫码登录进来的客户端永远拿不到 self 实名态。语义和
// applyRealnameToLoginResp 对齐:
//   - realname_verified 一律下发(true/false)，保留三态语义，key 存在即表示
//     "服务器已表态"，缺失表示"数据链路有问题"。这是客户端 parser 的硬契约。
//   - real_name / realname_verified_at 仅已实名时加（和 loginUserDetailResp
//     的 omitempty 语义对齐）。
//   - 查询失败只 warn 不阻断登录。
func (u *User) applyRealnameToAuthCodeMap(m map[string]interface{}, uid string) {
	if m == nil || uid == "" {
		return
	}
	// 默认已先标 false，避免 DB 查询分支里忘记写默认值。
	m["realname_verified"] = false
	vr, err := u.verificationDB.QueryByUID(uid)
	if err != nil {
		u.Warn("auth-code 登录查询实名认证记录失败", zap.Error(err), zap.String("uid", uid))
		return
	}
	if vr == nil {
		return
	}
	m["realname_verified"] = true
	if vr.RealName != "" {
		m["real_name"] = vr.RealName
	}
	if !vr.VerifiedAt.IsZero() {
		m["realname_verified_at"] = vr.VerifiedAt.Unix()
	}
}

// ValidateName checks that a display name does not contain the @ character,
// which is used as delimiter in token cache entries (uid@name@role).
// Allowing @ in names would enable privilege escalation via role injection.
func ValidateName(name string) error {
	if strings.Contains(name, "@") {
		return errors.New("名字不能包含@字符！")
	}
	return nil
}

// ==================== Aegis OIDC Phase 2d — verify-token 翻译层 ====================

// getVerifyTokenAegisURL 返回老 App 调 /v1/internal/verify-token 后我们要返回的跳转地址。
//
// OSS 部署者必须通过 OCTO_VERIFY_URL 环境变量配置自己的 Aegis/verify 服务入口。
// 内部线上环境通过部署脚本注入对应值，不再硬编码到源码里。
//
// 为保持向后兼容（没设置 env 时行为），保留了原 Mininglamp 内部 URL 作为默认值 ——
// 这样 internal dev / staging 不会因为漏配 env 而 regress。OSS release 流水线会通过
// module_rewrites 把这个 default 改成 example.com。
//
// 注意:
//   - URL 本身不再签 HMAC / JWT（YUJ-394），只是稳定代理一个地址。
//   - return_to 必须是 dmwork:// 深链,不能是 https://,避免钓鱼。
func getVerifyTokenAegisURL() string {
	if v := os.Getenv("OCTO_VERIFY_URL"); v != "" {
		return v
	}
	// Internal default for backward compat (pre-env config). OSS
	// builds rewrite this string via octo-release module_rewrites.
	return "https://accounts.example.com/profile/info?anchor=verification&return_to=octo://verified"
}

// verifyTokenAegisExpiresIn 老 App 合同里 expires_in 是秒数;Aegis URL 本身没有过期概念,
// 但老 App 拿到后会在这个窗口内打开浏览器,保持 5 分钟这个历史默认值即可。
const verifyTokenAegisExpiresIn = 300

// verifyTokenAegisRedirect 是 /v1/internal/verify-token 的 Aegis 翻译层 handler。
//
// Phase 1 把该接口改成 410 Gone,导致老 App 点"去认证"直接报错;Phase 2d 恢复为翻译层:
// 已登录用户请求 → 200 + {url: Aegis 账户页, expires_in: 300};
// 未登录用户 → AuthMiddleware 自动拒 401。
//
// 与老 verify-service 版本的区别:
//   - 不再签 5 分钟 JWT,URL 里没有任何用户态。
//   - 不携带 HMAC 签名,只是一个稳定的公开 URL。
//   - Aegis 页面自己走 OIDC session 识别用户,dmworkim 这边只负责把老 App 导过去。
func (u *User) verifyTokenAegisRedirect(c *wkhttp.Context) {
	// AuthMiddleware 已经保证未登录会被拒;这里再 double-check 一次 LoginUID,
	// 避免将来有人不小心把中间件摘掉导致 return_to 泄露给匿名用户。
	if strings.TrimSpace(c.GetLoginUID()) == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "login required"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"url":        getVerifyTokenAegisURL(),
		"expires_in": verifyTokenAegisExpiresIn,
	})
}

// ==================== Auth Verify API (for Gateway / Microservices) ====================

type authVerifyTokenReq struct {
	Token string `json:"token"`
}

type ownedBot struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type authVerifyTokenResp struct {
	UID       string     `json:"uid"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	OwnedBots []ownedBot `json:"owned_bots"`
}

// authVerifyToken validates a user token and returns identity + owned bots.
func (u *User) authVerifyToken(c *wkhttp.Context) {
	var req authVerifyTokenReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorf("invalid request: %v", err)
		return
	}
	if req.Token == "" {
		c.ResponseError(errors.New("token is required"))
		return
	}

	// Same Redis lookup as AuthMiddleware: "token:<value>" → versioned envelope
	// (v2 JSON) 或 legacy "uid@name[@role]"。auth.Decode 兼容两者。
	raw, cacheErr := u.ctx.Cache().Get(u.ctx.GetConfig().Cache.TokenCachePrefix + req.Token)
	if cacheErr != nil || strings.TrimSpace(raw) == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "invalid or expired token"})
		return
	}
	info, decodeErr := auth.Decode(raw)
	if decodeErr != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "malformed token data"})
		return
	}

	resp := authVerifyTokenResp{
		UID:       info.UID,
		Name:      info.Name,
		Role:      info.Role,
		OwnedBots: make([]ownedBot, 0),
	}

	// Query owned bots: robot.creator_uid = uid
	type botRow struct {
		RobotID string `db:"robot_id"`
		Name    string `db:"name"`
	}
	var bots []botRow
	_, err := u.db.session.SelectBySql(
		"SELECT r.robot_id, IFNULL(u.name,'') as name FROM robot r "+
			"INNER JOIN `user` u ON r.robot_id = u.uid "+
			"WHERE r.creator_uid = ? AND r.status = 1", resp.UID,
	).Load(&bots)
	if err == nil {
		for _, b := range bots {
			resp.OwnedBots = append(resp.OwnedBots, ownedBot{UID: b.RobotID, Name: b.Name})
		}
	}

	c.Response(resp)
}

type authVerifyBotReq struct {
	BotToken string `json:"bot_token"`
}

type authVerifyBotResp struct {
	BotUID    string `json:"bot_uid"`
	BotName   string `json:"bot_name"`
	OwnerUID  string `json:"owner_uid"`
	OwnerName string `json:"owner_name"`
	SpaceID   string `json:"space_id"`
}

// authVerifyBot validates a Bot token (BotFather Bearer token) and returns bot + owner info.
func (u *User) authVerifyBot(c *wkhttp.Context) {
	var req authVerifyBotReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorf("invalid request: %v", err)
		return
	}
	if req.BotToken == "" {
		c.ResponseError(errors.New("bot_token is required"))
		return
	}

	// Query robot by bot_token
	var botInfo struct {
		RobotID    string `db:"robot_id"`
		CreatorUID string `db:"creator_uid"`
	}
	err := u.db.session.Select("robot_id", "IFNULL(creator_uid,'') as creator_uid").
		From("robot").
		Where("bot_token = ? AND bot_token != '' AND status = 1", req.BotToken).
		LoadOne(&botInfo)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "invalid bot token"})
		return
	}

	// Get bot display name
	botName := botInfo.RobotID
	botUser, _ := u.userService.GetUser(botInfo.RobotID)
	if botUser != nil {
		botName = botUser.Name
	}

	// Get owner name
	ownerName := ""
	if botInfo.CreatorUID != "" {
		ownerUser, _ := u.userService.GetUser(botInfo.CreatorUID)
		if ownerUser != nil {
			ownerName = ownerUser.Name
		}
	}

	// Get bot's Space (first active space_member record)
	var spaceID string
	_ = u.db.session.Select("space_id").From("space_member").
		Where("uid = ? AND status = 1", botInfo.RobotID).
		OrderDir("created_at", false).
		Limit(1).
		LoadOne(&spaceID)

	c.Response(authVerifyBotResp{
		BotUID:    botInfo.RobotID,
		BotName:   botName,
		OwnerUID:  botInfo.CreatorUID,
		OwnerName: ownerName,
		SpaceID:   spaceID,
	})
}
