package user

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"go.uber.org/zap"
)

// 默认冷静期天数。app_config.destroy_cooling_off_days 未配置（=0）时回退使用。
const defaultDestroyCoolingOffDays = 7

// 申请注销：登录态 + 密码二次确认 → 进入冷静期
//
// POST /v1/user/destroy/apply  body: {"password":"..."}
func (u *User) destroyApply(c *wkhttp.Context) {
	var req struct {
		Password string `json:"password"`
	}
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	if req.Password == "" {
		respondUserRequestInvalid(c, "password")
		return
	}

	loginUID := c.GetLoginUID()
	// 防止已登录 session 被窃后无限次试密码：用 LoginGuard 阈值（与登录共用），
	// 用 destroy: 前缀隔离 key 空间，避免与登录失败计数串扰。
	guardKey := "destroy:" + loginUID
	if err := u.loginGuard.Check(guardKey); err != nil {
		u.Warn("注销申请被临时锁定", zap.String("uid", loginUID), zap.Error(err))
		respondUserError(c, errcode.ErrUserLoginLocked)
		return
	}

	userInfo, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询登录用户信息错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if userInfo == nil {
		respondUserError(c, errcode.ErrUserCurrentNotFound)
		return
	}
	switch userInfo.IsDestroy {
	case IsDestroyApplying:
		respondUserError(c, errcode.ErrUserAccountDestroying)
		return
	case IsDestroyDone:
		respondUserError(c, errcode.ErrUserAccountDestroyed)
		return
	}

	if userInfo.Password == "" {
		respondUserError(c, errcode.ErrUserPasswordNotSet)
		return
	}
	// 注销路径丢弃 needsMigration 是有意为之：账号即将进入冷静期，
	// 7 天后被匿名化，没必要为这个一次性密码重新计算 bcrypt 哈希。
	matched, _ := CheckPassword(req.Password, userInfo.Password)
	if !matched {
		u.loginGuard.RecordFailureLogged(guardKey)
		respondUserError(c, errcode.ErrUserPasswordIncorrect)
		return
	}
	u.loginGuard.ResetLogged(guardKey)

	days := u.destroyCoolingOffDays()
	now := time.Now()
	expireAt := now.Add(time.Duration(days) * 24 * time.Hour)
	switch err := u.db.applyDestroy(loginUID, now, expireAt); err {
	case nil:
		// 落库成功
	case ErrDestroyStateConflict:
		// 并发：另一请求已改变状态。返回业务冲突，避免给客户端假成功。
		respondUserError(c, errcode.ErrUserAccountStateChanged)
		return
	default:
		u.Error("申请注销失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserDestroyFailed)
		return
	}
	u.Info("用户申请注销", zap.String("uid", loginUID), zap.Time("expire_at", expireAt))
	c.Response(map[string]interface{}{
		"destroy_status":   IsDestroyApplying,
		"apply_at":         now.Unix(),
		"expire_at":        expireAt.Unix(),
		"cooling_off_days": days,
	})
}

// 撤销注销申请：登录态
//
// POST /v1/user/destroy/cancel
func (u *User) destroyCancel(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	userInfo, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询登录用户信息错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if userInfo == nil {
		respondUserError(c, errcode.ErrUserCurrentNotFound)
		return
	}
	if userInfo.IsDestroy != IsDestroyApplying {
		respondUserError(c, errcode.ErrUserNotDestroying)
		return
	}
	switch err := u.db.cancelDestroy(loginUID); err {
	case nil:
	case ErrDestroyStateConflict:
		respondUserError(c, errcode.ErrUserAccountStateChanged)
		return
	default:
		u.Error("撤销注销失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserDestroyFailed)
		return
	}
	u.Info("用户撤销注销", zap.String("uid", loginUID))
	c.ResponseOK()
}

// 查询注销状态：登录态
//
// GET /v1/user/destroy/status
func (u *User) destroyStatus(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	userInfo, err := u.db.QueryByUID(loginUID)
	if err != nil {
		u.Error("查询登录用户信息错误", zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	if userInfo == nil {
		respondUserError(c, errcode.ErrUserCurrentNotFound)
		return
	}
	// 注意：cooling_off_days 不下发——管理员事后调整该值会和用户实际 expire_at 不一致，引发歧义。
	// 客户端要展示「冷静期天数」时基于 (expire_at - apply_at) 自己换算。
	resp := map[string]interface{}{
		"destroy_status": userInfo.IsDestroy,
	}
	if userInfo.IsDestroy == IsDestroyApplying && userInfo.DestroyExpireAt.Valid {
		if userInfo.DestroyApplyAt.Valid {
			resp["apply_at"] = userInfo.DestroyApplyAt.Time.Unix()
		}
		resp["expire_at"] = userInfo.DestroyExpireAt.Time.Unix()
		resp["remaining_days"] = remainingDays(userInfo.DestroyExpireAt.Time)
	}
	c.Response(resp)
}

// 定时任务入口：扫描到期账号并执行最终注销。每 5 分钟由 ctx.Schedule 调用。
//
// 单次最多处理 batchSize 个用户：避免单批过大锁表，下一轮 5 分钟后继续。
func (u *User) checkDestroyExpired() {
	const batchSize uint64 = 100
	models, err := u.db.queryDestroyExpired(time.Now(), batchSize)
	if err != nil {
		u.Error("扫描到期注销用户失败", zap.Error(err))
		return
	}
	if len(models) == 0 {
		return
	}
	u.Info("开始执行到期注销", zap.Int("count", len(models)))
	var success, failed int
	for _, m := range models {
		if err := u.finalizeDestroy(m); err != nil {
			// 单个用户失败不阻塞批次：下一轮重试
			u.Error("执行到期注销失败", zap.String("uid", m.UID), zap.Error(err))
			failed++
			continue
		}
		success++
	}
	u.Info("到期注销批次完成", zap.Int("success", success), zap.Int("failed", failed), zap.Int("total", len(models)))
}

// 复用即时注销的最终化逻辑：匿名化 phone/username + 踢出全部设备。
// 群组/Space/OAuth 级联清理留待后续 PR。
func (u *User) finalizeDestroy(m *Model) error {
	// 毫秒时间戳 13 位；UnixNano 19 位会撑爆 varchar(40)，同毫秒重复概率极低。
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	phone := fmt.Sprintf("%s@%s@delete", m.Phone, stamp)
	username := anonymizeUsername(m.UID, m.Zone, phone, stamp)
	switch err := u.db.finalizeDestroyAccount(m.UID, username, phone); err {
	case nil:
		// 写入成功
	case ErrDestroyStateConflict:
		// 用户在我们选中后、写入前撤销了注销。直接跳过，下一轮扫描不会再选中（is_destroy 已经回到 0）。
		u.Info("用户已撤销注销，跳过 finalize", zap.String("uid", m.UID))
		return nil
	default:
		return fmt.Errorf("update user destroy: %w", err)
	}
	if err := u.ctx.QuitUserDevice(m.UID, -1); err != nil {
		// 设备踢出失败不阻塞 DB 状态更新——一旦写入 is_destroy=2，下次扫描不会再处理本账号。
		// 已知遗留风险：device 在 token 过期前仍能短暂访问；需要单独的 device-evict 重试任务（见 PR2）。
		u.Error("踢出登录设备失败", zap.String("uid", m.UID), zap.Error(err))
	}
	u.Info("已执行到期注销", zap.String("uid", m.UID))
	return nil
}

func (u *User) destroyCoolingOffDays() int {
	cfg, err := u.commonService.GetAppConfig()
	if err == nil && cfg != nil && cfg.DestroyCoolingOffDays > 0 {
		return cfg.DestroyCoolingOffDays
	}
	return defaultDestroyCoolingOffDays
}

// 匿名化 username 并保证 ≤ 40 字符（user.username 列约束）。
// 优先沿用旧的 `zone+phone@stamp@delete` 格式以便审计；溢出时（典型场景：海外长手机号）
// 回退到固定 26 字符的 hash 形式 `del_<stamp>_<8hex>`。
const usernameMaxLen = 40

func anonymizeUsername(uid, zone, phone, stamp string) string {
	candidate := zone + phone
	if len(candidate) <= usernameMaxLen {
		return candidate
	}
	sum := sha256.Sum256([]byte(uid + ":" + stamp))
	return "del_" + stamp + "_" + hex.EncodeToString(sum[:4])
}

func remainingDays(expireAt time.Time) int {
	d := time.Until(expireAt)
	if d <= 0 {
		return 0
	}
	days := int(d / (24 * time.Hour))
	if d%(24*time.Hour) > 0 {
		days++
	}
	return days
}
