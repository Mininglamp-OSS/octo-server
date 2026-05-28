package common

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Common Common
type Common struct {
	ctx *config.Context
	log.Log
	db             *db
	appConfigDB    *appConfigDB
	systemSettings *SystemSettings
	threadOn       int // 缓存 DM_THREAD_ON 环境变量
}

// New New
func New(ctx *config.Context) *Common {
	var threadOn int
	if t := strings.ToLower(os.Getenv("DM_THREAD_ON")); t == "true" || t == "1" {
		threadOn = 1
	}
	return &Common{
		ctx:            ctx,
		db:             newDB(ctx.DB()),
		appConfigDB:    newAppConfigDB(ctx),
		systemSettings: EnsureSystemSettings(ctx),
		Log:            log.NewTLog("common"),
		threadOn:       threadOn,
	}
}

// SystemSettings exposes the shared admin-tunable settings reader for
// callers that already hold a *Common. New consumers in other packages
// should prefer common.EnsureSystemSettings(ctx) directly.
func (cn *Common) SystemSettings() *SystemSettings {
	return cn.systemSettings
}

// Route 路由配置
func (cn *Common) Route(r *wkhttp.WKHttp) {
	common := r.Group("/v1/common", cn.ctx.AuthMiddleware(r))
	{
		common.POST("/appversion", cn.addAppVersion)             // 添加APP版本
		common.GET("/appversion/:os/:version", cn.getNewVersion) // 获取最新版本
		common.GET("/appversion/list", cn.appVersionList)        // 版本列表
		common.GET("/chatbg", cn.chatBgList)                     // 聊天背景列表
		common.GET("/appmodule", cn.appModule)                   // app模块列表
	}
	commonNoAuth := r.Group("/v1/common")
	{
		commonNoAuth.GET("/countries", cn.countriesList)

		commonNoAuth.GET("/appconfig", cn.appConfig) // app配置
		// commonNoAuth.GET("/keepalive", cn.getKeepAliveVideo)   // 获取后台运行引导视频
		commonNoAuth.GET("/updater/:os/:version", cn.updater)  // 版本更新检查（兼容tauri）
		commonNoAuth.GET("/pcupdater/:os", cn.getPCNewVersion) // pc版本更新检查
		commonNoAuth.GET("/changelog", cn.changelog)           // 版本更新日志（公开）
	}

	r.GET("/v1/health", func(c *wkhttp.Context) {
		var (
			statusMap = map[string]string{
				"status": "up",
				"db":     "up",
				"redis":  "up",
			}
			lastError error
		)

		err := cn.db.session.Ping()
		if err != nil {
			cn.Error("db ping error", zap.Error(err))
			lastError = err
			statusMap["db"] = "down"
		}

		_, err = cn.ctx.GetRedisConn().Ping()
		if err != nil {
			cn.Error("redis ping error", zap.Error(err))
			lastError = err
			statusMap["redis"] = "down"
		}

		if lastError != nil {
			statusMap["status"] = "down"
			statusMap["error"] = lastError.Error()
		}

		c.JSON(http.StatusOK, statusMap)
	})

	appConfigM, err := cn.insertAppConfigIfNeed()
	if err != nil {
		cn.Error("初始化应用配置失败", zap.Error(err))
		panic(err)
	}
	if appConfigM == nil {
		cn.Error("初始化应用配置返回空结果")
		panic(errors.New("初始化应用配置返回空结果"))
	}
	// 设置系统私钥（支持加密存储，向后兼容明文）
	privateKey, err := decryptKey(appConfigM.RSAPrivateKey)
	if err != nil {
		cn.Error("解密RSA私钥失败", zap.Error(err))
		panic(err)
	}
	cn.ctx.GetConfig().AppRSAPrivateKey = privateKey
	cn.ctx.GetConfig().AppRSAPubKey = appConfigM.RSAPublicKey

	// 启动期校验:DB 已写入 login.local_off=1 但部署没有任何第三方登录
	// 提供方,LocalLoginOff() 会自动回退为 false 避免锁死。把这个状态作为
	// error 日志显式打出,让运维一眼能看到"开关写了但当前不生效"。
	// 此处直接读 snapshot 是安全的:Load 刚刚完成,值就是 DB 当前值。
	cn.systemSettings.LogLocalLoginOffSafetyOverrideIfActive(
		cn.systemSettings.RawLocalLoginOffFromSnapshot())
}

