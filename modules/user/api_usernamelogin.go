package user

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkevent"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	event "github.com/Mininglamp-OSS/octo-server/modules/base/event"
	common "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// 通过用户名注册
func (u *User) usernameRegister(c *wkhttp.Context) {
	settings := common.EnsureSystemSettings(u.ctx)
	if settings.RegisterOff() {
		c.ResponseError(errors.New("注册通道暂不开放"))
		return
	}
	if !settings.RegisterUsernameOn() {
		c.ResponseError(errors.New("暂不支持用户名注册"))
		return
	}
	var req usernameRegisterReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if req.Username == "" {
		c.ResponseError(errors.New("用户名不能为空"))
		return
	}
	if strings.TrimSpace(req.Password) == "" {
		c.Response(errors.New("密码不能为空！"))
		return
	}
	if len(req.Password) < 6 {
		c.ResponseError(errors.New("密码不能少于6位"))
		return
	}
	if len(req.Username) < 8 || len(req.Username) > 22 {
		c.ResponseError(errors.New("用户名必须在8-22位"))
		return
	}
	userInfo, err := u.db.QueryByUsername(req.Username)
	if err != nil {
		u.Error("查询用户信息失败！", zap.String("username", req.Username))
		c.ResponseError(err)
		return
	}
	if userInfo != nil {
		c.ResponseError(errors.New("该用户名已存在"))
		return
	}
	if err := ValidateName(req.Name); err != nil {
		c.ResponseError(err)
		return
	}
	// 通过用户名注册
	u.registerWithUsername(req.Username, req.Name, req.Password, int(req.Flag), req.Device, c)
}

