package user

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
)

// 注销状态：0=正常，1=冷静期（可撤销），2=已注销（最终）
const (
	IsDestroyNo       = 0
	IsDestroyApplying = 1
	IsDestroyDone     = 2
)

// DB 用户db操作
type DB struct {
	session *dbr.Session
	ctx     *config.Context
}

// NewDB NewDB
func NewDB(ctx *config.Context) *DB {
	return &DB{
		session: ctx.DB(),
		ctx:     ctx,
	}
}

// QueryByKeyword 通过用户名查询用户信息
func (d *DB) QueryByKeyword(keyword string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("user").Where("(short_no=? and short_no<>'') or (username=? and username<>'') or (phone=? and phone<>'') or (email=? and email<>'')", keyword, keyword, keyword, keyword).Load(&model)
	return model, err
}

// QueryByUsername 通过用户名查询用户信息
// 支持用户名、手机号（格式：zone-phone）、邮箱三种方式登录
func (d *DB) QueryByUsername(username string) (*Model, error) {
	var model *Model
	// 检查是否为手机号格式（zone-phone）
	if idx := strings.Index(username, "-"); idx > 0 && idx < len(username)-1 {
		zone := username[:idx]
		phone := username[idx+1:]
		_, err := d.session.Select("*").From("user").Where("zone=? AND phone=?", zone, phone).Load(&model)
		return model, err
	}
	// 否则按用户名或邮箱查询
	_, err := d.session.Select("*").From("user").Where("username=? or email=?", username, username).Load(&model)
	return model, err
}

// QueryUIDsByUsernames 通过用户名查询用户uids
func (d *DB) QueryUIDsByUsernames(usernames []string) ([]string, error) {
	var uids []string
	_, err := d.session.Select("uid").From("user").Where("username in ?", usernames).Load(&uids)
	return uids, err
}

// QueryByUsernameCxt 通过用户名查询用户信息
func (d *DB) QueryByUsernameCxt(ctx context.Context, username string) (*Model, error) {
	span, _ := d.ctx.Tracer().StartSpanFromContext(ctx, "QueryByUsername")
	defer span.Finish()
	return d.QueryByUsername(username)
}

// QueryByEmail 通过邮箱查询用户信息
func (d *DB) QueryByEmail(email string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("user").Where("email=? and email<>''", email).Load(&model)
	return model, err
}

// QueryByPhone 通过手机号和区号查询用户信息
func (d *DB) QueryByPhone(zone string, phone string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("user").Where("zone=? and phone=?", zone, phone).Load(&model)
	return model, err
}

// 查询多个手机号用户
func (d *DB) QueryByPhones(phones []string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("user").Where("CONCAT(`zone`,`phone`) in ?", phones).Load(&models)
	return models, err
}

