package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// fakeCache implements octo-lib cache.Cache for unit tests; nil values map to
// the "cache miss" behaviour the parser must tolerate.
type fakeCache struct {
	store  map[string]string
	getErr error
}

func newFakeCache() *fakeCache { return &fakeCache{store: map[string]string{}} }

func (c *fakeCache) Set(key, value string) error { c.store[key] = value; return nil }
func (c *fakeCache) SetAndExpire(key, value string, _ time.Duration) error {
	c.store[key] = value
	return nil
}
func (c *fakeCache) Delete(key string) error { delete(c.store, key); return nil }
func (c *fakeCache) Get(key string) (string, error) {
	if c.getErr != nil {
		return "", c.getErr
	}
	return c.store[key], nil
}

const testPrefix = "token:"

func TestCacheTokenParserParseV2(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	encoded, err := Encode(TokenInfo{UID: "u1", Name: "alice", Role: "admin", Language: "zh-CN"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := c.Set(testPrefix+"tok1", encoded); err != nil {
		t.Fatalf("Set: %v", err)
	}

	p := NewCacheTokenParser(c, testPrefix)
	got, err := p.Parse(context.Background(), "tok1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := wkhttp.UserInfo{UID: "u1", Name: "alice", Role: "admin", Language: "zh-CN"}
	if got != want {
		t.Fatalf("Parse = %+v, want %+v", got, want)
	}
}

func TestCacheTokenParserParseLegacy(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	// legacy "uid@name@role" format must keep working during rollout.
	_ = c.Set(testPrefix+"tok1", "u1@alice@admin")
	_ = c.Set(testPrefix+"tok2", "u1@alice")

	p := NewCacheTokenParser(c, testPrefix)

	for _, tc := range []struct {
		token string
		want  wkhttp.UserInfo
	}{
		{"tok1", wkhttp.UserInfo{UID: "u1", Name: "alice", Role: "admin"}},
		{"tok2", wkhttp.UserInfo{UID: "u1", Name: "alice"}},
	} {
		got, err := p.Parse(context.Background(), tc.token)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.token, err)
		}
		if got != tc.want {
			t.Fatalf("Parse(%q) = %+v, want %+v", tc.token, got, tc.want)
		}
	}
}

func TestCacheTokenParserSentinelErrors(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	_ = c.Set(testPrefix+"bad", "garbage-no-at-sign")
	p := NewCacheTokenParser(c, testPrefix)

	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"empty_token", "   ", wkhttp.ErrTokenMissing},
		{"cache_miss", "absent", wkhttp.ErrTokenNotFound},
		{"malformed_payload", "bad", wkhttp.ErrTokenInvalid},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.Parse(context.Background(), tc.token)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Parse(%q): want %v, got %v", tc.token, tc.want, err)
			}
		})
	}
}

func TestCacheTokenParserPropagatesCacheError(t *testing.T) {
	t.Parallel()
	cacheErr := errors.New("redis down")
	c := &fakeCache{store: map[string]string{}, getErr: cacheErr}
	p := NewCacheTokenParser(c, testPrefix)

	_, err := p.Parse(context.Background(), "tok1")
	if !errors.Is(err, cacheErr) {
		t.Fatalf("Parse should propagate cache error via %%w, got %v", err)
	}
	// Cache errors must NOT collapse to ErrTokenNotFound — caller needs to
	// distinguish "session expired" (login again) from "infra down" (retry).
	if errors.Is(err, wkhttp.ErrTokenNotFound) || errors.Is(err, wkhttp.ErrTokenInvalid) {
		t.Fatalf("infra error must not masquerade as auth sentinel, got %v", err)
	}
}

func TestNewCacheTokenParserPanicsOnNilCache(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil cache")
		}
	}()
	_ = NewCacheTokenParser(nil, testPrefix)
}