// 获取后台运行引导视频
func (cn *Common) getKeepAliveVideo(c *wkhttp.Context) {
	videoName := c.Query("video_name")
	if videoName == "" {
		c.ResponseError(errors.New("视频名称不能为空"))
		return
	}

	// Sanitize: extract base filename to prevent path traversal
	videoName = filepath.Base(videoName)

	// Validate file extension
	if !strings.HasSuffix(strings.ToLower(videoName), ".mp4") {
		c.ResponseError(errors.New("仅支持mp4格式"))
		return
	}

	videoPath := filepath.Join("assets", "resources", "keepalive", videoName)

	// Verify resolved path stays within the expected directory
	absPath, err := filepath.Abs(videoPath)
	if err != nil {
		c.Writer.WriteHeader(http.StatusNotFound)
		return
	}
	baseDir, err := filepath.Abs("assets/resources/keepalive")
	if err != nil {
		c.Writer.WriteHeader(http.StatusNotFound)
		return
	}
	if !strings.HasPrefix(absPath, baseDir+string(filepath.Separator)) {
		c.ResponseError(errors.New("非法文件路径"))
		return
	}

	c.Header("Content-Type", "video/mp4")
	videoBytes, err := os.ReadFile(videoPath)
	if err != nil {
		cn.Error("视频不存在", zap.Error(err))
		c.Writer.WriteHeader(http.StatusNotFound)
		return
	}
	if _, err = c.Writer.Write(videoBytes); err != nil {
		cn.Error("写入视频数据失败", zap.Error(err))
	}
}

// 获取pc最新版本
func (cn *Common) getPCNewVersion(c *wkhttp.Context) {
	os := c.Param("os")
	tempOS := ""
	if os == "latest-mac.yml" {
		tempOS = "mac"
	}
	if os == "latest-linux.yml" {
		tempOS = "linx"
	}
	if os == "latest.yml" {
		tempOS = "windows"
	}
	model, err := cn.db.queryNewVersion(tempOS)
	if err != nil {
		cn.Error("查询最新版本错误", zap.Error(err))
		c.ResponseError(errors.New("查询最新版本错误"))
		return
	}
	if model == nil {
		c.Status(http.StatusNoContent)
		return
	}
	downloadURL := fmt.Sprintf("%s/%s", cn.ctx.GetConfig().External.APIBaseURL, model.DownloadURL)
	c.JSON(http.StatusOK, gin.H{
		"version":      model.AppVersion,
		"path":         downloadURL,
		"sha512":       model.Signature,
		"releaseNotes": model.UpdateDesc,
	})
	// if os == "latest-mac.yml" || os == "latest-linux.yml" || os == "latest.yml" {

	// }
}
func (cn *Common) updater(c *wkhttp.Context) {
	os := c.Param("os")
	oldVersion := c.Param("version")

	model, err := cn.db.queryNewVersion(os)
	if err != nil {
		cn.Error("查询最新版本错误", zap.Error(err))
		c.ResponseError(errors.New("查询最新版本错误"))
		return
	}
	if model == nil || model.AppVersion == oldVersion {
		c.Status(http.StatusNoContent)
		return
	}
	if os == "latest-mac.yml" || os == "latest-linux.yml" || os == "latest.yml" {
		c.JSON(http.StatusOK, gin.H{
			"version":      model.AppVersion,
			"path":         model.DownloadURL,
			"sha512":       model.Signature,
			"releaseNotes": model.UpdateDesc,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"url":       model.DownloadURL,
		"version":   model.AppVersion,
		"notes":     model.UpdateDesc,
		"pub_date":  time.Time(model.UpdatedAt).Format("2006-01-02T15:04:05Z"),
		"signature": model.Signature,
	})
}

// 查询app模块
func (cn *Common) appModule(c *wkhttp.Context) {
	modules, err := cn.db.queryAppModule()
	if err != nil {
		cn.Error("查询所有app模块错误", zap.Error(err))
		c.ResponseError(errors.New("查询所有app模块错误"))
		return
	}
	list := make([]*appModuleResp, 0)
	if len(modules) > 0 {
		for _, module := range modules {
			list = append(list, &appModuleResp{
				SID:    module.SID,
				Name:   module.Name,
				Desc:   module.Desc,
				Status: module.Status,
			})
		}
	}
	c.Response(list)
}

// 查询聊天背景列表
func (cn *Common) chatBgList(c *wkhttp.Context) {
	list, err := cn.db.queryChatBgs()
	if err != nil {
		cn.Error("查询所有聊天背景错误", zap.Error(err))
		c.ResponseError(errors.New("查询所有聊天背景错误"))
		return
	}
	resps := make([]*chatBgResp, 0)
	if len(list) == 0 {
		c.Response(resps)
		return
	}
	for index, model := range list {
		var lightColors = make([]string, 0)
		var darkColors = make([]string, 0)
		if model.IsSvg == 1 && index < len(defaultColorsLight) {
			lightColors = defaultColorsLight[index]
		}
		if model.IsSvg == 1 && index < len(defaultColorsDark) {
			darkColors = defaultColorsDark[index]
		}
		resps = append(resps, &chatBgResp{
			Cover:       model.Cover,
			Url:         model.Url,
			IsSvg:       model.IsSvg,
			LightColors: lightColors,
			DarkColors:  darkColors,
		})
	}
	c.Response(resps)
}
func (cn *Common) insertAppConfigIfNeed() (*appConfigModel, error) {

	appConfigM, err := cn.appConfigDB.query()
	if err != nil {
		return nil, err
	}
	if appConfigM != nil {
		return appConfigM, nil
	}

	privateKeyBuff := new(bytes.Buffer)
	publicKeyBuff := new(bytes.Buffer)

	bits := 2048
	// 生成私钥文件
	privateKey, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, err
	}
	derStream := x509.MarshalPKCS1PrivateKey(privateKey)
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: derStream,
	}
	err = pem.Encode(privateKeyBuff, block)
	if err != nil {
		return nil, err
	}
	// 生成公钥文件
	publicKey := &privateKey.PublicKey
	derPkix, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	block = &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: derPkix,
	}
	err = pem.Encode(publicKeyBuff, block)
	if err != nil {
		return nil, err
	}

	// Encrypt private key if master key is configured
	encPrivateKey, err := encryptKey(privateKeyBuff.String())
	if err != nil {
		return nil, fmt.Errorf("encrypt private key: %w", err)
	}

	appConfigM = &appConfigModel{
		RSAPrivateKey: encPrivateKey,
		RSAPublicKey:  publicKeyBuff.String(),
		Version:       1,
		SuperToken:    util.GenerUUID(),
		SuperTokenOn:  0,
		SearchByPhone: 1,
	}
	err = cn.appConfigDB.insert(appConfigM)
	return appConfigM, err
}

