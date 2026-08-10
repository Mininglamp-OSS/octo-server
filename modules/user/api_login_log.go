package user

import (
	"net"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// LoginLog 用户设置
type LoginLog struct {
	ctx *config.Context
	log.Log
	loginLogDB *LoginLogDB
}

// NewLoginLog 创建
func NewLoginLog(ctx *config.Context) *LoginLog {
	return &LoginLog{ctx: ctx, Log: log.NewTLog("loginLog"), loginLogDB: NewLoginLogDB(ctx.DB())}
}

// loginEventLogMsg 登录事件的结构化日志消息名。固定字符串 + 固定字段名，便于日志
// 采集中间件按 event 维度做过滤/告警（例如"同一 ip 5 分钟内 login_failed > N"），
// 不必依赖对 DB 表做轮询。DB 行仍然保留：getLastLoginIP 依赖它做欢迎语的"上次登录
// 信息"，审计侧也需要可 SQL 检索的登录历史。
const loginEventLogMsg = "login_event"

// normalizeLoginIP 把代理头中的候选值收窄为规范 IP 文本。
// GetClientPublicIP 为兼容现有反向代理会优先返回 X-Forwarded-For，该值在
// 代理信任边界配错时可由客户端控制。审计入库边界必须再校验：非 IP 存空串，
// 而不是让超长值撑爆 login_log.login_ip VARCHAR(40) 并丢失整条记录。
func normalizeLoginIP(value string) string {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil {
		return ""
	}
	return ip.String()
}

// resolveAccountFields 把原始登录标识符转成"掩码 + 盲索引"。原始值到此为止，
// 不进 DB、不进日志。主密钥未配置时盲索引为空（只剩掩码，精确检索能力降级），
// 属于降级而非阻断 —— 不能因为没配密钥就不记登录日志。
func resolveAccountFields(account string) (masked, hash string) {
	masked = maskLoginAccount(account)
	h, err := LoginAccountBlindHash(account)
	if err != nil {
		// 不记 account：错误信息里也不该出现原始标识符
		log.Warn("登录日志账号盲索引计算失败,本条只记掩码", zap.Error(err))
		return masked, ""
	}
	return masked, h
}

// logLoginEvent 输出结构化登录事件。只输出掩码与盲索引，绝不输出原始账号，
// 避免明文 PII 随日志采集流向下游。失败事件不带具体失败原因，与 errcode 的反枚举
// 口径一致（具体原因由各 handler 自己的 Warn/Error 记录）。
func (l *LoginLog) logLoginEvent(status int, uid, maskedAccount, accountHash, publicIP, loginType string) {
	result := "success"
	if status == loginStatusFailure {
		result = "failed"
	}
	l.Info(loginEventLogMsg,
		zap.String("event", "login"),
		zap.String("result", result),
		zap.String("login_type", loginType),
		zap.String("uid", uid),
		zap.String("account_masked", maskedAccount),
		zap.String("account_hash", accountHash),
		zap.String("login_ip", publicIP),
	)
}

// recordSuccess 记录一次成功登录。与"登录欢迎语"功能解耦——调用方在认证成功、
// 签发 token 之后直接调用，不再依赖 SendWelcomeMessageOn 开关（历史上 add 只在
// sentWelcomeMsg 内部调用，欢迎语关闭时登录记录完全不会写入，见 PR 说明）。
//
// account 传原始登录标识符即可，本函数负责掩码 + 盲索引，原始值不落库。
func (l *LoginLog) recordSuccess(uid, account, publicIP, loginType string) {
	publicIP = normalizeLoginIP(publicIP)
	masked, hash := resolveAccountFields(account)
	l.logLoginEvent(loginStatusSuccess, uid, masked, hash, publicIP, loginType)
	err := l.loginLogDB.insert(&LoginLogModel{
		UID:           uid,
		AccountMasked: masked,
		AccountHash:   hash,
		LoginIP:       publicIP,
		Status:        loginStatusSuccess,
		LoginType:     loginType,
	})
	if err != nil {
		l.Error("添加登录成功日志错误", zap.Error(err))
	}
}

// recordFailure 记录一次登录失败尝试。uid 留空——失败时通常还没有解析出 uid
// (账号不存在/密码错误都不暴露具体原因，反枚举口径同 errcode)，检索靠 account_hash。
func (l *LoginLog) recordFailure(account, publicIP, loginType string) {
	publicIP = normalizeLoginIP(publicIP)
	masked, hash := resolveAccountFields(account)
	l.logLoginEvent(loginStatusFailure, "", masked, hash, publicIP, loginType)
	err := l.loginLogDB.insert(&LoginLogModel{
		AccountMasked: masked,
		AccountHash:   hash,
		LoginIP:       publicIP,
		Status:        loginStatusFailure,
		LoginType:     loginType,
	})
	if err != nil {
		l.Error("添加登录失败日志错误", zap.Error(err))
	}
}

// getLastLoginIp 获取最后一次登录成功的ip
func (l *LoginLog) getLastLoginIP(uid string) *loginLogResp {
	model, err := l.loginLogDB.queryLastLoginIP(uid)
	if err != nil {
		l.Error("查询登录日志错误", zap.Error(err))
		return nil
	}
	if model != nil {
		return &loginLogResp{
			UID:      model.UID,
			CreateAt: model.CreatedAt.String(),
			LoginIP:  model.LoginIP,
		}
	}
	return nil
}

// loginLogResp 登录日志
type loginLogResp struct {
	UID      string
	CreateAt string
	LoginIP  string
}
