package agentmailgateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

func TestProvisionGatewayIdentityUsesExistingSignedClientContract(t *testing.T) {
	secret := []byte(strings.Repeat("g", 32))
	fixedNow := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != gatewayProvisioningPath {
			t.Errorf("provisioning request = %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read provisioning body: %v", err)
		}
		if string(body) != `{"localpart":"guobin"}` {
			t.Errorf("provisioning body = %s", body)
		}
		claims := verifyAssertionForTest(t, secret, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if claims.Issuer != assertionIssuer || claims.Subject != "user-a" || claims.SpaceID != "space-a" ||
			claims.Method != http.MethodPost || claims.RequestURI != gatewayProvisioningPath {
			t.Errorf("provisioning claims = %#v", claims)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":true}`)
	}))
	defer upstream.Close()

	gateway := newTestGateway(t, upstream.URL, secret, fixedNow)
	cache, err := lru.New[string, struct{}](8)
	if err != nil {
		t.Fatal(err)
	}
	gateway.provisioned = cache
	gateway.nonceSource = bytes.NewReader(make([]byte, 32))
	gateway.resolveLocalpart = func(_ context.Context, uid string) (string, error) {
		if uid != "user-a" {
			t.Fatalf("resolved uid = %q", uid)
		}
		return "guobin", nil
	}
	gateway.provisionIdentity = gateway.provisionGatewayIdentity

	if err := gateway.ensureProvisioned(context.Background(), "user-a", "space-a"); err != nil {
		t.Fatal(err)
	}
	if err := gateway.ensureProvisioned(context.Background(), "user-a", "space-a"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provisioning calls = %d, want cached single call", calls.Load())
	}
}

func TestProvisionGatewayIdentityBoundsStalledResponseBody(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.cfg.timeout = 20 * time.Millisecond
	stalled := newBlockingReadCloser()
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 1, Body: stalled}, nil
	})}

	done := make(chan error, 1)
	go func() {
		done <- gateway.provisionGatewayIdentity(context.Background(), "user-a", "space-a")
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled provisioning response unexpectedly succeeded")
		}
	case <-time.After(250 * time.Millisecond):
		_ = stalled.Close()
		<-done
		t.Fatal("stalled provisioning response outlived the gateway timeout")
	}
}

func TestEnsureProvisionedCoalescesConcurrentFirstUse(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	cache, err := lru.New[string, struct{}](8)
	if err != nil {
		t.Fatal(err)
	}
	gateway.provisioned = cache
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	gateway.provisionIdentity = func(context.Context, string, string) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil
	}

	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- gateway.ensureProvisioned(context.Background(), "user-a", "space-a")
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent provisioning calls = %d, want 1", calls.Load())
	}
}

func TestEnsureProvisionedLeaderCancellationDoesNotPoisonFollowers(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.cfg.timeout = 500 * time.Millisecond
	cache, err := lru.New[string, struct{}](8)
	if err != nil {
		t.Fatal(err)
	}
	gateway.provisioned = cache
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	gateway.provisionIdentity = func(ctx context.Context, _, _ string) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- gateway.ensureProvisioned(leaderCtx, "user-a", "space-a")
	}()
	<-started

	followerDone := make(chan error, 1)
	go func() {
		followerDone <- gateway.ensureProvisioned(context.Background(), "user-a", "space-a")
	}()
	cancelLeader()
	select {
	case err := <-leaderDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled leader error = %v, want context.Canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancelled leader remained blocked on shared provisioning")
	}

	close(release)
	select {
	case err := <-followerDone:
		if err != nil {
			t.Fatalf("healthy follower inherited leader cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy follower did not receive the shared provisioning result")
	}
	if calls.Load() != 1 {
		t.Fatalf("provisioning calls = %d, want 1 shared call", calls.Load())
	}
}

func TestEnsureProvisionedBoundsContinuouslyProgressingResponse(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	gateway.cfg.timeout = 30 * time.Millisecond
	gateway.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: &contextDripReadCloser{
				ctx:      request.Context(),
				interval: 5 * time.Millisecond,
			},
		}, nil
	})}
	gateway.provisionIdentity = gateway.provisionGatewayIdentity

	startedAt := time.Now()
	err := gateway.ensureProvisioned(context.Background(), "user-a", "space-a")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("continuously progressing response error = %v, want context deadline", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("continuously progressing response outlived total timeout: %v", elapsed)
	}
}

func TestEnsureProvisionedCacheHitRefreshesLRURecency(t *testing.T) {
	gateway := newTestGateway(t, "http://octo-mail.test", []byte(strings.Repeat("g", 32)), time.Now())
	cache, err := lru.New[string, struct{}](2)
	if err != nil {
		t.Fatal(err)
	}
	gateway.provisioned = cache
	keyA := "user-a\x00space-a"
	keyB := "user-b\x00space-a"
	keyC := "user-c\x00space-a"
	cache.Add(keyA, struct{}{})
	cache.Add(keyB, struct{}{})

	if err := gateway.ensureProvisioned(context.Background(), "user-a", "space-a"); err != nil {
		t.Fatal(err)
	}
	cache.Add(keyC, struct{}{})

	if !cache.Contains(keyA) || !cache.Contains(keyC) || cache.Contains(keyB) {
		t.Fatalf("cache recency = A:%v B:%v C:%v, want A and C retained",
			cache.Contains(keyA), cache.Contains(keyB), cache.Contains(keyC))
	}
}

func TestProxyDoesNotReachBusinessUpstreamWhenProvisioningFails(t *testing.T) {
	secret := []byte(strings.Repeat("g", 32))
	gateway := newTestGateway(t, "http://octo-mail.test", secret, time.Now())
	gateway.provisionIdentity = func(context.Context, string, string) error {
		return errors.New("provisioning unavailable")
	}
	var upstreamCalls atomic.Int32
	gateway.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}

	recorder := httptest.NewRecorder()
	newProxyRouter(gateway, "user-a", "space-a").ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, "/v1/mail-gateway/webapi/v0/mailboxes", nil),
	)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("failed provisioning reached upstream %d times", upstreamCalls.Load())
	}
}

func TestNormalizeGatewayLocalpartUsesStableServerIdentifiers(t *testing.T) {
	for name, test := range map[string]struct {
		candidates []string
		want       string
	}{
		"username":              {[]string{" Guo.Bin ", "1001", "uid-a"}, "guo.bin"},
		"fallback short number": {[]string{"中文", "1001", "uid-a"}, "1001"},
		"sanitize":              {[]string{"Alice / Ops", "", "uid-a"}, "alice-ops"},
		"collapse dots":         {[]string{"alice..ops", "", "uid-a"}, "alice.ops"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := normalizeGatewayLocalpart(test.candidates...); got != test.want {
				t.Fatalf("normalized localpart = %q, want %q", got, test.want)
			}
		})
	}
}

type contextDripReadCloser struct {
	ctx      context.Context
	interval time.Duration
}

func (r *contextDripReadCloser) Read(dst []byte) (int, error) {
	timer := time.NewTimer(r.interval)
	defer timer.Stop()
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-timer.C:
		dst[0] = 'x'
		return 1, nil
	}
}

func (*contextDripReadCloser) Close() error { return nil }
