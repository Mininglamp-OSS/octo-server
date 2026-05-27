package user

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gocraft/dbr/v2"
)

// userListColumns 是 /manager/user/list 主查询的 SELECT 列表与 GROUP BY 列表
// 二者必须一致，避免 ONLY_FULL_GROUP_BY 报错。`user.robot` 用于响应里的
// is_bot 字段，与 /v1/robot/space_bots (modules/robot/api.go:1065) 判定口径一致。
const userListColumns = "user.uid,user.name,user.username,user.email,user.status,user.phone,user.short_no,user.sex,user.is_destroy,user.created_at,user.gitee_uid,user.github_uid,user.wx_openid,user.robot"

// userListFilter 汇总 manager 端列表查询的过滤维度，方便 list/count 复用同一套
// WHERE 条件，避免分页 count 与实际 list 不一致。
//
// Exclude* 与 *Only 是对称的两个方向：前者用于"全部里剔除某类"，后者用于"只看某类"。
// 互斥校验由 handler 负责（同时传 ExcludeBot 与 BotOnly 应直接拒绝），DB 层只
// 老实拼接 WHERE，不做语义合法性判断。
type userListFilter struct {
	OnlineStatus  int  // -1 表示不过滤；0/1 过滤离线/在线
	ExcludeBot    bool // 剔除 user.robot=1 的账号
	ExcludeSystem bool // 剔除 pkg/space.SystemBots 中的系统 UID
	BotOnly       bool // 仅返回 user.robot=1 的账号
	SystemOnly    bool // 仅返回 pkg/space.SystemBots 中的系统 UID
}

// applyExcludes 把账号过滤维度翻译成 WHERE 子句（exclude_* 与 *_only 同处）。
// 不处理 OnlineStatus —— 那个只对 list 主查询有效，count 端不需要 JOIN user_online。
func (f userListFilter) applyExcludes(stmt *dbr.SelectStmt) *dbr.SelectStmt {
	if f.ExcludeBot {
		stmt = stmt.Where("user.robot=0")
	}
	if f.ExcludeSystem {
		stmt = stmt.Where("user.uid NOT IN ?", spacepkg.SystemBotList())
	}
	if f.BotOnly {
		stmt = stmt.Where("user.robot=1")
	}
	if f.SystemOnly {
		stmt = stmt.Where("user.uid IN ?", spacepkg.SystemBotList())
	}
	return stmt
}

type managerDB struct {
	session *dbr.Session
	ctx     *config.Context
}

