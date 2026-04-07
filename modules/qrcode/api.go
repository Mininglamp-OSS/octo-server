package qrcode

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// HandleResult 二维码处理结果
type HandleResult struct {
	Forward Forward                `json:"forward"` // 跳转方式
	Type    HandlerType            `json:"type"`    // 数据类型
	Data    map[string]interface{} `json:"data"`    // 数据
}

// NewHandleResult NewHandleResult
func NewHandleResult(forward Forward, typ HandlerType, data map[string]interface{}) *HandleResult {
	return &HandleResult{
		Forward: forward,
		Type:    typ,
		Data:    data,
	}
}

// QRCode 二维码
type QRCode struct {
	ctx *config.Context
	log.Log
	groupDB     *group.DB
	userService user.IService
}

// New New
func New(ctx *config.Context) *QRCode {
	return &QRCode{
		ctx:         ctx,
		Log:         log.NewTLog("QRCode"),
		groupDB:     group.NewDB(ctx),
		userService: user.NewService(ctx),
	}
}

// Route 路由配置
func (q *QRCode) Route(r *wkhttp.WKHttp) {
	// 获取二维码内的信息
	r.GET(q.ctx.GetConfig().QRCodeInfoURL, q.ctx.AuthMiddleware(r), q.handleQRCodeInfo)
}

// 处理二维码信息
func (q *QRCode) handleQRCodeInfo(c *wkhttp.Context) {
	token := c.GetHeader("token")
	if token == "" {
		c.ResponseError(errors.New("token不能为空！"))
		return
	}
	uidAndName, err := q.ctx.Cache().Get(q.ctx.GetConfig().Cache.TokenCachePrefix + token)
	if err != nil {
		q.Error("获取登录信息失败！", zap.Error(err))
		c.ResponseError(errors.New("获取登录信息失败！"))
		return
	}
	if strings.TrimSpace(uidAndName) == "" {
		c.String(http.StatusOK, fmt.Sprintf("请下载“%s”APP扫码！", q.ctx.GetConfig().AppName))
		return
	}
	uidAndNames := strings.Split(uidAndName, "@")
	if len(uidAndNames) == 0 {
		c.ResponseError(errors.New("登录信息格式错误！"))
		return
	}
	loginUID := uidAndNames[0]
	code := c.Param("code")

	if strings.HasPrefix(code, "user_") { // 用户资料二维码 格式： user_xxxx
		targetUID := code[len("user_"):]
		if targetUID == "" {
			c.ResponseError(errors.New("用户UID不能为空"))
			return
		}
		// Validate target user exists
		targetUser, err := q.userService.GetUser(targetUID)
		if err != nil {
			q.Error("查询目标用户失败", zap.String("targetUID", targetUID), zap.Error(err))
			c.ResponseError(errors.New("查询用户信息失败"))
			return
		}
		if targetUser == nil {
			c.ResponseError(errors.New("用户不存在"))
			return
		}
		c.Response(NewHandleResult(ForwardNative, HandlerTypeUserInfo, map[string]interface{}{
			"uid": targetUID,
		}))
		return
	}
	if strings.HasPrefix(code, "vercode_") {
		qrvercode := code[len("vercode_"):]
		userResp, err := q.userService.GetUserWithQRVercode(qrvercode)
		if err != nil {
			c.ResponseErrorf("通过qrvercode获取用户信息失败！", err)
			return
		}
		if userResp == nil {
			c.ResponseError(errors.New("用户不存在！"))
			return
		}
		c.Response(NewHandleResult(ForwardNative, HandlerTypeUserInfo, map[string]interface{}{
			"uid":     userResp.UID,
			"vercode": qrvercode,
		}))
		return
	}

	qrcodeContent, err := q.ctx.GetRedisConn().GetString(fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code))
	if err != nil {
		q.Error("获取二维码信息失败！", zap.Error(err))
		c.ResponseError(errors.New("获取二维码信息失败！"))
		return
	}
	if qrcodeContent == "" {
		q.Error("二维码或已过期！", zap.String("code", code))
		c.ResponseError(errors.New("二维码或已过期！"))
		return
	}
	var qrCodeModel common.QRCodeModel
	err = util.ReadJsonByByte([]byte(qrcodeContent), &qrCodeModel)
	if err != nil {
		q.Error("解码二维码信息失败！", zap.Error(err))
		c.ResponseError(errors.New("解码二维码信息失败！"))
		return
	}
	var result interface{}
	switch qrCodeModel.Type {
	case common.QRCodeTypeGroup: // 扫描入群
		result, err = q.handleJoinGroup(loginUID, qrCodeModel)
	case common.QRCodeTypeScanLogin: // 扫描登录
		result, err = q.handleScanLogin(loginUID, code, qrCodeModel)
	default:
		err = errors.New("不支持的扫码类型！")
	}
	if err != nil {
		q.Error("处理请求失败！", zap.Error(err))
		c.ResponseError(errors.New("处理请求失败！"))
		return
	}
	c.JSON(http.StatusOK, result)

}

