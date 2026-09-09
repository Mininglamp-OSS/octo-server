package agentmailgateway

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const fixedAssertionVector = "omg_eyJ2IjoxLCJpc3MiOiJvY3RvLXNlcnZlciIsInN1YiI6InVzZXItYSIsInNwYWNlIjoic3BhY2UtYSIsIm1ldGhvZCI6IlBPU1QiLCJ1cmkiOiIvd2ViYXBpL3YwL21lc3NhZ2VzP3g9MSIsImJvZHlfc2hhMjU2IjoiMjMwZDgzNThkYzhlODg5MGI0YzU4ZGVlYjYyOTEyZWUyZjIwMzU3YWU5MmE1Y2M4NjFiOThlNjhmZTMxYWNiNSIsImlhdCI6MTc4NTMxOTIwMCwiZXhwIjoxNzg1MzE5MjYwLCJub25jZSI6IkFBRUNBd1FGQmdjSUNRb0xEQTBPRHcifQ.djWC9k17lq-g2zuKORkSnQUuEKZK9dIYl8ktFBHSqIc"

func TestSignAssertionMatchesOctoMailWireVector(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	nonce := make([]byte, 16)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)

	token, err := signAssertion(
		secret,
		"user-a",
		"space-a",
		"post",
		"/webapi/v0/messages?x=1",
		[]byte("body"),
		now,
		bytes.NewReader(nonce),
	)
	if err != nil {
		t.Fatal(err)
	}
	if token != fixedAssertionVector {
		t.Fatalf("wire token mismatch\n got: %s\nwant: %s", token, fixedAssertionVector)
	}
}

func TestSignAssertionRejectsIncompleteIdentity(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	for name, identity := range map[string][2]string{
		"subject": {"", "space-a"},
		"space":   {"user-a", ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := signAssertion(secret, identity[0], identity[1], "GET", "/webapi/v0/mailboxes", nil, time.Now(), bytes.NewReader(make([]byte, 16)))
			if err == nil {
				t.Fatal("incomplete identity was accepted")
			}
		})
	}
}