// newManagerDB
func newManagerDB(ctx *config.Context) *managerDB {
	return &managerDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// 通过账号和密码查询用户信息
func (m *managerDB) queryUserInfoWithNameAndPwd(username string) (*managerLoginModel, error) {
	var model *managerLoginModel
	_, err := m.session.Select("*").From("user").Where("username=?", username).Load(&model)
	return model, err
}

// 获取用户列表
func (m *managerDB) queryUserListWithPage(pageSize, page uint64, f userListFilter) ([]*managerUserModel, error) {
	var users []*managerUserModel
	selectStm := m.session.Select(userListColumns+",max(user_online.online) online").From("user").LeftJoin("user_online", "user.uid=user_online.uid")
	if f.OnlineStatus != -1 {
		selectStm = selectStm.Where("user_online.online=?", f.OnlineStatus)
	}
	selectStm = f.applyExcludes(selectStm)
	selectStm = selectStm.GroupBy(userListColumns)

	_, err := selectStm.Offset((page-1)*pageSize).Limit(pageSize).OrderDir("user.created_at", false).Load(&users)
	return users, err
}

// 模糊查询用户列表
func (m *managerDB) queryUserListWithPageAndKeyword(keyword string, pageSize, page uint64, f userListFilter) ([]*managerUserModel, error) {
	var users []*managerUserModel
	like := "%" + keyword + "%"
	// SSO/OIDC 用户的 user.username 仅在 phone 非空时被填充(api.go createUserWithRespAndTx)
	// 邮箱登录场景下 username 为空,只能靠 user.email 定位用户,故 WHERE 必须包含 email。
	selectStm := m.session.Select(userListColumns+",max(user_online.online) online").From("user").LeftJoin("user_online", "user.uid=user_online.uid").Where("user.name like ? or user.username like ? or user.uid like ? or user.phone like ? or user.short_no like ? or user.email like ?", like, like, like, like, like, like)
	if f.OnlineStatus != -1 {
		selectStm = selectStm.Where("user_online.online=?", f.OnlineStatus)
	}
	selectStm = f.applyExcludes(selectStm)
	selectStm = selectStm.GroupBy(userListColumns)

	_, err := selectStm.Offset((page-1)*pageSize).Limit(pageSize).OrderDir("user.created_at", false).Load(&users)
	return users, err
}

// 用户总数（带过滤）— 与 queryUserListWithPage 走相同的 ExcludeBot/ExcludeSystem 条件，
// 保证 count 与 list 的样本一致，不会出现"列表少于 count"的分页错位。
// 不带 keyword 时的 count 也走这里，不再调用 DB.queryUserCount（那个无法感知过滤）。
func (m *managerDB) queryUserCount(f userListFilter) (int64, error) {
	var count int64
	stmt := m.session.Select("count(*)").From("user")
	stmt = f.applyExcludes(stmt)
	_, err := stmt.Load(&count)
	return count, err
}

// 模糊查询用户数量
func (m *managerDB) queryUserCountWithKeyWord(keyword string, f userListFilter) (int64, error) {
	var count int64
	like := "%" + keyword + "%"
	stmt := m.session.Select("count(*)").From("user").Where("user.name like ? or user.username like ? or user.uid like ? or user.phone like ? or user.short_no like ? or user.email like ?", like, like, like, like, like, like)
	stmt = f.applyExcludes(stmt)
	_, err := stmt.Load(&count)
	return count, err
}

// queryUserBlacklist 查询某个用户的黑名单
func (m *managerDB) queryUserBlacklists(uid string) ([]*managerUserBlacklistModel, error) {
	var users []*managerUserBlacklistModel
	_, err := m.session.Select("user_setting.*,`user`.name,`user`.uid").From("user_setting").LeftJoin("user", "user_setting.to_uid=user.uid").Where("user_setting.uid=? and user_setting.blacklist=1", uid).Load(&users)
	return users, err
}

// 通过status查询用户列表
func (m *managerDB) queryUserListWithStatus(status int, pageSize, page uint64) ([]*managerUserModel, error) {
	var users []*managerUserModel
	_, err := m.session.Select("*").From("user").Where("status=?", status).Offset((page-1)*pageSize).Limit(pageSize).OrderDir("updated_at", false).Load(&users)
	return users, err
}

// 通过status查询用户数量
func (m *managerDB) queryUserCountWithStatus(status int) (int64, error) {
	var count int64
	_, err := m.session.Select("count(*)").From("user").Where("status=?", status).Load(&count)
	return count, err
}

func (m *managerDB) queryUserOnline(uid string) ([]*userOnline, error) {
	var list []*userOnline
	_, err := m.session.Select("*").From("user_online").Where("uid=?", uid).Load(&list)
	return list, err
}

func (m *managerDB) queryUserWithNameAndRole(username string, role string) (*managerUserModel, error) {
	var user *managerUserModel
	_, err := m.session.Select("*").From("user").Where("username=? and role=?", username, role).Load(&user)
	return user, err
}

func (m *managerDB) queryUsersWithRole(role string) ([]*managerUserModel, error) {
	var list []*managerUserModel
	_, err := m.session.Select("*").From("user").Where("role=?", role).Load(&list)
	return list, err
}
func (m *managerDB) deleteUserWithUIDAndRole(uid, role string) error {
	_, err := m.session.DeleteFrom("user").Where("uid=? and role=?", uid, role).Exec()
	return err
}

type managerLoginModel struct {
	Username string
	UID      string
	Name     string
	Password string
	Role     string
	Language string // 偏好语言快照——AuthMiddleware 上的 LanguageResolver 在 Parse 时会刷新成最新值
}

type managerUserModel struct {
	Username  string
	Name      string
	UID       string
	Email     string
	Status    int
	Phone     string
	ShortNo   string
	WXOpenid  string // 微信openid
	GiteeUID  string // gitee uid
	GithubUID string // github uid
	Sex       int
	IsDestroy int
	Robot     int // 0.否 1.是；与 /v1/robot/space_bots 判定一致
	db.BaseModel
}

type managerUserBlacklistModel struct {
	Name string
	UID  string
	db.BaseModel
}

type userOnline struct {
	UID         string
	DeviceFlag  uint8 // 设备标记 0. APP 1.web
	LastOnline  int   // 最后一次在线时间
	LastOffline int   // 最后一次离线时间
	Online      int
	Version     int64 // 数据版本
	db.BaseModel
}
