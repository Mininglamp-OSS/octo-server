package permissionbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

func TestRuntimeAssociationCommitAtomicityAndNoRejoinSeed(t *testing.T) {
	s := fixtureStore(t, filepath.Join(t.TempDir(), "association.sqlite"))
	seed(t, s)
	_, e := s.Session.Exec(`ALTER TABLE robot ADD COLUMN bot_token TEXT DEFAULT 'bf_synthetic'`)
	require.NoError(t, e)
	c := &config.Context{}
	require.NoError(t, Install(c, s, fixturePin, nil))
	require.Error(t, Install(c, s, fixturePin, nil))
	r := RuntimeFor(c)
	require.NotNil(t, r.Store())
	require.Nil(t, RuntimeFor(nil).Store())
	require.False(t, r.HasFacts())
	for _, fail := range []bool{true, false} {
		tx, e := s.Session.Begin()
		require.NoError(t, e)
		_, e = tx.Exec(`INSERT INTO user VALUES(5,'bot2',1,0,1),(6,'bot3',1,0,1)`)
		require.NoError(t, e)
		_, e = tx.Exec(`INSERT INTO robot(id,robot_id,creator_uid,status) VALUES(5,'bot2','owner',1),(6,'bot3','owner',1)`)
		require.NoError(t, e)
		_, e = tx.Exec(`INSERT INTO space_member VALUES('space','bot2',1),('space','bot3',1)`)
		require.NoError(t, e)
		if fail {
			_, e = tx.Exec(`UPDATE user SET status=0 WHERE uid='bot3'`)
			require.NoError(t, e)
		}
		e = CommitAssociations(c, tx, Association{BotUID: "bot3", SpaceID: "space", New: true}, Association{BotUID: "bot2", SpaceID: "space", New: true})
		if fail {
			require.Error(t, e)
			tx.RollbackUnlessCommitted()
			var count int
			require.NoError(t, s.Session.QueryRow(`SELECT count(*) FROM space_member WHERE uid='bot2'`).Scan(&count))
			require.Zero(t, count)
		} else {
			require.NoError(t, e)
		}
	}
	key := fixtureKey
	key.BotUID = "bot2"
	p, e := s.Self(t.Context(), key)
	require.NoError(t, e)
	// Rejoin preserves the exact existing permanent header; missing old state
	// must fail instead of manufacturing another U/U incarnation.
	tx, e := s.Session.Begin()
	require.NoError(t, e)
	require.NoError(t, CommitAssociations(c, tx, Association{BotUID: "bot2", SpaceID: "space"}))
	after, e := s.Self(t.Context(), key)
	require.NoError(t, e)
	require.Equal(t, p, after)
	_, e = s.Session.Exec(`DELETE FROM bot_permission_scope WHERE bot_uid='bot2'`)
	require.NoError(t, e) // corruption of disposable fixture only
	tx, e = s.Session.Begin()
	require.NoError(t, e)
	e = CommitAssociations(c, tx, Association{BotUID: "bot2", SpaceID: "space"})
	require.ErrorIs(t, e, ErrUnavailable)
	tx.RollbackUnlessCommitted()
	var count int
	require.NoError(t, s.Session.QueryRow(`SELECT count(*) FROM bot_permission_scope WHERE bot_uid='bot2'`).Scan(&count))
	require.Zero(t, count)
}

// Synthetic protocol producer only. It is never installed by production code
// and establishes no real original-message/current-access authority.
type fixtureNative struct {
	response    []byte
	cutError    error
	reads       int
	validations int
}

func (f *fixtureNative) Read(context.Context, *dbr.Tx, Key, []byte) ([]byte, error) {
	f.reads++
	return bytes.Clone(f.response), nil
}
func (f *fixtureNative) ValidateCuts(context.Context, Key, []byte) error {
	f.validations++
	return f.cutError
}

