package project

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/octosign"
)

// Tests for the delivery-outcome classification.
//
// This is the substance of the outbound half: whether a failure retries or is
// terminal decides whether an undelivered revocation eventually lands or stops
// forever, and BOTH mistakes are silent. A terminal error retried forever hides
// a bug behind a growing backlog; a transient error treated as terminal abandons
// an event the peer would have accepted a second later.

func TestClassifyFleetResponse(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantOK    bool
		wantRetry bool
		wantClass string
	}{
		{name: "200 applied", status: 200, body: `{"status":"applied"}`, wantOK: true, wantClass: lifecycleErrNone},
		// "ignored" is a SUCCESS: the peer decided this statement was superseded
		// by one it already applied, which is the ordering behaviour the version
		// field exists to produce. Retrying it would loop forever.
		{name: "200 ignored is success", status: 200, body: `{"status":"ignored"}`, wantOK: true, wantClass: lifecycleErrNone},
		{name: "204 no content", status: 204, body: ``, wantOK: true, wantClass: lifecycleErrNone},

		// Terminal.
		{name: "409 idempotency conflict", status: 409, body: `{"error":{"code":"IDEMPOTENCY_CONFLICT"}}`, wantClass: lifecycleErrConflict},
		{name: "409 workspace id conflict", status: 409, body: `{"error":{"code":"WORKSPACE_ID_CONFLICT"}}`, wantClass: lifecycleErrConflict},
		{name: "400 malformed", status: 400, body: `{"error":{"code":"BAD_REQUEST"}}`, wantClass: lifecycleErrRejected},
		{name: "404 route missing", status: 404, body: ``, wantClass: lifecycleErrRejected},
		{name: "422 unprocessable", status: 422, body: ``, wantClass: lifecycleErrRejected},

		// Retryable.
		{name: "401 mid-rotation", status: 401, body: ``, wantRetry: true, wantClass: lifecycleErrAuth},
		{name: "403 mid-rotation", status: 403, body: ``, wantRetry: true, wantClass: lifecycleErrAuth},
		{name: "408 timeout", status: 408, body: ``, wantRetry: true, wantClass: lifecycleErrThrottled},
		{name: "429 throttled", status: 429, body: ``, wantRetry: true, wantClass: lifecycleErrThrottled},
		{name: "500", status: 500, body: ``, wantRetry: true, wantClass: lifecycleErrServer},
		{name: "502", status: 502, body: `<html>gateway</html>`, wantRetry: true, wantClass: lifecycleErrServer},
		{name: "503", status: 503, body: ``, wantRetry: true, wantClass: lifecycleErrServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLifecycleResponse(tc.status, []byte(tc.body))
			if got.OK != tc.wantOK {
				t.Errorf("OK: want %v, got %v", tc.wantOK, got.OK)
			}
			if got.Retryable != tc.wantRetry {
				t.Errorf("Retryable: want %v, got %v", tc.wantRetry, got.Retryable)
			}
			if got.Class != tc.wantClass {
				t.Errorf("Class: want %q, got %q", tc.wantClass, got.Class)
			}
		})
	}
}

// TestConflictIsNeverRetryable is called out on its own because it is the one
// refusal that must never be retried under any circumstance: the peer holds this
// event id with different content, or the id names a different entity on its
// side. Neither can change by trying again, and both need a human.
func TestConflictIsNeverRetryable(t *testing.T) {
	got := classifyLifecycleResponse(http.StatusConflict, []byte(`{"error":{"code":"IDEMPOTENCY_CONFLICT"}}`))
	if got.Retryable {
		t.Fatal("a 409 must never be retryable: retrying cannot change the answer and only delays the alert")
	}
	if got.OK {
		t.Fatal("a 409 is not a success")
	}
}

// TestAuthFailureIsRetryable pins the non-obvious call. A credential rotation
// that reaches the peer before it reaches this deployment produces a 401, and it
// resolves on its own. Treating it as terminal would abandon every queued
// revocation during a routine rotation.
func TestAuthFailureIsRetryable(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		got := classifyLifecycleResponse(status, nil)
		if !got.Retryable {
			t.Fatalf("%d must be retryable: a rotation in flight is transient, and giving up "+
				"would abandon queued revocations during a routine credential change", status)
		}
		if got.Class != lifecycleErrAuth {
			t.Errorf("%d: want class %q so an alert can distinguish it, got %q", status, lifecycleErrAuth, got.Class)
		}
	}
}

