package user

import (
	"context"
	"runtime/debug"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	common "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// emailSendCode 发送邮箱验证码
func (u *User) emailSendCode(c *wkhttp.Context) {
	type reqVO struct {
		Email    string `json:"email"`
		CodeType int    `json:"code_type"` // 0:注册 1:登录 2:忘记密码
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" {
		c.ResponseError(errors.New("邮箱不能为空"))
		return
	}
	if !isValidEmail(req.Email) {
		c.ResponseError(errors.New("邮箱格式不正确"))
		return
	}
	settings := common.EnsureSystemSettings(u.ctx)
	codeType := commonapi.CodeType(req.CodeType)
	if codeType == commonapi.CodeTypeRegister && settings.RegisterOff() {
		c.ResponseError(errors.New("注册通道暂不开放"))
		return
	}
	if !settings.RegisterEmailOn() {
		switch codeType {
		case commonapi.CodeTypeRegister:
			c.ResponseError(errors.New("暂不支持邮箱注册"))
			return
		case commonapi.CodeTypeEmailLogin:
			c.ResponseError(errors.New("暂不支持邮箱登录"))
			return
		default:
			// RegisterEmailOn controls email registration/login only. Password
			// recovery codes remain available for existing accounts.
		}
	}

	emailService := commonapi.NewEmailService(u.ctx, common.EnsureSystemSettings(u.ctx))
	if err := emailService.SendVerifyCode(context.Background(), req.Email, commonapi.CodeType(req.CodeType)); err != nil {
		u.Error("发送邮箱验证码失败", zap.String("email", req.Email), zap.Error(err))
		c.ResponseError(err)
		return
	}
	c.ResponseOK()
}

// emailRegister 邮箱注册
func (u *User) emailRegister(c *wkhttp.Context) {
	settings := common.EnsureSystemSettings(u.ctx)
	if settings.RegisterOff() {
		c.ResponseError(errors.New("注册通道暂不开放"))
		return
	}
	if !settings.RegisterEmailOn() {
		c.ResponseError(errors.New("暂不支持邮箱注册"))
		return
	}
	type reqVO struct {
		Email    string     `json:"email"`
		Code     string     `json:"code"`
		Password string     `json:"password"`
		Name     string     `json:"name"`
		Flag     uint8      `json:"flag"`
		Device   *deviceReq `json:"device"`
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" {
		c.ResponseError(errors.New("邮箱不能为空"))
		return
	}
	if !isValidEmail(req.Email) {
		c.ResponseError(errors.New("邮箱格式不正确"))
		return
	}
	if strings.TrimSpace(req.Password) == "" {
		c.ResponseError(errors.New("密码不能为空"))
		return
	}
	if len(strings.TrimSpace(req.Password)) < 6 {
		c.ResponseError(errors.New("密码长度不能少于6位"))
		return
	}

	// 验证邮箱验证码（仅非 release 模式且配置了 SMSCode 时走测试分支）
	if commonapi.IsTestCodeEnabled(u.ctx.GetConfig()) {
		if strings.TrimSpace(req.Code) == "" {
			c.ResponseError(errors.New("验证码不能为空"))
			return
		}
		if !commonapi.MatchTestCode(u.ctx.GetConfig(), req.Code) {
			c.ResponseError(errors.New("验证码错误"))
			return
		}
	} else {
		// 线上模式：必须提供验证码
		if strings.TrimSpace(req.Code) == "" {
			c.ResponseError(errors.New("验证码不能为空"))
			return
		}
		emailService := commonapi.NewEmailService(u.ctx, common.EnsureSystemSettings(u.ctx))
		if err := emailService.Verify(context.Background(), req.Email, req.Code, commonapi.CodeTypeRegister); err != nil {
			c.ResponseError(err)
			return
		}
	}

	// 检查邮箱是否已注册
	existUser, err := u.db.QueryByEmail(req.Email)
	if err != nil {
		u.Error("查询用户信息失败", zap.String("email", req.Email), zap.Error(err))
		c.ResponseError(errors.New("查询用户信息失败"))
		return
	}
	if existUser != nil {
		c.ResponseError(errors.New("该邮箱已被注册"))
		return
	}

	if err := ValidateName(req.Name); err != nil {
		c.ResponseError(err)
		return
	}

	uid := util.GenerUUID()
	model := &createUserModel{
		UID:      uid,
		Sex:      1,
		Name:     req.Name,
		Email:    req.Email,
		Username: req.Email,
		Password: req.Password,
		Flag:     int(req.Flag),
		Device:   req.Device,
	}

	tx, err := u.db.session.Begin()
	if err != nil {
		u.Error("创建事务失败", zap.Error(err))
		c.ResponseError(errors.New("创建事务失败"))
		return
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			u.Error("emailRegister panic recovered",
				zap.Any("recover", r),
				zap.String("stack", string(debug.Stack())))
			c.ResponseError(errors.New("注册失败，请重试"))
		}
	}()

	publicIP := util.GetClientPublicIP(c.Request)
	registerSpan := u.ctx.Tracer().StartSpan(
		"user.emailRegister",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	defer registerSpan.Finish()
	registerSpanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), registerSpan)

	result, err := u.createUserWithRespAndTx(registerSpanCtx, model, publicIP, nil, tx, func() error {
		if err := tx.Commit(); err != nil {
			tx.Rollback()
			u.Error("数据库事务提交失败", zap.Error(err))
			c.ResponseError(errors.New("数据库事务提交失败"))
			return err
		}
		return nil
	})
	if err != nil {
		tx.Rollback()
		c.ResponseError(errors.New("注册失败！"))
		return
	}
	c.Response(result)
}

