package cardactiondispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewRegistryAcceptsHTTPCallbackDestinations locks the http-actions
// follow-up: OCTO_CARD_ACTION_ROUTES[].url is itself the exact allowlist and
// http:// is a valid transport for every destination shape that only makes
// sense inside an operator-controlled network. Hostname form (K8s Service DNS,
// docker service name, host.docker.internal, IP literal) is intentionally not
// inspected.
func TestNewRegistryAcceptsHTTPCallbackDestinations(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"kubernetes service short form", "http://smart-summary.dmwork-test.svc:8080/v1/card-actions/decide"},
		{"kubernetes service full FQDN", "http://smart-summary.dmwork-test.svc.cluster.local:8080/v1/card-actions/decide"},
		{"docker compose service short name", "http://smart-summary:8080/v1/card-actions/decide"},
		{"host docker internal", "http://host.docker.internal:8080/v1/card-actions/decide"},
		{"IPv4 literal with port", "http://127.0.0.1:8080/v1/card-actions/decide"},
		{"IPv6 literal with port", "http://[::1]:8080/v1/card-actions/decide"},
		{"HTTPS still accepted", "https://tasks.internal/v1/card-actions/decide"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validRouteSpec()
			spec.URL = tt.url
			if _, err := NewRegistry([]RouteSpec{spec}, testGetenv); err != nil {
				t.Fatalf("NewRegistry() error = %v; want acceptance of %s", err, tt.url)
			}
		})
	}
}

// TestValidateCallbackURLRejectsUnsafeShapes ensures the scheme relaxation did
// not weaken the other URL-shape defenses. These previously lived only in the
// combined allowlist path.
func TestValidateCallbackURLRejectsUnsafeShapes(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"whitespace padding", " https://docs.internal/v1/card-actions/decide "},
		{"unsupported scheme file", "file:///etc/passwd"},
		{"unsupported scheme gopher", "gopher://docs.internal/v1/card-actions/decide"},
		{"non absolute relative", "/v1/card-actions/decide"},
		{"opaque form", "https:opaque"},
		{"credentials", "https://user:pass@docs.internal/v1/card-actions/decide"},
		{"query string", "https://docs.internal/v1/card-actions/decide?trace=1"},
		{"trailing question mark", "https://docs.internal/v1/card-actions/decide?"},
		{"fragment", "https://docs.internal/v1/card-actions/decide#anchor"},
		{"trailing hash empty fragment", "https://docs.internal/v1/card-actions/decide#"},
		{"embedded hash in path segment", "https://docs.internal/v1/card-actions/de#cide"},
		{"http missing host", "http:///v1/card-actions/decide"},
		{"http port only no host", "http://:8080/v1/card-actions/decide"},
		{"https port only no host", "https://:443/v1/card-actions/decide"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := validateCallbackURL(tt.url); err == nil {
				t.Fatalf("validateCallbackURL(%q) error = nil; want rejection", tt.url)
			}
		})
	}
}

// TestHTTPDelivererDeliversPlainHTTPWithHMACAndDisciplinedTransport is the
// end-to-end guard for the http-actions follow-up. It spins up a plain
// httptest.NewServer (no TLS) and checks that:
//   - the plain HTTP scheme is accepted by NewRegistry via the shared route
//     validator;
//   - HMAC signing still authenticates the request over cleartext;
//   - the deliverer refuses to follow redirects on HTTP just like on HTTPS;
//   - HTTP_PROXY environment variables cannot re-open SSRF for plain HTTP.
func TestHTTPDelivererDeliversPlainHTTPWithHMACAndDisciplinedTransport(t *testing.T) {
	// Guard against the deliverer accidentally honoring proxy env vars.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")

	var signatureOK atomic.Bool
	var callCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if Verify(testCallbackSecret, r.Header.Get(HeaderSignature), r.Method, r.URL.EscapedPath(),
			r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderEventID), raw) {
			signatureOK.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disposition":"applied","state":"approved","requester_uid":"user-a"}`))
	}))
	defer server.Close()

	if scheme := server.URL[:len("http://")]; scheme != "http://" {
		t.Fatalf("httptest.NewServer scheme = %q; want plain http", scheme)
	}

	registry := registryForTestServer(t, server.URL+"/v1/card-actions/decide", 1)
	route := registry.Resolve("notification", "docs", "access_request.decision").Route
	if route == nil {
		t.Fatal("plain http route did not register")
	}

	// Pass nil transport so the deliverer builds its production default
	// (Proxy=nil). This exercises the disciplined transport itself, not the
	// test server's transport.
	deliverer := NewHTTPDeliverer(nil, func() time.Time { return time.Unix(1_784_073_600, 0) })
	result, err := deliverer.Deliver(context.Background(), route, DecisionRequestFromEvent(testDispatchEvent()))
	if err != nil {
		t.Fatalf("Deliver() error = %v (HTTP_PROXY leaked into transport?)", err)
	}
	if !signatureOK.Load() {
		t.Fatal("callback HMAC signature did not verify over cleartext")
	}
	if result.State != StateApproved {
		t.Fatalf("Deliver() result state = %q, want approved", result.State)
	}
	if got := callCount.Load(); got != 1 {
		t.Fatalf("callback hit count = %d, want 1", got)
	}
	// Sanity: the deliverer's transport really is proxy-less.
	if os.Getenv("HTTP_PROXY") == "" {
		t.Fatal("test setup lost HTTP_PROXY before assertion")
	}
}

func TestHTTPDelivererRefusesRedirectsOnPlainHTTP(t *testing.T) {
	var redirected atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	registry := registryForTestServer(t, server.URL+"/start", 1)
	deliverer := NewHTTPDeliverer(nil, time.Now)
	_, err := deliverer.Deliver(context.Background(),
		registry.Resolve("notification", "docs", "access_request.decision").Route,
		DecisionRequestFromEvent(testDispatchEvent()))
	if err == nil {
		t.Fatal("Deliver() error = nil for plain-HTTP redirect")
	}
	if redirected.Load() {
		t.Fatal("HTTP deliverer followed a plain-HTTP callback redirect")
	}
	// Categorize the error the same way HTTPS redirect rejection does so
	// operator alerts stay consistent across schemes.
	var deliveryErr *DeliveryError
	if !errorsAsDelivery(err, &deliveryErr) || deliveryErr.Category != "redirect_rejected" {
		t.Fatalf("Deliver() error = %v; want DeliveryError{Category: redirect_rejected}", err)
	}
}

// errorsAsDelivery is a tiny helper so this file does not import "errors"
// just for the type assertion.
func errorsAsDelivery(err error, target **DeliveryError) bool {
	de, ok := err.(*DeliveryError)
	if !ok {
		return false
	}
	*target = de
	return true
}
