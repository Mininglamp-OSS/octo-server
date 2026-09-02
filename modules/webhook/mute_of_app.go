package webhook

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"go.uber.org/zap"
)

// shouldMuteAppPush 判定「手机静音」是否应抑制 App(主设备) 离线 Push。
// 仅当开关开启且有从设备(Web/PC)在线时抑制——无人在线时不抑制，保证离线可达。
func shouldMuteAppPush(muteOfApp int, webOnline, pcOnline bool) bool {
	return muteOfApp == 1 && (webOnline || pcOnline)
}

// filterMutedAppUIDs 从 toUids 中剔除需静音的 uid（镜像 excludePausedUIDs）。
func filterMutedAppUIDs(uids []string, muted map[string]struct{}) []string {
	if len(muted) == 0 {
		return append([]string(nil), uids...)
	}
	filtered := make([]string, 0, len(uids))
	for _, uid := range uids {
		if _, ok := muted[uid]; ok {
			continue
		}
		filtered = append(filtered, uid)
	}
	return filtered
}

// deviceOnline 查询指定设备是否在线，查询失败时 fail-open 返回 false（不抑制），
// 避免一次 DB 抖动静默吞掉离线通知。
func (w *Webhook) deviceOnline(uid string, flag config.DeviceFlag) bool {
	resp, err := w.userService.GetDeviceOnline(uid, flag)
	if err != nil {
		w.Error("查询设备在线状态失败，mute_of_app 不抑制", zap.Error(err), zap.String("uid", uid), zap.Uint8("deviceFlag", flag.Uint8()))
		return false
	}
	return resp != nil && resp.Online == 1
}
