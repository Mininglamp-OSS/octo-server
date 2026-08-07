package thread

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// PR-C review rounds 9–12 (yujiawei, Jerry-Xin): the thread source-message copy
// persists caller-supplied JSON under the *source message's* sender, and the id
// and the payload are never checked against each other. Three enforcement
// shapes were tried before this one, and the first two were each correct about
// the property they targeted while the next consequence went un-enumerated.
//
//   - Copy the caller's bytes verbatim (original). Any group member could
//     persist a message whose from_uid is a real bot's and whose top-level
//     template_ref / catalog_provenance they wrote.
//   - Substitute the server's stored bytes (round 10). Closed forgery, opened
//     an unchecked read: a single-message read applies five gates — visibles,
//     revoke/is_deleted, per-user deletion, both history offsets, Expire — and
//     a bare SELECT payload by id applies none, so a member could have the
//     server re-publish a revoked message's unredacted body under its original
//     sender into a channel the whole group is subscribed to.
//   - Strip the two top-level keys (round 11). Narrowed forgery without closing
//     it. The identity the action route actually falls back to is
//     card.metadata.octo.template, which lives *inside* the card body the
//     caller controls. A markerless frame naming a frozen-known template takes
//     the legacy-compatible branch, and static ActionContext does not authorize
//     the principal — so a forged card plus chosen Action.Submit data is
//     accepted as a trusted bot card, bypassing the "users cannot send cards"
//     ban entirely.
//
// Refusing type-17 outright is what closes it. It matches the user ingress in
// modules/message/api.go, where card rejection is absolute — independent of
// channel type and friendship, and taken before any DB work. The cost is that a
// thread created from a card message has no first message; that is deliberate.
func TestSourceMessageCopyRefusesCards(t *testing.T) {
	raw, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	code := stripGoComments(string(raw))

	if !strings.Contains(code, "isCardSourcePayload(req.SourceMessagePayload)") {
		t.Fatal("the copy path no longer refuses card payloads; stripping keys is not sufficient, " +
			"because the identity the action route trusts lives inside the card body")
	}

	// Reading stored content by message id would reintroduce the round-10
	// bypass, which applies none of the five gates a single-message read does.
	dbRaw, err := os.ReadFile("db.go")
	if err != nil {
		t.Fatalf("read db.go: %v", err)
	}
	payloadRead := regexp.MustCompile(`Select\([^)]*"payload"`)
	for _, line := range strings.Split(stripGoComments(string(dbRaw)), "\n") {
		if payloadRead.MatchString(line) {
			t.Fatalf("the copy path reads message content out of the message table by id:\n  %s\n"+
				"That bypasses visibles / revoke / per-user deletion / both history offsets / Expire.",
				strings.TrimSpace(line))
		}
	}
}

