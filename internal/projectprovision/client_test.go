package projectprovision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Mininglamp-OSS/octo-server/pkg/octosign"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// TestEnsureTreatsAnAbsentContainerIDAsRetryable separates the two shapes that
// used to share one category.
//
// A MISSING id is a malformed response; a DIFFERENT id is evidence the peer owns
// a container we do not know about. Only the second is terminal, and the
// difference is not cosmetic: container_id_mismatch is abandoned on the FIRST
// attempt, and abandoned has no automatic re-drive.
//
// The missing shape is what a peer serves while its ensure endpoint is still
// rolling out — a stub answering {}. That
// is precisely the window in which the first target gets enabled, so filing it
// as terminal would need a human to requeue every row created during it.
func TestEnsureTreatsAnAbsentContainerIDAsRetryable(t *testing.T) {
	// The reachable set, exactly: an empty or whitespace-only body does NOT reach
	// this branch — Decode returns io.EOF on it and the decode-failure branch
	// catches it first. `null` is here because the corrected comment names it and
	// the earlier test did not pin it.
	for _, body := range []string{`{}`, `{"container_id":""}`, `{"container_id":" "}`, `{"container_id":"\t\n"}`, `null`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			_, err := NewClient(nil, nil).Ensure(context.Background(), testTarget(server.URL+"/ensure"), EnsureRequest{
				ContainerID: "octows-deadbeef", ProjectID: "p1", OctoSpaceID: "s1",
			})
			if got := Category(err); got != "invalid_response" {
				t.Fatalf("category = %q, want invalid_response — an absent id is a malformed "+
					"response, not proof the peer owns a different container (err=%v)", got, err)
			}
		})
	}
}

