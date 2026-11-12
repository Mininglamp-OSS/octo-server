package permissionbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testServerHooks() ServerMessageHooks {
	return ServerMessageHooks{
		MessageTable:          func(string) string { return "message" },
		MessageUserExtraTable: func(string) string { return "message_user_extra" },
		ChannelOffsetTable:    func(string) string { return "channel_offset" },
		PayloadVisibleTo: func(payload []byte, uid string) bool {
			var v struct {
				Visibles []string `json:"visibles"`
			}
			if json.Unmarshal(payload, &v) != nil || len(v.Visibles) == 0 {
				return true
			}
			for _, x := range v.Visibles {
				if x == uid {
					return true
				}
			}
			return false
		},
		PayloadSpaceID: func(payload []byte) string {
			var v struct {
				SpaceID string `json:"space_id"`
			}
			_ = json.Unmarshal(payload, &v)
			return v.SpaceID
		},
		IsSystemUID: func(uid string) bool { return uid == "system" },
	}
}

type serverFacts struct {
	t        *testing.T
	s        Store
	p        *ServerMessageFactProvider
	dmChan   string
	parent   string
	threadCh string
}

func newServerFacts(t *testing.T) *serverFacts {
	path := filepath.Join(t.TempDir(), "server-facts.sqlite")
	s := fixtureStore(t, path)
	seed(t, s)
	seedAdapterTables(t, s)
	pair, err := PairReference("bot", "peer")
	require.NoError(t, err)
	parent := strings.Repeat("1", 32)
	for _, q := range []string{
		"CREATE TABLE message(message_id TEXT,message_seq INTEGER,from_uid TEXT,channel_id TEXT,channel_type INTEGER,`timestamp` INTEGER,payload BLOB,is_deleted INTEGER)",
		"CREATE TABLE message_extra(message_id TEXT UNIQUE,version INTEGER,revoke INTEGER,is_deleted INTEGER,content_edit TEXT,content_edit_hash TEXT)",
		"CREATE TABLE message_user_extra(uid TEXT,message_id TEXT,message_is_deleted INTEGER)",
		"CREATE TABLE channel_offset(uid TEXT,channel_id TEXT,channel_type INTEGER,message_seq INTEGER)",
		"CREATE TABLE channel_setting(channel_id TEXT,channel_type INTEGER,offset_message_seq INTEGER)",
	} {
		_, err := s.Session.Exec(q)
		require.NoError(t, err)
	}
	p, err := NewServerMessageFactProvider(ServerMessageFactConfig{
		Session: s.Session, Hooks: testServerHooks(), StoreIncarnation: strings.Repeat("5", 32), PermitKey: []byte(strings.Repeat("k", 32)),
	})
	require.NoError(t, err)
	return &serverFacts{t: t, s: s, p: p, dmChan: pair["id"].(string), parent: parent, threadCh: parent + "____123456789012345"}
}

func (f *serverFacts) msg(id string, seq int, from, channel string, ctype int, payload map[string]any) {
	b, _ := json.Marshal(payload)
	_, err := f.s.Session.Exec("INSERT INTO message VALUES(?,?,?,?,?,?,?,0)", id, seq, from, channel, ctype, 1700000000+seq, b)
	require.NoError(f.t, err)
}

func (f *serverFacts) exec(q string, args ...any) {
	_, err := f.s.Session.Exec(q, args...)
	require.NoError(f.t, err)
}

func (f *serverFacts) resolve(purpose string, refs ...string) []IMResolvedDelivery {
	out, cut, err := f.p.InspectDeliveredHeadersV1(f.t.Context(), fixtureKey, purpose, refs)
	require.NoError(f.t, err)
	require.Len(f.t, out, len(refs))
	require.NoError(f.t, f.p.ValidateIMCut(f.t.Context(), fixtureKey, cut.AuthorityID, cut.Incarnation, cut.Position, cut.ReadNonce))
	return out
}

func curVersion(id string, f *serverFacts) string {
	r := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, id, "x0_0"))
	return r[0].Header.NativeMessageVersion
}

