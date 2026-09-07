package projectprovision

import (
	"context"
	"encoding/json"
	"errors"
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

// TestEnsureSignsTheCanonicalRequest pins the wire contract a receiver has to
// implement: the same v1 HMAC over the same canonical string as a card-action
// callback, with the container id in the eventID slot.
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
	if Retryable(err) {
		t.Error("a container id mismatch must not be retryable; retrying cannot repair a wrong mapping")
	}
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
		status    int
		category  string
		retryable bool
	}{
		{http.StatusInternalServerError, "target_5xx", true},
		{http.StatusBadGateway, "target_5xx", true},
		{http.StatusTooManyRequests, "target_rate_limited", true},
		{http.StatusRequestTimeout, "target_timeout", true},
		{http.StatusUnauthorized, "target_rejected_credential", false},
		{http.StatusForbidden, "target_rejected_credential", false},
		// 404 is the shape of "the target has not deployed its ensure endpoint yet",
		// which is the EXPECTED state until brief precondition P-2 lands. It is not
		// retryable in the status sense, and the worker retries it anyway under its own
		// bounded budget — the distinct label is what makes the two situations
		// separable on a dashboard.
		{http.StatusNotFound, "target_no_ensure_endpoint", false},
		{http.StatusBadRequest, "target_4xx", false},
		{http.StatusConflict, "target_4xx", false},
	}
	for _, tc := range cases {
		if got := statusCategory(tc.status); got != tc.category {
			t.Errorf("statusCategory(%d) = %q, want %q", tc.status, got, tc.category)
		}
		if got := statusRetryable(tc.status); got != tc.retryable {
			t.Errorf("statusRetryable(%d) = %v, want %v", tc.status, got, tc.retryable)
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

// TestEnsureCancelledContextIsNotRetryable stops a shutdown from burning a retry.
func TestEnsureCancelledContextIsNotRetryable(t *testing.T) {
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
	if Retryable(err) {
		t.Error("a cancelled parent context must not be retryable: the worker is shutting down")
	}
	var ensureErr *EnsureError
	if !errors.As(err, &ensureErr) {
		t.Fatalf("error is not an *EnsureError: %T", err)
	}
}