// Insert 添加用户
func (d *DB) Insert(m *Model) error {
	_, err := d.session.InsertInto("user").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// Insert 添加用户
func (d *DB) insertTx(m *Model, tx *dbr.Tx) error {
	_, err := tx.InsertInto("user").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

// QueryByUID 通过用户uid查询用户信息
func (d *DB) QueryByUID(uid string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("user").Where("uid=?", uid).Load(&model)
	return model, err
}

// QueryByVercode 通过用户vercode查询用户信息
func (d *DB) QueryByVercode(vercode string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("user").Where("vercode=?", vercode).Load(&model)
	return model, err
}

// queryByQRVerCode 通过用户QRVercode查询用户信息
func (d *DB) queryByQRVerCode(QRVercode string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("user").Where("qr_vercode=?", QRVercode).Load(&model)
	return model, err
}

func (d *DB) queryByUIDs(uids []string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("user").Where("uid in ?", uids).Load(&models)
	return models, err
}
func (d *DB) queryAll() ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("user").Where("is_destroy=0 and bench_no='' ").Load(&models)
	return models, err
}

// QueryDetailByUID 查询用户详情
func (d *DB) QueryDetailByUID(uid string, loginUID string) (*Detail, error) {
	var detail *Detail
	_, err := d.session.Select("user.*,IFNULL(user_setting.mute,0) mute,IFNULL(user_setting.top,0) top,IFNULL(user_setting.chat_pwd_on,0) chat_pwd_on,IFNULL(user_setting.revoke_remind,0) revoke_remind,IFNULL(user_setting.screenshot,0) screenshot,IFNULL(user_setting.receipt,0) receipt").From("user").LeftJoin("user_setting", "user.uid=user_setting.to_uid and user_setting.uid=?").Where("user.uid=?", loginUID, uid).Load(&detail)
	return detail, err
}

// QueryDetailByUIDs 查询用户详情集合
func (d *DB) QueryDetailByUIDs(uids []string, loginUID string) ([]*Detail, error) {
	if len(uids) <= 0 {
		return nil, nil
	}
	var details []*Detail
	_, err := d.session.Select("user.*,IFNULL(user_setting.mute,0) mute,IFNULL(user_setting.top,0) top,IFNULL(user_setting.chat_pwd_on,0) chat_pwd_on,IFNULL(user_setting.revoke_remind,1) revoke_remind,IFNULL(user_setting.screenshot,0) screenshot,IFNULL(user_setting.receipt,0) receipt").From("user").LeftJoin("user_setting", "user.uid=user_setting.to_uid and user_setting.uid=?").Where("user.uid in ?", loginUID, uids).Load(&details)
	return details, err
}

// QueryByUIDs 根据用户uid查询用户信息
func (d *DB) QueryByUIDs(uids []string) ([]*Model, error) {
	if len(uids) <= 0 {
		return nil, nil
	}
	var models []*Model
	_, err := d.session.Select("*").From("user").Where("uid in ?", uids).Load(&models)
	return models, err
}

// QueryUserWithOnlyShortNo 通过short_no获取用户信息
func (d *DB) QueryUserWithOnlyShortNo(shortNo string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("user.name,user.username").From("user").Where("short_no=?", shortNo).Load(&model)
	return model, err
}

// allowedUpdateFields is the whitelist of columns that can be updated via UpdateUsersWithField.
var allowedUpdateFields = map[string]bool{
	"sex": true, "short_no": true, "name": true, "short_status": true,
	"search_by_phone": true, "search_by_short": true, "new_msg_notice": true,
	"msg_show_detail": true, "voice_on": true, "shock_on": true,
	"msg_expire_second": true, "is_upload_avatar": true,
	"chat_pwd": true, "lock_after_minute": true, "lock_screen_pwd": true,
	"password": true,
}

// UpdateUsersWithField 修改用户基本资料
func (d *DB) UpdateUsersWithField(field string, value string, uid string) error {
	if !allowedUpdateFields[field] {
		return fmt.Errorf("field %q is not allowed for update", field)
	}
	_, err := d.session.Update("user").Set(field, value).Where("uid=?", uid).Exec()
	return err
}

// UpdateUsersWithFieldTx 修改用户基本资料（事务版本）
func (d *DB) UpdateUsersWithFieldTx(field string, value string, uid string, tx *dbr.Tx) error {
	if !allowedUpdateFields[field] {
		return fmt.Errorf("field %q is not allowed for update", field)
	}
	_, err := tx.Update("user").Set(field, value).Where("uid=?", uid).Exec()
	return err
}

// AddOrRemoveBlacklist 添加黑名单
func (d *DB) AddOrRemoveBlacklistTx(uid string, touid string, blacklist int, version int64, tx *dbr.Tx) error {
	_, err := tx.Update("user_setting").Set("blacklist", blacklist).Set("version", version).Set("updated_at", dbr.Expr("Now()")).Where("uid=? and to_uid=?", uid, touid).Exec()
	return err
}

// Blacklists  黑名单列表
func (d *DB) Blacklists(uid string) ([]*BlacklistModel, error) {
	var models []*BlacklistModel
	_, err := d.session.Select("user.name,user.username,user.uid").From("user").LeftJoin("user_setting", "user.uid=user_setting.to_uid and user_setting.blacklist=1").Where("user_setting.uid=?", uid).Load(&models)
	return models, err
}

// QueryByCategory 根据用户分类查询用户列表
func (d *DB) QueryByCategory(category string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("user").Where("category=?", category).Load(&models)
	return models, err
}

func (d *DB) queryWithCategories(categories []string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("user").Where("category in ?", categories).Load(&models)
	return models, err
}

// QueryWithAppID 根据appID查询用户列表
func (d *DB) QueryWithAppID(appID string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("user").Where("app_id=? and status=1", appID).Load(&models)
	return models, err
}

// 查询总用户
func (d *DB) queryUserCount() (int64, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("user").Load(&count)
	return count, err
}

// 查询某天的注册数量
func (d *DB) queryRegisterCountWithDate(date string) (int64, error) {
	var count int64
	_, err := d.session.Select("count(*)").From("user").Where("date_format(created_at,'%Y-%m-%d')=?", date).Load(&count)
	return count, err
}

// 查询某个区间的注册数量
func (d *DB) queryRegisterCountWithDateSpace(startDate, endDate string) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").From("user").Where("date_format(created_at,'%Y-%m-%d')>=? and date_format(created_at,'%Y-%m-%d')<=?", startDate, endDate).Load(&models)
	return models, err
}