func TestServerMessageFactsDMPositivePathAndUnknowns(t *testing.T) {
	f := newServerFacts(t)
	f.msg("1001", 1, "peer", f.dmChan, 1, map[string]any{"type": 1, "content": "SECRET-BODY", "space_id": "space"})
	f.msg("1002", 2, "peer", f.dmChan, 1, map[string]any{"type": 1, "content": "untagged"})
	f.msg("1003", 3, "peer", f.dmChan, 1, map[string]any{"type": 1, "content": "other space", "space_id": "elsewhere"})

	ok := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "1001", "x0_0"))[0]
	require.Equal(t, "resolved", ok.Status)
	require.True(t, ok.MessageSourceExact && ok.MessageSpaceExact && ok.MessageAuthorExact && ok.MessageRecipientExact && ok.MessageVersionExact)
	require.True(t, ok.CallerCurrentRead && ok.BotCurrentRead && ok.CallerOriginalVisible && ok.BotOriginalVisible)
	require.Equal(t, "human", ok.Header.AuthorKind)
	require.Equal(t, "dm", ok.Header.Source.Kind)
	require.Equal(t, "1001", ok.Header.MessageID)
	// metadata only: nothing in the header carries the body.
	raw, _ := json.Marshal(ok.Header)
	require.NotContains(t, string(raw), "SECRET-BODY")
	sum := sha256.Sum256([]byte(`{"content":"SECRET-BODY","space_id":"space","type":1}`))
	require.Equal(t, hex.EncodeToString(sum[:]), ok.Header.PayloadSHA256)

	// Unknown version: reference without version is non-serving, not true.
	nv := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "1001", ""))[0]
	require.True(t, nv.MessageVersionUnknown)
	require.False(t, nv.MessageVersionExact)
	require.Equal(t, "predicate_unsupported", nv.unknownReason("admit"))

	// Untagged DM space is unknown; cross-space tag is a definite false.
	un := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "1002", "x0_0"))[0]
	require.True(t, un.MessageSpaceUnknown)
	require.False(t, un.MessageSpaceExact)
	cs := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "1003", "x0_0"))[0]
	require.False(t, cs.MessageSpaceUnknown)
	require.False(t, cs.MessageSpaceExact)
}

func TestServerMessageFactsMissingRevokedDeletedAndMalformed(t *testing.T) {
	f := newServerFacts(t)
	f.msg("2001", 1, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space"})
	f.msg("2002", 2, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space"})
	f.msg("2003", 3, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space"})
	f.exec("INSERT INTO message_extra VALUES('2002',5,1,0,NULL,'')")
	f.exec("INSERT INTO message_extra VALUES('2003',6,0,1,NULL,'')")
	f.exec("UPDATE message SET is_deleted=1 WHERE message_id='2001'")

	for id, reason := range map[string]string{"2001": "expired_reference", "2002": "expired_reference", "2003": "expired_reference", "9999": "mirror_lag"} {
		r := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, id, "x0_0"))[0]
		require.Equal(t, "unavailable", r.Status, id)
		require.Equal(t, reason, r.Reason, id)
	}
	// Malformed and foreign-channel references are non-serving, never errors that leak.
	bad := f.resolve("admit", "not-a-ref", ServerDeliveryReference(1, "x@y", "2001", "x0_0"), ServerDeliveryReference(9, f.dmChan, "2001", ""))
	for _, r := range bad {
		require.Equal(t, "unavailable", r.Status)
		require.Equal(t, "predicate_unsupported", r.Reason)
	}
}

func TestServerMessageFactsEditVisibilityAndClearHistory(t *testing.T) {
	f := newServerFacts(t)
	f.msg("3001", 10, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space"})
	before := curVersion("3001", f)
	require.Equal(t, "x0_0", before)

	// Edit changes the native version: the old reference is no longer version exact
	// and the digest follows the edited content.
	f.exec("INSERT INTO message_extra VALUES('3001',7,0,0,'EDITED','abc123')")
	edited := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "3001", before))[0]
	require.Equal(t, "resolved", edited.Status)
	require.False(t, edited.MessageVersionExact)
	require.Equal(t, "x7_abc123", edited.Header.NativeMessageVersion)
	sum := sha256.Sum256([]byte("EDITED"))
	require.Equal(t, hex.EncodeToString(sum[:]), edited.Header.PayloadSHA256)
	fresh := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "3001", "x7_abc123"))[0]
	require.True(t, fresh.MessageVersionExact)

	// visibles whitelist excluding the bot is a definite write-time false.
	f.msg("3002", 11, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space", "visibles": []string{"peer"}})
	vis := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "3002", "x0_0"))[0]
	require.True(t, vis.CallerOriginalVisible)
	require.False(t, vis.BotOriginalVisible)
	require.False(t, vis.BotCurrentRead)

	// Per-user delete and clear-history offset revoke current read but not identity.
	f.msg("3003", 12, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space"})
	f.exec("INSERT INTO message_user_extra VALUES('bot','3003',1)")
	ud := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "3003", "x0_0"))[0]
	require.False(t, ud.BotCurrentRead)
	require.True(t, ud.CallerCurrentRead)
	f.msg("3004", 13, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space"})
	f.exec("INSERT INTO channel_offset VALUES('peer','bot',1,13)") // DM offsets are keyed by the bare peer uid
	co := f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "3004", "x0_0"))[0]
	require.False(t, co.CallerCurrentRead)
	require.True(t, co.BotCurrentRead)
	f.exec("INSERT INTO channel_setting VALUES(?,1,13)", f.dmChan)
	require.False(t, f.resolve("admit", ServerDeliveryReference(1, f.dmChan, "3004", "x0_0"))[0].BotCurrentRead)
}

