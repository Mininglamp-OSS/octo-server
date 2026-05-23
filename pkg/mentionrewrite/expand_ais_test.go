// Unit tests for ExpandAisToBotUIDs — the second-pass mention
// chokepoint helper introduced for Mininglamp-OSS/octo-server#144.
// Each test pins one clause from the function's contract docstring.
package mentionrewrite

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/stretchr/testify/assert"
)

// channelTypeGroup mirrors common.ChannelTypeGroup.Uint8() but as a
// local literal so the table-driven tests below don't have to wrap
// every expected value in a type-conversion. Spot-checked once here
// against the live constant so a future renumbering of common.ChannelType
// fails fast.
const channelTypeGroup = uint8(2)

func init() {
	if got := common.ChannelTypeGroup.Uint8(); got != channelTypeGroup {
		panic("expand_ais_test: common.ChannelTypeGroup.Uint8() drifted from the value the tests pin (2). Update channelTypeGroup or audit which ChannelType this helper actually targets.")
	}
}

// successFetcher returns a deterministic bot UID slice and records
// whether it was invoked. Use this when a test wants to assert the
// expansion path ran (or did NOT run) without simulating a DB failure.
type successFetcher struct {
	uids   []string
	called int
}

func (f *successFetcher) fetch(channelID string) ([]string, error) {
	f.called++
	return f.uids, nil
}

