package thread

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// PR-C review rounds 9–10 (yujiawei): the thread source-message copy is a write
// path to persisted messages, and it was outside the marker trust boundary.
//
// CreateThread fixes the copied message's from_uid by reading the source row
// ("防止客户端伪造") but took the content from the request. So any active group
// member could persist a message in a CommunityTopic channel whose from_uid is
// a real bot's and whose content they wrote — including the server-only
// top-level template_ref / catalog_provenance, which the action path then reads
// as trusted producer identity. cardmsg.Validate never runs on this path, and
// it is not one of the four ingresses that reject those keys by name.
//
// The fix reads the content from the same row, which takes the request field
// out of the trust boundary rather than adding a fifth key-rejection — a
// rejection would break the legitimate case, since a client echoing back a card
// it was legitimately sent carries real markers.
//
// This guard covers the *field*, not one call site. The first version regexed
// `sendSourceMessage(...)` only, and was green while the same field still
// reached sendThreadCreatedMessage and from there the persisted parent-group
// notification's last_message preview — the exact "a later refactor
// reintroduces it by accident" case the guard was written for, present at the
// moment it was written.
func TestSourceMessagePayloadNeverLeavesTheDecisionToCopy(t *testing.T) {
	raw, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	code := stripGoComments(string(raw))

	if !strings.Contains(code, "QueryMessageSource(") {
		t.Fatal("the copy no longer reads the source row's payload via QueryMessageSource")
	}

	// Every surviving use must be either the struct field declaration or the
	// len() test that decides whether to copy at all. Anything else means the
	// caller's bytes are being read for their content.
	declaration := regexp.MustCompile(`SourceMessagePayload\s+json\.RawMessage`)
	decision := regexp.MustCompile(`len\(req\.SourceMessagePayload\)`)
	for _, line := range strings.Split(code, "\n") {
		if !strings.Contains(line, "SourceMessagePayload") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if declaration.MatchString(trimmed) || decision.MatchString(trimmed) {
			continue
		}
		t.Fatalf("the caller's source payload is used for its content, not just to decide whether to copy:\n  %s\n"+
			"Content must come from QueryMessageSource. Otherwise a group member can put chosen bytes "+
			"into a persisted message — under another sender's identity on the copy path, and into the "+
			"parent group's last_message preview on the notification path.", trimmed)
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
// message_count and the last_message preview used to be computed from
// len(req.SourceMessagePayload) while whether a copy happened came from the
// database read — two different values since the content moved server-side. A
// missing or empty source row produced a notification announcing a first
// message the new thread does not contain, with no later event to correct it.
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
		copied := []byte(`{"type":1,"content":"服务端存下来的内容"}`)
		payload := buildThreadCreatedPayload(shortID, name, channelID, "u-1", "创建者", &sourceID, copied)
		if got := payload["message_count"]; got != int64(1) {
			t.Fatalf("message_count = %v, want 1", got)
		}
		last, ok := payload["last_message"].(map[string]interface{})
		if !ok {
			t.Fatalf("last_message missing or wrong shape: %+v", payload["last_message"])
		}
		if last["content"] != "服务端存下来的内容" {
			t.Fatalf("preview content = %v, want the copied bytes' content", last["content"])
		}
	})
}
