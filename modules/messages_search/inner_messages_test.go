package messages_search

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/olivere/elastic"
)

// TestSingleMessageHit_ForwardCarriesInnerMessages pins the projection step
// for the forward (type=11) path: the outer hit is forward-kinded, carries
// outer_preview.child_count, and surfaces every msgs[] entry as an
// inner_messages[] item with sender_id / sent_at populated when the indexer
// has written them.
func TestSingleMessageHit_ForwardCarriesInnerMessages(t *testing.T) {
	tp := payloadTypeMergeForward
	doc := Doc{
		MessageID:  500,
		MessageSeq: 12,
		From:       "u_outer",
		Timestamp:  1717000500,
		Payload: &Payload{
			Type: &tp,
			MergeForward: &MergeForwardPayload{
				ChildCount: 2,
				Msgs: []MergeForwardMsg{
					{MessageID: 600, Type: 1, SearchText: "hello", From: "u1", Timestamp: 1717000060},
					{MessageID: 601, Type: 8, SearchText: "doc.pdf", From: "u2", Timestamp: 1717000120},
				},
			},
		},
	}
	h := &Handler{cfg: SearchConfig{}}
	hit := h.singleMessageHit(doc, "G1", 0, nil)

	if hit.MessageKind != "forward" {
		t.Fatalf("kind: got %q", hit.MessageKind)
	}
	if hit.OuterPreview == nil || hit.OuterPreview.ChildCount != 2 {
		t.Fatalf("outer_preview: %+v", hit.OuterPreview)
	}
	if len(hit.InnerMessages) != 2 {
		t.Fatalf("inner_messages len: got %d", len(hit.InnerMessages))
	}
	if hit.InnerMessages[0].MessageID != "600" || hit.InnerMessages[0].SenderID != "u1" {
		t.Errorf("inner[0]: %+v", hit.InnerMessages[0])
	}
	if hit.InnerMessages[0].SentAt == "" {
		t.Errorf("inner[0].sent_at must be populated when timestamp>0")
	}
	if hit.InnerMessages[1].SearchText != "doc.pdf" {
		t.Errorf("inner[1].search_text: %q", hit.InnerMessages[1].SearchText)
	}
}