// boolToFlag normalises a bool getter result to the 0/1 int flag used by
// existing appconfig JSON fields (phone_search_off, shortno_edit_off, ...).
// Keeps the wire shape int across the response so frontend doesn't need a
// special "boolean-or-int" decode path for one field.
func boolToFlag(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (cn *Common) appConfig(c *wkhttp.Context) {
	versionStr := c.Query("version")
	appConfigM, err := cn.appConfigDB.query()
	if err != nil {
		cn.Error("查询应用配置失败！", zap.Error(err))
		c.ResponseError(errors.New("查询应用配置失败！"))
		return
	}
	if appConfigM == nil {
		cn.Error("应用配置为空")
		c.ResponseError(errors.New("应用配置为空"))
		return
	}
	versionI64, err := strconv.ParseInt(versionStr, 10, 64)
	if err != nil && versionStr != "" {
		cn.Warn("解析版本号失败", zap.String("version", versionStr), zap.Error(err))
	}
	if versionI64 != 0 && int(versionI64) >= appConfigM.Version {
		c.JSON(http.StatusOK, &appConfigResp{
			Version:                appConfigM.Version,
			SystemBotUIDs:          spacepkg.SystemBotList(),
			LocalLoginOff:          boolToFlag(cn.systemSettings.LocalLoginOff()),
			DisableUserCreateSpace: boolToFlag(cn.systemSettings.SpaceDisableUserCreate()),
		})
		return
	}
	var phoneSearchOff int
	var shortnoEditOff int
	var revokeSecond int
	if cn.ctx.GetConfig().PhoneSearchOff {
		phoneSearchOff = 1
	}
	if cn.ctx.GetConfig().ShortNo.EditOff {
		shortnoEditOff = 1
	}
	if appConfigM.RevokeSecond == 0 {
		revokeSecond = -1
	} else {
		revokeSecond = appConfigM.RevokeSecond
	}

	c.JSON(http.StatusOK, &appConfigResp{
		Version:                        appConfigM.Version,
		PhoneSearchOff:                 phoneSearchOff,
		ShortnoEditOff:                 shortnoEditOff,
		WebURL:                         cn.ctx.GetConfig().External.WebLoginURL,
		RevokeSecond:                   revokeSecond,
		RegisterInviteOn:               appConfigM.RegisterInviteOn,
		SendWelcomeMessageOn:           appConfigM.SendWelcomeMessageOn,
		InviteSystemAccountJoinGroupOn: appConfigM.InviteSystemAccountJoinGroupOn,
		RegisterUserMustCompleteInfoOn: appConfigM.RegisterUserMustCompleteInfoOn,
		CanModifyApiUrl:                appConfigM.CanModifyApiUrl,
		ThreadOn:                       cn.threadOn,
		DestroyCoolingOffDays:          destroyCoolingOffDaysOrDefault(appConfigM.DestroyCoolingOffDays),
		OIDCAccountURL:                 oidcAccountURL(),
		OIDCResetPasswordURL:           oidcResetPasswordURL(),
		OIDCProviders:                  oidcProviders(),
		// YUJ-219-A / GH#1283：单一真源下发系统 Bot UID 列表，替代三端硬编码。
		SystemBotUIDs:          spacepkg.SystemBotList(),
		LocalLoginOff:          boolToFlag(cn.systemSettings.LocalLoginOff()),
		DisableUserCreateSpace: boolToFlag(cn.systemSettings.SpaceDisableUserCreate()),
	})
}

// oidcProviders 返回 OIDC provider 元数据数组,让前端不再硬编码 provider id/name/authorize_path。
//
// 单 provider 设计期下数组长度恒为 0 或 1:OIDC 启用时返回一个元素;关闭则空数组。
// 用 omitempty 让关闭状态下整个字段从 JSON 里消失,与现有 oidc_account_url 保持一致。
//
// 字段来源:
//   - id   : DM_OIDC_PROVIDER_ID (默认 "oidc"),与 oidc 模块路由路径段保持一致
//   - name : DM_OIDC_PROVIDER_NAME (默认 "SSO"),前端用于按钮/菜单文案
//   - authorize_path : 由 id 拼出 /v1/auth/oidc/<id>/authorize
//   - account_url / reset_password_url : 复用顶层老字段的取值逻辑
func oidcProviders() []oidcProviderResp {
	if !oidcEnabled() {
		return nil
	}
	id := os.Getenv("DM_OIDC_PROVIDER_ID")
	if id == "" || !providerIDRe.MatchString(id) {
		// 与 oidc 模块 LoadConfig 同义:非法 ID 视为未配置,回退到默认值,
		// 避免畸形值进 authorize_path 把前端引到不存在的路由。
		id = "oidc"
	}
	name := os.Getenv("DM_OIDC_PROVIDER_NAME")
	if name == "" {
		name = "SSO"
	}
	return []oidcProviderResp{{
		ID:               id,
		Name:             name,
		AuthorizePath:    "/v1/auth/oidc/" + id + "/authorize",
		AccountURL:       oidcAccountURL(),
		ResetPasswordURL: oidcResetPasswordURL(),
	}}
}

// providerIDRe 与 oidc/config.go 保持一致,避免 common 模块反向依赖 oidc 包。
// 两处同一来源是 ops env,语义统一即可。
var providerIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// oidcAccountURL 返回 OIDC 账户中心首页 URL，仅在 OIDC 启用时下发。
//
// 优先级:DM_OIDC_ACCOUNT_URL  >  DM_OIDC_PROVIDER_ISSUER  >  DM_OIDC_AEGIS_ISSUER。
// 多数标准 OIDC IdP（Aegis/Keycloak/...）的 issuer 即账户首页，可省一份配置；
// AEGIS_ISSUER 是过渡 alias，迁移完成后随 oidc 模块同步删除。
func oidcAccountURL() string {
	if !oidcEnabled() {
		return ""
	}
	if v := os.Getenv("DM_OIDC_ACCOUNT_URL"); v != "" {
		return v
	}
	if v := os.Getenv("DM_OIDC_PROVIDER_ISSUER"); v != "" {
		return v
	}
	return os.Getenv("DM_OIDC_AEGIS_ISSUER")
}

// oidcResetPasswordURL 返回 OIDC 修改/重置密码 URL，仅在 OIDC 启用时下发。
// issuer 不一定等于重置密码页，这里不做回退，缺省即不下发，前端隐藏对应入口。
func oidcResetPasswordURL() string {
	if !oidcEnabled() {
		return ""
	}
	return os.Getenv("DM_OIDC_RESET_PASSWORD_URL")
}

// oidcEnabled reports whether OIDC is intended to be on. Parsing must match
// modules/oidc/config.go:getBool (strconv.ParseBool) byte-for-byte and stay
// in lockstep with isOIDCFullyConfigured in system_settings.go — diverging
// here causes a front-end lockout where local_login_off=1 hides the local
// card while oidc_providers / oidc_account_url are silently omitted because
// this function rejects spellings like "T" or "True" that the OIDC module
// itself accepts. See PR #104 P0 (Jerry-Xin).
func oidcEnabled() bool {
	v := os.Getenv("DM_OIDC_ENABLED")
	if v == "" {
		return false
	}
	enabled, err := strconv.ParseBool(v)
	return err == nil && enabled
}

// 兼容历史 app_config 行（NOT NULL DEFAULT 7 在迁移前的行为）：值 ≤ 0 时回退为 7。
func destroyCoolingOffDaysOrDefault(v int) int {
	if v > 0 {
		return v
	}
	return 7
}

func (cn *Common) countriesList(c *wkhttp.Context) {
	c.JSON(http.StatusOK, Countrys())
}

// 添加app版本
func (cn *Common) addAppVersion(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		c.ResponseError(err)
		return
	}
	var req appVersionReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	err = cn.check(req)
	if err != nil {
		c.ResponseError(err)
		return
	}
	_, err = cn.db.insertAppVersion(&appVersionModel{
		AppVersion:  req.AppVersion,
		OS:          req.OS,
		IsForce:     req.IsForce,
		UpdateDesc:  req.UpdateDesc,
		DownloadURL: req.DownloadURL,
		Signature:   req.Signature,
	})
	if err != nil {
		cn.Error("添加更新记录错误", zap.Error(err))
		c.ResponseError(errors.New("添加更新记录错误"))
		return
	}
	c.ResponseOK()
}

