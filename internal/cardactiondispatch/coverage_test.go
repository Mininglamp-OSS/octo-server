package cardactiondispatch

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	rd "github.com/go-redis/redis"
)

type idleDispatchQueue struct {
	claims   atomic.Int32
	reclaims atomic.Int32
}

func (q *idleDispatchQueue) Claim(time.Time, time.Duration) (*Lease, error) {
	q.claims.Add(1)
	return nil, nil
}
func (*idleDispatchQueue) Renew(int64, string, time.Time, time.Duration) (bool, error) {
	return true, nil
}
func (*idleDispatchQueue) Defer(int64, string, time.Time) (bool, error) { return true, nil }
func (*idleDispatchQueue) Ack(int64, string) (bool, error)              { return true, nil }
func (*idleDispatchQueue) Nack(Lease, time.Time, time.Duration, int, string) (NackOutcome, error) {
	return NackRequeued, nil
}
func (q *idleDispatchQueue) ReclaimExpired(time.Time, int) (int, error) {
	q.reclaims.Add(1)
	return 0, nil
}

func TestDispatcherLifecycleStartsStopsAndRejectsDoubleStart(t *testing.T) {
	registry, err := NewRegistry([]RouteSpec{validRouteSpec()}, []string{validRouteSpec().URL}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	queue := &idleDispatchQueue{}
	dispatcher, err := NewDispatcher(queue, registry, &stubDeliverer{}, FinalizerFunc(func(context.Context, Event, DecisionResult) error { return nil }), DispatcherConfig{
		LeaseDuration: time.Second, PollInterval: time.Millisecond, ReclaimInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := dispatcher.Start(ctx); err == nil {
		t.Fatal("second Start() error = nil")
	}
	deadline := time.After(time.Second)
	for queue.claims.Load() == 0 || queue.reclaims.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("dispatcher did not poll and reclaim before deadline")
		default:
		}
	}
	dispatcher.Stop()
	dispatcher.Stop() // idempotent
	if !strings.Contains(dispatcher.String(), "lease=1s") {
		t.Fatalf("String() = %q", dispatcher.String())
	}
}

type stubDeliverer struct{}

func (*stubDeliverer) Deliver(context.Context, *Route, DecisionRequest) (DecisionResult, error) {
	return DecisionResult{}, nil
}

func TestConfigAndValidationErrorPathsFailClosed(t *testing.T) {
	if got := LoadAllowedURLs(" https://a.internal/x, ,https://b.internal/y "); len(got) != 2 {
		t.Fatalf("LoadAllowedURLs() = %v", got)
	}
	for _, raw := range []string{
		`not-json`,
		`[{"sender_uid":"notification","unknown":true}]`,
		`[] {}`,
		`[{"sender_uid":"notification","owner":"docs","action_type":"access_request.decision","url":"https://docs.internal/x","secret_env":"OCTO_DOCS_CARD_ACTION_SECRET","timeout_ms":-1}]`,
	} {
		if _, err := LoadRouteSpecs(raw); err == nil {
			t.Fatalf("LoadRouteSpecs(%q) error = nil", raw)
		}
	}

	tests := []struct {
		name   string
		mutate func(*RouteSpec)
	}{
		{"invalid sender", func(s *RouteSpec) { s.SenderUID = "iwh_bad" }},
		{"invalid owner", func(s *RouteSpec) { s.Owner = "Bad Owner" }},
		{"invalid action", func(s *RouteSpec) { s.ActionType = "Bad Action" }},
		{"invalid secret env", func(s *RouteSpec) { s.SecretEnv = "bad-env" }},
		{"timeout too small", func(s *RouteSpec) { s.Timeout = time.Millisecond }},
		{"attempts too many", func(s *RouteSpec) { s.MaxAttempts = 11 }},
		{"base backoff too small", func(s *RouteSpec) { s.BaseBackoff = time.Millisecond }},
		{"max backoff below base", func(s *RouteSpec) { s.MaxBackoff = 500 * time.Millisecond }},
		{"concurrency too high", func(s *RouteSpec) { s.MaxInFlight = 101 }},
		{"query URL", func(s *RouteSpec) { s.URL += "?dynamic=1" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validRouteSpec()
			tt.mutate(&spec)
			_, err := NewRegistry([]RouteSpec{spec}, []string{spec.URL}, testGetenv)
			if err == nil {
				t.Fatal("NewRegistry() error = nil")
			}
		})
	}
}

func TestDecisionAndQueueInputBounds(t *testing.T) {
	invalidResults := []string{
		`{"disposition":"applied","state":"approved"} {}`,
		`{"disposition":"applied","state":"approved","requester_uid":" padded "}`,
		`{"disposition":"applied","state":"approved","display":{"":"x"}}`,
	}
	for _, raw := range invalidResults {
		if _, err := DecodeDecisionResult(strings.NewReader(raw)); err == nil {
			t.Fatalf("DecodeDecisionResult(%q) error = nil", raw)
		}
	}

	client := rd.NewClient(&rd.Options{Addr: "127.0.0.1:1"})
	defer client.Close()
	if _, err := NewRedisQueue(nil, QueueConfig{Prefix: "x", LiveTTL: time.Second, DLQRetention: time.Second}); err == nil {
		t.Fatal("NewRedisQueue(nil) error = nil")
	}
	if _, err := NewRedisQueue(client, QueueConfig{Prefix: "bad prefix", LiveTTL: time.Second, DLQRetention: time.Second}); err == nil {
		t.Fatal("NewRedisQueue(bad prefix) error = nil")
	}
	queue, err := NewRedisQueue(client, QueueConfig{Prefix: "bounds", LiveTTL: time.Second, DLQRetention: time.Second})
	if err != nil {
		t.Fatalf("NewRedisQueue() error = %v", err)
	}
	if err := queue.Enqueue(Event{}, time.Now()); err == nil {
		t.Fatal("Enqueue(invalid event) error = nil")
	}
	if _, err := queue.Claim(time.Now(), 0); err == nil {
		t.Fatal("Claim(zero lease) error = nil")
	}
	if _, err := queue.Nack(Lease{}, time.Now(), -time.Second, 1, "x"); err == nil {
		t.Fatal("Nack(negative delay) error = nil")
	}
	if _, err := queue.ReclaimExpired(time.Now(), 0); err == nil {
		t.Fatal("ReclaimExpired(zero limit) error = nil")
	}
}

func TestTruncatePreservesUTF8WithinByteLimit(t *testing.T) {
	tests := []struct {
		name  string
		value string
		limit int
		want  string
	}{
		{name: "multibyte boundary", value: "你好", limit: 4, want: "你"},
		{name: "ASCII boundary", value: "abcdef", limit: 4, want: "abcd"},
		{name: "below limit", value: "你好", limit: 6, want: "你好"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.value, tt.limit)
			if got != tt.want || len(got) > tt.limit || !utf8.ValidString(got) {
				t.Fatalf("truncate(%q, %d) = %q, want %q valid UTF-8 within byte limit", tt.value, tt.limit, got, tt.want)
			}
		})
	}
}

func TestDeliveryErrorCarriesRetryMetadataWithoutResponseBody(t *testing.T) {
	cause := errors.New("dial timeout")
	err := &DeliveryError{Category: "transport_failed", retryable: true, cause: cause}
	if !Retryable(err) || !errors.Is(err, cause) || !strings.Contains(err.Error(), "transport_failed") {
		t.Fatalf("delivery error metadata lost: %v", err)
	}
	statusErr := &DeliveryError{Category: "consumer_5xx", Status: 503}
	if !strings.Contains(statusErr.Error(), "503") {
		t.Fatalf("status error = %q", statusErr.Error())
	}
}