func TestServerMessageFactsGroupAndThreadUnknownOriginalVisibility(t *testing.T) {
	f := newServerFacts(t)
	f.msg("4001", 1, "peer", "group", 2, map[string]any{"type": 1})
	f.msg("4002", 2, "peer", f.threadCh, 5, map[string]any{"type": 1})
	f.msg("4003", 3, "departed", "group", 2, map[string]any{"type": 1})

	g := f.resolve("admit", ServerDeliveryReference(2, "group", "4001", "x0_0"))[0]
	require.Equal(t, "resolved", g.Status)
	require.True(t, g.MessageSpaceExact && g.MessageRecipientExact && g.BotCurrentRead && g.CallerCurrentRead && g.BotCurrentSend)
	// No membership history exists in octo-server: original visibility is unknown, not true.
	require.False(t, g.CallerOriginalVisible)
	require.False(t, g.BotOriginalVisible)
	require.True(t, g.CallerOriginalVisibleUnknown && g.BotOriginalVisibleUnknown)
	require.Equal(t, "predicate_unsupported", g.unknownReason("admit"))
	require.Equal(t, "", g.unknownReason("reply"), "reply does not consult original visibility")

	th := f.resolve("contributor", ServerDeliveryReference(5, f.threadCh, "4002", "x0_0"))[0]
	require.Equal(t, "thread", th.Header.Source.Kind)
	require.NotNil(t, th.Header.ParentGroup)
	require.Equal(t, f.parent, *th.Header.ParentGroup)
	require.True(t, th.CallerParentCurrentRead && th.BotParentCurrentRead && th.BotOriginalVisibleUnknown)

	// Departed author: current read is false (choice-B intent: departure is not repaired by history).
	dep := f.resolve("admit", ServerDeliveryReference(2, "group", "4003", "x0_0"))[0]
	require.False(t, dep.CallerCurrentRead)
	require.True(t, dep.BotCurrentRead)

	// Deleted thread and disbanded/disabled groups are non-serving.
	f.exec("UPDATE thread SET status=3")
	require.Equal(t, "expired_reference", f.resolve("admit", ServerDeliveryReference(5, f.threadCh, "4002", "x0_0"))[0].Reason)
	f.exec("UPDATE `group` SET status=0 WHERE group_no='group'")
	require.Equal(t, "expired_reference", f.resolve("admit", ServerDeliveryReference(2, "group", "4001", "x0_0"))[0].Reason)
}