// emailLogin 邮箱登录（验证码方式）
func (u *User) emailLogin(c *wkhttp.Context) {
	if !common.EnsureSystemSettings(u.ctx).RegisterEmailOn() {
		c.ResponseError(errors.New("暂不支持邮箱登录"))
		return
	}
	type reqVO struct {
		Email    string     `json:"email"`
		Code     string     `json:"code"`
		Password string     `json:"password"`
		Flag     uint8      `json:"flag"`
		Device   *deviceReq `json:"device"`
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" {
		c.ResponseError(errors.New("邮箱不能为空"))
		return
	}
	if !isValidEmail(req.Email) {
		c.ResponseError(errors.New("邮箱格式不正确"))
		return
	}
	if req.Code == "" && req.Password == "" {
		c.ResponseError(errors.New("验证码或密码不能为空"))
		return
	}
	// 仅密码登录走 guard；验证码登录有独立的发送频控 + 验证次数限制，不纳入 guard 计数。
	if req.Password != "" {
		if err := u.loginGuard.Check(req.Email); err != nil {
			u.Warn("邮箱登录被临时锁定", zap.String("email", req.Email), zap.Error(err))
			c.ResponseError(err)
			return
		}
	}

	loginSpan := u.ctx.Tracer().StartSpan(
		"user.emailLogin",
		opentracing.ChildOf(c.GetSpanContext()),
	)
	defer loginSpan.Finish()
	loginSpanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), loginSpan)

	userInfo, err := u.db.QueryByEmail(req.Email)
	if err != nil {
		u.Error("查询用户信息失败", zap.String("email", req.Email), zap.Error(err))
		c.ResponseError(errors.New("查询用户信息失败"))
		return
	}
	if userInfo == nil {
		// 密码登录路径统一返回通用错误消息避免枚举；验证码登录路径不涉及密码，保留原提示。
		if req.Password != "" {
			u.loginGuard.RecordFailureLogged(req.Email)
			c.ResponseError(errors.New("邮箱或密码错误"))
			return
		}
		c.ResponseError(errors.New("该邮箱未注册"))
		return
	}
	if userInfo.IsDestroy == IsDestroyDone || userInfo.Status == 0 {
		// 密码路径同样泄露账号状态，统一为通用错误 + 计入失败计数
		if req.Password != "" {
			u.loginGuard.RecordFailureLogged(req.Email)
			c.ResponseError(errors.New("邮箱或密码错误"))
			return
		}
		c.ResponseError(errors.New("该账号已注销或被禁用"))
		return
	}

	// 优先验证码登录，其次密码登录
	if req.Code != "" {
		emailService := commonapi.NewEmailService(u.ctx, common.EnsureSystemSettings(u.ctx))
		if err := emailService.Verify(loginSpanCtx, req.Email, req.Code, commonapi.CodeTypeEmailLogin); err != nil {
			c.ResponseError(err)
			return
		}
	} else {
		matched, needsMigration := CheckPassword(req.Password, userInfo.Password)
		if !matched {
			u.loginGuard.RecordFailureLogged(req.Email)
			c.ResponseError(errors.New("邮箱或密码错误"))
			return
		}
		u.loginGuard.ResetLogged(req.Email)
		if needsMigration {
			if newHash, hashErr := HashPassword(req.Password); hashErr == nil {
				_ = u.db.updatePassword(newHash, userInfo.UID)
			}
		}
	}

	result, err := u.execLogin(userInfo, config.DeviceFlag(req.Flag), req.Device, loginSpanCtx)
	if err != nil {
		c.ResponseError(err)
		return
	}
	c.Response(result)
	publicIP := util.GetClientPublicIP(c.Request)
	go u.sentWelcomeMsg(publicIP, userInfo.UID)
}

// emailForgetPwd 邮箱忘记密码（重置密码）
func (u *User) emailForgetPwd(c *wkhttp.Context) {
	type reqVO struct {
		Email    string `json:"email"`
		Code     string `json:"code"`
		Password string `json:"new_password"`
	}
	var req reqVO
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("请求数据格式有误！"))
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" {
		c.ResponseError(errors.New("邮箱不能为空"))
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		c.ResponseError(errors.New("验证码不能为空"))
		return
	}
	if strings.TrimSpace(req.Password) == "" {
		c.ResponseError(errors.New("新密码不能为空"))
		return
	}
	if len(strings.TrimSpace(req.Password)) < 6 {
		c.ResponseError(errors.New("密码长度不能少于6位"))
		return
	}

	// 验证验证码
	emailService := commonapi.NewEmailService(u.ctx, common.EnsureSystemSettings(u.ctx))
	if err := emailService.Verify(context.Background(), req.Email, req.Code, commonapi.CodeTypeForgetLoginPWD); err != nil {
		c.ResponseError(err)
		return
	}

	userInfo, err := u.db.QueryByEmail(req.Email)
	if err != nil {
		u.Error("查询用户信息失败", zap.String("email", req.Email), zap.Error(err))
		c.ResponseError(errors.New("查询用户信息失败"))
		return
	}
	if userInfo == nil {
		c.ResponseError(errors.New("该邮箱未注册"))
		return
	}

	newHash, err := HashPassword(req.Password)
	if err != nil {
		u.Error("密码哈希失败", zap.Error(err))
		c.ResponseError(errors.New("密码处理失败"))
		return
	}
	if err := u.db.updatePassword(newHash, userInfo.UID); err != nil {
		u.Error("更新密码失败", zap.Error(err))
		c.ResponseError(errors.New("重置密码失败"))
		return
	}
	c.ResponseOK()
}

// isValidEmail 简单的邮箱格式校验
func isValidEmail(email string) bool {
	at := strings.Index(email, "@")
	if at < 1 {
		return false
	}
	dot := strings.LastIndex(email[at:], ".")
	return dot > 1 && at+dot < len(email)-1
}