// 处理扫描登录
func (q *QRCode) handleScanLogin(loginUID string, uuid string, qrCodeModel common.QRCodeModel) (interface{}, error) {
	authCode := util.GenerUUID()
	err := q.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), util.ToJson(map[string]interface{}{
		"scaner": loginUID, // 二维码扫码者即是登录者
		"type":   common.AuthCodeTypeScanLogin,
		"uuid":   uuid,
	}), time.Minute*10)
	if err != nil {
		return nil, err
	}
	var pubkey string
	if qrCodeModel.Data != nil && qrCodeModel.Data["pub_key"] != nil {
		pubkey, _ = qrCodeModel.Data["pub_key"].(string)
	}
	qrcodeInfo := common.NewQRCodeModel(common.QRCodeTypeScanLogin, map[string]interface{}{
		"app_id": "wukongchat",
		"status": common.ScanLoginStatusScanned,
		"uid":    loginUID,
	})
	err = q.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.QRCodeCachePrefix, uuid), util.ToJson(qrcodeInfo), time.Minute*5)
	if err != nil {
		q.Error("设置扫描登录二维码信息失败！", zap.Error(err))
		return nil, err
	}
	user.SendQRCodeInfo(uuid, qrcodeInfo)
	return NewHandleResult(ForwardNative, HandlerTypeLoginConfirm, map[string]interface{}{
		"auth_code": authCode,
		"pub_key":   pubkey,
	}), nil
}

// 处理扫码入群
func (q *QRCode) handleJoinGroup(loginUID string, qrCodeModel common.QRCodeModel) (interface{}, error) {
	groupNo, ok := qrCodeModel.Data["group_no"].(string)
	if !ok {
		return nil, errors.New("invalid QR code data: missing or invalid group_no")
	}
	generator, ok := qrCodeModel.Data["generator"].(string)
	if !ok {
		return nil, errors.New("invalid QR code data: missing or invalid generator")
	}

	exist, err := q.groupDB.ExistMember(loginUID, groupNo) // 已在群内
	if err != nil {
		return nil, err
	}
	if exist {
		return NewHandleResult(ForwardNative, HandlerTypeGroup, map[string]interface{}{
			"group_no": groupNo,
		}), nil
	}
	// 查询群信息用于预览
	groupModel, err := q.groupDB.QueryWithGroupNo(groupNo)
	if err != nil {
		return nil, err
	}
	if groupModel == nil {
		return nil, errors.New("群不存在")
	}
	memberCount, err := q.groupDB.QueryMemberCount(groupNo)
	if err != nil {
		return nil, err
	}

	authCode := util.GenerUUID()
	err = q.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), util.ToJson(map[string]interface{}{
		"group_no":  groupNo,   // 群编号
		"generator": generator, // 二维码生成者
		"scaner":    loginUID,  // 二维码扫码者
		"type":      common.AuthCodeTypeJoinGroup,
	}), time.Minute*30)
	if err != nil {
		return nil, err
	}
	return NewHandleResult(ForwardNative, HandlerTypeGroup, map[string]interface{}{
		"group_no":     groupNo,
		"auth_code":    authCode,
		"name":         groupModel.Name,
		"avatar":       fmt.Sprintf("groups/%s/avatar", groupNo),
		"member_count": memberCount,
	}), nil
}
