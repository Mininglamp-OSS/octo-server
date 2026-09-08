package internal_membership

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// stallingBody delivers a complete JSON object and then never terminates: every
// subsequent Read blocks until the test releases it.
//
// That is not a contrived shape. A peer whose process is paged out mid-write, a
// proxy that buffers, or a client that declares a Content-Length it never fills
// all produce it, with no malice and no invalid token required.
type stallingBody struct {
	payload []byte
	sent    bool
	release chan struct{}
	// blocked is closed the first time a Read has to wait, so the test can tell
	// "the handler finished before the socket stalled" from "the socket stalled
	// and the handler still finished".
	blocked chan struct{}
	noted   bool
}

func (b *stallingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		n := copy(p, b.payload)
		return n, nil
	}
	if !b.noted {
		b.noted = true
		close(b.blocked)
	}
	<-b.release
	return 0, io.EOF
}

func (b *stallingBody) Close() error { return nil }

// TestVerifyDoesNotWaitOnTheSocketAfterACompleteBody is the availability half of
// the body parse, and it is the reason the trailing check reads only buffered
// bytes.
//
// A second decoder.Decode after the first cannot return without reading at least
// one more byte — that is the only way to distinguish "another value" from "end
// of input". With a body that stalls after one complete object, the handler parks
// there for as long as the peer holds the connection: MaxBytesReader bounds bytes,
// not time, and the route is served by a zero-value http.Server. The strict IP
// limiter caps how fast requests ARRIVE, not how many never finish, so goroutines
// and connections accumulate without bound on the authorization path.
//
// This repository already adjudicated the identical hazard (PR #837 P1, the "Do
// not restore it" note in modules/bot_api/register.go) and solved it in
// modules/bot_task by inspecting decoder.Buffered, which cannot wait on the
// socket. This test is what keeps the solved shape from being "tidied" back.
func TestVerifyDoesNotWaitOnTheSocketAfterACompleteBody(t *testing.T) {
	body := &stallingBody{
		payload: []byte(`{"space_id":"s","project_id":"p","uids":["u1"]}`),
		release: make(chan struct{}),
		blocked: make(chan struct{}),
	}
	// Released at the end so a handler that DOES block is unwedged and the test
	// binary can exit with a failure rather than a timeout panic.
	defer close(body.release)

	req := httptest.NewRequest(http.MethodPost, verifyPath, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(internalTokenHeader, testInternalToken)

	store := &stubStore{}
	router := newRouter(newTestModule(store))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler is still parked on the socket after a COMPLETE request body. " +
			"A trailing check that reads from the connection turns any stalled peer — no " +
			"malice needed — into a pinned goroutine and connection on the authorization " +
			"path. Inspect decoder.Buffered instead; see this test's comment.")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for a complete body, got %d (%s)", w.Code, w.Body.String())
	}
	if store.memberCalls != 1 {
		t.Fatalf("the store must be reached exactly once, got %d calls", store.memberCalls)
	}
}

// TestVerifyStillRejectsTrailingContent pins that the availability fix cost
// nothing on the strictness side.
//
// Both shapes below arrive complete, so decoder.Buffered already holds the tail
// and no socket read is needed to see it.
func TestVerifyStillRejectsTrailingContent(t *testing.T) {
	cases := map[string][]byte{
		"a second object":    []byte(`{"space_id":"s","project_id":"p","uids":["u1"]}{}`),
		"a bare token":       []byte(`{"space_id":"s","project_id":"p","uids":["u1"]} 7`),
		"unterminated array": []byte(`{"space_id":"s","project_id":"p","uids":["u1"]}[`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			store := &stubStore{}
			w := doPost(t, newRouter(newTestModule(store)), testInternalToken, raw)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
			}
			if store.memberCalls != 0 {
				t.Fatal("a body with trailing content reached the store")
			}
		})
	}

	// Trailing WHITESPACE is not trailing content: a client that pretty-prints
	// with a final newline sends a valid request.
	store := &stubStore{}
	w := doPost(t, newRouter(newTestModule(store)), testInternalToken,
		[]byte("{\"space_id\":\"s\",\"project_id\":\"p\",\"uids\":[\"u1\"]}\n\n"))
	if w.Code != http.StatusOK {
		t.Fatalf("a trailing newline must not be rejected: got %d (%s)", w.Code, w.Body.String())
	}
}

// TestDecodeVerifyRequestNeverDecodesTwice is the source guard.
//
// The behavioural test above needs a stalling body to observe the defect; the
// shape itself is one line, so a future edit "restoring strictness" would look
// harmless in review. This names the exact call that must not come back.
func TestDecodeVerifyRequestNeverDecodesTwice(t *testing.T) {
	raw, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func decodeVerifyRequest(")
	if start < 0 {
		t.Fatal("decodeVerifyRequest not found — if it moved, point this guard at it rather than deleting it")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}

	if strings.Count(body, "decoder.Decode(") != 1 {
		t.Error("decodeVerifyRequest must call decoder.Decode exactly once. A second Decode " +
			"(or decoder.Token, or decoder.More) has to read another byte off the socket to " +
			"tell a second value from end-of-input, which parks the handler on a stalled peer.")
	}
	for _, banned := range []string{"decoder.Token(", "decoder.More("} {
		if strings.Contains(body, banned) {
			t.Errorf("decodeVerifyRequest must not call %s — same socket read, same hang", banned)
		}
	}
	if !strings.Contains(body, "decoder.Buffered()") {
		t.Error("the trailing check must inspect decoder.Buffered(), which cannot wait on the " +
			"socket; that is the shape modules/bot_task settled on after PR #837")
	}
}