func TestServerMessageFactsHistoryAndTwoStepDisclosure(t *testing.T) {
	f := newServerFacts(t)
	for i := 1; i <= 5; i++ {
		f.msg(strconv.Itoa(5000+i), i, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space", "content": "BODY" + strconv.Itoa(i)})
	}
	f.exec("INSERT INTO message_extra VALUES('5003',2,1,0,NULL,'')") // revoked
	src := ServerSourceReference(1, f.dmChan)
	p1, err := f.p.ListHistoryHeadersV1(t.Context(), fixtureKey, src, nil, 3)
	require.NoError(t, err)
	require.False(t, p1.End)
	require.NotNil(t, p1.NextCursor)
	require.Len(t, p1.Items, 3)
	require.Equal(t, "5005", p1.Items[0].Header.MessageID)
	require.Equal(t, "unavailable", p1.Items[2].Status, "revoked 5003 must not be listed")
	p2, err := f.p.ListHistoryHeadersV1(t.Context(), fixtureKey, src, p1.NextCursor, 3)
	require.NoError(t, err)
	require.True(t, p2.End)
	require.Len(t, p2.Items, 2)
	raw, _ := json.Marshal(p1)
	require.NotContains(t, string(raw), "BODY")
	_, err = f.p.ListHistoryHeadersV1(t.Context(), fixtureKey, src, nil, 33)
	require.ErrorIs(t, err, ErrUnavailable)

	// Step two: permit only for current, version-exact, provably visible headers.
	h5 := p1.Items[0].Header
	items, _, err := f.p.IssueHistoryDisclosureV1(t.Context(), fixtureKey, "ctx1", "epoch1", []string{h5.HeaderReference, "garbage"})
	require.NoError(t, err)
	require.Equal(t, "resolved", items[0].Status)
	require.NotEmpty(t, items[0].Permit)
	require.Equal(t, h5.PayloadSHA256, items[0].PayloadSHA256)
	require.Equal(t, "unavailable", items[1].Status)
	require.NoError(t, f.p.ConsumeDisclosurePermit(fixtureKey, "ctx1", "epoch1", h5.HeaderReference, h5.PayloadSHA256, items[0].Permit))
	require.ErrorIs(t, f.p.ConsumeDisclosurePermit(fixtureKey, "ctx1", "epoch1", h5.HeaderReference, h5.PayloadSHA256, items[0].Permit), ErrReference, "one use")
	require.ErrorIs(t, f.p.ConsumeDisclosurePermit(fixtureKey, "ctx2", "epoch1", h5.HeaderReference, h5.PayloadSHA256, items[0].Permit), ErrReference, "bound to context")

	// Edit after listing: the header reference is stale; no permit is issued.
	f.exec("INSERT INTO message_extra VALUES('5005',9,0,0,'EDITED','h1')")
	stale, _, err := f.p.IssueHistoryDisclosureV1(t.Context(), fixtureKey, "ctx1", "epoch1", []string{h5.HeaderReference})
	require.NoError(t, err)
	require.Equal(t, "unavailable", stale[0].Status)
	require.Empty(t, stale[0].Permit)
	// Revocation after listing also withdraws it.
	f.exec("INSERT INTO message_extra VALUES('5004',3,1,0,NULL,'')")
	gone, _, err := f.p.IssueHistoryDisclosureV1(t.Context(), fixtureKey, "ctx1", "epoch1", []string{p1.Items[1].Header.HeaderReference})
	require.NoError(t, err)
	require.Equal(t, "unavailable", gone[0].Status)

	// Group history cannot be disclosed: original visibility is unknown.
	f.msg("5100", 1, "peer", "group", 2, map[string]any{"type": 1})
	gp, err := f.p.ListHistoryHeadersV1(t.Context(), fixtureKey, ServerSourceReference(2, "group"), nil, 2)
	require.NoError(t, err)
	require.Equal(t, "resolved", gp.Items[0].Status)
	gd, _, err := f.p.IssueHistoryDisclosureV1(t.Context(), fixtureKey, "ctx1", "epoch1", []string{gp.Items[0].Header.HeaderReference})
	require.NoError(t, err)
	require.Equal(t, "unavailable", gd[0].Status)
	require.Equal(t, "disclosure_unavailable", gd[0].Reason)
}

func TestServerMessageFactsConfigAndModeGuards(t *testing.T) {
	s := fixtureStore(t, filepath.Join(t.TempDir(), "guards.sqlite"))
	_, err := NewServerMessageFactProvider(ServerMessageFactConfig{Session: s.Session, Hooks: testServerHooks(), StoreIncarnation: "short", PermitKey: []byte(strings.Repeat("k", 32))})
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = NewServerMessageFactProvider(ServerMessageFactConfig{Session: s.Session, StoreIncarnation: strings.Repeat("5", 32), PermitKey: []byte(strings.Repeat("k", 32))})
	require.ErrorIs(t, err, ErrUnavailable, "shared server hooks are mandatory; no divergent defaults")
	_, err = NewServerMessageFactProvider(ServerMessageFactConfig{Session: s.Session, Hooks: testServerHooks(), StoreIncarnation: strings.Repeat("5", 32), PermitKey: []byte("short")})
	require.ErrorIs(t, err, ErrUnavailable)

	require.NoError(t, RejectPlainMessageSyncFactSource(NativeFactProviderModeServerStoreV1))
	require.ErrorIs(t, RejectPlainMessageSyncFactSource("plain_message_sync"), ErrUnavailable)
	require.ErrorIs(t, RejectPlainMessageSyncFactSource("sql_message_mirror"), ErrUnavailable)

	f := newServerFacts(t)
	_, _, err = f.p.InspectDeliveredHeadersV1(t.Context(), fixtureKey, "bogus", nil)
	require.ErrorIs(t, err, ErrUnavailable)
	require.Error(t, f.p.ValidateIMCut(t.Context(), fixtureKey, "other", strings.Repeat("5", 32), "1", strings.Repeat("a", 32)))
	require.Error(t, f.p.ValidateIMCut(t.Context(), fixtureKey, f.p.authorityID, strings.Repeat("6", 32), "1", strings.Repeat("a", 32)))
	_ = time.Now
}

func TestServerMessageFactsThroughSQLNativeFactAdapter(t *testing.T) {
	ctx := t.Context()
	driver := newSyntheticExternalAuthorityDriver(fixturePin.Digest)
	authority, err := NewFencedAuthorityAdapter(ctx, driver, fixturePin.Digest)
	require.NoError(t, err)
	f := newServerFacts(t)
	f.s.Authority = authority
	f.msg("6001", 1, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space", "content": "TOP-SECRET"})
	f.msg("6002", 2, "peer", f.dmChan, 1, map[string]any{"type": 1}) // untagged: unknown DM space
	f.msg("6003", 3, "peer", "group", 2, map[string]any{"type": 1})
	adapter, err := NewSQLNativeFactAdapter(authority, f.p)
	require.NoError(t, err)
	adapter.testNoLocks = true
	c := &config.Context{}
	require.NoError(t, Install(c, f.s, fixturePin, adapter))
	rt := RuntimeFor(c)
	require.NotNil(t, rt)

	refs := []string{
		ServerDeliveryReference(1, f.dmChan, "6001", "x0_0"),
		ServerDeliveryReference(1, f.dmChan, "6002", "x0_0"),
		ServerDeliveryReference(2, "group", "6003", "x0_0"),
		ServerDeliveryReference(1, f.dmChan, "6001", ""),
		ServerDeliveryReference(1, f.dmChan, "6999", "x0_0"),
	}
	for _, tc := range []struct {
		purpose string
		want    []string // status per ref
	}{
		{"admit", []string{"resolved", "unavailable", "unavailable", "unavailable", "unavailable"}},
		{"reply", []string{"resolved", "unavailable", "resolved", "unavailable", "unavailable"}},
	} {
		list := make([]any, len(refs))
		for i, r := range refs {
			list[i] = map[string]any{"delivery_reference": r}
		}
		req := map[string]any{
			"schema_version": json.Number("1"), "profile_version": json.Number("1"), "request_id": strings.Repeat("b", 32),
			"operation": "resolve", "purpose": tc.purpose, "references": list,
		}
		out, err := rt.Facts(ctx, fixtureKey, bytes.NewReader(canonical(req)))
		require.NoError(t, err, tc.purpose)
		require.NotContains(t, string(out), "TOP-SECRET")
		results := fixtureObject(t, out)["results"].([]any)
		require.Len(t, results, len(refs))
		for i, want := range tc.want {
			require.Equal(t, want, results[i].(map[string]any)["status"], "%s ref %d", tc.purpose, i)
		}
		if tc.purpose == "admit" {
			preds := results[0].(map[string]any)["predicates"].(map[string]any)
			require.Equal(t, true, preds["message_version_exact"])
			require.Equal(t, true, preds["bot_original_visible"])
			require.Equal(t, "predicate_unsupported", results[2].(map[string]any)["reason"])
			require.Equal(t, "mirror_lag", results[4].(map[string]any)["reason"])
		}
	}
}

func sha256OfJSON(v map[string]any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestServerMessageFactsBindDeliveredMessagePositiveAndRaceRefusals(t *testing.T) {
	ctx := t.Context()
	driver := newSyntheticExternalAuthorityDriver(fixturePin.Digest)
	authority, err := NewFencedAuthorityAdapter(ctx, driver, fixturePin.Digest)
	require.NoError(t, err)
	f := newServerFacts(t)
	f.s.Authority = authority

	dmPayload := map[string]any{"type": 1, "space_id": "space", "content": "DM-SECRET-BODY"}
	dmDigest := sha256OfJSON(dmPayload)
	f.msg("7001", 1, "peer", f.dmChan, 1, dmPayload)

	groupPayload := map[string]any{"type": 1, "content": "GROUP-SECRET-BODY"}
	groupDigest := sha256OfJSON(groupPayload)
	f.msg("7002", 2, "peer", "group", 2, groupPayload)

	threadPayload := map[string]any{"type": 1, "content": "THREAD-SECRET-BODY"}
	threadDigest := sha256OfJSON(threadPayload)
	f.msg("7003", 3, "peer", f.threadCh, 5, threadPayload)

	untaggedDMPayload := map[string]any{"type": 1, "content": "UNTAGGED-DM-SECRET"}
	untaggedDMDigest := sha256OfJSON(untaggedDMPayload)
	f.msg("7004", 4, "peer", f.dmChan, 1, untaggedDMPayload)

	crossSpaceDMPayload := map[string]any{"type": 1, "space_id": "other-space", "content": "CROSS-SPACE-SECRET"}
	crossSpaceDMDigest := sha256OfJSON(crossSpaceDMPayload)
	f.msg("7005", 5, "peer", f.dmChan, 1, crossSpaceDMPayload)

	adapter, err := NewSQLNativeFactAdapter(authority, f.p)
	require.NoError(t, err)
	adapter.testNoLocks = true
	c := &config.Context{}
	require.NoError(t, Install(c, f.s, fixturePin, adapter))
	rt := RuntimeFor(c)
	require.NotNil(t, rt)
	require.True(t, rt.HasDeliveryBinding())

	callBind := func(req map[string]any) map[string]any {
		out, err := rt.BindDeliveredMessage(ctx, fixtureKey, bytes.NewReader(canonical(req)))
		require.NoError(t, err)
		for _, secret := range []string{"DM-SECRET-BODY", "GROUP-SECRET-BODY", "THREAD-SECRET-BODY", "UNTAGGED-DM-SECRET", "CROSS-SPACE-SECRET", "EDITED-BODY"} {
			require.NotContains(t, string(out), secret)
		}
		return fixtureObject(t, out)["binding"].(map[string]any)
	}

	// 1. Positive path: space-tagged DM binds with all purposes resolvable and zero unknowns.
	bDM := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-dm-1",
		"channel_type":            json.Number("1"),
		"channel_id":              f.dmChan,
		"message_id":              "7001",
		"message_seq":             "1",
		"from_uid":                "peer",
		"observed_payload_sha256": dmDigest,
	})
	require.Equal(t, "bound", bDM["status"])
	require.Equal(t, true, bDM["current"])
	require.Equal(t, "x0_0", bDM["native_message_version"])
	require.Equal(t, ServerDeliveryReference(1, f.dmChan, "7001", "x0_0"), bDM["delivery_reference"])
	require.Empty(t, bDM["unknown_predicates"].([]any))
	require.Equal(t, []any{"admit", "contributor", "reply"}, bDM["resolvable_purposes"].([]any))

	// 2. Positive path: Group and Thread bind for reply, while explicitly listing
	// caller_original_visible and bot_original_visible in unknown_predicates (never fabricated true).
	bGroup := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-grp-1",
		"channel_type":            json.Number("2"),
		"channel_id":              "group",
		"message_id":              "7002",
		"message_seq":             "2",
		"from_uid":                "peer",
		"observed_payload_sha256": groupDigest,
	})
	require.Equal(t, "bound", bGroup["status"])
	require.Equal(t, []any{"bot_original_visible", "caller_original_visible"}, bGroup["unknown_predicates"].([]any))
	require.Equal(t, []any{"reply"}, bGroup["resolvable_purposes"].([]any))
	grpProven := bGroup["proven_predicates"].(map[string]any)
	require.NotContains(t, grpProven, "caller_original_visible")
	require.NotContains(t, grpProven, "bot_original_visible")

	bThread := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-thd-1",
		"channel_type":            json.Number("5"),
		"channel_id":              f.threadCh,
		"message_id":              "7003",
		"message_seq":             "3",
		"from_uid":                "peer",
		"observed_payload_sha256": threadDigest,
	})
	require.Equal(t, "bound", bThread["status"])
	require.Equal(t, []any{"bot_original_visible", "caller_original_visible"}, bThread["unknown_predicates"].([]any))
	require.Equal(t, []any{"reply"}, bThread["resolvable_purposes"].([]any))

	// 3. Untagged DM binds reference metadata but marks message_space_exact as unknown (non-serving for resolve).
	bUntagged := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-untagged-1",
		"channel_type":            json.Number("1"),
		"channel_id":              f.dmChan,
		"message_id":              "7004",
		"message_seq":             "4",
		"from_uid":                "peer",
		"observed_payload_sha256": untaggedDMDigest,
	})
	require.Equal(t, "bound", bUntagged["status"])
	require.Equal(t, []any{"message_space_exact"}, bUntagged["unknown_predicates"].([]any))
	require.Empty(t, bUntagged["resolvable_purposes"].([]any))
	require.NotContains(t, bUntagged["proven_predicates"].(map[string]any), "message_space_exact")

	// 4. Race/mismatch negative path: concurrent edit in message_extra changes effective payload digest.
	// A current-version lookup alone cannot prove the pre-edit frame's version, so binding with the
	// pre-edit observed_payload_sha256 explicitly refuses with payload_digest_mismatch.
	editedBytes := `{"content":"EDITED-BODY","space_id":"space","type":1}`
	editedSum := sha256.Sum256([]byte(editedBytes))
	editedDigest := hex.EncodeToString(editedSum[:])
	f.exec("INSERT INTO message_extra VALUES('7001',4,0,0,?,'e44')", editedBytes)

	bRace := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-race-1",
		"channel_type":            json.Number("1"),
		"channel_id":              f.dmChan,
		"message_id":              "7001",
		"message_seq":             "1",
		"from_uid":                "peer",
		"observed_payload_sha256": dmDigest,
	})
	require.Equal(t, "refused", bRace["status"])
	require.Equal(t, "payload_digest_mismatch", bRace["reason"])
	require.Equal(t, false, bRace["current"])
	require.Equal(t, "x4_e44", bRace["native_message_version"])

	// Stale expected_native_version also refuses with version_mismatch even if digest matches edited payload.
	bStaleVer := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-ver-1",
		"channel_type":            json.Number("1"),
		"channel_id":              f.dmChan,
		"message_id":              "7001",
		"message_seq":             "1",
		"from_uid":                "peer",
		"observed_payload_sha256": editedDigest,
		"expected_native_version": "x0_0",
	})
	require.Equal(t, "refused", bStaleVer["status"])
	require.Equal(t, "version_mismatch", bStaleVer["reason"])
	require.Equal(t, "x4_e44", bStaleVer["native_message_version"])

	// Matching edited digest and expected_native_version binds to x4_e44.
	bEditedOK := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-edited-ok",
		"channel_type":            json.Number("1"),
		"channel_id":              f.dmChan,
		"message_id":              "7001",
		"message_seq":             "1",
		"from_uid":                "peer",
		"observed_payload_sha256": editedDigest,
		"expected_native_version": "x4_e44",
	})
	require.Equal(t, "bound", bEditedOK["status"])
	require.Equal(t, ServerDeliveryReference(1, f.dmChan, "7001", "x4_e44"), bEditedOK["delivery_reference"])

	// 5. Sender or sequence mismatch refuses with identity_mismatch.
	bBadSender := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-sender-bad",
		"channel_type":            json.Number("2"),
		"channel_id":              "group",
		"message_id":              "7002",
		"message_seq":             "2",
		"from_uid":                "departed",
		"observed_payload_sha256": groupDigest,
	})
	require.Equal(t, "refused", bBadSender["status"])
	require.Equal(t, "identity_mismatch", bBadSender["reason"])

	bBadSeq := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-seq-bad",
		"channel_type":            json.Number("2"),
		"channel_id":              "group",
		"message_id":              "7002",
		"message_seq":             "99",
		"from_uid":                "peer",
		"observed_payload_sha256": groupDigest,
	})
	require.Equal(t, "refused", bBadSeq["status"])
	require.Equal(t, "identity_mismatch", bBadSeq["reason"])

	// 6. Revoked, missing, and cross-space messages refuse with exact reasons.
	f.exec("UPDATE message_extra SET revoke=1 WHERE message_id='7001'")
	bRevoked := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-revoked",
		"channel_type":            json.Number("1"),
		"channel_id":              f.dmChan,
		"message_id":              "7001",
		"message_seq":             "1",
		"from_uid":                "peer",
		"observed_payload_sha256": editedDigest,
	})
	require.Equal(t, "refused", bRevoked["status"])
	require.Equal(t, "expired_reference", bRevoked["reason"])

	bMissing := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-missing",
		"channel_type":            json.Number("1"),
		"channel_id":              f.dmChan,
		"message_id":              "7999",
		"message_seq":             "99",
		"from_uid":                "peer",
		"observed_payload_sha256": dmDigest,
	})
	require.Equal(t, "refused", bMissing["status"])
	require.Equal(t, "mirror_lag", bMissing["reason"])

	bCrossSpace := callBind(map[string]any{
		"schema_version":          json.Number("1"),
		"operation":               "bind_delivery",
		"request_id":              "req-bind-cross-space",
		"channel_type":            json.Number("1"),
		"channel_id":              f.dmChan,
		"message_id":              "7005",
		"message_seq":             "5",
		"from_uid":                "peer",
		"observed_payload_sha256": crossSpaceDMDigest,
	})
	require.Equal(t, "refused", bCrossSpace["status"])
	require.Equal(t, "visibility_denied", bCrossSpace["reason"])
}

