package space

import (
	"github.com/gocraft/dbr/v2"
)

// DM (Person) per-Space presence index (issue #484).
//
// A DM is a single physical channel; multi-Space isolation was emulated by
// scanning the shared Recents window for a payload.space_id tag, which made a
// DM's visibility depend on which Space last filled that window (symptom 2:
// DMs mutually hiding between Spaces). This table records, authoritatively and
// window-independently, that a DM pair has had at least one message in a given
// Space. It is keyed by common.GetFakeChannelIDWith(uidA, uidB) — a symmetric
// canonical pair id, so the webhook write side (sender, peer) and the
// conversation read side (viewer, peer) compute the same key.
//
// Readers OR this signal with the legacy Recents scan, so a missing row never
// hides a currently-visible DM; population is incremental (no backfill).

// UpsertDMSpacePresence records that fakeChannelID has a message in spaceID.
// Idempotent; last_timestamp only moves forward. Best-effort by contract —
// callers (webhook ingest) log and continue on error and must NOT let a failure
// here roll back message persistence.
func UpsertDMSpacePresence(session *dbr.Session, fakeChannelID, spaceID string, ts int64) error {
	if fakeChannelID == "" || spaceID == "" {
		return nil
	}
	_, err := session.InsertBySql(
		"INSERT INTO dm_space_presence (fake_channel_id, space_id, last_timestamp) "+
			"VALUES (?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE last_timestamp=GREATEST(last_timestamp, VALUES(last_timestamp))",
		fakeChannelID, spaceID, ts,
	).Exec()
	return err
}

// DMSpacePresenceSet returns the subset of fakeChannelIDs that have at least one
// message tagged with spaceID, as a set keyed by fake_channel_id. Empty input
// (or empty spaceID) returns an empty set without querying.
func DMSpacePresenceSet(session *dbr.Session, fakeChannelIDs []string, spaceID string) (map[string]bool, error) {
	set := make(map[string]bool, len(fakeChannelIDs))
	if len(fakeChannelIDs) == 0 || spaceID == "" {
		return set, nil
	}
	var present []string
	_, err := session.Select("fake_channel_id").From("dm_space_presence").
		Where("fake_channel_id IN ? AND space_id = ?", fakeChannelIDs, spaceID).
		Load(&present)
	if err != nil {
		return nil, err
	}
	for _, id := range present {
		set[id] = true
	}
	return set, nil
}
