package robot

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestStripBareMentionAllForBot_StripsAllAndInjectsHumans(t *testing.T) {
	in := []byte(`{"type":1,"content":"hi","mention":{"all":1}}`)
	out := stripBareMentionAllForBot(in)

	if gjson.GetBytes(out, "mention.all").Exists() {
		t.Fatalf("expected mention.all to be stripped, got %s", out)
	}
	if gjson.GetBytes(out, "mention.humans").Int() != 1 {
		t.Fatalf("expected mention.humans=1, got %s", out)
	}
}

func TestStripBareMentionAllForBot_PreservesAisAndUids(t *testing.T) {
	in := []byte(`{"type":1,"mention":{"all":1,"ais":1,"uids":["bot_a"]}}`)
	out := stripBareMentionAllForBot(in)

	if gjson.GetBytes(out, "mention.all").Exists() {
		t.Fatalf("expected mention.all stripped, got %s", out)
	}
	if gjson.GetBytes(out, "mention.ais").Int() != 1 {
		t.Fatalf("expected mention.ais preserved, got %s", out)
	}
	if gjson.GetBytes(out, "mention.humans").Int() != 1 {
		t.Fatalf("expected mention.humans injected, got %s", out)
	}
	uids := gjson.GetBytes(out, "mention.uids").Array()
	if len(uids) != 1 || uids[0].String() != "bot_a" {
		t.Fatalf("expected mention.uids preserved, got %s", out)
	}
}

func TestStripBareMentionAllForBot_KeepsExistingHumans(t *testing.T) {
	// humans already set: do not clobber, just strip all.
	in := []byte(`{"mention":{"all":1,"humans":1}}`)
	out := stripBareMentionAllForBot(in)
	if gjson.GetBytes(out, "mention.all").Exists() {
		t.Fatalf("expected mention.all stripped, got %s", out)
	}
	if gjson.GetBytes(out, "mention.humans").Int() != 1 {
		t.Fatalf("expected mention.humans=1, got %s", out)
	}
}

func TestStripBareMentionAllForBot_NoOpWithoutAll(t *testing.T) {
	in := []byte(`{"type":1,"mention":{"ais":1,"uids":["bot_a"]}}`)
	out := stripBareMentionAllForBot(in)
	if string(out) != string(in) {
		t.Fatalf("expected no-op, got %s", out)
	}
}

func TestStripBareMentionAllForBot_NoOpWithoutMention(t *testing.T) {
	in := []byte(`{"type":1,"content":"hi"}`)
	out := stripBareMentionAllForBot(in)
	if string(out) != string(in) {
		t.Fatalf("expected no-op, got %s", out)
	}
}

func TestStripBareMentionAllForBot_FalsyAllIsNoOp(t *testing.T) {
	in := []byte(`{"mention":{"all":0}}`)
	out := stripBareMentionAllForBot(in)
	if string(out) != string(in) {
		t.Fatalf("expected no-op for all=0, got %s", out)
	}
}

func TestStripBareMentionAllForBot_Idempotent(t *testing.T) {
	in := []byte(`{"mention":{"all":1}}`)
	once := stripBareMentionAllForBot(in)
	twice := stripBareMentionAllForBot(once)
	if string(once) != string(twice) {
		t.Fatalf("expected idempotent, once=%s twice=%s", once, twice)
	}
}

func TestStripBareMentionAllForBot_MalformedPayloadIsNoOp(t *testing.T) {
	in := []byte(`not json`)
	out := stripBareMentionAllForBot(in)
	if string(out) != string(in) {
		t.Fatalf("expected malformed payload returned unchanged, got %s", out)
	}
}

func TestStripBareMentionAllForBot_PreservesMessageIDPrecision(t *testing.T) {
	in := []byte(`{"message_id":9223372036854775807,"mention":{"all":1}}`)
	out := stripBareMentionAllForBot(in)
	var doc map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if n, ok := doc["message_id"].(json.Number); !ok || n.String() != "9223372036854775807" {
		t.Fatalf("message_id precision lost, got %v (%s)", doc["message_id"], out)
	}
}

func TestStripBareMentionAllForBot_DoesNotMutateCaller(t *testing.T) {
	in := []byte(`{"mention":{"all":1}}`)
	orig := string(in)
	_ = stripBareMentionAllForBot(in)
	if string(in) != orig {
		t.Fatalf("caller payload mutated: %s", in)
	}
}
