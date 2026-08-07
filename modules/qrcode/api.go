package qrcode

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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
		respondQRCodeTokenRequired(c)
		return
	}
	raw, err := q.ctx.Cache().Get(q.ctx.GetConfig().Cache.TokenCachePrefix + token)
	if err != nil {
		q.Error("获取登录信息失败！", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrQRCodeQueryFailed, nil, nil)
		return
	}
	if strings.TrimSpace(raw) == "" {
		c.String(http.StatusOK, fmt.Sprintf("请下载“%s”APP扫码！", q.ctx.GetConfig().AppName))
		return
	}
	info, decodeErr := auth.Decode(raw)
	if decodeErr != nil {
		httperr.ResponseErrorL(c, errcode.ErrQRCodeTokenInvalid, nil, nil)
		return
	}
	loginUID := info.UID
	code := c.Param("code")

	if strings.HasPrefix(code, "user_") { // 用户资料二维码 格式： user_xxxx
		targetUID := code[len("user_"):]
		if targetUID == "" {
			respondQRCodeRequestInvalid(c, "uid")
			return
		}
		// Validate target user exists
		targetUser, err := q.userService.GetUser(targetUID)
		if err != nil {
			q.Error("查询目标用户失败", zap.String("targetUID", targetUID), zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrQRCodeQueryFailed, nil, nil)
			return
		}
		if targetUser == nil {
			httperr.ResponseErrorL(c, errcode.ErrQRCodeUserNotFound, nil, nil)
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
			q.Error("通过qrvercode获取用户信息失败", zap.Error(err))
			httperr.ResponseErrorL(c, errcode.ErrQRCodeQueryFailed, nil, nil)
			return
		}
		if userResp == nil {
			httperr.ResponseErrorL(c, errcode.ErrQRCodeUserNotFound, nil, nil)
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
		httperr.ResponseErrorL(c, errcode.ErrQRCodeQueryFailed, nil, nil)
		return
	}
	if qrcodeContent == "" {
		q.Error("二维码或已过期！", zap.String("code", code))
		httperr.ResponseErrorL(c, errcode.ErrQRCodeNotFound, nil, nil)
		return
	}
	var qrCodeModel common.QRCodeModel
	err = util.ReadJsonByByte([]byte(qrcodeContent), &qrCodeModel)
	if err != nil {
		q.Error("解码二维码信息失败！", zap.Error(err))
		respondQRCodeRequestInvalid(c, "code")
		return
	}
	var result interface{}
	switch qrCodeModel.Type {
	case common.QRCodeTypeGroup: // 扫描入群
		result, err = q.handleJoinGroup(loginUID, qrCodeModel)
	case common.QRCodeTypeScanLogin: // 扫描登录
		result, err = q.handleScanLogin(loginUID, code, qrCodeModel)
	default:
		err = errQRCodeUnsupportedType
	}
	if err != nil {
		q.Error("处理请求失败！", zap.Error(err))
		respondQRCodeHandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)

}

// scanLoginAuthCodeTTL 是扫码登录授权码的存活时间。
//
// authCode 可被 POST /v1/user/login_authcode/{code} 直接兑换成 scaner 的完整 token，
// 且该接口不校验兑换者身份，所以这里的 TTL 就是「凭据一旦外泄的可利用窗口」。原值
// 10 分钟远超实际所需 —— 从扫码到浏览器完成兑换通常在数秒内。收敛到 5 分钟，既压窄
// 窗口，又给弱网 / 用户在确认页停顿留足余量。
const scanLoginAuthCodeTTL = 5 * time.Minute

// 处理扫描登录
func (q *QRCode) handleScanLogin(loginUID string, uuid string, qrCodeModel common.QRCodeModel) (interface{}, error) {
	authCode := util.GenerUUID()
	err := q.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", common.AuthCodeCachePrefix, authCode), util.ToJson(map[string]interface{}{
		"scaner": loginUID,
		"type":   common.AuthCodeTypeScanLogin,
		"uuid":   uuid,
	}), scanLoginAuthCodeTTL)
	if err != nil {
		q.Error("生成扫码登录授权码失败", zap.Error(err))
		return nil, fmt.Errorf("%w: scan login auth code", errQRCodeInternalStoreFailed)
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
		return nil, fmt.Errorf("%w: scan login state", errQRCodeInternalStoreFailed)
	}
	user.SendQRCodeInfo(uuid, qrcodeInfo)

	// 把「请求登录的那一端是谁」一并回给手机，供确认弹窗展示。
	//
	// 扫码登录唯一挡得住 QRLJacking（攻击者自己申请二维码、贴到钓鱼页诱导他人扫码）
	// 的位置就是这里 —— 服务端无法区分「受害者扫了攻击者的码」和「本人扫了自己的
	// 码」，只有确认的人能。前提是他看得到请求方的设备与 IP。读取失败不阻断扫码，
	// 退化为不展示。**移动端渲染前，本字段不产生任何防护效果。**
	var originResp map[string]interface{}
	if origin, oErr := user.LoadScanLoginOrigin(q.ctx.GetRedisConn(), uuid); oErr != nil {
		q.Warn("读取扫码登录来源失败", zap.String("uuid", uuid), zap.Error(oErr))
	} else if origin != nil {
		originResp = map[string]interface{}{
			"ip":           origin.IP,
			"device_name":  origin.DeviceName,
			"device_model": origin.DeviceModel,
			"user_agent":   origin.UserAgent,
		}
	}

	return NewHandleResult(ForwardNative, HandlerTypeLoginConfirm, map[string]interface{}{
		"auth_code": authCode,
		"pub_key":   pubkey,
		"origin":    originResp,
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
		return nil, fmt.Errorf("%w: group_no", errQRCodeGroupDataInvalid)
	}
	generator, ok := qrCodeModel.Data["generator"].(string)
	if !ok {
		return nil, fmt.Errorf("%w: generator", errQRCodeGroupDataInvalid)
	}

	// 查询群信息用于预览
	groupModel, err := q.groupDB.QueryWithGroupNo(groupNo)
	if err != nil {
		q.Error("查询群信息失败", zap.Error(err))
		return nil, fmt.Errorf("%w: group", errQRCodeInternalQueryFailed)
	}
	if groupModel == nil {
		return nil, errQRCodeGroupNotFound
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
			return nil, fmt.Errorf("%w: space membership", errQRCodeInternalQueryFailed)
		}
		if !isMember {
			return nil, errQRCodeGroupSpaceForbidden
		}
	}

	memberCount, err := q.groupDB.QueryMemberCount(groupNo)
	if err != nil {
		q.Error("查询群成员数失败", zap.Error(err))
		return nil, fmt.Errorf("%w: member count", errQRCodeInternalQueryFailed)
	}

	exist, err := q.groupDB.ExistMember(loginUID, groupNo)
	if err != nil {
		q.Error("查询群成员失败", zap.Error(err))
		return nil, fmt.Errorf("%w: membership", errQRCodeInternalQueryFailed)
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
		return nil, fmt.Errorf("%w: join group auth code", errQRCodeInternalStoreFailed)
	}
	return NewHandleResult(ForwardNative, HandlerTypeGroup, map[string]interface{}{
		"group_no":     groupNo,
		"auth_code":    authCode,
		"name":         groupModel.Name,
		"avatar":       fmt.Sprintf("groups/%s/avatar", groupNo),
		"member_count": memberCount,
	}), nil
}