// 用户名登录
func (u *User) usernameLogin(c *wkhttp.Context) {
	var req loginReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if err := req.Check(); err != nil {
		c.ResponseError(err)
		return
	}
	if len(req.Username) < 8 || len(req.Username) > 22 {
		c.ResponseError(errors.New("用户名必须在8-22位"))
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
	if userInfo == nil {
		u.loginGuard.RecordFailureLogged(req.Username)
		// 统一错误消息，避免枚举账号
		c.ResponseError(errors.New("用户名或密码错误"))
		return
	}
	// 已注销账号拒绝登录；冷静期账号允许登录（响应中附带注销状态提示）
	if userInfo.IsDestroy == IsDestroyDone || userInfo.Status == 0 {
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
	// 自动将旧 MD5 密码迁移到 bcrypt
	if needsMigration {
		if newHash, hashErr := HashPassword(req.Password); hashErr == nil {
			_ = u.db.updatePassword(newHash, userInfo.UID)
		}
	}

	result, err := u.execLogin(userInfo, config.DeviceFlag(req.Flag), req.Device, loginSpanCtx)
	if err != nil {
		c.ResponseError(err)
		return
	}
	needUploadWeb3PublicKey := 0
	if userInfo.Web3PublicKey == "" {
		needUploadWeb3PublicKey = 1
	}
	c.Response(map[string]interface{}{
		"data":                      result,
		"need_upload_web3publickey": needUploadWeb3PublicKey,
	})
	publicIP := util.GetClientPublicIP(c.Request)
	go u.sentWelcomeMsg(publicIP, userInfo.UID)
}
func (u *User) registerWithUsername(username string, name string, password string, flag int, device *deviceReq, c *wkhttp.Context) {
	registerSpan := u.ctx.Tracer().StartSpan(
		"user.register",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	defer registerSpan.Finish()
	registerSpanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), registerSpan)

	registerSpan.SetTag("username", username)

	uid := util.GenerUUID()
	var model = &createUserModel{
		UID:      uid,
		Sex:      1,
		Name:     name,
		Zone:     "",
		Phone:    "",
		Username: username,
		Password: password,
		Flag:     flag,
		Device:   device,
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
	publicIP := util.GetClientPublicIP(c.Request)
	result, err := u.createUserWithRespAndTx(registerSpanCtx, model, publicIP, nil, tx, func() error {
		err := tx.Commit()
		if err != nil {
			tx.Rollback()
			u.Error("数据库事务提交失败", zap.Error(err))
			c.ResponseError(errors.New("数据库事务提交失败"))
			return nil
		}
		return nil
	})
	if err != nil {
		tx.Rollback()
		c.ResponseError(errors.New("注册失败！"))
		return
	}
	c.Response(map[string]interface{}{
		"data":                      result,
		"need_upload_web3publickey": 1,
	})
}

// 通过web3公钥重置登录密码
func (u *User) resetPwdWithWeb3PublicKey(c *wkhttp.Context) {
	type reqVO struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		VerifyText string `json:"verify_text"` // 明文
		SignText   string `json:"sign_text"`   // 签名后字符串
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if req.Username == "" {
		c.ResponseError(errors.New("用户名不能为空"))
		return
	}
	if req.Password == "" {
		c.ResponseError(errors.New("密码不能为空"))
		return
	}
	if req.VerifyText == "" {
		c.ResponseError(errors.New("校验字符不能为空"))
		return
	}
	if req.SignText == "" {
		c.ResponseError(errors.New("签名字符不能为空"))
		return
	}
	user, err := u.db.QueryByUsername(req.Username)
	if err != nil {
		u.Error("查询用户信息错误", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if user == nil {
		c.ResponseError(errors.New("该用户不存在"))
		return
	}
	if user.Web3PublicKey == "" {
		c.ResponseError(errors.New("该用户未上传公钥"))
		return
	}
	// 判断签名明文是否存在
	cacheKey := fmt.Sprintf("web3_verify:%s_%s", user.UID, Web3VerifyPassword)
	verifyText, err := u.ctx.GetRedisConn().GetString(cacheKey)
	if err != nil {
		u.Error("获取签名信息错误", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if verifyText == "" || req.VerifyText != verifyText {
		c.ResponseError(errors.New("签名信息不存在"))
		return
	}

	verify, err := u.verifySignature(user.Web3PublicKey, req.VerifyText, req.SignText)
	if err != nil {
		c.ResponseError(errors.New("校验签名错误"))
		return
	}
	if !verify {
		c.ResponseError(errors.New("签名错误"))
		return
	}

	newHash, err := HashPassword(req.Password)
	if err != nil {
		u.Error("密码哈希失败", zap.Error(err))
		c.ResponseError(errors.New("密码处理失败"))
		return
	}
	updateMap := map[string]interface{}{}
	updateMap["password"] = newHash
	err = u.db.updateUser(updateMap, user.UID)
	if err != nil {
		u.Error("修改用户密码错误", zap.Error(err))
		c.ResponseError(err)
		return
	}
	err = u.ctx.GetRedisConn().Del(cacheKey)
	if err != nil {
		u.Error("清除缓存错误", zap.Error(err))
	}
	c.ResponseOK()
}

// 校验签名
func (u *User) verifySignature(publicKey, verifyText, signText string) (bool, error) {
	orgpublicKeyBytes, err := hex.DecodeString(publicKey)
	if err != nil {
		u.Error("解码公钥错误", zap.Error(err))
		return false, err
	}
	publicKeyECDSA, err := crypto.DecompressPubkey(orgpublicKeyBytes)
	if err != nil {
		u.Error("解压公钥错误", zap.Error(err))
		return false, err
	}
	publicKeyBytes := crypto.FromECDSAPub(publicKeyECDSA)
	signData, err := hex.DecodeString(signText)
	if err != nil {
		u.Error("解码签名文件错误", zap.Error(err))
		return false, err
	}
	// ECDSA 签名至少 65 字节 (32 + 32 + 1 recovery id)
	// VerifySignature 需要 64 字节 (不含 recovery id)
	if len(signData) < 65 {
		return false, errors.New("签名数据长度不足")
	}
	prefix := "\x19Ethereum Signed Message:\n" + fmt.Sprint(len(verifyText)) + verifyText
	hash := crypto.Keccak256Hash([]byte(prefix))

	verifyed := crypto.VerifySignature(publicKeyBytes, hash.Bytes(), signData[:len(signData)-1])
	return verifyed, nil
}

// 上传web3Key
func (u *User) uploadWeb3PublicKey(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	type reqVO struct {
		Web3PublicKey string `json:"web3_public_key"`
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}

	if req.Web3PublicKey == "" {
		c.ResponseError(errors.New("公钥不能为空"))
		return
	}
	userInfo, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询用户信息失败！", zap.String("uid", loginUID))
		c.ResponseError(err)
		return
	}
	if userInfo == nil || userInfo.Status == 0 || userInfo.IsDestroy == IsDestroyDone {
		c.ResponseError(errors.New("该用户不存在或被封禁"))
		return
	}
	if userInfo.Web3PublicKey != "" {
		c.ResponseError(errors.New("该用户已上传过公钥信息"))
		return
	}

	updateMap := map[string]interface{}{}
	updateMap["web3_public_key"] = req.Web3PublicKey
	err = u.db.updateUser(updateMap, loginUID)
	if err != nil {
		u.Error("修改用户公钥错误", zap.Error(err))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// 验签
func (u *User) web3verifySignature(c *wkhttp.Context) {
	type reqVO struct {
		VerifyText string `json:"verify_text"`
		SignText   string `json:"sign_text"`
		Type       string `json:"type"` // password | login
		Username   string `json:"username"`
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if req.Username == "" {
		c.ResponseError(errors.New("用户名不能为空"))
		return
	}
	if req.VerifyText == "" {
		c.ResponseError(errors.New("校验字符不能为空"))
		return
	}
	if req.SignText == "" {
		c.ResponseError(errors.New("签名字符不能为空"))
		return
	}
	if req.Type == "" || (req.Type != Web3VerifyLogin && req.Type != Web3VerifyPassword) {
		c.ResponseError(errors.New("验证类型不匹配"))
		return
	}

	user, err := u.db.QueryByUsername(req.Username)
	if err != nil {
		u.Error("查询用户信息错误", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if user == nil {
		c.ResponseError(errors.New("该用户不存在"))
		return
	}
	if user.Web3PublicKey == "" {
		c.ResponseError(errors.New("该用户未上传公钥"))
		return
	}
	// 判断签名明文是否存在
	cacheKey := fmt.Sprintf("web3_verify:%s_%s", user.UID, req.Type)
	verifyText, err := u.ctx.GetRedisConn().GetString(cacheKey)
	if err != nil {
		u.Error("获取签名信息错误", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if verifyText == "" || req.VerifyText != verifyText {
		c.ResponseError(errors.New("签名信息不存在"))
		return
	}

	verify, err := u.verifySignature(user.Web3PublicKey, req.VerifyText, req.SignText)
	if err != nil {
		c.ResponseError(errors.New("校验签名错误"))
		return
	}
	if !verify {
		c.ResponseError(errors.New("签名错误"))
		return
	}
	err = u.ctx.GetRedisConn().Del(cacheKey)
	if err != nil {
		u.Error("清除缓存错误", zap.Error(err))
	}
	c.ResponseOK()
}

// 获取验证字符串
func (u *User) getVerifyText(c *wkhttp.Context) {
	username := c.Query("username")
	verifyType := c.Query("type")
	if username == "" {
		c.ResponseError(errors.New("用户名不能为空"))
		return
	}
	if verifyType == "" || (verifyType != Web3VerifyLogin && verifyType != Web3VerifyPassword) {
		c.ResponseError(errors.New("验证类型不匹配"))
		return
	}
	user, err := u.db.QueryByUsername(username)
	if err != nil {
		u.Error("查询用户信息错误", zap.Error(err))
		c.ResponseError(err)
		return
	}
	if user == nil || user.IsDestroy == IsDestroyDone || user.Status == 0 {
		c.ResponseError(errors.New("该用户不存在或被禁用"))
		return
	}
	if user.Web3PublicKey == "" {
		c.ResponseError(errors.New("该用户尚未上传公钥"))
		return
	}
	// 使用足够长的随机字符串，不包含可预测的时间戳
	// 32 字节随机字符串提供足够的熵防止暴力破解
	verifyText := util.GetRandomString(32)
	cacheKey := fmt.Sprintf("web3_verify:%s_%s", user.UID, verifyType)
	err = u.ctx.GetRedisConn().SetAndExpire(cacheKey, verifyText, time.Minute*5)
	if err != nil {
		u.Error("缓存校验信息错误", zap.Error(err))
		c.ResponseError(err)
		return
	}
	c.Response(map[string]interface{}{
		"verify_text": verifyText,
	})

}

// 修改登录密码
func (u *User) updatePwd(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	type reqVO struct {
		Password    string `json:"password"`
		NewPassword string `json:"new_password"`
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	if req.Password == "" || req.NewPassword == "" {
		c.ResponseError(errors.New("密码不能为空"))
		return
	}
	if req.Password == req.NewPassword {
		c.ResponseError(errors.New("新密码不能和旧密码相同"))
		return
	}
	userInfo, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询用户资料错误", zap.Error(err))
		c.ResponseError(errors.New("查询用户资料错误"))
		return
	}
	if userInfo == nil {
		c.ResponseError(errors.New("该用户不存在"))
		return
	}
	matched, _ := CheckPassword(req.Password, userInfo.Password)
	if !matched {
		c.ResponseError(errors.New("旧密码错误"))
		return
	}
	newHash, err := HashPassword(req.NewPassword)
	if err != nil {
		u.Error("密码哈希失败", zap.Error(err))
		c.ResponseError(errors.New("密码处理失败"))
		return
	}
	err = u.db.UpdateUsersWithField("password", newHash, userInfo.UID)
	if err != nil {
		u.Error("修改登录密码错误", zap.Error(err))
		c.ResponseError(errors.New("修改登录密码错误"))
		return
	}
	c.ResponseOK()
}

type usernameRegisterReq struct {
	Name     string     `json:"name"`     // 昵称
	Username string     `json:"username"` // 用户名
	Password string     `json:"password"`
	Flag     uint8      `json:"flag"`   // 注册设备的标记 0.APP 1.PC
	Device   *deviceReq `json:"device"` //注册用户设备信息
}

// sendBotWelcomeMessages is now a no-op.
// BotFather welcome is handled by botfather module's event handlers
// (handleUserRegisterEvent / handleSpaceMemberJoinEvent).
func (u *User) sendBotWelcomeMessages(uid string) {
}

// fireSpaceMemberJoinEvent 触发 SpaceMemberJoin 事件
func (u *User) fireSpaceMemberJoinEvent(uid string, spaceID string) {
	tx, err := u.ctx.DB().Begin()
	if err != nil {
		u.Error("开启SpaceMemberJoin事件事务失败", zap.Error(err))
		return
	}
	eventID, err := u.ctx.EventBegin(&wkevent.Data{
		Event: event.SpaceMemberJoin,
		Type:  wkevent.Message,
		Data: map[string]interface{}{
			"uid":      uid,
			"space_id": spaceID,
		},
	}, tx)
	if err != nil {
		tx.Rollback()
		u.Error("开启SpaceMemberJoin事件失败", zap.Error(err), zap.String("uid", uid), zap.String("spaceID", spaceID))
		return
	}
	if err = tx.Commit(); err != nil {
		u.Error("提交SpaceMemberJoin事件事务失败", zap.Error(err))
		return
	}
	u.ctx.EventCommit(eventID)
}
