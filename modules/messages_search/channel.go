package messages_search

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
)

// channelType constants: kept local because octo-lib exposes them as method
// values (common.ChannelTypePerson.Uint8() etc.) and we want a single source
// for the validator and the reverse-map helpers.
const (
	channelTypePerson uint8 = 1
	channelTypeGroup  uint8 = 2
	channelTypeThread uint8 = 5
)

// normalizedChannelID converts the request body's channel_type / channel_id
// pair into the OS document's `channelId` field value.
//
//   - channelType=1 (p2p) — the request channel_id is the *peer* uid; OS stores
//     the fakeChannelID generated from (loginUID, peerUID) by the WuKongIM
//     kernel, so we recompute it the same way.
//   - channelType=2 (group)  — the channel_id is the group_no and OS uses it
//     verbatim.
//   - channelType=5 (thread) — already in `{group_no}____{thread_short_id}`
//     form, OS uses it verbatim.
func normalizedChannelID(channelType uint8, channelID, loginUID string) string {
	switch channelType {
	case channelTypePerson:
		return common.GetFakeChannelIDWith(loginUID, channelID)
	default:
		return channelID
	}
}

// groupNoFromChannel extracts the parent group_no from the request channel
// context. Used by sender JOIN to look up group member remarks.
//
//   - thread (5): split on `____`; if it doesn't parse we return "" so the
//     caller falls back to plain user lookup rather than blowing up the
//     request.
//   - group (2): the channel_id is the group_no.
//   - other: empty.
func groupNoFromChannel(channelType uint8, channelID string) string {
	switch channelType {
	case channelTypeGroup:
		return channelID
	case channelTypeThread:
		groupNo, _, err := thread.ParseChannelID(channelID)
		if err != nil {
			return ""
		}
		return groupNo
	default:
		return ""
	}
}

// encodeChannelID returns the channel_id that should be echoed back to the
// client. The wire protocol always echoes the request channel_id verbatim, so
// p2p callers see the peer uid (not the fakeChannelID) and threads see the
// composite `{group_no}____{thread_short_id}` form.
func encodeChannelID(channelID string) string {
	return channelID
}

// peerFromFakeChannelID reverses the DM fakeChannelID (format `"uidA@uidB"`,
// hash-sorted) back to the peer uid relative to loginUID. Global search hits
// carry doc.ChannelID = common.GetFakeChannelIDWith(loginUID, peerUID) for DM
// (channelType=1), and the wire contract wants the peer uid so the frontend
// can render `new Channel(channel_id, channel_type)` and jump to the DM.
//
// Contract mirrors indexer reversePeer (octo-search-indexer/internal/esindex/
// buildraw.go:349 — reversePeer). If the input isn't a well-formed fake
// channelID (no `@`, more than one `@`, or neither side matches loginUID) we
// return the input unchanged as a defensive fallback rather than crash.
//
// Single-channel callers do NOT use this helper — the request already carries
// the peer uid in req.ChannelID, and encodeChannelID echoes it back.
func peerFromFakeChannelID(fakeID, loginUID string) string {
	parts := strings.SplitN(fakeID, "@", 3)
	if len(parts) != 2 {
		return fakeID
	}
	if parts[0] == loginUID {
		return parts[1]
	}
	return parts[0]
}

// fakeChannelIDFor is a thin alias over common.GetFakeChannelIDWith so callers
// in this package have a single import surface for building an OS `channelId`
// from a (loginUID, peerUID) pair.
func fakeChannelIDFor(loginUID, peerUID string) string {
	return common.GetFakeChannelIDWith(loginUID, peerUID)
}
