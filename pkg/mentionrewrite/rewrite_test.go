// Unit tests for the (now pass-through) RewriteMention helper.
//
// Originally these tests pinned a Plan X (YUJ-1389) inbound rewrite:
// `mention.all=1` was rewritten to also carry `mention.ais=1` so
// legacy `@所有人` traffic auto-fanned-out to all AI bots without an
// SDK update. Product intent was corrected in
// Mininglamp-OSS/octo-server#142 — legacy `@所有人` MUST NOT trigger
// bots — so the rewrite block was removed and the helper is now a
// strict pass-through. These tests have been updated to assert the
// pass-through contract: the inbound mention map is returned untouched
// (modulo nil / non-map defenses).
//
// The companion thin re-export in modules/message/mention_rewrite.go
// has its own colocated test file that exercises the same shapes
// through the message-package symbol, so a future refactor moving the
// helper does not have to update two suites in lockstep — the contract
// is asserted here and the shim test only verifies the shim is not a
// stub.
package mentionrewrite

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRewriteMention_AllOnly — the canonical legacy-client shape. After
// Mininglamp-OSS/octo-server#142 the inbound `mention.all=1` MUST be
// passed through untouched: `all=1` is preserved (legacy read-side
// clients still render `@所有人` from it) and NEITHER `ais` NOR
// `humans` is auto-set. The product intent is that legacy `@所有人`
// no longer triggers bots — bots are only triggered by an explicit
// client-supplied `ais=1`.
func TestRewriteMention_AllOnly(t *testing.T) {
	payload := map[string]interface{}{
		"type":    1,
		"content": "@所有人 hi",
		"mention": map[string]interface{}{
			"all": json.Number("1"),
		},
	}

	out := RewriteMention(payload)

	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["all"],
		"all=1 must be preserved (legacy read-side clients render @所有人 from it)")
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs,
		"#142: ais MUST NOT be inferred from legacy all=1 — bots only fire on explicit ais=1")
	_, hasHumans := mention["humans"]
	assert.False(t, hasHumans,
		"humans must NOT be auto-set — humans is the explicit human-notification signal")
	// Non-mention fields untouched.
	assert.Equal(t, 1, out["type"])
	assert.Equal(t, "@所有人 hi", out["content"])
}

// TestRewriteMention_HumansOnly — explicit humans=1 from a new client
// passes through untouched. ais MUST NOT be set as a side effect.
func TestRewriteMention_HumansOnly(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"humans": json.Number("1"),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["humans"])
	_, hasAll := mention["all"]
	assert.False(t, hasAll, "humans-only input must NOT gain a legacy all=1")
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs)
}

// TestRewriteMention_AIsOnly — explicit ais=1 from a new client passes
// through untouched. humans MUST NOT be set as a side effect.
func TestRewriteMention_AIsOnly(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"ais": json.Number("1"),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["ais"])
	_, hasHumans := mention["humans"]
	assert.False(t, hasHumans, "ais-only input must NOT gain humans=1")
	_, hasAll := mention["all"]
	assert.False(t, hasAll)
}

// TestRewriteMention_HumansAndAIs — combined `@所有人 + @所有 AI` from a
// new client passes through untouched.
func TestRewriteMention_HumansAndAIs(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"humans": json.Number("1"),
			"ais":    json.Number("1"),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["humans"])
	assert.Equal(t, json.Number("1"), mention["ais"])
	_, hasAll := mention["all"]
	assert.False(t, hasAll)
}

// TestRewriteMention_AllPlusUIDs — legacy `@所有人 + @alice + @bob`. The
// uids array MUST be preserved verbatim. Post-#142 there is also no
// implicit ais=1 — the rewrite is a strict pass-through.
func TestRewriteMention_AllPlusUIDs(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"all":  json.Number("1"),
			"uids": []interface{}{"uid_alice", "uid_bob"},
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["all"], "all preserved")
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs, "#142: no implicit ais from legacy all=1")
	assert.Equal(t,
		[]interface{}{"uid_alice", "uid_bob"},
		mention["uids"],
		"uids array must be preserved verbatim")
}

