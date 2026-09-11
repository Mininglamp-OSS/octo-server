package webhook

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
)

// silenceable 由「支持静音」的离线推送负载实现。
//
// 这是一个可选接口：resolveEffectiveAppMute 判定某个接收者静音生效后，推送侧
// 只对实现了本接口的负载调用 Silence()，其余厂商负载原样发送。因此新增一个
// 厂商的静音支持 = 给它的 Payload 实现 Silence()，无需改动 Push 接口，也不会
// 悄悄影响已有厂商的线上行为。
//
// 当前实现方：IOSPayload。HMS / MI 的声音字段同样写死（push_hms.go 的 sound、
// push_mi.go 的 sound_uri），属于同类缺口但本次不在范围内 —— 它们尚未实现本
// 接口，所以静音对安卓仍是 no-op。
type silenceable interface {
	// Silence 让该负载以「无声」形态下发：只去掉声音，横幅与角标照常。
	Silence()
}

// deviceOnlineChecker 抽象设备在线查询，便于在测试中注入。
// 生产实现是 user.OnlineService。
type deviceOnlineChecker interface {
	DeviceOnline(uid string, device config.DeviceFlag) (bool, error)
}

// resolveEffectiveAppMute 计算每个接收者的「手机静音」(user.mute_of_app) 是否
// **真正生效**，返回 uid -> 是否静音。
//
// 为什么不能直接用 user.mute_of_app 的值：
//
//	该列的契约是「当pc登录后有效」，而它与 iOS 本地状态是两份独立数据、且必然
//	漂移 —— iOS 在 PC/Web 下线时只清本地 NSUserDefaults，不回写服务端
//	(WKOnlineStatusManager.m)，DB 里会长期残留 1。若无条件采信，用户关掉 Web
//	之后手机推送将**永久无声**，比「静音不生效」更难排查。指望客户端回写也不
//	可靠：Web 断开的那一刻 App 可能已被杀死或在后台。所以在线校验必须由服务端
//	在推送时做。
//
// 性能：只有 mute_of_app == 1 的接收者才会触发在线查询。绝大多数用户未开静音，
// 因此批量阶段通常是 0 次查询，不会在 PushPool 扇出前产生 N+1
// （与 api.go 中账号级通知暂停的批量查询同一考量）。
//
// 失败姿态：查询出错按**有声**处理（fail-open）。一条听不到的通知等同于丢消息，
// 与 filterPausedUIDs 对通知偏好的 fail-open 取向一致。
func resolveEffectiveAppMute(users []*user.Resp, checker deviceOnlineChecker, logger func(uid string, err error)) map[string]bool {
	if len(users) == 0 || checker == nil {
		return nil
	}
	var muted map[string]bool
	for _, u := range users {
		if u == nil || u.MuteOfApp != 1 {
			continue // 未开启静音：不查在线，直接有声
		}
		online, err := hasDesktopSession(u.UID, checker)
		if err != nil {
			if logger != nil {
				logger(u.UID, err)
			}
			continue // fail-open：查不出在线状态就照常发声
		}
		if !online {
			continue // 静音已失效（PC/Web 均不在线），照常发声
		}
		if muted == nil {
			muted = make(map[string]bool, 1)
		}
		muted[u.UID] = true
	}
	return muted
}

// hasDesktopSession 判断该用户是否存在 PC 或 Web 在线会话。
// 与 modules/user/api_online.go 下发「PC 在线」面板的判定口径保持一致：
// 先看 PC，未命中再看 Web。
func hasDesktopSession(uid string, checker deviceOnlineChecker) (bool, error) {
	pcOnline, err := checker.DeviceOnline(uid, config.PC)
	if err != nil {
		return false, err
	}
	if pcOnline {
		return true, nil
	}
	return checker.DeviceOnline(uid, config.Web)
}
