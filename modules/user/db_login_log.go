package user

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

// LoginLogDB 登录日志DB
type LoginLogDB struct {
	session *dbr.Session
}

// NewLoginLogDB NewDB
func NewLoginLogDB(session *dbr.Session) *LoginLogDB {
	return &LoginLogDB{
		session: session,
	}
}

// insert 添加登录日志
func (l *LoginLogDB) insert(m *LoginLogModel) error {
	_, err := l.session.InsertInto("login_log").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// queryLastLoginIP 查询最后一次登录成功的日志(status=1)。加了失败记录后过滤
// status，避免欢迎语把一次失败尝试的 IP/时间当成"上次登录信息"展示给用户。
func (l *LoginLogDB) queryLastLoginIP(uid string) (*LoginLogModel, error) {
	var model *LoginLogModel
	_, err := l.session.Select("*").From("login_log").Where("uid=? AND status=1", uid).OrderDir("created_at", false).Limit(1).Load(&model)
	if err != nil {
		return nil, err
	}
	return model, nil
}

// LoginLogModel 登录日志
//
// 不落账号明文：登录标识符对手机号注册用户就是手机号、对邮箱登录就是邮箱，
// 直接落库会让这张表变成一个新的明文 PII 存储（并且会随日志采集进入下游）。
// 改为 AccountMasked（人读）+ AccountHash（精确检索）双列，见
// maskLoginAccount / LoginAccountBlindHash。
type LoginLogModel struct {
	LoginIP       string // 登录IP
	UID           string // 登录成功时的用户 uid；失败时为空(账号不存在场景)
	Status        int    // 1成功 2失败
	LoginType     string // 登录方式:username/email/manager/register/github/gitee/oidc/wechat/scan_login/phone_verify
	AccountMasked string // 登录尝试账号的双侧受限掩码(如 138****1234 / a***@x***)，供人读
	AccountHash   string // 登录尝试账号的盲索引，供按账号精确检索(失败记录没有 uid，只能靠它)
	db.BaseModel
}

// login_log.status 的枚举值。
const (
	loginStatusSuccess = 1
	loginStatusFailure = 2
	// loginStatusPendingSecondFactor 记录"密码与角色都通过，但二次认证尚未通过"。
	// 单列一个取值而不是复用成功/失败：它没签发 token（不是成功），凭据却是对的
	// （不是失败），而且是审计上最有价值的一类事件——正确口令 + 拿不到验证码，
	// 意味着口令很可能已泄露。并进 status=2 会被密码错误的噪音淹没。
	loginStatusPendingSecondFactor = 3
)