// TestRewriteMention_AllPlusEntities — v2 client shape (mention.entities
// is the new-format inline-mention metadata, see
// modules/message/validation_test.go:885+). RewriteMention must NOT
// drop or mutate the entities array. Post-#142 there is also no
// implicit ais=1.
func TestRewriteMention_AllPlusEntities(t *testing.T) {
	entities := []interface{}{
		map[string]interface{}{"uid": "__all__", "offset": json.Number("0"), "length": json.Number("4")},
	}
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"all":      json.Number("1"),
			"uids":     []interface{}{},
			"entities": entities,
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["all"])
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs, "#142: no implicit ais from legacy all=1")
	assert.True(t, reflect.DeepEqual(entities, mention["entities"]),
		"entities array must survive the rewrite untouched")
}

// TestRewriteMention_UIDsOnly — no broadcast flag, just per-user
// mentions. Nothing to rewrite.
func TestRewriteMention_UIDsOnly(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"uids": []interface{}{"uid_alice"},
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	_, hasAll := mention["all"]
	_, hasHumans := mention["humans"]
	_, hasAIs := mention["ais"]
	assert.False(t, hasAll)
	assert.False(t, hasHumans)
	assert.False(t, hasAIs)
	assert.Equal(t, []interface{}{"uid_alice"}, mention["uids"])
}

// TestRewriteMention_NilPayload — defensive: nil in, nil out, no panic.
func TestRewriteMention_NilPayload(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RewriteMention panicked on nil payload: %v", r)
		}
	}()
	assert.Nil(t, RewriteMention(nil))
}

// TestRewriteMention_EmptyPayload — empty map in, empty map out, no
// panic, no key inserted at the top level.
func TestRewriteMention_EmptyPayload(t *testing.T) {
	out := RewriteMention(map[string]interface{}{})
	assert.NotNil(t, out)
	assert.Empty(t, out)
}

// TestRewriteMention_NoMentionKey — payload without a `mention` key
// must be returned untouched (don't synthesize an empty mention map).
func TestRewriteMention_NoMentionKey(t *testing.T) {
	payload := map[string]interface{}{
		"type":    1,
		"content": "hi",
	}
	out := RewriteMention(payload)
	_, hasMention := out["mention"]
	assert.False(t, hasMention, "no mention key must remain absent")
}

// TestRewriteMention_MentionIsNil — explicit nil mention is treated the
// same as absent. No mutation.
func TestRewriteMention_MentionIsNil(t *testing.T) {
	payload := map[string]interface{}{"mention": nil}
	out := RewriteMention(payload)
	assert.Nil(t, out["mention"])
}

// TestRewriteMention_MentionIsNonMap — malformed mention (string / int /
// array) must NOT panic. Leave untouched.
func TestRewriteMention_MentionIsNonMap(t *testing.T) {
	cases := []map[string]interface{}{
		{"mention": "weird"},
		{"mention": 42},
		{"mention": []interface{}{"a", "b"}},
		{"mention": true},
	}
	for _, payload := range cases {
		orig := payload["mention"]
		out := RewriteMention(payload)
		assert.Equal(t, orig, out["mention"],
			"non-map mention must be returned untouched, no panic")
	}
}

// TestRewriteMention_AllAsFloat — json.Decoder *without* UseNumber will
// produce float64. Post-#142 the helper is a pass-through, so a
// float-encoded `all=1` survives verbatim and no `ais` field is
// synthesized.
func TestRewriteMention_AllAsFloat(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"all": float64(1),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs, "#142: no implicit ais from legacy all=1 (any numeric form)")
	assert.Equal(t, float64(1), mention["all"], "original all value preserved verbatim")
}

