// Package webhook · YUJ-660 / Mininglamp-OSS#33 unit tests for the PERSONAL
// offline-push SpaceID resolution path.
//
// Mobile push (APNs/FCM/HMS/VIVO/MI) reads payloadInfo.SpaceID for client-side
// Space filtering of system tray pushes. Without this fix, msgResp.SpaceID was
// always "" on PERSONAL because resolveSpaceChannelID is a no-op on PERSONAL
// (channel_id is the bare peer uid) → ParseChannelID returns ("", peerID) →
// every PERSONAL offline push leaked across Spaces (parallel to the realtime
// WS push leak this PR closes).
//
// resolvePersonalOfflinePushSpaceID now sources from payload.space_id first
// (which the dispatch-layer fixes in this PR guarantee), falling back to
// ParseChannelID for legacy prefixed channel_ids and Signal-encrypted payloads.
package webhook

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/stretchr/testify/assert"
)

// newTestWebhook builds a *Webhook with logger wired so the helper can call
// w.Warn without nil-panic.
func newTestWebhook() *Webhook {
	return &Webhook{Log: log.NewTLog("Webhook-test")}
}

// makePersonalOfflineMsg builds a msgOfflineNotify pre-populated for PERSONAL.
// PayloadMap lives on the embedded MsgResp, set via the field selector.
func makePersonalOfflineMsg(channelID string, payload map[string]interface{}) msgOfflineNotify {
	var m msgOfflineNotify
	m.ChannelID = channelID
	m.ChannelType = common.ChannelTypePerson.Uint8()
	m.PayloadMap = payload
	return m
}

func TestResolvePersonalOfflinePushSpaceID_PayloadInjected(t *testing.T) {
	// Dispatch-layer fixes inject authoritative payload.space_id on PERSONAL.
	// This is the steady-state, post-rollout path and must be the source of truth.
	w := newTestWebhook()
	msg := makePersonalOfflineMsg("peer_uid_bare", map[string]interface{}{
		"space_id": "space_A",
		"content":  "hi",
	})

	got := w.resolvePersonalOfflinePushSpaceID(msg)
	assert.Equal(t, "space_A", got,
		"PERSONAL offline push must source SpaceID from payload.space_id")
}

func TestResolvePersonalOfflinePushSpaceID_PayloadEmpty_BareUID_FallsThroughEmpty(t *testing.T) {
	// Legacy / unfixed dispatcher: payload has no space_id and channel_id is the
	// bare peer uid. resolvePersonalOfflinePushSpaceID returns "" and emits the
	// observability warn for ops alerting (regression bait: any future bypass
	// of dispatch-layer enrichment shows up as non-zero counts of this warn).
	w := newTestWebhook()
	msg := makePersonalOfflineMsg("peer_uid_bare", map[string]interface{}{
		"content": "hi",
		// no space_id
	})
	msg.FromUID = "sender_uid"

	got := w.resolvePersonalOfflinePushSpaceID(msg)
	assert.Equal(t, "", got,
		"empty payload.space_id + bare-uid channel_id → SpaceID is '' (warn fires)")
}

func TestResolvePersonalOfflinePushSpaceID_NilPayloadMap_BareUID_FallsThroughEmpty(t *testing.T) {
	// Signal-encrypted PERSONAL with bare-uid channel_id: PayloadMap is nil and
	// ParseChannelID returns "". Same observability warn. Documents that
	// Signal-encrypted bare-uid PERSONAL pushes have no Space context — this is
	// acceptable because the encrypted body never leaks DM content via push.
	w := newTestWebhook()
	msg := makePersonalOfflineMsg("peer_uid_bare", nil)

	got := w.resolvePersonalOfflinePushSpaceID(msg)
	assert.Equal(t, "", got)
}

func TestResolvePersonalOfflinePushSpaceID_LegacyPrefixedChannelID(t *testing.T) {
	// Signal-encrypted PERSONAL: PayloadMap is nil (body not parsed). Legacy
	// fallback: parse prefixed channel_id `s{spaceID}_{uid}`. ParseChannelID
	// requires the spaceID to be registered (32-hex, length-aware match).
	const testSpaceID = "00112233445566778899aabbccddeeff"
	space.RegisterSpaceIDs([]string{testSpaceID})
	defer space.RegisterSpaceIDs(nil)

	w := newTestWebhook()
	msg := makePersonalOfflineMsg("s"+testSpaceID+"_peerB", nil)

	got := w.resolvePersonalOfflinePushSpaceID(msg)
	assert.Equal(t, testSpaceID, got,
		"Signal-encrypted PERSONAL with legacy prefixed channel_id falls back to ParseChannelID")
}

func TestResolvePersonalOfflinePushSpaceID_PayloadWinsOverChannelIDPrefix(t *testing.T) {
	// Both payload.space_id and prefixed channel_id present → payload wins
	// (dispatch-layer authoritative source). Guards against drift where a
	// legacy stored channel_id has the wrong Space prefix but the dispatch
	// layer correctly tagged the payload.
	const altSpaceID = "ffeeddccbbaa99887766554433221100"
	space.RegisterSpaceIDs([]string{altSpaceID})
	defer space.RegisterSpaceIDs(nil)

	w := newTestWebhook()
	msg := makePersonalOfflineMsg(
		"s"+altSpaceID+"_peerB",
		map[string]interface{}{"space_id": "space_authoritative"},
	)

	got := w.resolvePersonalOfflinePushSpaceID(msg)
	assert.Equal(t, "space_authoritative", got,
		"payload.space_id is authoritative; channel_id prefix is fallback only")
}

func TestResolvePersonalOfflinePushSpaceID_NonStringPayloadValueIgnored(t *testing.T) {
	// Defensive: if payload.space_id deserializes to a non-string (corrupted
	// upstream), don't crash and fall through to ParseChannelID.
	const testSpaceID = "00112233445566778899aabbccddeeff"
	space.RegisterSpaceIDs([]string{testSpaceID})
	defer space.RegisterSpaceIDs(nil)

	w := newTestWebhook()
	msg := makePersonalOfflineMsg(
		"s"+testSpaceID+"_peerB",
		map[string]interface{}{"space_id": 12345},
	)

	got := w.resolvePersonalOfflinePushSpaceID(msg)
	assert.Equal(t, testSpaceID, got,
		"non-string payload.space_id is ignored; falls through to channel_id parse")
}

func TestResolvePersonalOfflinePushSpaceID_EmptyStringPayloadValueIgnored(t *testing.T) {
	// payload.space_id == "" means dispatcher tagged "no Space" — fall through
	// to channel_id parse and ultimately the warn. This guards against silently
	// preferring "" over a legacy channel-id-derived SpaceID.
	const testSpaceID = "00112233445566778899aabbccddeeff"
	space.RegisterSpaceIDs([]string{testSpaceID})
	defer space.RegisterSpaceIDs(nil)

	w := newTestWebhook()
	msg := makePersonalOfflineMsg(
		"s"+testSpaceID+"_peerB",
		map[string]interface{}{"space_id": ""},
	)

	got := w.resolvePersonalOfflinePushSpaceID(msg)
	assert.Equal(t, testSpaceID, got,
		"empty-string payload.space_id should fall through to channel_id prefix parse")
}