// TestDetailNeverLeaksTheBody pins that what reaches last_error and the logs is a
// short low-cardinality summary — status plus the peer error code — and never
// the response body. The column is what an operator pastes into a ticket.
func TestDetailNeverLeaksTheBody(t *testing.T) {
	secret := "SECRET-TOKEN-abcdef0123456789 and a very long html error page"
	got := classifyLifecycleResponse(500, []byte(secret))
	if strings.Contains(got.Detail, "SECRET") || strings.Contains(got.Detail, "html") {
		t.Fatalf("detail leaked the response body: %q", got.Detail)
	}
	if got.Detail != "http 500" {
		t.Fatalf("want a bare status summary, got %q", got.Detail)
	}
}

// TestDetailCarriesThePeerErrorCode: the peer states its body code is
// authoritative and the status alone is not enough, so the code has to survive
// into what an operator reads.
func TestDetailCarriesThePeerErrorCode(t *testing.T) {
	got := classifyLifecycleResponse(409, []byte(`{"error":{"code":"WORKSPACE_ID_CONFLICT"}}`))
	if !strings.Contains(got.Detail, "WORKSPACE_ID_CONFLICT") {
		t.Fatalf("detail must carry the peer error code, got %q", got.Detail)
	}
}

func TestTopLevelErrorCodeIsAlsoAccepted(t *testing.T) {
	got := classifyLifecycleResponse(409, []byte(`{"code":"IDEMPOTENCY_CONFLICT"}`))
	if !strings.Contains(got.Detail, "IDEMPOTENCY_CONFLICT") {
		t.Fatalf("a top-level code must be read too, got %q", got.Detail)
	}
}

// TestUnparsableBodyStillClassifies: a peer returning HTML or nothing at all
// must not turn into an unhandled path.
func TestUnparsableBodyStillClassifies(t *testing.T) {
	for _, body := range []string{"", "not json", "<html></html>", "{"} {
		got := classifyLifecycleResponse(503, []byte(body))
		if !got.Retryable || got.Class != lifecycleErrServer {
			t.Fatalf("body %q: want retryable server error, got %+v", body, got)
		}
	}
}