// 获取最新版本
func (cn *Common) getNewVersion(c *wkhttp.Context) {
	os := c.Param("os")
	version := c.Param("version")
	if os == "" {
		c.ResponseError(errors.New("平台类型不能为空"))
		return
	}
	if version == "" {
		c.ResponseError(errors.New("版本号不能为空"))
		return
	}
	model, err := cn.db.queryNewVersion(os)
	if err != nil {
		cn.Error("查询最新版本错误", zap.Error(err))
		c.ResponseError(errors.New("查询最新版本错误"))
		return
	}
	if model == nil || model.AppVersion == version {
		c.Response(map[string]interface{}{})
		return
	}
	c.Response(&appVersionResp{
		AppVersion:  model.AppVersion,
		OS:          model.OS,
		DownloadURL: model.DownloadURL,
		IsForce:     model.IsForce,
		UpdateDesc:  model.UpdateDesc,
		CreatedAt:   model.CreatedAt.String(),
	})
}

// 查询总记录
func (cn *Common) appVersionList(c *wkhttp.Context) {
	err := c.CheckLoginRole()
	if err != nil {
		c.ResponseError(err)
		return
	}
	pageIndex, pageSize := c.GetPage()
	list, err := cn.db.queryAppVersionListWithPage(uint64(pageSize), uint64(pageIndex))
	if err != nil {
		cn.Error("查询版本列表错误", zap.Error(err))
		c.ResponseError(errors.New("查询版本列表错误"))
		return
	}
	count, err := cn.db.queryCount()
	if err != nil {
		cn.Error("查询总数量错误", zap.Error(err))
		c.ResponseError(errors.New("查询总数量错误"))
		return
	}
	resps := make([]*appVersionResp, 0)
	if len(list) == 0 {
		c.Response(map[string]interface{}{
			"count": count,
			"list":  resps,
		})
		return
	}

	for _, model := range list {
		resps = append(resps, &appVersionResp{
			AppVersion:  model.AppVersion,
			OS:          model.OS,
			IsForce:     model.IsForce,
			UpdateDesc:  model.UpdateDesc,
			DownloadURL: model.DownloadURL,
			CreatedAt:   model.CreatedAt.String(),
		})
	}
	c.Response(map[string]interface{}{
		"count": count,
		"list":  resps,
	})
}