func TestServerMessageFactsDurablePermitLedgerSurvivesRestart(t *testing.T) {
	f := newServerFacts(t)
	f.exec(ServerDisclosurePermitDDL)

	p1, err := NewServerMessageFactProvider(ServerMessageFactConfig{
		Session:             f.s.Session,
		Hooks:               testServerHooks(),
		StoreIncarnation:    strings.Repeat("5", 32),
		PermitKey:           []byte(strings.Repeat("k", 32)),
		DurablePermitLedger: true,
	})
	require.NoError(t, err)

	f.msg("8001", 1, "peer", f.dmChan, 1, map[string]any{"type": 1, "space_id": "space", "content": "PERMIT-BODY"})
	src := ServerSourceReference(1, f.dmChan)
	page, err := p1.ListHistoryHeadersV1(t.Context(), fixtureKey, src, nil, 2)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	hdr := page.Items[0].Header

	items, _, err := p1.IssueHistoryDisclosureV1(t.Context(), fixtureKey, "ctx-durable", "1", []string{hdr.HeaderReference})
	require.NoError(t, err)
	require.Equal(t, "resolved", items[0].Status)
	permit := items[0].Permit
	require.NotEmpty(t, permit)

	// Consume the permit once in process 1.
	require.NoError(t, p1.ConsumeDisclosurePermit(fixtureKey, "ctx-durable", "1", hdr.HeaderReference, hdr.PayloadSHA256, permit))

	// Simulate a process restart by constructing a brand-new provider instance
	// with a fresh in-memory map against the same SQLite database.
	p2, err := NewServerMessageFactProvider(ServerMessageFactConfig{
		Session:             f.s.Session,
		Hooks:               testServerHooks(),
		StoreIncarnation:    strings.Repeat("5", 32),
		PermitKey:           []byte(strings.Repeat("k", 32)),
		DurablePermitLedger: true,
	})
	require.NoError(t, err)

	// Replaying the consumed permit after restart is rejected by the SQL permit ledger.
	require.ErrorIs(t, p2.ConsumeDisclosurePermit(fixtureKey, "ctx-durable", "1", hdr.HeaderReference, hdr.PayloadSHA256, permit), ErrReference)

	// Re-requesting IssueHistoryDisclosureV1 for the already-consumed (context, epoch, headerRef)
	// after restart refuses to resurrect the consumed permit.
	reissue, _, err := p2.IssueHistoryDisclosureV1(t.Context(), fixtureKey, "ctx-durable", "1", []string{hdr.HeaderReference})
	require.NoError(t, err)
	require.Equal(t, "unavailable", reissue[0].Status)
	require.Equal(t, "disclosure_unavailable", reissue[0].Reason)
}
