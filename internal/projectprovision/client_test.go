package projectprovision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/octosign"
)

const testSecret = "0123456789abcdef0123456789abcdef" // 32 bytes, the configured minimum

func testTarget(url string) Target {
	return Target{Name: "fleet", EnsureURL: url, Secret: testSecret, Timeout: 2 * time.Second}
}

// TestEnsureSignsTheCanonicalRequest pins the wire contract a receiver has to implement:
// the same v1 HMAC over the same canonical string as a card-action callback, with
// sha256(container_id) — not the id — in the event-id slot.
//
// Verified with octosign.Verify rather than by re-deriving the hex here,
// because the point of reusing that package is that a receiver can verify with the
// code it already has — a hand-rolled expectation in this test could agree with
// itself while disagreeing with Verify.
func TestEnsureSignsTheCanonicalRequest(t *testing.T) {
	var (
		gotSignature string
		gotTimestamp string
		gotEventID   string
		gotBody      []byte
		gotPath      string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get(HeaderSignature)
		gotTimestamp = r.Header.Get(HeaderTimestamp)
		gotEventID = r.Header.Get(HeaderEventID)
		gotPath = r.URL.EscapedPath()
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = buf
		_ = json.NewEncoder(w).Encode(EnsureResponse{ContainerID: "octows-deadbeef", Slug: "octows-deadbeef"})
	}))
	defer server.Close()

	resp, err := NewClient(nil, nil).Ensure(context.Background(), testTarget(server.URL+"/api/internal/workspaces/ensure"), EnsureRequest{
		ContainerID: "octows-deadbeef",
		ProjectID:   "p1",
		OctoSpaceID: "s1",
		Name:        "octo-project",
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if resp.ContainerID != "octows-deadbeef" {
		t.Fatalf("container id = %q", resp.ContainerID)
	}
	// The header must carry the HASH, never the container id itself: headers get
	// picked up by reverse-proxy log formats and APM agents in a way request bodies do
	// not, and until R2/R3 land the id is a capability. This is the regression guard for
	// that — the body still carries the real id, which is where the receiver reads it.
	if gotEventID == "octows-deadbeef" {
		t.Errorf("%s carries the raw container id; a capability must not travel in a header", HeaderEventID)
	}
	if want := containerEventID("octows-deadbeef"); gotEventID != want {
		t.Errorf("%s = %q, want sha256(container_id) = %q — the receiver recomputes this to verify",
			HeaderEventID, gotEventID, want)
	}
	if !strings.Contains(string(gotBody), "octows-deadbeef") {
		t.Error("the body no longer carries the real container id; the receiver has nothing to key on")
	}
	if !octosign.Verify(testSecret, gotSignature, http.MethodPost, gotPath, gotTimestamp, gotEventID, gotBody) {
		t.Errorf("signature %q does not verify against the canonical request", gotSignature)
	}
	if !octosign.Verify(testSecret, gotSignature, http.MethodPost, gotPath, gotTimestamp, gotEventID, gotBody) {
		return
	}
	// A different secret must not verify — otherwise the assertion above would pass
	// for a client that signed with an empty key.
	if octosign.Verify(testSecret+"x", gotSignature, http.MethodPost, gotPath, gotTimestamp, gotEventID, gotBody) {
		t.Error("signature verified under the wrong secret; the guard above is vacuous")
	}
}

// TestContainerEventIDIsAOneWayStableDerivation pins the two properties the receiver
// and the outbox each depend on: stable across replays of the same job (so the
// canonical string does not change), and one-way (so a proxy log holding the value
// does not hold the capability).
func TestContainerEventIDIsAOneWayStableDerivation(t *testing.T) {
	const id = "octows-cafebabecafebabecafebabecafebabe"
	first, second := containerEventID(id), containerEventID(id)
	if first != second {
		t.Fatalf("not deterministic: %q vs %q", first, second)
	}
	if strings.Contains(first, id) || strings.Contains(id, first) {
		t.Errorf("the derived event id %q still contains the container id", first)
	}
	if first == containerEventID(id+"x") {
		t.Error("two different container ids derived the same event id")
	}
	if len(first) != 64 {
		t.Errorf("event id length = %d, want 64 hex chars", len(first))
	}
}

// TestEnsureRejectsAMismatchedContainerID pins the fail-loud-and-permanent branch.
// A target that answers with a different id means our mapping row points at a
// container nobody owns, and no number of retries repairs that.
func TestEnsureRejectsAMismatchedContainerID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(EnsureResponse{ContainerID: "octows-somethingelse"})
	}))
	defer server.Close()

	_, err := NewClient(nil, nil).Ensure(context.Background(), testTarget(server.URL+"/ensure"), EnsureRequest{
		ContainerID: "octows-deadbeef", ProjectID: "p1", OctoSpaceID: "s1",
	})
	if Category(err) != "container_id_mismatch" {
		t.Fatalf("category = %q, want container_id_mismatch (err=%v)", Category(err), err)
	}
	// There is no Retryable predicate any more (see the note in client.go): whether a
	// category is permanent is the worker's decision, because it deliberately retries some
	// statuses an HTTP-level predicate would call non-retryable. The permanence of THIS
	// category is asserted where it is acted on —
	// TestWorkerAbandonsAPermanentFailureImmediately in modules/project.
}

