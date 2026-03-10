package openapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

type OpenAPI struct {
	ctx                      *config.Context
	appService               app.IService
	openapiAuthcodePrefix    string
	openapiAccessTokenPrefix string
	userService              user.IService
}

func New(ctx *config.Context) *OpenAPI {

	return &OpenAPI{
		ctx:                      ctx,
		appService:               app.NewService(ctx),
		openapiAuthcodePrefix:    "openapi:authcodePrefix:",
		openapiAccessTokenPrefix: "openapi:accessTokenPrefix:",
		userService:              user.NewService(ctx),
	}
}

const (
	maxParamLength = 128
)

var validParamRegexp = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

func validateParam(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maxParamLength {
		return fmt.Errorf("%s exceeds maximum length", name)
	}
	if !validParamRegexp.MatchString(value) {
		return fmt.Errorf("%s contains invalid characters", name)
	}
	return nil
}

// Route 路由配置
func (o *OpenAPI) Route(r *wkhttp.WKHttp) {
	// 不需要认证
	openapinoauth := r.Group("/v1")
	{
		// #################### openapi ####################
		openapinoauth.GET("/openapi/access_token", o.accessTokenGet) // 获取用户的授权access_token
		openapinoauth.GET("/openapi/userinfo", o.userinfoGet)        // 获取用户信息
	}
	// 需要用户认证
	openapi := r.Group("/v1", o.ctx.AuthMiddleware(r))
	{
		// #################### openapi ####################
		openapi.GET("/openapi/authcode", o.authcodeGet) // 获取用户的授权authcode
	}
}

func (o *OpenAPI) accessTokenGet(c *wkhttp.Context) {
	authcode := c.Query("authcode")
	appKey := c.Query("app_key")

	if err := validateParam("authcode", authcode); err != nil {
		c.ResponseError(err)
		return
	}
	if err := validateParam("app_key", appKey); err != nil {
		c.ResponseError(err)
		return
	}

	appID, uid, err := o.getOpenapiAuthcodeCache(authcode)
	if err != nil {
		c.ResponseError(err)
		return
	}
	appResp, err := o.appService.GetApp(appID)
	if err != nil {
		c.ResponseError(err)
		return
	}
	if appResp == nil {
		c.ResponseError(fmt.Errorf("app not found"))
		return
	}
	if appResp.Status != app.StatusEnable {
		c.ResponseError(fmt.Errorf("app is not enabled"))
		return
	}
	if appResp.AppKey != appKey {
		c.ResponseError(fmt.Errorf("app_key does not match"))
		return
	}
	accessToken := util.GenerUUID()

	second := 24 * 7 * 3600

	err = o.setOpenapiAccessToken(uid, appID, accessToken, time.Second*time.Duration(second))
	if err != nil {
		c.ResponseError(err)
		return
	}

	// Delete authcode after successful token exchange to prevent replay attacks
	_ = o.deleteOpenapiAuthcodeCache(authcode)

	c.JSON(http.StatusOK, gin.H{
		"access_token": accessToken,
		"expire":       second,
	})

}

func (o *OpenAPI) userinfoGet(c *wkhttp.Context) {
	accessToken := c.Query("access_token")

	if err := validateParam("access_token", accessToken); err != nil {
		c.ResponseError(err)
		return
	}

	appID, uid, err := o.getOpenapiAccessToken(accessToken)
	if err != nil {
		c.ResponseError(err)
		return
	}
	if appID == "" || uid == "" {
		c.ResponseError(fmt.Errorf("invalid or expired access_token"))
		return
	}
	user, err := o.userService.GetUser(uid)
	if err != nil {
		c.ResponseError(err)
		return
	}
	if user == nil {
		c.ResponseError(fmt.Errorf("user not found"))
		return
	}
	avatarURL := fmt.Sprintf("%s/%s", o.ctx.GetConfig().External.APIBaseURL, o.ctx.GetConfig().GetAvatarPath(user.UID))
	c.JSON(http.StatusOK, gin.H{
		"uid":    user.UID,
		"name":   user.Name,
		"avatar": avatarURL,
		"app_id": appID,
	})
}

func (o *OpenAPI) authcodeGet(c *wkhttp.Context) {
	uid := c.GetLoginUID()

	appID := c.Query("app_id")

	if err := validateParam("app_id", appID); err != nil {
		c.ResponseError(err)
		return
	}

	// Validate app exists and is enabled
	appResp, err := o.appService.GetApp(appID)
	if err != nil {
		c.ResponseError(errors.New("查询应用失败"))
		return
	}
	if appResp == nil {
		c.ResponseError(errors.New("应用不存在"))
		return
	}
	if appResp.Status != app.StatusEnable {
		c.ResponseError(errors.New("应用未启用"))
		return
	}

	authcode := util.GenerUUID()

	err = o.setOpenapiAuthcodeCache(uid, appID, authcode)
	if err != nil {
		c.ResponseError(err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"authcode": authcode,
	})

}

type tokenData struct {
	AppID string `json:"app_id"`
	UID   string `json:"uid"`
}

func (o *OpenAPI) setOpenapiAuthcodeCache(uid, appID, authcode string) error {
	data, _ := json.Marshal(tokenData{AppID: appID, UID: uid})
	return o.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", o.openapiAuthcodePrefix, authcode), string(data), time.Minute*5)
}

func (o *OpenAPI) deleteOpenapiAuthcodeCache(authcode string) error {
	return o.ctx.GetRedisConn().Del(fmt.Sprintf("%s%s", o.openapiAuthcodePrefix, authcode))
}

func (o *OpenAPI) getOpenapiAuthcodeCache(authcode string) (string, string, error) {
	dataStr, err := o.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", o.openapiAuthcodePrefix, authcode))
	if err != nil {
		return "", "", err
	}
	var data tokenData
	if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
		return "", "", fmt.Errorf("invalid authcode data: %w", err)
	}
	return data.AppID, data.UID, nil
}

func (o *OpenAPI) setOpenapiAccessToken(uid, appID, accessToken string, expire time.Duration) error {
	data, _ := json.Marshal(tokenData{AppID: appID, UID: uid})
	return o.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", o.openapiAccessTokenPrefix, accessToken), string(data), expire)
}

func (o *OpenAPI) getOpenapiAccessToken(accessToken string) (string, string, error) {
	dataStr, err := o.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", o.openapiAccessTokenPrefix, accessToken))
	if err != nil {
		return "", "", err
	}
	var data tokenData
	if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
		return "", "", fmt.Errorf("invalid access_token data: %w", err)
	}
	return data.AppID, data.UID, nil
}