// TestEnvelopeShape pins the wire contract, including that the payload bytes are
// passed through verbatim rather than re-marshalled.
func TestEnvelopeShape(t *testing.T) {
	version := int64(7)
	row := lifecycleEventRow{
		EventID:        "11111111-2222-4333-8444-555555555555",
		EventType:      LifecycleEventMemberRevoked,
		ProjectID:      "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		SpaceID:        "sp1",
		ProjectVersion: &version,
		Payload:        json.RawMessage(`{"subject_uid":"u1","member_epoch":43,"reason":"removed"}`),
		OccurredAt:     time.Date(2026, 9, 7, 10, 45, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(row.envelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	for _, key := range []string{"event_id", "event_type", "project_id", "space_id", "project_version", "occurred_at", "payload"} {
		if _, ok := got[key]; !ok {
			t.Errorf("envelope is missing %q", key)
		}
	}
	// Milliseconds, matching the contract's example envelope and the DATETIME(3)
	// column the value comes from. time.RFC3339 drops the fractional part, so the
	// millisecond was stored and then discarded on the wire.
	if got["occurred_at"] != "2026-09-07T10:45:00.000Z" {
		t.Errorf("occurred_at must be RFC3339 UTC with milliseconds, got %v", got["occurred_at"])
	}
	payload, ok := got["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload must be an object, got %T", got["payload"])
	}
	if payload["subject_uid"] != "u1" {
		t.Errorf("payload was not passed through verbatim: %v", payload)
	}
}

// TestEnvelopeOmitsAbsentVersion: an event carrying no lifecycle version must
// omit the key rather than send null or zero. Zero is a real version — the value
// pre-migration rows carry — so sending it would read as an ordering claim.
func TestEnvelopeSpellsAnAbsentVersionAsExplicitNull(t *testing.T) {
	row := lifecycleEventRow{
		EventID:    "id",
		EventType:  LifecycleEventRestored,
		ProjectID:  "p",
		SpaceID:    "s",
		Payload:    json.RawMessage(`{}`),
		OccurredAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(row.envelope())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// PRESENT and null, not omitted. The contract says an event that does not
	// order by version carries project_version: null (§2, §3), and an omitted key
	// is a different wire shape. Go decoders map both to a nil pointer, which is
	// why omitempty looked free — but a peer validating the envelope against a
	// schema written from §2 answers a plain 4xx, and a plain 4xx is terminal
	// here, so the first event abandoned would be member_revoked.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := decoded["project_version"]
	if !ok {
		t.Fatalf("project_version must be present as null, not omitted: %s", raw)
	}
	if string(v) != "null" {
		t.Fatalf("an absent version must be spelled null, got %s", v)
	}
}

// TestSendMatchesTheFrozenContract drives the real client against a real server
// and checks the bytes the peer will verify.
//
// Everything here is a wire-contract statement from
// docs/project-lifecycle-contract.md §1. None of it is observable from the
// classification tests above, which start at the response — and a signature that
// covers the wrong string fails as "the peer rejects everything with 401", which
// reads as a credential problem and is not one.
func TestSendMatchesTheFrozenContract(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"

	var (
		gotMethod  string
		gotPath    string
		gotHeaders http.Header
		gotBody    []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := newLifecycleHTTPClient(srv.URL+"/internal/project-events", secret, 5*time.Second)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	env := lifecycleEventEnvelope{
		EventID:    "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		EventType:  LifecycleEventMemberRevoked,
		ProjectID:  "7c9e6679-7425-40de-944b-e07fc1f90ae7",
		SpaceID:    "sp-1",
		OccurredAt: "2026-09-08T04:00:00Z",
		Payload:    json.RawMessage(`{"subject_uid":"u1","member_epoch":12,"reason":"space_removed"}`),
	}
	if res := client.Send(context.Background(), env); !res.OK {
		t.Fatalf("want success, got %+v", res)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %s", gotMethod)
	}
	if gotPath != "/internal/project-events" {
		t.Errorf("path: want the configured path verbatim, got %s", gotPath)
	}

	// The event id travels in the header AND the body, identical. That pairing is
	// what lets the peer recognise a redelivery whose first response was lost.
	if h := gotHeaders.Get(octosign.HeaderEventID); h != env.EventID {
		t.Errorf("%s: want %s, got %s", octosign.HeaderEventID, env.EventID, h)
	}
	var sent lifecycleEventEnvelope
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("body is not the envelope: %v", err)
	}
	if sent.EventID != env.EventID {
		t.Errorf("body event_id must equal the header, got %s", sent.EventID)
	}

	// Version in the header, not the body — the peer routes on it before parsing.
	if v := gotHeaders.Get(lifecycleEventVersionHeader); v != lifecycleEventVersion {
		t.Errorf("%s: want %s, got %q", lifecycleEventVersionHeader, lifecycleEventVersion, v)
	}

	// The signature must verify over exactly method + path + timestamp + event id
	// + body. Recomputed here with the peer's own verifier rather than compared to
	// a literal, so this fails if either side of the canonical string moves.
	ts := gotHeaders.Get(octosign.HeaderTimestamp)
	if ts == "" {
		t.Fatal("no timestamp header; the signature cannot be replay-bounded without one")
	}
	if !octosign.Verify(secret, gotHeaders.Get(octosign.HeaderSignature),
		http.MethodPost, "/internal/project-events", ts, env.EventID, gotBody) {
		t.Fatal("signature does not verify over method+path+timestamp+event_id+body — the peer " +
			"would reject every event, and it would look like a bad credential")
	}

	// No bearer anywhere. An outbound bearer is a credential the peer could send
	// back to us; the HMAC proves possession without the secret crossing the wire.
	if a := gotHeaders.Get("Authorization"); a != "" {
		t.Errorf("no Authorization header may be sent, got %q", a)
	}
}

// TestClientRefusesEndpointsThatCannotBeSigned pins the three URL shapes that
// would produce a signature the peer cannot reproduce.
//
// Each fails at construction rather than at delivery, because the failure at
// delivery is a 401 on every event — indistinguishable from a wrong secret, and
// discovered only once a revocation is already queued.
func TestClientRefusesEndpointsThatCannotBeSigned(t *testing.T) {
	cases := map[string]string{
		"relative":     "/internal/project-events",
		"no path":      "https://peer.invalid",
		"root only":    "https://peer.invalid/",
		"query string": "https://peer.invalid/events?tenant=a",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := newLifecycleHTTPClient(raw, "s", time.Second); err == nil {
				t.Errorf("want a refusal for %q: the signature covers the path and not the "+
					"query, so this endpoint cannot be verified by the peer", raw)
			}
		})
	}
}

// TestClientRefusesAShortSecret puts a floor under the HMAC key.
//
// The URL checks above are about the signature covering the right STRING; this is
// about the key being worth signing with. A short shared secret is recoverable
// offline from one captured request, and every guarantee the envelope makes then
// belongs to whoever recovered it.
func TestClientRefusesAShortSecret(t *testing.T) {
	const url = "https://peer.invalid/internal/project-events"
	if _, err := newLifecycleHTTPClient(url, strings.Repeat("k", lifecycleEventMinSecretBytes-1), time.Second); err == nil {
		t.Fatalf("a %d-byte secret must be refused", lifecycleEventMinSecretBytes-1)
	}
	if _, err := newLifecycleHTTPClient(url, strings.Repeat("k", lifecycleEventMinSecretBytes), time.Second); err != nil {
		t.Fatalf("a %d-byte secret is exactly the floor and must be accepted, got %v",
			lifecycleEventMinSecretBytes, err)
	}
}

// TestClientRefusesRedirectsAndProxies is P1-1: the two hardening properties the
// repository's other two signed outbound clients already carry, and this one did
// not.
//
// The redirect half is the one that produces a WRONG ANSWER rather than a
// failure. Go rewrites a 302 POST into a GET and drops the body; if the target
// answers 2xx, the delivery is classified OK, the row is marked delivered, the
// counter increments "delivered" and the backlog gauge falls — while the peer
// never saw the event. For a member revocation that is a permanent loss reported
// as a success.
func TestClientRefusesRedirectsAndProxies(t *testing.T) {
	var landed int32
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&landed, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/internal/project-events", http.StatusFound)
	}))
	defer redirector.Close()

	c, err := newLifecycleHTTPClient(redirector.URL+"/internal/project-events",
		strings.Repeat("k", 32), 2*time.Second)
	require.NoError(t, err)

	res := c.Send(context.Background(), lifecycleEventEnvelope{
		EventID: "11111111-1111-4111-8111-111111111111", EventType: LifecycleEventMemberRevoked,
		ProjectID: "p", SpaceID: "s", OccurredAt: "2026-09-08T00:00:00.000Z",
		Payload: json.RawMessage(`{}`),
	})

	assert.False(t, res.OK,
		"a redirect must never be reported as delivered: Go drops the body on a 302 POST, so "+
			"a 2xx from the target would mark the event delivered while the peer never saw it")
	assert.Equal(t, lifecycleErrRedirect, res.Class)
	assert.True(t, res.Retryable,
		"a peer migration that temporarily fronts the endpoint self-heals; for a revocation, "+
			"abandoning after the budget beats abandoning on the first attempt")
	assert.Zero(t, atomic.LoadInt32(&landed), "the client must not follow the redirect at all")

	// And the transport refuses the ambient proxy, so a deployment-level setting
	// cannot interpose the revocation feed.
	require.NotNil(t, c.client.Transport)
	tr, ok := c.client.Transport.(*http.Transport)
	require.True(t, ok, "the transport must be a configured *http.Transport, not the default")
	assert.Nil(t, tr.Proxy, "HTTP(S)_PROXY must not be honoured for a signed outbound feed")
}