func nativeFixture(t *testing.T, op string) (map[string]any, map[string]any) {
	t.Helper()
	base := map[string]any{"schema_version": json.Number("1"), "profile_version": json.Number("1"), "request_id": strings.Repeat("a", 32), "operation": op}
	k, _ := asObject(fixtureKey)
	time := "2026-11-09T00:00:00Z"
	nonce := strings.Repeat("b", 32)
	response := map[string]any{"schema_version": json.Number("1"), "profile_version": json.Number("1"), "key": k, "request_id": base["request_id"], "read_id": strings.Repeat("c", 32), "observed_at": time, "authorities": []any{map[string]any{"authority_id": "synthetic-native", "kind": "im", "incarnation": strings.Repeat("1", 32), "position": "synthetic_position", "read_nonce": nonce, "consistency": "ack_before_read_start", "started_at": time, "completed_at": time}}}
	h := map[string]any{"header_version": json.Number("1"), "header_reference": "header1", "message_id": "1", "source": map[string]any{"kind": "group", "id": "group"}, "parent_group": nil, "space_id": "space", "author_uid": "peer", "author_kind": "human", "recipient_bot_uid": "bot", "native_message_version": "version1", "payload_sha256": strings.Repeat("d", 64), "delivery": map[string]any{"stream_id": "synthetic-stream", "stream_incarnation": strings.Repeat("2", 32), "sequence": "1"}}
	switch op {
	case "resolve":
		base["purpose"] = "admit"
		base["references"] = []any{map[string]any{"delivery_reference": "delivery1"}}
		var schema map[string]any
		require.NoError(t, json.Unmarshal(nativeWireSchema, &schema))
		var properties map[string]any
		for _, v := range schema["$defs"].(map[string]any)["Resolved"].(map[string]any)["oneOf"].([]any) {
			props := v.(map[string]any)["properties"].(map[string]any)
			if props["profile"].(map[string]any)["const"] == "group_admit_v1" {
				properties = props["predicates"].(map[string]any)["properties"].(map[string]any)
			}
		}
		predicates := map[string]any{}
		for name := range properties {
			predicates[name] = true
		}
		response["results"] = []any{map[string]any{"reference": "delivery1", "status": "resolved", "header": h, "profile": "group_admit_v1", "authority_refs": []any{nonce}, "predicates": predicates}}
	case "history_headers":
		base["source_reference"] = "source1"
		base["cursor"] = nil
		base["page_size"] = json.Number("1")
		response["page"] = map[string]any{"source_reference": "source1", "page_id": strings.Repeat("e", 32), "items": []any{h}, "next_cursor": nil, "end": true, "authority_refs": []any{nonce}}
	case "history_disclosure":
		base["context_id"] = strings.Repeat("f", 32)
		base["context_epoch"] = "1"
		base["headers"] = []any{map[string]any{"header_reference": "header1"}}
		response["results"] = []any{map[string]any{"reference": "header1", "status": "resolved", "permit": "one_use_synthetic", "header_reference": "header1", "context_id": base["context_id"], "context_epoch": "1", "payload_sha256": strings.Repeat("d", 64), "authority_refs": []any{nonce}}}
	}
	sqlNonce := strings.Repeat("e", 32)
	response["authorities"] = append(response["authorities"].([]any), map[string]any{"authority_id": "synthetic-sql", "kind": "sql", "incarnation": strings.Repeat("a", 32), "position": "synthetic_position", "read_nonce": sqlNonce, "consistency": "ack_before_read_start", "started_at": time, "completed_at": time})
	if rows, ok := response["results"].([]any); ok {
		for _, v := range rows {
			row := v.(map[string]any)
			row["authority_refs"] = append(row["authority_refs"].([]any), sqlNonce)
		}
	}
	if p, ok := response["page"].(map[string]any); ok {
		p["authority_refs"] = append(p["authority_refs"].([]any), sqlNonce)
	}
	return base, response
}
func TestNativeMetadataWirePositiveAndConflictingAuthority(t *testing.T) {
	for _, op := range []string{"resolve", "history_headers", "history_disclosure"} {
		t.Run(op, func(t *testing.T) {
			s := fixtureStore(t, filepath.Join(t.TempDir(), "facts.sqlite"))
			seed(t, s)
			req, resp := nativeFixture(t, op)
			source := &fixtureNative{response: canonical(resp)}
			c := &config.Context{}
			require.NoError(t, Install(c, s, fixturePin, source))
			r := RuntimeFor(c)
			out, e := r.Facts(t.Context(), fixtureKey, bytes.NewReader(canonical(req)))
			require.NoError(t, e)
			require.JSONEq(t, string(source.response), string(out))
			require.Equal(t, 1, source.validations)
			source.cutError = ErrUnavailable
			_, e = r.Facts(t.Context(), fixtureKey, bytes.NewReader(canonical(req)))
			require.ErrorIs(t, e, ErrUnavailable)
			source.cutError = nil
			resp["request_id"] = strings.Repeat("0", 32)
			source.response = canonical(resp)
			_, e = r.Facts(t.Context(), fixtureKey, bytes.NewReader(canonical(req)))
			require.ErrorIs(t, e, ErrUnavailable)
			resp["request_id"] = req["request_id"]
			resp["key"].(map[string]any)["bot_uid"] = "other"
			source.response = canonical(resp)
			_, e = r.Facts(t.Context(), fixtureKey, bytes.NewReader(canonical(req)))
			require.ErrorIs(t, e, ErrUnavailable)
		})
	}
}