// changelog 公开版本更新日志
func (cn *Common) changelog(c *wkhttp.Context) {
	list, err := cn.db.queryAppVersionListWithPage(200, 1)
	if err != nil {
		cn.Error("查询版本列表错误", zap.Error(err))
		c.ResponseError(errors.New("查询版本列表错误"))
		return
	}
	resps := make([]*appVersionResp, 0, len(list))
	for _, model := range list {
		resps = append(resps, &appVersionResp{
			AppVersion:  model.AppVersion,
			OS:          model.OS,
			IsForce:     model.IsForce,
			UpdateDesc:  model.UpdateDesc,
			DownloadURL: model.DownloadURL,
			CreatedAt:   model.CreatedAt.String(),
		})
	}
	c.Response(resps)
}

func (cn *Common) check(req appVersionReq) error {
	if req.AppVersion == "" {
		return errors.New("请输入版本号")
	}
	if req.UpdateDesc == "" {
		return errors.New("请输入更新说明")
	}
	if req.OS == "" {
		return errors.New("请输入升级平台")
	}
	if req.OS == "android" && req.DownloadURL == "" {
		return errors.New("Android平台请传入下载地址")
	}
	return nil
}

type appModuleResp struct {
	SID    string `json:"sid"`
	Name   string `json:"name"`
	Desc   string `json:"desc"`
	Status int    `json:"status"` // 模块状态 1.可选 0.不可选 2.选中不可编辑
}