// TestEnsureDecodeFailureHasSafeDetail keeps a malformed 200 response actionable
// without persisting response bytes, which can contain the container capability.
func TestEnsureDecodeFailureHasSafeDetail(t *testing.T) {
	const containerID = "octows-deadbeef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>" + containerID + "</html>"))
	}))
	defer server.Close()

	_, err := NewClient(nil, nil).Ensure(context.Background(), testTarget(server.URL+"/ensure"), EnsureRequest{
		ContainerID: containerID, ProjectID: "p1", OctoSpaceID: "s1",
	})
	if got := Category(err); got != "invalid_response" {
		t.Fatalf("category = %q, want invalid_response (err=%v)", got, err)
	}
	if got, want := Summary(err), "response body was not valid JSON"; got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
	if strings.Contains(Summary(err), containerID) {
		t.Fatalf("Summary leaks container id: %q", Summary(err))
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
		{"ok_http", Target{Name: "drive", EnsureURL: "http://drive.internal/v1/internal/drive/spaces", Auth: AuthInternalToken, InternalToken: testSecret}, false},
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
		// The EMPTY fragment. url.Parse leaves Fragment == "" for a bare trailing
		// "#", so a parsed.Fragment check passes it while the separator is still in
		// the configured value. cardactiondispatch documents this case and checks the
		// raw string; this package claimed alignment and had the weaker check.
		{"empty_fragment", Target{Name: "fleet", EnsureURL: "https://fleet.internal/ensure#", Secret: testSecret}, true},
		// A secret mounted from a file carries a trailing newline. Refused rather
		// than trimmed: trimming lets a subtly wrong mount work, and the failure it
		// otherwise produces is a whole retry budget of 401s indistinguishable from a
		// rotated secret, ending in abandoned, which has no automatic re-drive.
		{"secret_trailing_newline", Target{Name: "fleet", EnsureURL: "https://fleet.internal/ensure", Secret: testSecret + "\n"}, true},
		{"secret_leading_space", Target{Name: "fleet", EnsureURL: "https://fleet.internal/ensure", Secret: " " + testSecret}, true},
		{"secret_trailing_space", Target{Name: "fleet", EnsureURL: "https://fleet.internal/ensure", Secret: testSecret + " "}, true},
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

// TestValidateTargetRejectsThePublishedConformanceSecrets closes the gap between "long
// enough" and "secret".
//
// conformance.go ships two real, working secrets in a non-test source file of a public
// repository, and both are 33 bytes — comfortably past minSecretBytes. So the obvious
// operator move, copying one into OCTO_PROJECT_PROVISION_FLEET_SECRET to try the endpoint,
// booted clean while holding a published HMAC key; conformanceTamperedBody is literally the
// forgery that key authorises, a valid signature over an attacker-chosen project_id. The
// MAC is load-bearing here because ValidateTarget deliberately does not require TLS.
func TestValidateTargetRejectsThePublishedConformanceSecrets(t *testing.T) {
	const ensureURL = "https://fleet.internal/api/internal/workspaces/ensure"
	for _, secret := range []string{conformanceSecret, conformanceOtherSecret} {
		// Non-vacuity: the length floor must not be what rejects these, or this test would
		// still pass with the published-value check deleted.
		if len(secret) < minSecretBytes {
			t.Fatalf("this test only means something while the published secret clears the length floor: %d", len(secret))
		}
		err := ValidateTarget(Target{Name: "fleet", EnsureURL: ensureURL, Secret: secret})
		if err == nil {
			t.Fatal("a published conformance secret was accepted as a production credential")
		}
		if !strings.Contains(err.Error(), "conformance") {
			t.Fatalf("rejection does not say why: %v", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("the rejection echoed the secret it rejected")
		}
	}

	// A real secret of the same length still passes, so what is rejected is the published
	// VALUE — not the length, and not a substring of it.
	if err := ValidateTarget(Target{
		Name: "fleet", EnsureURL: ensureURL, Secret: strings.Repeat("z", len(conformanceSecret)),
	}); err != nil {
		t.Fatalf("a real secret of the same length was rejected: %v", err)
	}
}

// TestTransportDetailNamesTheReasonAndNeverTheRequest is the Q-2 fix's core property.
//
// Before it, a target that was down produced `last_error =
// "transport_failed: projectprovision: transport_failed"` — the outcome label twice — while
// connection-refused vs DNS vs deadline sat in the unexported cause, reachable only through
// Unwrap, which nothing in the repository calls. That field is what the runbook sends a
// human to read, the sweep appends to it because it is the only durable per-row evidence,
// and this package has no logger, so an OOM-killed pod leaves no log line either.
//
// The second half of the assertion is the part that must not regress: the detail is drawn
// from the transport error's INNER error, so nothing we sent can reach it.
func TestTransportDetailNamesTheReasonAndNeverTheRequest(t *testing.T) {
	const containerID = "octows-00112233445566778899aabbccddeeff"
	req := EnsureRequest{
		ContainerID: containerID, ProjectID: "p-secret", OctoSpaceID: "s-secret", Name: "n",
	}
	// A listener that is bound and then closed: the port is reachable and refuses, which is
	// what a target whose Pod is not up looks like.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	refusedURL := "http://" + ln.Addr().String() + "/api/internal/workspaces/ensure"
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	c := NewClient(nil, nil)
	_, err = c.Ensure(context.Background(), Target{
		Name: "fleet", EnsureURL: refusedURL, Secret: testSecret, Timeout: 2 * time.Second,
	}, req)
	if err == nil {
		t.Fatal("a closed port produced no error")
	}
	if Category(err) != "transport_failed" {
		t.Fatalf("category = %q, want transport_failed", Category(err))
	}

	summary := Summary(err)
	if summary == "" {
		t.Fatal("Summary is empty: last_error would carry the outcome label and nothing else")
	}
	// The reason, not a restatement of the label.
	if strings.Contains(summary, "transport_failed") {
		t.Fatalf("Summary repeats the category the outcome label already carries: %q", summary)
	}
	if !strings.Contains(summary, "refused") {
		t.Fatalf("Summary does not name the transport reason: %q", summary)
	}

	// Nothing we sent, in either the summary or the full error string.
	for _, secret := range []string{containerID, "p-secret", "s-secret", testSecret} {
		for label, text := range map[string]string{"Summary": summary, "Error": err.Error()} {
			if strings.Contains(text, secret) {
				t.Fatalf("%s leaks %q: %q", label, secret, text)
			}
		}
	}
	// The URL is deliberately excluded too: *url.Error's own message embeds it, so this
	// asserts the inner error was taken rather than the wrapper.
	if strings.Contains(summary, refusedURL) {
		t.Fatalf("Summary carries the full URL, so it is the wrapper not the inner error: %q", summary)
	}
}

// TestTransportDetailDistinguishesADeadlineFromARefusal — the two need different operator
// actions (a slow target vs a target that is not up), so they must not read alike. The
// deadline string is fixed rather than the standard library's, so a Go wording change
// cannot silently rewrite what an operator greps for.
func TestTransportDetailDistinguishesADeadlineFromARefusal(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer blocked.Close()

	c := NewClient(nil, nil)
	_, err := c.Ensure(context.Background(), Target{
		// A path is required by ValidateTarget, and without one this test measured
		// invalid_target instead of a deadline — it passed for the wrong reason.
		Name: "fleet", EnsureURL: blocked.URL + "/api/internal/workspaces/ensure",
		Secret: testSecret, Timeout: 50 * time.Millisecond,
	}, EnsureRequest{ContainerID: "octows-x", ProjectID: "p", OctoSpaceID: "s", Name: "n"})
	if err == nil {
		t.Fatal("a 500ms handler under a 50ms timeout produced no error")
	}
	if got := Summary(err); got != "deadline exceeded (per-call timeout)" {
		t.Fatalf("deadline summary = %q, want the fixed string", got)
	}
}

// TestSummaryAddsOnlyWhatTheOutcomeLabelDoesNotCarry pins the three shapes the worker
// writes, so the 255-byte column is not spent restating its own label.
func TestSummaryAddsOnlyWhatTheOutcomeLabelDoesNotCarry(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"detail wins", &EnsureError{Category: "transport_failed", Detail: "dial tcp: refused"}, "dial tcp: refused"},
		{"status when there is no detail", &EnsureError{Category: "target_5xx", Status: 500}, "status 500"},
		{"nothing to add", &EnsureError{Category: "invalid_request"}, ""},
		{"a non-ensure error keeps its own text", errors.New("boom"), "boom"},
		{"nil", nil, ""},
	} {
		if got := Summary(tc.err); got != tc.want {
			t.Errorf("%s: Summary = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestBoundedSingleLineIsSafeForA255ByteColumn covers the two ways this value could break
// the write it feeds: a control character forging a second field in a log line, and a
// byte-wise cut splitting a rune into an invalid UTF-8 sequence that MySQL in strict mode
// rejects — which would fail the UPDATE and leave the row pending and unsweepable, the
// zombie this module already closed once.
func TestBoundedSingleLineIsSafeForA255ByteColumn(t *testing.T) {
	// Newline/CR/tab become a space so words do not run together; any other control
	// character is DROPPED rather than spaced, which is why NUL leaves "cd".
	if got := boundedSingleLine("a\nb\tc\x00d", 64); got != "a b cd" {
		t.Errorf("control characters survived: %q", got)
	}
	// Multi-byte runes, with the bound falling mid-rune.
	const wide = "測試測試測試" // 3 bytes each
	got := boundedSingleLine(wide, 7)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
	if len(got) > 7 {
		t.Fatalf("truncation exceeded the bound: %d bytes", len(got))
	}
	if got != "測試" {
		t.Fatalf("expected a clean two-rune prefix, got %q", got)
	}
	if got := boundedSingleLine(strings.Repeat("x", 500), maxDetailBytes); len(got) != maxDetailBytes {
		t.Errorf("bound not applied: %d bytes", len(got))
	}
}

// TestConformanceEpochLabelMatchesTheConstant keeps a comment honest that a reader would
// otherwise trust and a well-meaning editor would "fix" the wrong way.
//
// The label said 2026 while the epoch is 2025. The value cannot change — the published
// signatures were computed over it — so the label is what has to track the value, and this
// reads the source rather than a copy of it.
func TestConformanceEpochLabelMatchesTheConstant(t *testing.T) {
	src, err := os.ReadFile("conformance.go")
	if err != nil {
		t.Fatalf("read conformance.go: %v", err)
	}
	m := regexp.MustCompile(`conformanceNow is a fixed wall clock \((\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)\)`).
		FindSubmatch(src)
	if m == nil {
		t.Fatal("the conformanceNow comment no longer states a wall clock; this guard is now blind")
	}
	want := time.Unix(conformanceNow, 0).UTC().Format("2006-01-02T15:04:05Z")
	if got := string(m[1]); got != want {
		t.Errorf("comment says %s, conformanceNow is %s — fix the COMMENT, not the constant "+
			"(the published signatures were computed over it)", got, want)
	}
}

// TestEnsureRefusesANonOKSuccessStatus pins P2-C: the published contract
// specifies a SYNCHRONOUS 200 carrying container_id, and this client used to
// accept any 2xx.
//
// 202 is the case that matters. It means the peer accepted the request and will
// act on it later, so treating it as success writes status=ready — "we
// successfully created the container" — against a container that may not exist.
// Every later decision that reads ready would be reading a promise as a fact.
//
// It retries rather than being terminal: unlike a 4xx, nothing here says the peer
// will answer this way forever, and a mid-rollout peer should not burn a row.
func TestEnsureRefusesANonOKSuccessStatus(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusCreated, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				// A VALID body, so the refusal is about the status and nothing else.
				_, _ = w.Write([]byte(`{"container_id":"octows-deadbeef"}`))
			}))
			defer server.Close()

			_, err := NewClient(nil, nil).Ensure(context.Background(), testTarget(server.URL+"/ensure"), EnsureRequest{
				ContainerID: "octows-deadbeef", ProjectID: "p1", OctoSpaceID: "s1",
			})
			if err == nil {
				t.Fatalf("%d with a valid body must NOT be accepted: the contract requires a "+
					"synchronous 200, and 202 means the container may not exist yet", status)
			}
			if got := Category(err); got != "invalid_response" {
				t.Fatalf("category = %q, want invalid_response (err=%v)", got, err)
			}
			if summary := Summary(err); !strings.Contains(summary, "synchronous 200") {
				t.Errorf("last_error must say what the peer got wrong, got %q", summary)
			}
		})
	}
}