// TestEnsureErrorNeverCarriesTheContainerID is the confidentiality guard.
//
// Until fleet's R2 and drive's R3 land, knowing a container id is close enough to
// holding access to the container, and this error string is written verbatim into
// octo_project_provisioning.last_error and into a Warn log line.
func TestEnsureErrorNeverCarriesTheContainerID(t *testing.T) {
	const containerID = "octows-cafebabecafebabecafebabecafebabe"
	cases := map[string]http.HandlerFunc{
		"5xx": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom "+containerID, http.StatusInternalServerError)
		},
		"4xx": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope "+containerID, http.StatusBadRequest)
		},
		"mismatch": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(EnsureResponse{ContainerID: "other"})
		},
		"garbage_body": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json " + containerID))
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			_, err := NewClient(nil, nil).Ensure(context.Background(), testTarget(server.URL+"/ensure"), EnsureRequest{
				ContainerID: containerID, ProjectID: "p1", OctoSpaceID: "s1",
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), containerID) {
				t.Errorf("error string leaks the container id: %q", err.Error())
			}
			// The target's response text must not survive either: it is attacker- or
			// misconfiguration-controlled and lands in last_error.
			if strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "nope") {
				t.Errorf("error string echoes the target's response body: %q", err.Error())
			}
		})
	}
}

// TestStatusClassification pins the label set and the retry verdict per status.
//
// The labels are metric label values, so they are a contract: renaming one silently
// splits a dashboard series.
func TestStatusClassification(t *testing.T) {
	cases := []struct {
		status   int
		category string
	}{
		{http.StatusInternalServerError, "target_5xx"},
		{http.StatusBadGateway, "target_5xx"},
		{http.StatusTooManyRequests, "target_rate_limited"},
		{http.StatusRequestTimeout, "target_timeout"},
		{http.StatusUnauthorized, "target_rejected_credential"},
		{http.StatusForbidden, "target_rejected_credential"},
		// 404 gets its own label because it is the shape of "the target has not deployed
		// its ensure endpoint yet" — the EXPECTED state until brief precondition P-2
		// lands — and the worker retries it deliberately. Separating it from target_5xx
		// is what lets a dashboard tell "not shipped yet" from "shipped and broken".
		{http.StatusNotFound, "target_no_ensure_endpoint"},
		{http.StatusBadRequest, "target_4xx"},
		{http.StatusConflict, "target_4xx"},
	}
	for _, tc := range cases {
		if got := statusCategory(tc.status); got != tc.category {
			t.Errorf("statusCategory(%d) = %q, want %q", tc.status, got, tc.category)
		}
	}
}

// TestEnsureRefusesRedirects pins that a redirect is not followed.
//
// Following one would re-send the signed body to a host the signature was not
// computed for, and would let a compromised or misconfigured target hand
// provisioning traffic to somewhere else entirely.
func TestEnsureRefusesRedirects(t *testing.T) {
	var elsewhereHits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhereHits++
		_ = json.NewEncoder(w).Encode(EnsureResponse{ContainerID: "octows-deadbeef"})
	}))
	defer elsewhere.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/ensure", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	_, err := NewClient(nil, nil).Ensure(context.Background(), testTarget(server.URL+"/ensure"), EnsureRequest{
		ContainerID: "octows-deadbeef", ProjectID: "p1", OctoSpaceID: "s1",
	})
	if Category(err) != "target_redirect_refused" {
		t.Fatalf("category = %q, want target_redirect_refused (err=%v)", Category(err), err)
	}
	if elsewhereHits != 0 {
		t.Errorf("the redirect target was contacted %d times; redirects must not be followed", elsewhereHits)
	}
}

