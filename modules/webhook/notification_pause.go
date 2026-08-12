package webhook

// filterPausedUIDs applies the account-level pause only when the lookup
// succeeded. A lookup error must not turn a best-effort notification feature
// into a silent drop of the entire offline-push batch.
func filterPausedUIDs(uids []string, paused map[string]struct{}, lookupErr error) []string {
	if lookupErr != nil {
		return uids
	}
	return excludePausedUIDs(uids, paused)
}

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