type chatBgResp struct {
	Cover       string   `json:"cover"`
	Url         string   `json:"url"`
	IsSvg       int      `json:"is_svg"`
	LightColors []string `json:"light_colors"`
	DarkColors  []string `json:"dark_colors"`
}

type appConfigResp struct {
	Version                        int    `json:"version"`
	WebURL                         string `json:"web_url"`
	PhoneSearchOff                 int    `json:"phone_search_off"`
	ShortnoEditOff                 int    `json:"shortno_edit_off"`
	RevokeSecond                   int    `json:"revoke_second"`
	AppleSignIn                    int    `json:"apple_sign_in"`
	RegisterInviteOn               int    `json:"register_invite_on"`                  // 开启注册邀请机制
	SendWelcomeMessageOn           int    `json:"send_welcome_message_on"`             // 开启注册登录发送欢迎语
	InviteSystemAccountJoinGroupOn int    `json:"invite_system_account_join_group_on"` // 开启系统账号加入群聊
	RegisterUserMustCompleteInfoOn int    `json:"register_user_must_complete_info_on"` // 注册用户必须填写完整信息
	CanModifyApiUrl                int    `json:"can_modify_api_url"`                  // 允许修改api地址
	ThreadOn                       int    `json:"thread_on"`                           // 子区功能开关
	DestroyCoolingOffDays          int    `json:"destroy_cooling_off_days"`            // 注销冷静期天数（默认 7）
	OIDCAccountURL                 string `json:"oidc_account_url,omitempty"`          // OIDC 账户中心首页 URL（保留兼容老前端，新前端读 oidc_providers[].account_url）
	OIDCResetPasswordURL           string `json:"oidc_reset_password_url,omitempty"`   // OIDC 修改/重置密码 URL（保留兼容老前端）
	// OIDCProviders 单 provider 元数据数组（本期长度 ≤ 1）。让前端不再硬编码 provider id/name/authorize_path，
	// 接入新 IdP 时只改部署 env 即可。OIDC 关闭时整个字段被 omitempty 隐去。
	OIDCProviders []oidcProviderResp `json:"oidc_providers,omitempty"`

	// SystemBotUIDs 下发系统 Bot UID 列表（目前 botfather / u_10000 / fileHelper）。
	//
	// 背景 (YUJ-219-A / GH#1283，对应 analysis-report.md §4.2)：
	// 三端（Android / iOS / Web）原先各自硬编码系统 Bot 集合，跨端漂移：
	//   - 后端 pkg/space/query.go :: SystemBots = {botfather, u_10000, fileHelper}
	//   - Android ChatActivity.SYSTEM_BOTS = {"botfather"}    ← 漏 u_10000 / fileHelper
	//   - iOS 仅用 WKApp.config.botfatherUID                  ← 也只有 botfather
	// 结果：Android 端点开 u_10000 / fileHelper 时本地 filter 完全失效，
	// 跨 Space 历史消息全量暴露。
	//
	// 解法：后端作为单一真源通过 appconfig 下发 SystemBotUIDs；各端启动时
	// 消费此字段并替换硬编码常量，保持与后端 SystemBotList() 完全一致。
	// 未来系统 Bot 列表调整（加新 Bot / 改名）只需改后端，无需同步三端。
	SystemBotUIDs []string `json:"system_bot_uids"`

	// LocalLoginOff 控制前端是否隐藏"本地账号登录"卡片（用户名 / 手机号 / 邮箱
	// 三种本地登录方式的统一开关）。值来源于 system_setting login.local_off，
	// 默认 0；为 1 时前端只渲染 SSO/第三方登录入口。
	//
	// 与 app_config.version 解耦：即使客户端命中 version 短路分支，也必须能拿到
	// 最新值，否则 admin 切换开关后老客户端会被本地缓存住。和 SystemBotUIDs 同理。
	LocalLoginOff int `json:"local_login_off"`

	// DisableUserCreateSpace 控制客户端是否隐藏「创建空间」入口。
	// 来源 system_setting space.disable_user_create,回退到 env
	// DM_SPACE_DISABLE_USER_CREATE,默认 0(允许创建)。
	//
	// 与 app_config.version 解耦的原因同 LocalLoginOff：admin 在管理台 toggle
	// 后老客户端命中 version 短路分支仍必须看到最新值，否则被本地缓存住失去
	// 实时性。后端 POST /v1/space/create 也走同一个 getter 校验,客户端隐藏
	// 与服务端拒绝由单一真源驱动,不存在前后端漂移。
	DisableUserCreateSpace int `json:"disable_user_create_space"`
}

