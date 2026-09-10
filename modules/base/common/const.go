package common

// CodeType 验证码类型
type CodeType int

const (
	// CodeTypeRegister 注册
	CodeTypeRegister CodeType = iota
	// CodeTypePayPWD 支付密码
	CodeTypePayPWD
	// CodeTypeForgetLoginPWD 忘记登录密码
	CodeTypeForgetLoginPWD
	// CodeTypeCheckMobile 校验指定手机号是否正确
	CodeTypeCheckMobile
	// DestroyAccount 注销账号
	CodeTypeDestroyAccount
	CodeTypeEmailLogin
	// CodeTypeOIDCBind OIDC 自助绑定 OTP。单独 keyspace 避免与
	// register/login/forget-pwd 等流程的 SMS 计数器串扰。
	CodeTypeOIDCBind
	// CodeTypeManagerLogin 管理控制台登录 MFA OTP。必须使用独立的
	// 验证码、限流、失败计数和发送状态 key，避免公开用户验证码流程影响
	// 管理端登录。
	CodeTypeManagerLogin
)

const (
	// CacheKeySMSCode 短信验证码的缓存key
	CacheKeySMSCode string = "smscode:"
)
