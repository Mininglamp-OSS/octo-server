package internal_membership

import (
	"errors"
	"io"
	"net"
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
// Read the coverage narrowly, because it is narrower than it looks: every body
// here is an in-memory reader, so the tail is ALWAYS already buffered and the
// assertion cannot fail for the reason that matters on a real socket. A tail
// arriving in a later TCP segment is accepted by decoder.Buffered and no test
// here would notice. That is the deliberate half of the trade recorded on
// decodeVerifyRequest — trailing content is rejected when it has arrived, and
// waiting to find out whether more is coming is the hang the shape exists to
// avoid.
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

// TestVerifyDoesNotWaitForeverOnAnIncompleteBody covers the OTHER read that can
// park the handler: the first Decode, on a body that never finishes.
//
// decoder.Buffered fixes the trailing check and does nothing for this one — a
// caller that sends `{"space_id":"a` and stops leaves Decode blocked on the
// socket. It is bounded by a read deadline, and that deadline only reaches the
// connection through a REAL server: httptest.ResponseRecorder has no connection,
// so SetReadDeadline returns ErrNotSupported and the in-process tests above
// cannot see this property at all. Hence a real listener.
func TestVerifyDoesNotWaitForeverOnAnIncompleteBody(t *testing.T) {
	if readBodyTimeout > 20*time.Second {
		t.Skipf("readBodyTimeout is %s; this test would outlive the suite", readBodyTimeout)
	}

	// Held in a variable, not built inline at the assertion: an assertion against a
	// FRESH stub cannot fail, and this test carried exactly that defect for a round
	// — a regression letting a half-sent body reach the database would have stayed
	// green. The sibling cases above get this right.
	store := &stubStore{}
	router := newRouter(newTestModule(store))
	srv := httptest.NewServer(router)
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// A Content-Length the client never satisfies — the shape a stalled peer or a
	// buffering proxy produces without any malice.
	head := "POST " + verifyPath + " HTTP/1.1\r\n" +
		"Host: " + strings.TrimPrefix(srv.URL, "http://") + "\r\n" +
		"Content-Type: application/json\r\n" +
		internalTokenHeader + ": " + testInternalToken + "\r\n" +
		"Content-Length: 200\r\n\r\n" +
		`{"space_id":"a`
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The handler must give up on its own. Read with a ceiling comfortably past
	// the deadline but well short of "forever", so a regression fails rather than
	// hangs the suite.
	if err := conn.SetReadDeadline(time.Now().Add(readBodyTimeout + 10*time.Second)); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	start := time.Now()
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	elapsed := time.Since(start)

	if err != nil && n == 0 {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("the server never answered and never closed the connection after %s: an "+
				"incomplete body parks the handler goroutine and its connection for as long as "+
				"the peer holds the socket. MaxBytesReader bounds bytes, not time, and this "+
				"route has no ReadTimeout — see boundBodyReadTime.", elapsed)
		}
		// A reset/EOF is the server dropping the stalled connection: also bounded.
		t.Logf("server closed the stalled connection after %s (%v)", elapsed, err)
		return
	}
	t.Logf("server answered after %s: %q", elapsed, strings.SplitN(string(buf[:n]), "\r\n", 2)[0])

	if store.memberCalls != 0 {
		t.Fatalf("an incomplete body reached the store %d time(s): the decode must fail before "+
			"any lookup, or a half-sent request becomes a database query", store.memberCalls)
	}
}

// TestStalledBodyIsBoundedEvenWhenAuthRejects covers the half an UNAUTHENTICATED
// caller can reach, which is the more dangerous one.
//
// The deadline used to be installed inside decodeVerifyRequest, so it only ever
// covered requests that reached the handler. Everything that aborted at the
// limiter or at auth got none — and aborting does not end the story: net/http
// marks every server request body doEarlyClose, and finishRequest then DRAINS a
// declared remainder under 256 KiB with a plain blocking read. With no
// ReadTimeout that read is unbounded, so a caller with a wrong token could POST
// a Content-Length it never fills, collect its 401, and still hold a goroutine
// and a connection for as long as it liked. No credential required.
//
// Installing the deadline as the first handler on both routes is what covers it,
// and this is the test that can tell the two placements apart: it presents a
// token the server rejects, so the handler never runs.
func TestStalledBodyIsBoundedEvenWhenAuthRejects(t *testing.T) {
	router := newRouter(newTestModule(&stubStore{}))
	srv := httptest.NewServer(router)
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	head := "POST " + verifyPath + " HTTP/1.1\r\n" +
		"Host: " + strings.TrimPrefix(srv.URL, "http://") + "\r\n" +
		"Content-Type: application/json\r\n" +
		internalTokenHeader + ": definitely-the-wrong-token-value\r\n" +
		"Content-Length: 4096\r\n\r\n" +
		`{"space_id":"a`
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(readBodyTimeout + 10*time.Second)); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	start := time.Now()
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	elapsed := time.Since(start)

	// A 401 arriving promptly is expected — auth answers without reading the
	// body. The property under test is what happens NEXT: the connection must
	// not stay pinned while the server drains a body the client never sends.
	if err == nil && n > 0 {
		t.Logf("first response after %s: %q", elapsed, strings.SplitN(string(buf[:n]), "\r\n", 2)[0])
		if err := conn.SetReadDeadline(time.Now().Add(readBodyTimeout + 10*time.Second)); err != nil {
			t.Fatalf("set client deadline: %v", err)
		}
		start = time.Now()
		_, err = conn.Read(buf)
		elapsed = time.Since(start)
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the connection was still open %s after a REJECTED request whose body never "+
			"arrived. An abort does not end the read: net/http drains the declared remainder "+
			"after the handler returns, unbounded without a deadline — so an unauthenticated "+
			"caller pins a goroutine and a connection. Mount boundBodyReadTime as the FIRST "+
			"handler, not from inside the handler.", elapsed)
	}
	t.Logf("connection closed after %s (%v)", elapsed, err)
}