// stripGoComments removes // and /* */ comments so the guards read code rather
// than the prose describing it — the comments here necessarily name the field.
func stripGoComments(source string) string {
	source = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(source, "")
	lines := strings.Split(source, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "//"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// The parent-group notification must describe the thread that actually exists.
// message_count and the last_message preview are derived from the bytes that
// were actually copied — empty when the payload was a refused card, when the
// send failed, or when no source was given.
func TestThreadCreatedNotificationDescribesTheCopyThatHappened(t *testing.T) {
	const shortID, name, channelID = "t-1", "设计评审", "g-1@t-1"
	sourceID := int64(9001)

	t.Run("no copy issued means no first message is advertised", func(t *testing.T) {
		payload := buildThreadCreatedPayload(shortID, name, channelID, "u-1", "创建者", &sourceID, nil)
		if got := payload["message_count"]; got != int64(0) {
			t.Fatalf("message_count = %v, want 0 when nothing was copied", got)
		}
		if _, present := payload["last_message"]; present {
			t.Fatalf("a preview was advertised for a message the thread does not contain: %+v", payload["last_message"])
		}
	})

	t.Run("a copy that happened is advertised from the copied bytes", func(t *testing.T) {
		copied := []byte(`{"type":1,"content":"实际拷贝进去的内容"}`)
		payload := buildThreadCreatedPayload(shortID, name, channelID, "u-1", "创建者", &sourceID, copied)
		if got := payload["message_count"]; got != int64(1) {
			t.Fatalf("message_count = %v, want 1", got)
		}
		last, ok := payload["last_message"].(map[string]interface{})
		if !ok {
			t.Fatalf("last_message missing or wrong shape: %+v", payload["last_message"])
		}
		if last["content"] != "实际拷贝进去的内容" {
			t.Fatalf("preview content = %v, want the copied bytes' content", last["content"])
		}
	})
}

// The refusal has to key off the wire type, not off the marker keys — a frame
// with the markers already removed is still a card, and still carries the
// in-body template identity that makes it actionable.
func TestCardDetectionCoversMarkerlessFrames(t *testing.T) {
	markerless := []byte(`{"type":17,"card_version":"1.5","profile":"octo/v2",` +
		`"card":{"type":"AdaptiveCard","metadata":{"octo":{"protocol":"octo-card@1.0",` +
		`"template":{"id":"docs.access-request","version":"0.3.0"}}}}}`)
	var probe map[string]interface{}
	if err := json.Unmarshal(markerless, &probe); err != nil {
		t.Fatal(err)
	}
	if _, hasRef := probe["template_ref"]; hasRef {
		t.Fatal("fixture is not markerless")
	}
	if _, hasProvenance := probe["catalog_provenance"]; hasProvenance {
		t.Fatal("fixture is not markerless")
	}
	if !isCardSourcePayload(markerless) {
		t.Fatal("a markerless type-17 frame was not recognised as a card; " +
			"it still carries metadata.octo.template, which is the identity the action route trusts")
	}
	if isCardSourcePayload([]byte(`{"type":1,"content":"文本"}`)) {
		t.Fatal("a plain text message was refused as a card")
	}
}

// A refused or failed copy must not leave the parent group announcing a source
// the thread does not have. source_message_id, message_count and the preview
// all describe the same thing, so they are derived from the same fact: whether
// bytes were actually copied.
//
// The input that matters is the *inconsistent* one — a request that named a
// source paired with nothing copied — because that is the state CreateThread
// hands over for a refused card or a failed send. An earlier revision of this
// test passed an already-nil id, which asserted only that nil stays nil: the
// decision it meant to guard lived at the call site and deleting it kept the
// test green (lml2468, review round 13). The decision now lives in the builder,
// so driving the builder drives the decision.
func TestThreadCreatedNotificationOmitsTheSourceWhenNothingWasCopied(t *testing.T) {
	sourceID := int64(9001)

	refused := buildThreadCreatedPayload("t-1", "设计评审", "g-1@t-1", "u-1", "创建者", &sourceID, nil)
	if got, present := refused["source_message_id"]; present {
		t.Fatalf("the parent group announced source_message_id=%v for a message the thread "+
			"does not contain; a client following that id finds an empty thread", got)
	}
	if got := refused["message_count"]; got != int64(0) {
		t.Fatalf("message_count = %v, want 0 when the copy was refused", got)
	}

	withCopy := buildThreadCreatedPayload("t-1", "设计评审", "g-1@t-1", "u-1", "创建者",
		&sourceID, []byte(`{"type":1,"content":"文本"}`))
	if withCopy["source_message_id"] != sourceID {
		t.Fatalf("source_message_id = %v, want %d when the copy happened", withCopy["source_message_id"], sourceID)
	}
}

// The caller-supplied payload is persisted, and the request contract has to say
// so. An earlier revision read the content server-side, and when that was
// reverted the comment stating "this field must never reach persisted data"
// stayed behind — a false rule on the one exported field whose misuse is an
// impersonation vector, which is the highest-value thing to get wrong.
func TestSourceMessagePayloadContractCommentMatchesTheCode(t *testing.T) {
	raw, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	text := string(raw)
	idx := strings.Index(text, "SourceMessagePayload json.RawMessage")
	if idx < 0 {
		t.Fatal("SourceMessagePayload field not found")
	}
	// The doc comment immediately above the field.
	head := text[:idx]
	blockStart := strings.LastIndex(head, "\n\n")
	comment := head[blockStart:]

	for _, stale := range []string{
		"不得流向任何被持久化的字段",
		"内容一律以服务端从消息行",
	} {
		if strings.Contains(comment, stale) {
			t.Fatalf("the field's contract still claims %q, but the code persists the caller's bytes "+
				"and verifies only from_uid", stale)
		}
	}
	if !strings.Contains(comment, "会被原样持久化") {
		t.Fatal("the field's contract no longer states that the caller's bytes are persisted; " +
			"a consumer written against a softer claim reintroduces the impersonation path")
	}
}