// TestValidateTargetRefusesAnUntrimmedURL is P2-8: "reject, do not trim" applied
// to one of the two configured values is a principle with a hole in it. The
// sibling validator refuses an untrimmed raw value before parsing, and this one
// now does too.
func TestValidateTargetRefusesAnUntrimmedURL(t *testing.T) {
	for _, raw := range []string{
		"https://peer.invalid/ensure ",
		" https://peer.invalid/ensure",
		"https://peer.invalid/ensure\n",
		"",
	} {
		tgt := testTarget(raw)
		tgt.EnsureURL = raw
		if err := ValidateTarget(tgt); err == nil {
			t.Errorf("%q must be refused: it survives url.Parse, reaches EscapedPath() as %%20, "+
				"signature and wire path agree, and the peer answers 404 — retried to abandoned", raw)
		}
	}
}

func driveTarget(url string) Target {
	return Target{
		Name:          "drive",
		EnsureURL:     url,
		Auth:          AuthInternalToken,
		InternalToken: "drive-internal-token-0123456789abcdef",
		Timeout:       2 * time.Second,
	}
}

func driveRequestFixture(projectID string) DriveRequest {
	return DriveRequest{
		Name:          "完整项目名",
		OctoSpaceID:   "space-1",
		SuperAdminUID: "owner-1",
		ProjectID:     projectID,
	}
}

func TestCreateDriveSpaceNameCharacterLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		text      string
		wantError bool
	}{
		{"project CJK maximum", strings.Repeat("中", 30), false},
		{"Drive Unicode boundary", strings.Repeat("中😀", 32), false},
		{"over Drive boundary", strings.Repeat("中😀", 32) + "文", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body DriveRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode Drive request: %v", err)
				}
				received <- body.Name
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
			req := driveRequestFixture("project-name-boundary")
			req.Name = tc.text
			_, err := NewClient(nil, nil).CreateDriveSpace(context.Background(),
				driveTarget(server.URL+"/v1/internal/drive/spaces"), req)
			if tc.wantError {
				if err == nil || Category(err) != "invalid_request" {
					t.Fatalf("expected invalid_request, got %v", err)
				}
				select {
				case <-received:
					t.Fatal("an over-limit name must be rejected before HTTP")
				default:
				}
				return
			}
			if err != nil {
				t.Fatalf("valid Unicode name rejected: %v", err)
			}
			if got := <-received; got != tc.text {
				t.Fatalf("Drive received %q, want complete name %q", got, tc.text)
			}
		})
	}
}

func TestCreateDriveSpaceUsesInternalTokenAndProjectBody(t *testing.T) {
	const projectID = "project-1"
	var got DriveRequest
	var gotPath string
	var gotToken string
	var gotSignature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get(HeaderInternalToken)
		gotSignature = r.Header.Get(HeaderSignature)
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode Drive request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		// The response contains the remote resource id, but the caller must not depend on
		// or persist it: project_id is the stable lookup key on this integration.
		_, _ = w.Write([]byte(`{"id":"shared:remote-space","project_id":"project-1"}`))
	}))
	defer server.Close()

	target := driveTarget(server.URL + "/v1/internal/drive/spaces")
	resp, err := NewClient(nil, nil).CreateDriveSpace(context.Background(), target, driveRequestFixture(projectID))
	if err != nil {
		t.Fatalf("CreateDriveSpace: %v", err)
	}
	if resp.Duplicate {
		t.Fatal("a 201 response must be reported as a new success, not duplicate")
	}
	if gotPath != "/v1/internal/drive/spaces" {
		t.Fatalf("path = %q, want /v1/internal/drive/spaces", gotPath)
	}
	if gotToken != target.InternalToken {
		t.Fatalf("X-Internal-Token = %q, want configured internal token", gotToken)
	}
	if gotSignature != "" {
		t.Fatalf("Drive internal-token request carried Fleet HMAC header %q", gotSignature)
	}
	if got != driveRequestFixture(projectID) {
		t.Fatalf("body = %+v, want %+v", got, driveRequestFixture(projectID))
	}
}