// TestRewriteMention_AllAsBool — defensive: some Go callers might
// construct payloads with `all: true`. Post-#142 the helper is a
// pass-through; the bool form survives verbatim and no `ais` field is
// synthesized.
func TestRewriteMention_AllAsBool(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"all": true,
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs, "#142: no implicit ais from legacy all=true")
	assert.Equal(t, true, mention["all"])
}

// TestRewriteMention_AllZero — `all=0` is the "no @所有人" sentinel; the
// rewrite is a pass-through and must NOT add ais / humans fields.
func TestRewriteMention_AllZero(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"all": json.Number("0"),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs, "all=0 must not gain ais=1")
	_, hasHumans := mention["humans"]
	assert.False(t, hasHumans, "all=0 must not gain humans=1")
}

// TestRewriteMention_AllAndAIsBothSet — a forward-compat new client
// might already set BOTH all and ais. Post-#142 the helper is a
// pass-through, so both keys survive verbatim and humans is not
// inferred.
func TestRewriteMention_AllAndAIsBothSet(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"all": json.Number("1"),
			"ais": json.Number("1"),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	// Both still present, neither mutated.
	assert.Equal(t, json.Number("1"), mention["all"])
	assert.Equal(t, json.Number("1"), mention["ais"])
	_, hasHumans := mention["humans"]
	assert.False(t, hasHumans,
		"humans must NOT be inferred from all=1 — only set explicitly by client")
}

// TestRewriteMention_AllPlusHumans — a client that explicitly wants to
// notify humans alongside legacy `@所有人` sets both `all=1` and
// `humans=1`. Post-#142 both keys pass through untouched and no
// implicit `ais=1` is inserted.
func TestRewriteMention_AllPlusHumans(t *testing.T) {
	payload := map[string]interface{}{
		"mention": map[string]interface{}{
			"all":    json.Number("1"),
			"humans": json.Number("1"),
		},
	}
	out := RewriteMention(payload)
	mention := out["mention"].(map[string]interface{})
	assert.Equal(t, json.Number("1"), mention["all"], "all preserved")
	assert.Equal(t, json.Number("1"), mention["humans"], "client-supplied humans untouched")
	_, hasAIs := mention["ais"]
	assert.False(t, hasAIs, "#142: no implicit ais from legacy all=1")
}

// TestRewriteMention_Idempotent — RewriteMention(RewriteMention(p))
// must equal RewriteMention(p) for every input shape. Trivially true
// for a pass-through helper, but the property is the contract that
// lets us drop the helper at three independent chokepoints + any
// future listener / relay without worrying about repeated rewrites.
func TestRewriteMention_Idempotent(t *testing.T) {
	inputs := []map[string]interface{}{
		nil,
		{},
		{"mention": map[string]interface{}{"all": json.Number("1")}},
		{"mention": map[string]interface{}{"humans": json.Number("1")}},
		{"mention": map[string]interface{}{"ais": json.Number("1")}},
		{"mention": map[string]interface{}{
			"humans": json.Number("1"), "ais": json.Number("1"),
		}},
		{"mention": map[string]interface{}{
			"all":  json.Number("1"),
			"uids": []interface{}{"x", "y"},
		}},
		{"mention": map[string]interface{}{
			"all":      json.Number("1"),
			"entities": []interface{}{map[string]interface{}{"uid": "__all__"}},
		}},
	}
	for i, in := range inputs {
		once := RewriteMention(cloneShallow(in))
		twice := RewriteMention(RewriteMention(cloneShallow(in)))
		assert.True(t, reflect.DeepEqual(once, twice),
			"input %d not idempotent: once=%v twice=%v", i, once, twice)
	}
}

// cloneShallow makes a one-level copy of the payload and the nested
// mention map so the two arms of the idempotency test do not share
// mutable state.
func cloneShallow(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		if m, ok := v.(map[string]interface{}); ok {
			inner := make(map[string]interface{}, len(m))
			for ik, iv := range m {
				inner[ik] = iv
			}
			out[k] = inner
			continue
		}
		out[k] = v
	}
	return out
}