// TestSingleMessageHit_NonForwardHasNoInnerMessages: text/file/etc must NOT
// surface inner_messages. omitempty drops the key from the wire form.
func TestSingleMessageHit_NonForwardHasNoInnerMessages(t *testing.T) {
	tp := payloadTypeText
	doc := Doc{
		MessageID: 7,
		Timestamp: 1717000000,
		Payload:   &Payload{Type: &tp, Text: &TextPayload{Content: "plain"}},
	}
	h := &Handler{cfg: SearchConfig{}}
	hit := h.singleMessageHit(doc, "G1", 0, nil)
	if hit.InnerMessages != nil {
		t.Fatalf("text hit must not carry inner_messages: %+v", hit.InnerMessages)
	}
	out, err := json.Marshal(hit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(out); contains(got, "inner_messages") {
		t.Fatalf("wire form must omit inner_messages, got %s", got)
	}
}

// TestBuildMessageHits_SenderJoinFillsInnerNames asserts the end-to-end
// behaviour: a single page with both an outer sender and forward-child
// senders triggers exactly ONE GetUsers/GetMembers round trip and the
// resulting names land on inner_messages[].sender_name.
func TestBuildMessageHits_SenderJoinFillsInnerNames(t *testing.T) {
	uSvc := &stubUserSvc{
		users: []*user.Resp{
			{UID: "u_outer", Name: "Outer"},
			{UID: "u1", Name: "Alice"},
			{UID: "u2", Name: "Bob"},
		},
	}
	gSvc := &stubGroupSvc{
		members: []*group.MemberResp{
			{UID: "u1", Remark: "AliceRemark"}, // group remark wins over user.Name
		},
	}
	h := newTestHandler(uSvc, gSvc)

	tp := payloadTypeMergeForward
	src, _ := json.Marshal(map[string]any{
		"messageId":  500,
		"messageSeq": 12,
		"from":       "u_outer",
		"channelId":  "G1",
		"timestamp":  int64(1717000500),
		"payload": map[string]any{
			"type": tp,
			"mergeForward": map[string]any{
				"childCount": 2,
				"msgs": []map[string]any{
					{"messageId": 600, "type": 1, "searchText": "hello", "from": "u1", "timestamp": int64(1717000060)},
					{"messageId": 601, "type": 1, "searchText": "world", "from": "u2", "timestamp": int64(1717000120)},
				},
			},
		},
	})
	rawSrc := json.RawMessage(src)
	hits := []*elastic.SearchHit{{Source: &rawSrc}}

	out := h.buildMessageHits(context.Background(), hits, SearchMessagesReq{
		ChannelType: channelTypeGroup,
		ChannelID:   "G1",
	}, "viewer")
	if len(out) != 1 {
		t.Fatalf("len: got %d", len(out))
	}
	hit := out[0]
	if hit.SenderName != "Outer" {
		t.Errorf("outer sender_name: got %q", hit.SenderName)
	}
	if len(hit.InnerMessages) != 2 {
		t.Fatalf("inner len: %d", len(hit.InnerMessages))
	}
	if hit.InnerMessages[0].SenderName != "AliceRemark" {
		t.Errorf("inner[0] should pick group remark, got %q", hit.InnerMessages[0].SenderName)
	}
	if hit.InnerMessages[1].SenderName != "Bob" {
		t.Errorf("inner[1] should fall back to user.Name, got %q", hit.InnerMessages[1].SenderName)
	}
	// Single page → single GetUsers / GetMembers batch (regression guard
	// against per-child round trips).
	if uSvc.calls != 1 {
		t.Errorf("expected 1 GetUsers call, got %d", uSvc.calls)
	}
	if gSvc.calls != 1 {
		t.Errorf("expected 1 GetMembers call, got %d", gSvc.calls)
	}
}

// TestBuildMessageHits_PartialInnerSenderOmitted: when the indexer hasn't yet
// written msgs[].from / msgs[].timestamp, the inner item must NOT carry a
// sender_id (and therefore no sender_name lookup) on the wire.
func TestBuildMessageHits_PartialInnerSenderOmitted(t *testing.T) {
	uSvc := &stubUserSvc{users: []*user.Resp{{UID: "u_outer", Name: "Outer"}}}
	gSvc := &stubGroupSvc{}
	h := newTestHandler(uSvc, gSvc)

	src, _ := json.Marshal(map[string]any{
		"messageId":  501,
		"messageSeq": 13,
		"from":       "u_outer",
		"channelId":  "G1",
		"timestamp":  int64(1717000600),
		"payload": map[string]any{
			"type": payloadTypeMergeForward,
			"mergeForward": map[string]any{
				"childCount": 1,
				"msgs": []map[string]any{
					{"messageId": 700, "type": 1, "searchText": "legacy"},
				},
			},
		},
	})
	rawSrc := json.RawMessage(src)
	hits := []*elastic.SearchHit{{Source: &rawSrc}}

	out := h.buildMessageHits(context.Background(), hits, SearchMessagesReq{
		ChannelType: channelTypeGroup,
		ChannelID:   "G1",
	}, "viewer")
	if len(out) != 1 || len(out[0].InnerMessages) != 1 {
		t.Fatalf("shape: %+v", out)
	}
	im := out[0].InnerMessages[0]
	if im.SenderID != "" || im.SenderName != "" || im.SentAt != "" {
		t.Errorf("legacy inner item must not surface sender/timestamp: %+v", im)
	}
	wire, _ := json.Marshal(im)
	for _, k := range []string{`"sender_id"`, `"sender_name"`, `"sent_at"`} {
		if contains(string(wire), k) {
			t.Errorf("legacy wire form must omit %s, got %s", k, wire)
		}
	}
}

// TestBuildMessageHits_EmptyMsgsHasNoInnerMessages: a forward card with
// childCount but an empty msgs[] array (or msgs key absent) must omit the
// inner_messages field entirely so the client can't read "[]" as a signal.
func TestBuildMessageHits_EmptyMsgsHasNoInnerMessages(t *testing.T) {
	tp := payloadTypeMergeForward
	doc := Doc{
		MessageID: 9,
		Timestamp: 1717000000,
		Payload: &Payload{
			Type:         &tp,
			MergeForward: &MergeForwardPayload{ChildCount: 3 /* msgs nil */},
		},
	}
	h := &Handler{cfg: SearchConfig{}}
	hit := h.singleMessageHit(doc, "G1", 0, nil)
	if hit.InnerMessages != nil {
		t.Fatalf("empty msgs[] must not surface inner_messages: %+v", hit.InnerMessages)
	}
}

// contains is a tiny helper to keep test imports lean (avoid pulling strings
// into multiple test files).
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