func TestNativeMetadataCorrelationAndKnownNegative(t *testing.T) {
	for _, name := range []string{"unknown_read_nonce", "partial_results", "wrong_recipient", "overflow_message", "wrong_profile", "known_negative"} {
		t.Run(name, func(t *testing.T) {
			req, resp := nativeFixture(t, "resolve")
			_, _, e := nativeRequest(bytes.NewReader(canonical(req)))
			require.NoError(t, e)
			row := resp["results"].([]any)[0].(map[string]any)
			switch name {
			case "unknown_read_nonce":
				row["authority_refs"] = []any{strings.Repeat("0", 32)}
			case "partial_results":
				resp["results"] = []any{}
			case "wrong_recipient":
				row["header"].(map[string]any)["recipient_bot_uid"] = "other"
			case "overflow_message":
				row["header"].(map[string]any)["message_id"] = "9223372036854775808"
			case "wrong_profile":
				row["profile"] = "group_reply_v1"
			case "known_negative":
				row["predicates"].(map[string]any)["caller_current_read"] = false
			}
			out, e := nativeResponse(fixtureKey, req, canonical(resp))
			if name == "known_negative" {
				require.NoError(t, e)
				require.Contains(t, string(out), `"caller_current_read":false`)
			} else {
				require.ErrorIs(t, e, ErrUnavailable)
			}
		})
	}
}

func TestNativeClosedProfilesAcrossDMGroupThreadAndUnsupported(t *testing.T) {
	var schema map[string]any
	require.NoError(t, json.Unmarshal(nativeWireSchema, &schema))
	profiles := schema["$defs"].(map[string]any)["Resolved"].(map[string]any)["oneOf"].([]any)
	for _, entry := range profiles {
		props := entry.(map[string]any)["properties"].(map[string]any)
		profile := props["profile"].(map[string]any)["const"].(string)
		t.Run(profile, func(t *testing.T) {
			req, resp := nativeFixture(t, "resolve")
			_, _, e := nativeRequest(bytes.NewReader(canonical(req)))
			require.NoError(t, e)
			row := resp["results"].([]any)[0].(map[string]any)
			row["profile"] = profile
			header := row["header"].(map[string]any)
			parts := strings.Split(profile, "_")
			if profile != "unsupported_principal_v1" {
				req["purpose"] = parts[1]
				switch parts[0] {
				case "dm":
					ref, e := PairReference("bot", "peer")
					require.NoError(t, e)
					header["source"] = ref
				case "thread":
					parent := strings.Repeat("1", 32)
					header["source"] = map[string]any{"kind": "thread", "id": parent + "____123456789012345"}
					header["parent_group"] = parent
				}
			} else {
				header["author_kind"] = "system"
				header["author_uid"] = "system"
			}
			predicates := map[string]any{}
			for name, v := range props["predicates"].(map[string]any)["properties"].(map[string]any) {
				if constant, ok := v.(map[string]any)["const"]; ok {
					predicates[name] = constant
				} else {
					predicates[name] = true
				}
			}
			row["predicates"] = predicates
			_, e = nativeResponse(fixtureKey, req, canonical(resp))
			require.NoError(t, e)
		})
	}
}