// TestValidateMatchesTheProvisioningValidator is P1-3. This is the third signed
// outbound client against the same peer and the same signing scheme, and its
// validator was a strict subset of the one already guarding the other channel.
//
// The scheme check is the load-bearing one: `htps://peer/hook` is a
// one-character typo that parses to a real Host and Path, so the enqueue gate
// opened and every attempt failed with "unsupported protocol scheme" — which is
// classified RETRYABLE, so every event burned twelve attempts and abandoned.
func TestValidateMatchesTheProvisioningValidator(t *testing.T) {
	good := strings.Repeat("k", 32)
	for name, raw := range map[string]string{
		"typo'd scheme":   "htps://peer.invalid/internal/project-events",
		"non-http scheme": "ftp://peer.invalid/internal/project-events",
		"userinfo":        "https://user:pw@peer.invalid/internal/project-events",
		"bare query":      "https://peer.invalid/internal/project-events?",
		"fragment":        "https://peer.invalid/internal/project-events#frag",
		"no host":         "https://:8080/internal/project-events",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateLifecycleEndpoint(raw, good); err == nil {
				t.Fatalf("%q must be refused; projectprovision.ValidateTarget refuses it for the "+
					"same signing scheme", raw)
			}
		})
	}
	if err := validateLifecycleEndpoint("https://peer.invalid/internal/project-events", good); err != nil {
		t.Fatalf("a well-formed endpoint must still be accepted, got %v", err)
	}
}