// TestEnsureBoundsTheResponseBody pins that an unbounded response cannot be read
// into memory. A target answering with megabytes is a misdirected route, not a
// container id.
func TestEnsureBoundsTheResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A valid JSON prefix followed by far more than the cap, so a client that read
		// the whole body would still decode successfully.
		_, _ = w.Write([]byte(`{"container_id":"octows-deadbeef","pad":"` + strings.Repeat("x", maxResponseBytes*4) + `"}`))
	}))
	defer server.Close()

	_, err := NewClient(nil, nil).Ensure(context.Background(), testTarget(server.URL+"/ensure"), EnsureRequest{
		ContainerID: "octows-deadbeef", ProjectID: "p1", OctoSpaceID: "s1",
	})
	if Category(err) != "invalid_response" {
		t.Fatalf("category = %q, want invalid_response — the body cap must truncate the decode", Category(err))
	}
}

// TestValidateTarget pins the configuration bar, which is enforced at process start
// so a typo cannot become a first-delivery surprise.
func TestValidateTarget(t *testing.T) {
	cases := []struct {
		name    string
		target  Target
		wantErr bool
	}{
		{"ok", Target{Name: "fleet", EnsureURL: "https://fleet.internal/api/internal/workspaces/ensure", Secret: testSecret}, false},
		{"ok_http", Target{Name: "drive", EnsureURL: "http://drive.internal/v1/internal/drive/spaces/ensure", Secret: testSecret}, false},
		{"no_name", Target{EnsureURL: "https://x.internal/ensure", Secret: testSecret}, true},
		{"short_secret", Target{Name: "fleet", EnsureURL: "https://x.internal/ensure", Secret: "short"}, true},
		{"no_scheme", Target{Name: "fleet", EnsureURL: "fleet.internal/ensure", Secret: testSecret}, true},
		{"bad_scheme", Target{Name: "fleet", EnsureURL: "file:///etc/passwd", Secret: testSecret}, true},
		{"no_host", Target{Name: "fleet", EnsureURL: "https:///ensure", Secret: testSecret}, true},
		// A credential in the URL would be published by every log line that prints it.
		{"userinfo", Target{Name: "fleet", EnsureURL: "https://u:p@fleet.internal/ensure", Secret: testSecret}, true},
		// A bare origin is nearly always a truncated env value.
		{"origin_only", Target{Name: "fleet", EnsureURL: "https://fleet.internal", Secret: testSecret}, true},
		{"root_path", Target{Name: "fleet", EnsureURL: "https://fleet.internal/", Secret: testSecret}, true},
		// A query string travels OUTSIDE the MAC — CanonicalRequest signs the path only —
		// so `?tenant=A` is unauthenticated and an on-path rewrite still verifies.
		{"query", Target{Name: "fleet", EnsureURL: "https://fleet.internal/ensure?tenant=A", Secret: testSecret}, true},
		// The bare trailing "?" form: RawQuery is empty but the separator survives onto
		// the wire, so ForceQuery has to be checked too.
		{"force_query", Target{Name: "fleet", EnsureURL: "https://fleet.internal/ensure?", Secret: testSecret}, true},
		{"fragment", Target{Name: "fleet", EnsureURL: "https://fleet.internal/ensure#frag", Secret: testSecret}, true},
		// Host alone keeps the ":port", so a host-less URL with a port would pass a
		// `Host != ""` check; Hostname() is what actually rejects it.
		{"port_without_host", Target{Name: "fleet", EnsureURL: "http://:8080/ensure", Secret: testSecret}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTarget(tc.target)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateTarget = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), testSecret) {
				t.Errorf("validation error leaks the secret: %q", err.Error())
			}
		})
	}
}

// TestEnsureRejectsAnIncompleteRequest keeps the three identifying fields
// mandatory. A row missing one of them means the enqueue side is broken, and
// sending it would create an unattributable container.
func TestEnsureRejectsAnIncompleteRequest(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer server.Close()
	client := NewClient(nil, nil)
	for _, req := range []EnsureRequest{
		{ProjectID: "p1", OctoSpaceID: "s1"},
		{ContainerID: "c1", OctoSpaceID: "s1"},
		{ContainerID: "c1", ProjectID: "p1"},
	} {
		if _, err := client.Ensure(context.Background(), testTarget(server.URL+"/ensure"), req); Category(err) != "invalid_request" {
			t.Errorf("Ensure(%+v) category = %q, want invalid_request", req, Category(err))
		}
	}
	if hits != 0 {
		t.Errorf("an incomplete request reached the target %d times", hits)
	}
}

