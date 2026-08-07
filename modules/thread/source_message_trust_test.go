package thread

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

// PR-C review rounds 9–11 (yujiawei): the thread source-message copy writes a
// persisted message from bytes that arrived from outside, so it is a boundary
// for the server-only catalog markers, and it took three attempts to place the
// enforcement correctly. The two rejected shapes are recorded here because each
// was defensible in isolation and wrong in consequence.
//
//   - Copy the caller's payload verbatim (original). Any active group member
//     could persist a message whose from_uid is a real bot's and whose
//     top-level template_ref / catalog_provenance they wrote, which the action
//     path then reads as trusted producer identity.
//   - Substitute the server's stored bytes (round 10). That closed forgery and
//     opened something worse: message_id became an unchecked read credential.
//     A single-message read applies five gates — visibles, revoke/is_deleted,
//     per-user deletion, both history offsets, Expire — and a bare
//     SELECT payload applies none, so a member could have the server
//     re-publish a revoked message's unredacted body (redaction happens in the
//     response layer, not in storage) under its original sender, into a channel
//     the whole group is subscribed to.
//   - Strip the two keys from the caller's payload (now). The property needed
//     is "these keys cannot arrive from outside", and stripping is the minimal
//     enforcement of exactly that: the caller can only supply bytes they
//     already hold, so no authorization surface is required, and the result is
//     a legal unmarked frame — which is what a copy should be, since it never
//     passed the rendering boundary that authored the original.
func TestSourceMessageCopyStripsServerOnlyMarkers(t *testing.T) {
	marked := []byte(`{"type":17,"card_version":"1.5","profile":"octo/v2",` +
		`"template_ref":{"id":"docs.access-request","version":"0.3.0"},` +
		`"catalog_provenance":{"version":1,"principal_type":"internal_producer",` +
		`"principal_id":"docs-notify","space_id":"space-1"},` +
		`"card":{"type":"AdaptiveCard","body":[]}}`)

	stripped, err := cardmsg.StripCatalogMarkers(marked)
	if err != nil {
		t.Fatalf("StripCatalogMarkers: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(stripped, &decoded); err != nil {
		t.Fatalf("stripped payload is not valid JSON: %v", err)
	}
	if _, present := decoded[cardmsg.CatalogTemplateRefKey]; present {
		t.Fatal("template_ref survived the strip")
	}
	if _, present := decoded[cardmsg.CatalogProvenanceKey]; present {
		t.Fatal("catalog_provenance survived the strip")
	}
	// Everything else is the caller's own content and must be preserved — the
	// legitimate case is a client echoing back a card it was really sent.
	if decoded["type"] != float64(17) || decoded["profile"] != "octo/v2" {
		t.Fatalf("the strip damaged the caller's frame: %+v", decoded)
	}
	if _, present := decoded["card"]; !present {
		t.Fatal("the strip dropped the card body")
	}

	t.Run("an unmarked payload is returned byte-identical", func(t *testing.T) {
		plain := []byte(`{"type":1,"content":"你好"}`)
		out, err := cardmsg.StripCatalogMarkers(plain)
		if err != nil {
			t.Fatalf("StripCatalogMarkers: %v", err)
		}
		if string(out) != string(plain) {
			t.Fatalf("an unmarked payload was rewritten: %s", out)
		}
	})

	t.Run("a non-object payload is passed through", func(t *testing.T) {
		for _, raw := range []string{`"just a string"`, `[1,2,3]`, `not json at all`} {
			out, err := cardmsg.StripCatalogMarkers([]byte(raw))
			if err != nil {
				t.Fatalf("StripCatalogMarkers(%s): %v", raw, err)
			}
			if string(out) != raw {
				t.Fatalf("payload %s was rewritten to %s", raw, out)
			}
		}
	})
}

// The guard is over the boundary, not over one call site. Its first version
// regexed sendSourceMessage only and was green while the same field still
// reached sendThreadCreatedMessage and the persisted parent-group preview.
//
// What must hold now: the copy path never passes req.SourceMessagePayload on
// without stripping it, and it does not read message content out of the message
// table by id.
func TestSourceMessageCopyEnforcesTheBoundaryInSource(t *testing.T) {
	raw, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	code := stripGoComments(string(raw))

	if !strings.Contains(code, "cardmsg.StripCatalogMarkers(req.SourceMessagePayload)") {
		t.Fatal("the caller's payload is no longer stripped before it is copied")
	}

	// Reading stored content by message id would reintroduce the round-10
	// bypass: that read applies none of the five gates a single-message read
	// applies, so it hands back revoked, deleted, visibles-excluded and
	// pre-offset content on the strength of an id alone.
	//
	// The query lives in db.go, so this half of the guard has to read that file
	// — the first version checked service.go only and was blind to the very
	// mutation it names.
	dbRaw, err := os.ReadFile("db.go")
	if err != nil {
		t.Fatalf("read db.go: %v", err)
	}
	dbCode := stripGoComments(string(dbRaw))
	payloadRead := regexp.MustCompile(`Select\([^)]*"payload"`)
	for _, line := range strings.Split(dbCode, "\n") {
		if payloadRead.MatchString(line) {
			t.Fatalf("the copy path reads message content out of the message table by id:\n  %s\n"+
				"That bypasses visibles / revoke / per-user deletion / both history offsets / Expire, "+
				"so a member can have the server re-publish content they may not read.",
				strings.TrimSpace(line))
		}
	}

	declaration := regexp.MustCompile(`SourceMessagePayload\s+json\.RawMessage`)
	stripCall := regexp.MustCompile(`StripCatalogMarkers\(req\.SourceMessagePayload\)`)
	decision := regexp.MustCompile(`len\(req\.SourceMessagePayload\)`)
	for _, line := range strings.Split(code, "\n") {
		if !strings.Contains(line, "SourceMessagePayload") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if declaration.MatchString(trimmed) || stripCall.MatchString(trimmed) || decision.MatchString(trimmed) {
			continue
		}
		t.Fatalf("the caller's payload is used unstripped:\n  %s\n"+
			"Every use must be the declaration, the len() test that decides whether to copy, "+
			"or the strip itself.", trimmed)
	}
}

// stripGoComments removes // and /* */ comments so the guard reads code rather
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
//
// message_count and the last_message preview are derived from the bytes
// actually copied, not from the request field. Those were the same value before
// round 10 and diverged when the content moved server-side; they are the same
// value again now, and deriving from the copy keeps them correct if the copy is
// ever skipped for another reason.
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
