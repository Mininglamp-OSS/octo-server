package thread

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// PR-C review round 9 (yujiawei): the thread source-message copy is a write
// path to a persisted card frame, and it was outside the marker trust boundary.
//
// CreateThread fixes the copied message's from_uid by reading the source row
// ("防止客户端伪造") but took the payload from the request. So any active group
// member could persist a message in a CommunityTopic channel whose from_uid is
// a real bot's and whose content they wrote — including the server-only
// top-level template_ref / catalog_provenance, which the action path then reads
// as trusted producer identity. cardmsg.Validate never runs on this path, and
// it is not one of the four ingresses that reject those keys by name.
//
// The fix is that the copy uses the server's own bytes, which takes
// source_message_payload out of the trust boundary entirely rather than adding
// a fifth key-rejection — a rejection would break the legitimate case, since a
// client echoing a card it was legitimately sent would be carrying real
// markers.
//
// This is a source guard rather than a behavioural test because the property is
// "these bytes never reach that call", which is exactly the kind of thing a
// later refactor reintroduces by accident.
func TestSourceMessageCopyNeverUsesCallerPayload(t *testing.T) {
	source, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	text := string(source)

	call := regexp.MustCompile(`sendSourceMessage\([^)]*\)`)
	found := call.FindAllString(text, -1)
	if len(found) == 0 {
		t.Fatal("no sendSourceMessage call found; this guard has lost its subject")
	}
	for _, site := range found {
		if strings.Contains(site, "SourceMessagePayload") {
			t.Fatalf("the thread copy passes the caller's payload to the copied message: %s\n"+
				"It must pass the payload read from the source row, or a group member can "+
				"persist forged server-only card markers under another sender's identity.", site)
		}
	}

	// The query the copy must read from has to return both halves of the source
	// row; a from_uid-only query is what left the payload caller-controlled.
	if !strings.Contains(text, "QueryMessageSource(") {
		t.Fatal("the copy no longer reads the source row's payload via QueryMessageSource")
	}
}
