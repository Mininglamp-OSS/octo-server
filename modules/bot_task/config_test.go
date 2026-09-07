package bot_task

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSourceRegistry(t *testing.T) {
	token := strings.Repeat("a", minSourceTokenBytes)
	registry, err := parseSourceRegistry(`{"loop":{"token":"` + token + `","enabled":true,"allowed_bot_uids":["bot-1"]}}`)
	if err != nil {
		t.Fatalf("parse valid registry: %v", err)
	}
	if !registry["loop"].allowsBot("bot-1") || registry["loop"].allowsBot("bot-2") {
		t.Fatalf("unexpected allow-list behavior: %+v", registry["loop"])
	}

	bad := []string{
		``,
		`null`,
		`{}`,
		`{"loop":{"token":"short","enabled":true,"allowed_bot_uids":["*"]}}`,
		`{" loop":{"token":"` + token + `","enabled":true,"allowed_bot_uids":["*"]}}`,
		`{"loop":{"token":"` + token + `","enabled":true,"allowed_bot_uids":[]}} {}`,
		`{"loop":{"token":"` + token + `","enabled":true,"allowed_bot_uids":[]},"doc":{"token":"` + token + `","enabled":true,"allowed_bot_uids":[]}}`,
	}
	for _, raw := range bad {
		if _, err := parseSourceRegistry(raw); err == nil {
			t.Fatalf("parseSourceRegistry(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestNormalizeTaskRequestAndFingerprint(t *testing.T) {
	req := taskRequest{
		Source: " loop ", TaskType: " custom ", IdempotencyKey: " key ",
		BotUID: " bot-1 ", ActorUID: " user-1 ", SessionKey: " thread-1 ",
		Prompt: "do work", Context: json.RawMessage(`{"z":1,"a":2}`),
		Metadata: json.RawMessage(`{"schema_version":"future"}`),
	}
	normalized, err := normalizeTaskRequest(req)
	if err != nil {
		t.Fatalf("normalizeTaskRequest: %v", err)
	}
	if normalized.Source != "loop" || normalized.BotUID != "bot-1" {
		t.Fatalf("identifiers were not trimmed: %+v", normalized)
	}
	second := normalized
	second.Context = json.RawMessage(`{"a":2,"z":1}`)
	second, err = normalizeTaskRequest(second)
	if err != nil {
		t.Fatalf("normalize second task: %v", err)
	}
	if taskFingerprint(normalized) != taskFingerprint(second) {
		t.Fatal("semantically identical JSON objects must have the same fingerprint")
	}

	if string(normalized.Metadata) != `{"schema_version":"future"}` {
		t.Fatalf("metadata was interpreted instead of passed through: %s", normalized.Metadata)
	}
}

func TestNormalizeJSONObjectPreservesLargeJSONNumbers(t *testing.T) {
	first, err := normalizeJSONObject(json.RawMessage(`{"reply_to":9007199254740993}`), true, maxContextBytes)
	if err != nil {
		t.Fatal(err)
	}
	second, err := normalizeJSONObject(json.RawMessage(`{"reply_to":9007199254740992}`), true, maxContextBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != `{"reply_to":9007199254740993}` {
		t.Fatalf("large integer changed: %s", first)
	}
	if string(first) == string(second) {
		t.Fatalf("distinct large integers collapsed: %s", first)
	}
}

func TestValidBearerToken(t *testing.T) {
	if !validBearerToken("Bearer secret", "secret") {
		t.Fatal("valid bearer token rejected")
	}
	for _, header := range []string{"secret", "bearer secret", "Bearer wrong", "Bearer "} {
		if validBearerToken(header, "secret") {
			t.Fatalf("invalid bearer token accepted: %q", header)
		}
	}
}
