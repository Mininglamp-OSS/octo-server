package webhook

import (
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
	//
	// 契约：调用方在 GetPayload 返回之后、Push 之前调用，因此 GetPayload 的每个
	// 实现必须返回**全新实例**，不得复用或缓存跨接收者的 Payload —— 否则一个
	// 接收者的静音会泄漏给同批次的其他人。现有六个实现均为每次新建。
	Silence()
}

// desktopOnlineLookup 批量查询「这批 uid 里谁有 PC/Web 在线会话」。
// 生产实现是 user.IService.DesktopOnlineUIDs；独立成函数类型只为测试注入，
// 不额外往 Webhook 上挂依赖。
type desktopOnlineLookup func(uids []string) (map[string]bool, error)

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
// 在线信号自身的时效边界（已知且可接受）：
//
//	user_online 也可能滞后 —— 若 IM 的下线回调丢失，纠正要等 onlineStatusCheck
//	（每 5 分钟一轮，单轮最多 1000 行，见 modules/user/api_online.go）。也就是说
//	静音有可能在用户关掉 Web 之后多持续若干分钟。这仍然可接受，因为两种陈旧的
//	**量级不同**：mute_of_app 的残留是无界的（永不清除），user_online 的滞后是
//	有界的（分钟级且有定时纠正）。把无界降成有界正是本判定的目的；若日后要进一步
//	收紧，方向是改用 last_online 新鲜度而非 online 标志位。
//
// 性能：整批只做一次查询，且仅在存在 mute_of_app == 1 的接收者时才发起 ——
// 无人开启静音的批次为 0 次查询。
//
// 失败姿态：查询出错按**有声**处理（fail-open）。一条听不到的通知等同于丢消息，
// 与 filterPausedUIDs 对通知偏好的 fail-open 取向一致。
func resolveEffectiveAppMute(users []*user.Resp, lookup desktopOnlineLookup, onErr func(mutedCount int, err error)) map[string]bool {
	if len(users) == 0 || lookup == nil {
		return nil
	}

	// 先挑出声明了静音的接收者；没有就完全不查库。
	var candidates []string
	for _, u := range users {
		if u != nil && u.MuteOfApp == 1 {
			candidates = append(candidates, u.UID)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	desktopOnline, err := lookup(candidates)
	if err != nil {
		// fail-open：查不出在线状态就整批照常发声。整批记一条日志，
		// 避免一次 DB 抖动在广播场景刷出成百上千条重复错误。
		if onErr != nil {
			onErr(len(candidates), err)
		}
		return nil
	}

	var muted map[string]bool
	for _, uid := range candidates {
		if !desktopOnline[uid] {
			continue // 静音已失效（PC/Web 均不在线），照常发声
		}
		if muted == nil {
			muted = make(map[string]bool, len(candidates))
		}
		muted[uid] = true
	}
	return muted
}
