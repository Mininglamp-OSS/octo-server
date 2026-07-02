package space

import (
	"github.com/gocraft/dbr/v2"
)

// DMSpacePresenceAnySet returns the subset of fakeChannelIDs that have at least
// one dm_space_presence row in ANY Space, as a set keyed by fake_channel_id.
// Combined with DMSpacePresenceSet(ids, spaceID) a caller can derive
// "tracked, but belongs exclusively to other Spaces" — the positive, durable
// evidence required before hiding a DM from the default-Space catch-all
// (issue #484 follow-up). Empty input returns an empty set without querying.
func DMSpacePresenceAnySet(session *dbr.Session, fakeChannelIDs []string) (map[string]bool, error) {
	set := make(map[string]bool, len(fakeChannelIDs))
	if len(fakeChannelIDs) == 0 {
		return set, nil
	}
	var present []string
	_, err := session.SelectBySql(
		"SELECT DISTINCT fake_channel_id FROM dm_space_presence WHERE fake_channel_id IN ?",
		fakeChannelIDs,
	).Load(&present)
	if err != nil {
		return nil, err
	}
	for _, id := range present {
		set[id] = true
	}
	return set, nil
}
