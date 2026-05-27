package qrcode

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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
	raw, err := q.ctx.Cache().Get(q.ctx.GetConfig().Cache.TokenCachePrefix + token)
	if err != nil {
		q.Error("获取登录信息失败！", zap.Error(err))
		c.ResponseError(errors.New("获取登录信息失败！"))
		return
	}
	if strings.TrimSpace(raw) == "" {
		c.String(http.StatusOK, fmt.Sprintf("请下载“%s”APP扫码！", q.ctx.GetConfig().AppName))
		return
	}
	info, decodeErr := auth.Decode(raw)
	if decodeErr != nil {
		c.ResponseError(errors.New("登录信息格式错误！"))
		return
	}
	loginUID := info.UID
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
		c.ResponseError(err)
		return
	}
	c.JSON(http.StatusOK, result)

}

// 处理扫描登录
func (q *QRCode) handleScanLogin(loginUID string, uuid string, qrCodeModel common.QRCodeModel) (interface{}, error) {
	authCode := util.GenerUUID()
	err := q.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), util.ToJson(map[string]interface{}{
		"scaner": loginUID,
		"type":   common.AuthCodeTypeScanLogin,
		"uuid":   uuid,
	}), time.Minute*10)
	if err != nil {
		q.Error("生成扫码登录授权码失败", zap.Error(err))
		return nil, errors.New("生成登录授权码失败，请稍后重试")
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
		q.Error("设置扫描登录二维码信息失败", zap.Error(err))
		return nil, errors.New("扫码登录失败，请稍后重试")
	}
	user.SendQRCodeInfo(uuid, qrcodeInfo)
	return NewHandleResult(ForwardNative, HandlerTypeLoginConfirm, map[string]interface{}{
		"auth_code": authCode,
		"pub_key":   pubkey,
	}), nil
}

// 处理扫码入群
func (q *QRCode) handleJoinGroup(loginUID string, qrCodeModel common.QRCodeModel) (interface{}, error) {
	// GH #1319 / Direction A：零 Space 用户禁止通过扫码入群。
	// 与 modules/group/api.go::groupScanJoin / groupInviteAuthorize 的预检语义对齐，
	// 避免三端绕过「注册后必先加入 Space」产品约束。三端收到
	// HandlerTypeGroup + status="need_space" 后应拉起 SpaceGate/JoinSpaceGuide，
	// Gate 完成后可以直接用同一 qrcode UUID 重新扫一次恢复入群流程。
	if spacemod.GetUserDefaultSpaceID(q.ctx, loginUID) == "" {
		groupNoForHint, _ := qrCodeModel.Data["group_no"].(string)
		return NewHandleResult(ForwardNative, HandlerTypeGroup, map[string]interface{}{
			"status":   "need_space",
			"msg":      "请先加入一个 Space 后再入群",
			"group_no": groupNoForHint,
		}), nil
	}

	groupNo, ok := qrCodeModel.Data["group_no"].(string)
	if !ok {
		return nil, errors.New("invalid QR code data: missing or invalid group_no")
	}
	generator, ok := qrCodeModel.Data["generator"].(string)
	if !ok {
		return nil, errors.New("invalid QR code data: missing or invalid generator")
	}

	// 查询群信息用于预览
	groupModel, err := q.groupDB.QueryWithGroupNo(groupNo)
	if err != nil {
		q.Error("查询群信息失败", zap.Error(err))
		return nil, errors.New("获取群信息失败，请稍后重试")
	}
	if groupModel == nil {
		return nil, errors.New("群不存在")
	}

	// 扫码预检仅拦截「群禁止外部成员且扫码者非 Space 成员」的场景。
	// 外部群（is_external_group=1）和 allow_external=1（默认）场景下，预检放行，
	// 真正的入群鉴权（外部成员识别 / allow_external / invite 审批等）由 groupScanJoin 完成。
	// 用 spacepkg.CheckMembership 保持和 groupScanJoin、H5 authorize 预检的语义一致
	// （同时校验 space.status=1：Space 被禁用时一律按非成员处理）。
	if groupModel.SpaceID != "" && groupModel.AllowExternal == 0 {
		isMember, err := spacepkg.CheckMembership(q.ctx.DB(), groupModel.SpaceID, loginUID)
		if err != nil {
			q.Error("查询空间成员失败", zap.Error(err))
			return nil, errors.New("校验空间成员失败，请稍后重试")
		}
		if !isMember {
			return nil, errors.New("该群仅允许本空间成员加入")
		}
	}

	memberCount, err := q.groupDB.QueryMemberCount(groupNo)
	if err != nil {
		q.Error("查询群成员数失败", zap.Error(err))
		return nil, errors.New("获取群信息失败，请稍后重试")
	}

	exist, err := q.groupDB.ExistMember(loginUID, groupNo)
	if err != nil {
		q.Error("查询群成员失败", zap.Error(err))
		return nil, errors.New("获取群信息失败，请稍后重试")
	}
	if exist {
		return NewHandleResult(ForwardNative, HandlerTypeGroup, map[string]interface{}{
			"group_no":     groupNo,
			"name":         groupModel.Name,
			"avatar":       fmt.Sprintf("groups/%s/avatar", groupNo),
			"member_count": memberCount,
			"is_member":    true,
		}), nil
	}

	authCode := util.GenerUUID()
	err = q.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), util.ToJson(map[string]interface{}{
		"group_no":  groupNo,
		"generator": generator,
		"scaner":    loginUID,
		"type":      common.AuthCodeTypeJoinGroup,
	}), time.Minute*30)
	if err != nil {
		q.Error("生成入群授权码失败", zap.Error(err))
		return nil, errors.New("生成入群授权码失败，请稍后重试")
	}
	return NewHandleResult(ForwardNative, HandlerTypeGroup, map[string]interface{}{
		"group_no":     groupNo,
		"auth_code":    authCode,
		"name":         groupModel.Name,
		"avatar":       fmt.Sprintf("groups/%s/avatar", groupNo),
		"member_count": memberCount,
	}), nil
}