func TestCreateDriveSpaceOnlyAcceptsExactProjectDuplicateConflict(t *testing.T) {
	const projectID = "project-1"
	cases := []struct {
		name       string
		body       string
		wantOK     bool
		wantDup    bool
		wantStatus string
	}{
		{
			name:    "same project",
			body:    `{"error":"conflict","message":"workspace_id \"project-1\" already bound to a space"}`,
			wantOK:  true,
			wantDup: true,
		},
		{
			name:       "different project",
			body:       `{"error":"conflict","message":"workspace_id \"project-2\" already bound to a space"}`,
			wantStatus: "target_4xx",
		},
		{
			name:       "generic conflict",
			body:       `{"error":"conflict","message":"space name already exists"}`,
			wantStatus: "target_4xx",
		},
		{
			name:       "folder depth conflict",
			body:       `{"error":"folder_depth_exceeded","message":"folder depth limit exceeded"}`,
			wantStatus: "target_4xx",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			resp, err := NewClient(nil, nil).CreateDriveSpace(
				context.Background(),
				driveTarget(server.URL+"/v1/internal/drive/spaces"),
				driveRequestFixture(projectID),
			)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("same-project duplicate returned error: %v", err)
				}
				if !resp.Duplicate {
					t.Fatal("same-project duplicate must be marked idempotent success")
				}
				return
			}
			if err == nil {
				t.Fatal("non-identical 409 must remain a failure")
			}
			if got := Category(err); got != tc.wantStatus {
				t.Fatalf("category = %q, want %q (err=%v)", got, tc.wantStatus, err)
			}
			if resp.Duplicate {
				t.Fatal("non-identical 409 was incorrectly accepted as duplicate")
			}
		})
	}
}

func TestCreateDriveSpaceDoesNotUseFleetHMACSecret(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { hits++ }))
	defer server.Close()

	target := Target{
		Name:      "drive",
		EnsureURL: server.URL + "/v1/internal/drive/spaces",
		Auth:      AuthInternalToken,
		Secret:    testSecret,
		Timeout:   2 * time.Second,
	}
	_, err := NewClient(nil, nil).CreateDriveSpace(context.Background(), target, driveRequestFixture("project-1"))
	if err == nil || Category(err) != "invalid_target" {
		t.Fatalf("Drive with only Fleet Secret = %v, want invalid_target", err)
	}
	if hits != 0 {
		t.Fatalf("invalid Drive credential reached target %d times", hits)
	}
}