type oidcProviderResp struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	AuthorizePath    string `json:"authorize_path"`
	AccountURL       string `json:"account_url,omitempty"`
	ResetPasswordURL string `json:"reset_password_url,omitempty"`
}

type appVersionReq struct {
	AppVersion  string `json:"app_version"`  // 版本号
	OS          string `json:"os"`           // 平台 android｜ios
	IsForce     int    `json:"is_force"`     // 是否强制更新
	UpdateDesc  string `json:"update_desc"`  // 更新说明
	DownloadURL string `json:"download_url"` // 下载地址
	Signature   string `json:"signature"`    // 文件签名
}

type appVersionResp struct {
	AppVersion  string `json:"app_version"`  // 版本号
	OS          string `json:"os"`           // 平台 android｜ios
	IsForce     int    `json:"is_force"`     // 是否强制更新
	UpdateDesc  string `json:"update_desc"`  // 更新说明
	DownloadURL string `json:"download_url"` // 下载地址
	CreatedAt   string `json:"created_at"`   //更新时间
}

// Country Country
type Country struct {
	Code string `json:"code"`
	Icon string `json:"icon"`
	Name string `json:"name"`
}

var defaultColorsLight = [][]string{
	{"a6B0CDEB", "a69FB0EA", "a6BBEAD5", "a6B2E3DD"},
	{"a640CDDE", "a6AC86ED", "a6E984D8", "a6EFD359"},
	{"a6DBDDBB", "a66BA587", "a6D5D88D", "a688B884"},
	{"a6DAEACB", "a6A2B4FF", "a6ECCBFF", "a6B9E2FF"},
	{"a6B2B1EE", "a6D4A7C9", "a66C8CD4", "a64CA3D4"},
	{"a6DCEB92", "a68FE1D6", "a667A3F2", "a685D685"},
	{"a68ADBF2", "a6888DEC", "a6E39FEA", "a6679CED"},
	{"a6FFC3B2", "a6E2C0FF", "a6FFE7B2", "a6FDFF8C"},
	{"a697BEEB", "a6B1E9EA", "a6C6B1EF", "a6EFB7DC"},
	{"a6E4B2EA", "a68376C2", "a6EAB9D9", "a6B493E6"},
	{"a6D1A3E2", "a6EDD594", "a6E5A1D0", "a6ECD893"},
	{"a6EAA36E", "a6F0E486", "a6F29EBF", "a6E8C06E"},
	{"a67EC289", "a6E4D573", "a6AFD677", "a6F0C07A"},
}
var defaultColorsDark = [][]string{
	{"a6A4DBFF", "a6009FDD", "a6527BDD", "a673B6DD"},
	{"a6FEC496", "a6DD6CB9", "a6962FBF", "a64F5BD5"},
	{"a6E4B2EA", "a68376C2", "a6EAB9D9", "a6B493E6"},
	{"a6EAA36E", "a6F0E486", "a6F29EBF", "a6E8C06E"},
	{"a68ADBF2", "a6888DEC", "a6E39FEA", "a6679CED"},
	{"a6E4B2EA", "a68376C2", "a6EAB9D9", "a6B493E6"},
	{"a627FF03", "a6FC31FF", "a600FEFF", "a6FFFC00"},
	{"a6FEC496", "a6DD6CB9", "a6962FBF", "a64F5BD5"},
	{"a6EAA36E", "a6F0E486", "a6F29EBF", "a6E8C06E"},
	{"a6FAF4D2", "a6CEA668", "a6DDB56D", "a6BAA161"},
	{"a6A4DBFF", "a6009FDD", "a6527BDD", "a673B6DD"},
	{"a6E4B2EA", "a68376C2", "a6EAB9D9", "a6B493E6"},
	{"a6EAA36E", "a6F0E486", "a6F29EBF", "a6E8C06E"},
}

