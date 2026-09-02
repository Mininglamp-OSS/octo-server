package webhook

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
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

// mutedUIDsFromOnline 依据「开启手机静音的候选 uid」及其 Web/PC 在线记录，算出需静音的 uid 集合。
// onlineRows 为一次批量查询返回的从设备在线记录（仅 online=1），据此复用 shouldMuteAppPush 判定，
// 避免对每个候选 uid 逐个查库的 N+1。只处理候选集合内的 uid，其余记录忽略。
func mutedUIDsFromOnline(candidateUIDs []string, onlineRows []*config.OnlinestatusResp) map[string]struct{} {
	web := make(map[string]struct{}, len(onlineRows))
	pc := make(map[string]struct{}, len(onlineRows))
	for _, r := range onlineRows {
		if r == nil || r.Online != 1 {
			continue
		}
		switch config.DeviceFlag(r.DeviceFlag) {
		case config.Web:
			web[r.UID] = struct{}{}
		case config.PC:
			pc[r.UID] = struct{}{}
		}
	}
	muted := make(map[string]struct{})
	for _, uid := range candidateUIDs {
		_, webOnline := web[uid]
		_, pcOnline := pc[uid]
		if shouldMuteAppPush(1, webOnline, pcOnline) {
			muted[uid] = struct{}{}
		}
	}
	return muted
}