func (d *DB) updateUser(userMap map[string]interface{}, uid string) error {
	_, err := d.session.Update("user").SetMap(userMap).Where("uid=?", uid).Exec()
	return err
}

func (d *DB) updatePassword(password string, uid string) error {
	_, err := d.session.Update("user").Set("password", password).Where("uid=?", uid).Exec()
	return err
}

// destroyAccountFromState 注销账户：仅当当前 is_destroy=expectedState 时执行，
// 写入 is_destroy=2 + 匿名化字段并清空冷静期时间戳。否则返回 ErrDestroyStateConflict。
//
// 两个调用入口要求不同的前置状态：
//   - 即时注销（legacy DELETE /v1/destroy/:code）：要求 expectedState=0
//   - 冷静期到期 finalize：要求 expectedState=1
//
// 守卫的核心目的：避免「finalize 选中过期记录 → 用户在窗口内 cancel → finalize 仍把 is_destroy 覆写为 2」吞掉撤销。
func (d *DB) destroyAccountFromState(uid, username, phone string, expectedState int) error {
	res, err := d.session.Update("user").SetMap(map[string]interface{}{
		"phone":             phone,
		"username":          username,
		"is_destroy":        2,
		"destroy_apply_at":  nil,
		"destroy_expire_at": nil,
	}).Where("uid=? AND is_destroy=?", uid, expectedState).Exec()
	if err != nil {
		return fmt.Errorf("destroy account: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("destroy account: rows affected: %w", err)
	}
	if n == 0 {
		return ErrDestroyStateConflict
	}
	return nil
}

// 即时注销（legacy）：要求当前 is_destroy=0。
func (d *DB) destroyAccount(uid, username, phone string) error {
	return d.destroyAccountFromState(uid, username, phone, IsDestroyNo)
}

// 冷静期到期 finalize：要求当前 is_destroy=1，防止与并发 cancel 竞争。
func (d *DB) finalizeDestroyAccount(uid, username, phone string) error {
	return d.destroyAccountFromState(uid, username, phone, IsDestroyApplying)
}

// ErrDestroyStateConflict 表示目标 uid 不在期望的注销状态：通常意味着并发请求已抢先改写。
// 调用方需以「业务冲突」而非「服务端错误」对外呈现。
var ErrDestroyStateConflict = errors.New("destroy state conflict")

// 申请注销：进入冷静期 is_destroy=1。
// WHERE 条件 + RowsAffected 检查保证并发请求中只有一个会成功落库；其余返回 ErrDestroyStateConflict。
func (d *DB) applyDestroy(uid string, applyAt, expireAt time.Time) error {
	res, err := d.session.Update("user").SetMap(map[string]interface{}{
		"is_destroy":        1,
		"destroy_apply_at":  applyAt,
		"destroy_expire_at": expireAt,
	}).Where("uid=? AND is_destroy=0", uid).Exec()
	if err != nil {
		return fmt.Errorf("apply destroy: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("apply destroy: rows affected: %w", err)
	}
	if n == 0 {
		return ErrDestroyStateConflict
	}
	return nil
}

// 撤销注销申请：恢复 is_destroy=0，清空申请/到期时间。
// RowsAffected 检查防止误把「未在冷静期」当成「已撤销」。
func (d *DB) cancelDestroy(uid string) error {
	res, err := d.session.Update("user").SetMap(map[string]interface{}{
		"is_destroy":        0,
		"destroy_apply_at":  nil,
		"destroy_expire_at": nil,
	}).Where("uid=? AND is_destroy=1", uid).Exec()
	if err != nil {
		return fmt.Errorf("cancel destroy: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cancel destroy: rows affected: %w", err)
	}
	if n == 0 {
		return ErrDestroyStateConflict
	}
	return nil
}

// 扫描所有冷静期已到期、待执行最终注销的用户。
// ORDER BY destroy_expire_at ASC：保证最早过期的优先处理；批次满 100 时
// 下一轮 5 分钟后能继续推进，避免新过期记录把老记录挤出每轮窗口。
func (d *DB) queryDestroyExpired(now time.Time, limit uint64) ([]*Model, error) {
	var models []*Model
	_, err := d.session.Select("*").
		From("user").
		Where("is_destroy=1 AND destroy_expire_at IS NOT NULL AND destroy_expire_at <= ?", now).
		OrderAsc("destroy_expire_at").
		Limit(limit).
		Load(&models)
	return models, err
}

func (d *DB) queryWithWXOpenIDAndWxUnionidCtx(ctx context.Context, wxOpenid, wxUnionid string) (*Model, error) {
	span, _ := d.ctx.Tracer().StartSpanFromContext(ctx, "queryWithWXOpenIDAndWxUnionid")
	defer span.Finish()
	return d.queryWithWXOpenIDAndWxUnionid(wxOpenid, wxUnionid)
}

// 通过微信openid和unionid查询用户
func (d *DB) queryWithWXOpenIDAndWxUnionid(wxOpenid, wxUnionid string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("user").Where("wx_openid=? and wx_unionid=?", wxOpenid, wxUnionid).Load(&model)
	return model, err
}

// 通过gitee uid查询用户
func (d *DB) queryWithGiteeUID(giteeUID string) (*Model, error) {

	var model *Model
	_, err := d.session.Select("*").From("user").Where("gitee_uid=?", giteeUID).Load(&model)
	return model, err
}

// 通过github uid查询用户
func (d *DB) queryWithGithubUID(githubUID string) (*Model, error) {
	var model *Model
	_, err := d.session.Select("*").From("user").Where("github_uid=?", githubUID).Load(&model)
	return model, err
}

func (d *DB) updateUserMsgExpireSecond(uid string, msgExpireSecond int64) error {
	_, err := d.session.Update("user").Set("msg_expire_second", msgExpireSecond).Where("uid=?", uid).Exec()
	return err
}
func (d *DB) queryUserRedDot(uid, category string) (*userRedDotModel, error) {
	var model *userRedDotModel
	_, err := d.session.Select("*").From("user_red_dot").Where("uid=? and category=?", uid, category).Load(&model)
	return model, err
}
func (d *DB) insertUserRedDotTx(m *userRedDotModel, tx *dbr.Tx) error {
	_, err := tx.InsertInto("user_red_dot").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}

func (d *DB) insertUserRedDot(m *userRedDotModel) error {
	_, err := d.session.InsertInto("user_red_dot").Columns(util.AttrToUnderscore(m)...).Record(m).Exec()
	return err
}
func (d *DB) updateUserRedDot(m *userRedDotModel) error {
	_, err := d.session.Update("user_red_dot").SetMap(map[string]interface{}{
		"count":  m.Count,
		"is_dot": m.IsDot,
	}).Where("uid=? and category=?", m.UID, m.Category).Exec()
	return err
}

func (d *DB) updateUserRedDotTx(m *userRedDotModel, tx *dbr.Tx) error {
	_, err := tx.Update("user_red_dot").SetMap(map[string]interface{}{
		"count":  m.Count,
		"is_dot": m.IsDot,
	}).Where("uid=? and category=?", m.UID, m.Category).Exec()
	return err
}

// ------------ model ------------

// BlacklistModel 黑名单用户
type BlacklistModel struct {
	UID      string // 用户唯一id
	Name     string // 用户名称
	Username string // 用户名
	db.BaseModel
}

// Detail 详情
type Detail struct {
	Model
	Mute         int // 免打扰
	Top          int // 置顶
	ChatPwdOn    int //是否开启聊天密码
	Screenshot   int //截屏通知
	RevokeRemind int //撤回提醒
	Receipt      int //消息回执
	db.BaseModel
}

// Model 用户db model
type Model struct {
	AppID             string //app id
	UID               string // 用户唯一id
	Name              string // 用户名称
	Username          string // 用户名
	Email             string // email地址
	Password          string // 用户密码
	Category          string //用户分类
	Sex               int    //性别
	ShortNo           string //唯一短编号
	ShortStatus       int    //唯一短编号是否修改0.否1.是
	Zone              string //区号
	Phone             string //手机号
	ChatPwd           string //聊天密码
	LockScreenPwd     string // 锁屏密码
	LockAfterMinute   int    // 在几分钟后锁屏 0表示立即
	DeviceLock        int    //是否开启设备锁
	SearchByPhone     int    //是否可以通过手机号搜索0.否1.是
	SearchByShort     int    //是否可以通过短编号搜索0.否1.是
	NewMsgNotice      int    //新消息通知0.否1.是
	MsgShowDetail     int    //显示消息通知详情0.否1.是
	VoiceOn           int    //声音0.否1.是
	ShockOn           int    //震动0.否1.是
	OfflineProtection int    // 离线保护
	Version           int64
	Status            int    // 状态 0.禁用 1.启用
	Vercode           string //验证码
	QRVercode         string // 二维码验证码
	IsUploadAvatar    int    // 是否上传过头像0:未上传1:已上传
	Role              string // 角色 admin/superAdmin
	Robot             int    // 机器人0.否1.是
	MuteOfApp         int    // app是否禁音（当pc登录的时候app可以设置禁音，当pc登录后有效）
	IsDestroy         int           // 注销状态 0.正常 1.注销申请中（冷静期） 2.已注销
	DestroyApplyAt    dbr.NullTime  // 注销申请时间
	DestroyExpireAt   dbr.NullTime  // 注销到期执行时间
	WXOpenid          string // 微信openid
	WXUnionid         string // 微信unionid
	GiteeUID          string // gitee uid
	GithubUID         string // github uid
	Web3PublicKey     string // web3公钥
	MsgExpireSecond   int64  // 消息过期时长
	Language          string // 用户语言偏好（BCP 47，空表示沿用 OCTO_DEFAULT_LANGUAGE）
	db.BaseModel
}

// type userSetting struct {
// 	UID          string
// 	ToUID        string
// 	Top          int
// 	Mute         int
// 	Blacklist    int //是否在黑名单
// 	ChatPwdOn    int // 是否开启聊天密码
// 	Screenshot   int //截屏通知
// 	RevokeRemind int //撤回提醒
// 	Receipt      int //消息回执
// }

type userRedDotModel struct {
	UID      string
	Count    int    // 未读数量
	IsDot    int    // 是否显示红点 1.是 0.否
	Category string // 红点分类
	db.BaseModel
}