// Countrys Countrys
func Countrys() []*Country {

	return []*Country{
		{
			Code: "0086",
			Icon: "🇨🇳",
			Name: "中国",
		},
		{
			Code: "001",
			Icon: "🇺🇸",
			Name: "美国",
		},
		{
			Code: "00853",
			Icon: "🇲🇴",
			Name: "中国澳门",
		},
		{
			Code: "001",
			Icon: "🇨🇦",
			Name: "加拿大",
		},
		{
			Code: "007",
			Icon: "🇰🇿",
			Name: "哈萨克斯坦",
		},
		{
			Code: "00998",
			Icon: "🇺🇿",
			Name: "乌兹别克斯坦",
		},
		{
			Code: "00996",
			Icon: "🇰🇬",
			Name: "吉尔吉斯斯坦",
		},
		{
			Code: "0090",
			Icon: "🇹🇷",
			Name: "土耳其",
		},
		{
			Code: "0033",
			Icon: "🇫🇷",
			Name: "法国",
		},
		{
			Code: "0049",
			Icon: "🇩🇪",
			Name: "德国",
		},
		{
			Code: "0044",
			Icon: "🇬🇧",
			Name: "英国",
		},
		{
			Code: "0039",
			Icon: "🇮🇹",
			Name: "意大利",
		},
		{
			Code: "00886",
			Icon: "🇹🇼",
			Name: "中国台湾",
		},
		{
			Code: "0060",
			Icon: "🇲🇾",
			Name: "马来西亚",
		},
		{
			Code: "0062",
			Icon: "🇮🇩",
			Name: "印度尼西亚",
		},
		{
			Code: "0061",
			Icon: "🇦🇺",
			Name: "澳大利亚",
		},
		{
			Code: "0064",
			Icon: "🇳🇿",
			Name: "新西兰",
		},
		{
			Code: "0063",
			Icon: "🇵🇭",
			Name: "菲律宾",
		},
		{
			Code: "0065",
			Icon: "🇸🇬",
			Name: "新加坡",
		},
		{
			Code: "0066",
			Icon: "🇹🇭",
			Name: "泰国",
		},
		{
			Code: "00673",
			Icon: "🇧🇳",
			Name: "文莱",
		},
		{
			Code: "0081",
			Icon: "🇯🇵",
			Name: "日本",
		},
		{
			Code: "0082",
			Icon: "🇰🇷",
			Name: "韩国",
		},
		{
			Code: "0084",
			Icon: "🇻🇳",
			Name: "越南",
		},
		{
			Code: "00852",
			Icon: "🇭🇰",
			Name: "中国香港",
		},
		{
			Code: "00855",
			Icon: "🇰🇭",
			Name: "柬埔寨",
		},
		{
			Code: "00856",
			Icon: "🇱🇦",
			Name: "老挝",
		},
		{
			Code: "00880",
			Icon: "🇧🇩",
			Name: "孟加拉国",
		},
		{
			Code: "0091",
			Icon: "🇮🇳",
			Name: "印度",
		},
		{
			Code: "0094",
			Icon: "🇱🇰",
			Name: "斯里兰卡",
		},
		{
			Code: "0095",
			Icon: "🇲🇲",
			Name: "缅甸",
		},
		{
			Code: "00960",
			Icon: "🇲🇻",
			Name: "马尔代夫",
		},
		{
			Code: "00976",
			Icon: "🇲🇳",
			Name: "蒙古",
		},
		{
			Code: "00975",
			Icon: "🇧🇹",
			Name: "不丹",
		},
		{
			Code: "007",
			Icon: "🇷🇺",
			Name: "俄罗斯",
		},
		{
			Code: "0030",
			Icon: "🇬🇷",
			Name: "希腊",
		},
		{
			Code: "0031",
			Icon: "🇳🇱",
			Name: "荷兰",
		},
		{
			Code: "0034",
			Icon: "🇪🇸",
			Name: "西班牙",
		},
		{
			Code: "00351",
			Icon: "🇵🇹",
			Name: "葡萄牙",
		},
		{
			Code: "00353",
			Icon: "🇮🇪",
			Name: "爱尔兰",
		},
		{
			Code: "0041",
			Icon: "🇨🇭",
			Name: "瑞士",
		},
		{
			Code: "0045",
			Icon: "🇩🇰",
			Name: "丹麦",
		},
		{
			Code: "0046",
			Icon: "🇸🇪",
			Name: "瑞典",
		},
		{
			Code: "0047",
			Icon: "🇳🇴",
			Name: "挪威",
		},
		{
			Code: "0055",
			Icon: "🇧🇷",
			Name: "巴西",
		},
	}
}
