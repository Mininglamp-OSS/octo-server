package webhook

func excludePausedUIDs(uids []string, paused map[string]struct{}) []string {
	if len(paused) == 0 {
		return uids
	}
	filtered := make([]string, 0, len(uids))
	for _, uid := range uids {
		if _, ok := paused[uid]; ok {
			continue
		}
		filtered = append(filtered, uid)
	}
	return filtered
}