// TestEnsureCancelledContextIsClassified pins that a shutdown produces a typed error
// rather than a bare one, so the worker labels the attempt instead of reporting
// "unclassified". It is not treated specially beyond that: `attempts` was already
// incremented at claim, so there is nothing to save, and the row simply stays pending.
func TestEnsureCancelledContextIsClassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(EnsureResponse{ContainerID: "octows-deadbeef"})
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewClient(nil, nil).Ensure(ctx, testTarget(server.URL+"/ensure"), EnsureRequest{
		ContainerID: "octows-deadbeef", ProjectID: "p1", OctoSpaceID: "s1",
	})
	if err == nil {
		t.Fatal("expected an error on a cancelled context")
	}
	var ensureErr *EnsureError
	if !errors.As(err, &ensureErr) {
		t.Fatalf("error is not an *EnsureError: %T", err)
	}
	if Category(err) != "transport_failed" {
		t.Errorf("category = %q, want transport_failed", Category(err))
	}
}

// TestConformanceVectorsMatchTheImplementation is the anti-drift pin.
//
// The vectors are literal hex because a receiver in another repository cannot import this
// package and has to copy them. That copy is only worth anything if the literals still
// describe what Sign produces, so every signature is recomputed here from the vector's own
// inputs. A published vector that has drifted from the code is worse than no vector: it
// sends the other team chasing a mismatch that is ours.
func TestConformanceVectorsMatchTheImplementation(t *testing.T) {
	vectors := ConformanceVectors()
	if len(vectors) != 4 {
		t.Fatalf("expected 4 vectors, got %d", len(vectors))
	}
	seen := map[string]bool{}
	for _, v := range vectors {
		if seen[v.Name] {
			t.Errorf("duplicate vector name %q; names are cross-repo identifiers", v.Name)
		}
		seen[v.Name] = true

		// The secret the signature was produced WITH is not always v.Secret — that is the
		// point of wrong_secret — so pick it per vector rather than assuming.
		signingSecret := v.Secret
		signedBody := v.Body
		switch v.Name {
		case "wrong_secret":
			signingSecret = conformanceOtherSecret
		case "tampered_body":
			// Signed over the ORIGINAL body, delivered with the tampered one.
			signedBody = conformanceBody
		}
		want := octosign.Sign(signingSecret, v.Method, v.Path, v.Timestamp, v.EventID, []byte(signedBody))
		if v.Signature != want {
			t.Errorf("vector %q signature drifted:\n have %s\n want %s", v.Name, v.Signature, want)
		}
	}
}

// TestConformanceVectorsExerciseEveryEnforceableClause keeps the set honest.
//
// Three clauses are checkable from outside (authentication, integrity, freshness) and each
// needs at least one vector that must be REFUSED, plus one that must be accepted so a
// receiver that refuses everything cannot pass. Idempotency is deliberately not covered:
// it takes two deliveries and a state assertion, so it is a review item rather than a
// vector.
func TestConformanceVectorsExerciseEveryEnforceableClause(t *testing.T) {
	byName := map[string]ConformanceVector{}
	accepted, refused := 0, 0
	for _, v := range ConformanceVectors() {
		byName[v.Name] = v
		if v.MustAccept {
			accepted++
		} else {
			refused++
		}
	}
	for _, required := range []string{"valid", "stale_timestamp", "tampered_body", "wrong_secret"} {
		if _, ok := byName[required]; !ok {
			t.Errorf("vector %q is missing; it covers a clause nothing else does", required)
		}
	}
	if accepted == 0 {
		t.Error("no vector must be accepted; a receiver that refuses everything would pass")
	}
	if refused < 3 {
		t.Errorf("only %d refusal vectors; authentication, integrity and freshness each need one", refused)
	}
	// The freshness vector must be OUTSIDE the documented window, or it proves nothing.
	stale := byName["stale_timestamp"]
	var ts int64
	if _, err := fmt.Sscanf(stale.Timestamp, "%d", &ts); err != nil {
		t.Fatalf("stale vector timestamp %q is not an integer", stale.Timestamp)
	}
	if stale.NowUnix-ts <= MaxSkewSeconds {
		t.Errorf("stale vector is only %ds old but the window is %ds; it would legitimately be accepted",
			stale.NowUnix-ts, MaxSkewSeconds)
	}
	// And the baseline must be INSIDE it, or "valid" is not valid.
	valid := byName["valid"]
	if _, err := fmt.Sscanf(valid.Timestamp, "%d", &ts); err != nil {
		t.Fatalf("valid vector timestamp %q is not an integer", valid.Timestamp)
	}
	if valid.NowUnix-ts > MaxSkewSeconds {
		t.Errorf("the baseline vector is %ds old, outside the %ds window", valid.NowUnix-ts, MaxSkewSeconds)
	}
}