// truncatingServer answers with status and a Content-Length that overstates the
// body it actually writes, then drops the connection.
//
// Hijacked rather than written through ResponseWriter because net/http will not
// help you lie: it reconciles Content-Length with what was written and closes
// cleanly, which is the opposite of the condition under test. The client's
// io.ReadAll therefore fails with an unexpected EOF, which is what a peer dying
// mid-response looks like from this side.
func truncatingServer(t *testing.T, status int, declared int, partial string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		hj, ok := w.(http.Hijacker)
		require.True(t, ok, "test server must support hijacking")
		conn, buf, err := hj.Hijack()
		require.NoError(t, err)
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n")
		_, _ = buf.WriteString("Content-Type: application/json\r\n")
		_, _ = buf.WriteString("Content-Length: " + strconv.Itoa(declared) + "\r\n\r\n")
		_, _ = buf.WriteString(partial)
		_ = buf.Flush()
	}))
}

func sendToTruncatingPeer(t *testing.T, status int, declared int, partial string) lifecycleDeliveryResult {
	t.Helper()
	srv := truncatingServer(t, status, declared, partial)
	defer srv.Close()

	client, err := newLifecycleHTTPClient(srv.URL+"/internal/project-events",
		"0123456789abcdef0123456789abcdef", 5*time.Second)
	require.NoError(t, err)

	return client.Send(context.Background(), lifecycleEventEnvelope{
		EventID:    "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		EventType:  LifecycleEventMemberRevoked,
		ProjectID:  "7c9e6679-7425-40de-944b-e07fc1f90ae7",
		SpaceID:    "sp-1",
		OccurredAt: "2026-09-08T04:00:00Z",
		Payload:    json.RawMessage(`{"subject_uid":"u1","member_epoch":12,"reason":"space_removed"}`),
	})
}

// TestATruncated2xxIsNotDelivered pins the one direction a discarded read error
// loses an event.
//
// Send used to throw io.ReadAll's error away, so a 200 whose body arrived
// half-written marked the event DELIVERED against a response this process never
// finished reading — and lifecycleErrNetwork's own doc claims to cover truncated
// responses, so the enum documented a class the code could not emit.
//
// Retrying is free in the other direction: the peer fingerprints the payload
// behind event_id, so a redelivery is deduplicated rather than applied twice.
func TestATruncated2xxIsNotDelivered(t *testing.T) {
	got := sendToTruncatingPeer(t, http.StatusOK, 4096, `{"st`)

	assert.False(t, got.OK,
		"a 200 whose body could not be read must not be recorded as delivered: the "+
			"write that marks it delivered is the one that stops it being retried")
	assert.True(t, got.Retryable, "a truncated response is transient, so it must retry")
	assert.Equal(t, lifecycleErrNetwork, got.Class,
		"and it must land in the class whose doc already claims to cover it")
	assert.NotContains(t, got.Detail, "127.0.0.1",
		"detail must not carry host or port, same rule as the dial branch")
}

// TestATruncatedTerminalRefusalStaysTerminal is the other half, and the reason
// the capture is conditioned on the status rather than applied to every response.
//
// 409 is the one refusal that must never retry. The status line arrives intact
// BEFORE the body, so a truncated body costs only the peer's error code — which
// detail() already tolerates. Reclassifying it as a network error would spend the
// whole attempt budget re-asking a question whose answer cannot change, and then
// abandon the event for the wrong reason.
func TestATruncatedTerminalRefusalStaysTerminal(t *testing.T) {
	got := sendToTruncatingPeer(t, http.StatusConflict, 4096, `{"er`)

	assert.False(t, got.OK)
	assert.False(t, got.Retryable,
		"a truncated 409 is still a permanent refusal; retrying it cannot change the verdict")
	assert.Equal(t, lifecycleErrConflict, got.Class,
		"the status line decides this, not the body that failed to arrive")
}