// TestExpandAis_GroupChannelExpands — the happy path. mention.ais=1
// in a GROUP channel must produce mention.uids = bots, untouched
// other fields, and exactly one fetchBotUIDs call.
func TestExpandAis_GroupChannelExpands(t *testing.T) {
	payload := map[string]interface{}{
		"type":    1,
		"content": "@所有 AI hi",
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	f := &successFetcher{uids: []string{"bot_a", "bot_b"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	assert.Equal(t, 1, f.called, "fetchBotUIDs must be invoked exactly once for an ais=1 GROUP payload")
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["ais"], "ais must be preserved untouched (clause 2)")
	uids, _ := mention["uids"].([]interface{})
	assert.ElementsMatch(t, []interface{}{"bot_a", "bot_b"}, uids,
		"mention.uids must contain every bot UID returned by fetchBotUIDs")
}

// TestExpandAis_PersonalChannelNoop — channelType != GROUP must skip
// the expansion entirely (no DB call, no mention.uids mutation).
// This is the explicit out-of-scope constraint from the issue body.
func TestExpandAis_PersonalChannelNoop(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	f := &successFetcher{uids: []string{"bot_a"}}

	out := ExpandAisToBotUIDs(payload, common.ChannelTypePerson.Uint8(), "user_1", f.fetch)

	assert.Equal(t, 0, f.called, "fetchBotUIDs MUST NOT be invoked for PERSONAL channels")
	mention := out["mention"].(map[string]interface{})
	_, hasUIDs := mention["uids"]
	assert.False(t, hasUIDs, "mention.uids must not be created in PERSONAL channels")
}

// TestExpandAis_CommunityTopicNoop — COMMUNITY_TOPIC (or any
// non-GROUP channel type) is explicitly out of scope per the issue
// body's "GROUP channels only" constraint. Locked to keep a future
// channel-type expansion from accidentally enabling it.
func TestExpandAis_CommunityTopicNoop(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	f := &successFetcher{uids: []string{"bot_a"}}

	// communityTopic = whatever is currently used for community topics;
	// in this codebase it sits at ChannelTypeCommunity or beyond — using
	// a clearly-not-GROUP value (255) is enough to lock the contract
	// without coupling to a specific community-topic numeric value.
	out := ExpandAisToBotUIDs(payload, uint8(255), "topic_1", f.fetch)

	assert.Equal(t, 0, f.called, "fetchBotUIDs MUST NOT be invoked for non-GROUP channels")
	mention := out["mention"].(map[string]interface{})
	_, hasUIDs := mention["uids"]
	assert.False(t, hasUIDs)
}

// TestExpandAis_AllOnlyNoop — legacy `@所有人` (mention.all=1) without
// an explicit mention.ais MUST NOT trigger bot expansion. This is the
// product-intent restated in Mininglamp-OSS/octo-server#142: legacy
// `@所有人` no longer triggers bots. The pass-through RewriteMention
// already preserves this, and ExpandAis must not undo it.
func TestExpandAis_AllOnlyNoop(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"all": json.Number("1"),
		},
	}
	f := &successFetcher{uids: []string{"bot_a"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	assert.Equal(t, 0, f.called, "fetchBotUIDs MUST NOT be invoked when ais is not truthy (all=1 alone)")
	mention := out["mention"].(map[string]interface{})
	_, hasUIDs := mention["uids"]
	assert.False(t, hasUIDs)
	assert.Equal(t, json.Number("1"), mention["all"], "all=1 must be preserved")
}

// TestExpandAis_HumansOnlyNoop — `mention.humans=1` without ais is a
// human-broadcast-only signal and must not trigger bot expansion.
// Same clause 1 lock as the all-only case but exercising the humans
// path to make sure the truthy-one check on `ais` is the only gate.
func TestExpandAis_HumansOnlyNoop(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"humans": json.Number("1"),
		},
	}
	f := &successFetcher{uids: []string{"bot_a"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	assert.Equal(t, 0, f.called)
	mention := out["mention"].(map[string]interface{})
	_, hasUIDs := mention["uids"]
	assert.False(t, hasUIDs)
}

// TestExpandAis_Dedup — bot UIDs already present in mention.uids
// (e.g. the user explicitly @-mentioned the bot in addition to
// `@所有 AI`) must not be duplicated. Clause 3.
func TestExpandAis_Dedup(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais":  json.Number("1"),
			"uids": []interface{}{"bot_a", "user_x"},
		},
	}
	f := &successFetcher{uids: []string{"bot_a", "bot_b", "bot_a"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	mention := out["mention"].(map[string]interface{})
	uids, _ := mention["uids"].([]interface{})
	assert.ElementsMatch(t, []interface{}{"bot_a", "user_x", "bot_b"}, uids,
		"bot_a must not be duplicated; pre-existing user_x must be preserved")
}

// TestExpandAis_Idempotent — calling the helper twice on the same
// payload must produce exactly the same mention.uids slice. This is
// the cross-call dedup clause that lets PR #138's
// injectBotUIDIntoMentionUIDs and this helper coexist without
// double-appending.
func TestExpandAis_Idempotent(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	f := &successFetcher{uids: []string{"bot_a", "bot_b"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)
	out = ExpandAisToBotUIDs(out, channelTypeGroup, "group_1", f.fetch)

	mention := out["mention"].(map[string]interface{})
	uids, _ := mention["uids"].([]interface{})
	assert.ElementsMatch(t, []interface{}{"bot_a", "bot_b"}, uids,
		"second call must not duplicate any UID")
}

// TestExpandAis_NilPayload — clause 4: nil payload must return nil
// without panicking. Mirrors RewriteMention's defensive shape so the
// three chokepoint call sites can chain the two helpers without
// per-call nil guards.
func TestExpandAis_NilPayload(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ExpandAisToBotUIDs panicked on nil payload: %v", r)
		}
	}()
	assert.Nil(t, ExpandAisToBotUIDs(nil, channelTypeGroup, "group_1", func(string) ([]string, error) {
		return []string{"bot_a"}, nil
	}))
}

// TestExpandAis_EmptyMention — payload has no `mention` key. Must
// return the payload untouched (clause 4) and never invoke the
// fetcher.
func TestExpandAis_EmptyMention(t *testing.T) {
	payload := map[string]interface{}{"type": 1}
	f := &successFetcher{uids: []string{"bot_a"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	assert.Equal(t, 0, f.called)
	_, hasMention := out["mention"]
	assert.False(t, hasMention)
}

// TestExpandAis_NonMapMention — mention exists but is not a map
// (string / array / number). Must forward untouched (clause 4) — same
// defensive posture as RewriteMention.
func TestExpandAis_NonMapMention(t *testing.T) {
	payload := map[string]interface{}{"mention": "not-a-map"}
	f := &successFetcher{uids: []string{"bot_a"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	assert.Equal(t, 0, f.called)
	assert.Equal(t, "not-a-map", out["mention"])
}

// TestExpandAis_NonArrayUIDs — mention exists, ais=1, but
// mention.uids is a malformed shape (e.g. a string). Must forward
// untouched (clause 4) without mutating the malformed field.
func TestExpandAis_NonArrayUIDs(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais":  json.Number("1"),
			"uids": "bot_a",
		},
	}
	f := &successFetcher{uids: []string{"bot_b"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	// The fetcher MAY have been called (ais gate passed); the
	// important contract is that the malformed uids field is left
	// untouched rather than overwritten with an []interface{} value.
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, "bot_a", mention["uids"],
		"malformed mention.uids must be forwarded untouched, not overwritten")
}

// TestExpandAis_FetcherError — clause 5: fetchBotUIDs returning an
// error degrades to a no-op so a transient DB failure can't drop the
// inbound message.
func TestExpandAis_FetcherError(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	bad := func(string) ([]string, error) { return nil, errors.New("db down") }

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", bad)

	mention := out["mention"].(map[string]interface{})
	_, hasUIDs := mention["uids"]
	assert.False(t, hasUIDs, "fetcher error must leave mention.uids absent")
}

// TestExpandAis_NoBotMembers — fetcher succeeds but returns an
// empty list (no bots in the channel). Must be a no-op so we don't
// allocate an empty mention.uids slice that downstream consumers
// would interpret as "0 mentions explicitly".
func TestExpandAis_NoBotMembers(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	f := &successFetcher{uids: nil}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	mention := out["mention"].(map[string]interface{})
	_, hasUIDs := mention["uids"]
	assert.False(t, hasUIDs)
}

// TestExpandAis_NilFetcher — a nil callback (programming error at a
// call site) must be a no-op, not a panic. Defence-in-depth against a
// future chokepoint that wires the helper before its fetcher is
// ready.
func TestExpandAis_NilFetcher(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ExpandAisToBotUIDs panicked on nil fetcher: %v", r)
		}
	}()
	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", nil)
	mention := out["mention"].(map[string]interface{})
	_, hasUIDs := mention["uids"]
	assert.False(t, hasUIDs)
}

// TestExpandAis_EmptyChannelID — clause 6: defensive on empty
// channel_id. The HTTP handlers already reject empty channel_id, but
// the helper must stay safe to drop into future ingresses.
func TestExpandAis_EmptyChannelID(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	f := &successFetcher{uids: []string{"bot_a"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "", f.fetch)

	assert.Equal(t, 0, f.called)
	mention := out["mention"].(map[string]interface{})
	_, hasUIDs := mention["uids"]
	assert.False(t, hasUIDs)
}

// TestExpandAis_PreservesOtherMentionKeys — clause 2: only
// mention.uids is touched. all / humans / ais / entities and any
// future sibling keys must be forwarded byte-identical.
func TestExpandAis_PreservesOtherMentionKeys(t *testing.T) {
	entities := []interface{}{
		map[string]interface{}{"type": "ai_all", "offset": 0, "length": 5},
	}
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"all":      json.Number("1"),
			"humans":   json.Number("1"),
			"ais":      json.Number("1"),
			"entities": entities,
		},
	}
	f := &successFetcher{uids: []string{"bot_a"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["all"], "all must be preserved")
	assert.Equal(t, json.Number("1"), mention["humans"], "humans must be preserved")
	assert.Equal(t, json.Number("1"), mention["ais"], "ais must be preserved")
	assert.Equal(t, entities, mention["entities"], "entities must be preserved verbatim")
	uids, _ := mention["uids"].([]interface{})
	assert.ElementsMatch(t, []interface{}{"bot_a"}, uids)
}

// TestExpandAis_BoolAisAlsoTruthy — clients sometimes serialize the
// truthy signal as a JSON boolean (`"ais": true`) rather than `1`.
// The shared isTruthyOne helper accepts bool, and we want to confirm
// the expansion path runs for that shape too.
func TestExpandAis_BoolAisAlsoTruthy(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": true,
		},
	}
	f := &successFetcher{uids: []string{"bot_a"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	assert.Equal(t, 1, f.called)
	mention := out["mention"].(map[string]interface{})
	uids, _ := mention["uids"].([]interface{})
	assert.ElementsMatch(t, []interface{}{"bot_a"}, uids)
}

// TestExpandAis_AisZeroNoop — `ais=0` must NOT trigger expansion.
// Same gate as ais-absent (clause 1).
func TestExpandAis_AisZeroNoop(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("0"),
		},
	}
	f := &successFetcher{uids: []string{"bot_a"}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	assert.Equal(t, 0, f.called)
	mention := out["mention"].(map[string]interface{})
	_, hasUIDs := mention["uids"]
	assert.False(t, hasUIDs)
}

// TestExpandAis_EmptyBotUID — fetcher returns a list containing the
// empty string (defensive shape if a malformed member row sneaks
// through). Empty strings must be skipped, not appended to
// mention.uids.
func TestExpandAis_EmptyBotUID(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	f := &successFetcher{uids: []string{"", "bot_a", ""}}

	out := ExpandAisToBotUIDs(payload, channelTypeGroup, "group_1", f.fetch)

	mention := out["mention"].(map[string]interface{})
	uids, _ := mention["uids"].([]interface{})
	assert.ElementsMatch(t, []interface{}{"bot_a"}, uids)
}
