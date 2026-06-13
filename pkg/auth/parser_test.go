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

// stubResolver is used to exercise the LanguageResolver hook without pulling
// modules/user into pkg/auth's test deps.
type stubResolver struct {
	lang string
	err  error
	gotUID string
}

func (s *stubResolver) Resolve(_ context.Context, uid string) (string, error) {
	s.gotUID = uid
	return s.lang, s.err
}

func TestCacheTokenParserResolverUpgradesLanguage(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	encoded, _ := Encode(TokenInfo{UID: "u1", Name: "alice", Role: "admin", Language: "zh-CN"})
	_ = c.Set(testPrefix+"tok1", encoded)

	resolver := &stubResolver{lang: "en-US"}
	p := NewCacheTokenParser(c, testPrefix, WithLanguageResolver(resolver))
	got, err := p.Parse(context.Background(), "tok1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Language != "en-US" {
		t.Fatalf("Language = %q, want resolver value en-US", got.Language)
	}
	if resolver.gotUID != "u1" {
		t.Fatalf("resolver got uid %q, want u1", resolver.gotUID)
	}
}

func TestCacheTokenParserResolverFailureKeepsSnapshot(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	encoded, _ := Encode(TokenInfo{UID: "u1", Name: "alice", Language: "zh-CN"})
	_ = c.Set(testPrefix+"tok1", encoded)

	resolver := &stubResolver{err: errors.New("redis down")}
	p := NewCacheTokenParser(c, testPrefix, WithLanguageResolver(resolver))
	got, err := p.Parse(context.Background(), "tok1")
	if err != nil {
		t.Fatalf("Parse must not surface resolver failure, got %v", err)
	}
	if got.Language != "zh-CN" {
		t.Fatalf("Language = %q, want snapshot zh-CN (resolver failed)", got.Language)
	}
}

// TestCacheTokenParserResolverEmptyClearsSnapshot pins the documented
// UserLanguageResolver contract: an empty (no-error) resolver result is
// authoritative "no explicit preference" and must drop the token-cache
// snapshot so EarlyMiddleware's Accept-Language / default wins. Without
// this, clearing user.language in the DB would not free a previously
// minted token from a stale language until the next login — the very
// stale-read regression flagged in PR #181 review.
func TestCacheTokenParserResolverEmptyClearsSnapshot(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	encoded, _ := Encode(TokenInfo{UID: "u1", Name: "alice", Language: "zh-CN"})
	_ = c.Set(testPrefix+"tok1", encoded)

	resolver := &stubResolver{lang: ""}
	p := NewCacheTokenParser(c, testPrefix, WithLanguageResolver(resolver))
	got, err := p.Parse(context.Background(), "tok1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Language != "" {
		t.Fatalf("Language = %q, want \"\" (resolver authoritative empty must drop snapshot)", got.Language)
	}
}

func TestWithLanguageResolverNilIsNoOp(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	encoded, _ := Encode(TokenInfo{UID: "u1", Name: "alice"})
	_ = c.Set(testPrefix+"tok1", encoded)

	p := NewCacheTokenParser(c, testPrefix, WithLanguageResolver(nil))
	if p.resolver != nil {
		t.Fatal("nil resolver option must not set the field")
	}
	if _, err := p.Parse(context.Background(), "tok1"); err != nil {
		t.Fatalf("Parse should still succeed without a resolver, got %v", err)
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

// stubRoleResolver exercises the RoleResolver hook without pulling modules/user
// into pkg/auth's test deps.
type stubRoleResolver struct {
	role   string
	err    error
	gotUID string
}

func (s *stubRoleResolver) ResolveRole(_ context.Context, uid string) (string, error) {
	s.gotUID = uid
	return s.role, s.err
}

// TestCacheTokenParserRoleResolverOverridesSnapshot pins the core revocation
// fix: the role returned to AuthMiddleware comes from the resolver (DB truth),
// not the value baked into the token at issuance.
func TestCacheTokenParserRoleResolverOverridesSnapshot(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	// Token was minted while the user was superAdmin...
	encoded, _ := Encode(TokenInfo{UID: "u1", Name: "alice", Role: "superAdmin"})
	_ = c.Set(testPrefix+"tok1", encoded)

	// ...but the DB now says plain admin (partial demotion).
	resolver := &stubRoleResolver{role: "admin"}
	p := NewCacheTokenParser(c, testPrefix, WithRoleResolver(resolver))
	got, err := p.Parse(context.Background(), "tok1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Role != "admin" {
		t.Fatalf("Role = %q, want resolver value admin (token snapshot superAdmin must not win)", got.Role)
	}
	if resolver.gotUID != "u1" {
		t.Fatalf("resolver got uid %q, want u1", resolver.gotUID)
	}
}

// TestCacheTokenParserRoleResolverEmptyDropsRole covers full demotion: a token
// minted while admin must stop granting admin the moment the DB role is empty.
func TestCacheTokenParserRoleResolverEmptyDropsRole(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	encoded, _ := Encode(TokenInfo{UID: "u1", Name: "alice", Role: "admin"})
	_ = c.Set(testPrefix+"tok1", encoded)

	resolver := &stubRoleResolver{role: ""}
	p := NewCacheTokenParser(c, testPrefix, WithRoleResolver(resolver))
	got, err := p.Parse(context.Background(), "tok1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Role != "" {
		t.Fatalf("Role = %q, want \"\" (resolver authoritative empty must drop the baked-in admin role)", got.Role)
	}
}

// TestCacheTokenParserRoleResolverFailureKeepsSnapshot pins fail-open: a
// cache/DB outage must not 5xx auth, so the token's role snapshot is preserved.
func TestCacheTokenParserRoleResolverFailureKeepsSnapshot(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	encoded, _ := Encode(TokenInfo{UID: "u1", Name: "alice", Role: "admin"})
	_ = c.Set(testPrefix+"tok1", encoded)

	resolver := &stubRoleResolver{err: errors.New("redis down")}
	p := NewCacheTokenParser(c, testPrefix, WithRoleResolver(resolver))
	got, err := p.Parse(context.Background(), "tok1")
	if err != nil {
		t.Fatalf("Parse must not surface resolver failure, got %v", err)
	}
	if got.Role != "admin" {
		t.Fatalf("Role = %q, want snapshot admin (resolver failed)", got.Role)
	}
}

func TestWithRoleResolverNilIsNoOp(t *testing.T) {
	t.Parallel()
	c := newFakeCache()
	encoded, _ := Encode(TokenInfo{UID: "u1", Name: "alice", Role: "admin"})
	_ = c.Set(testPrefix+"tok1", encoded)

	p := NewCacheTokenParser(c, testPrefix, WithRoleResolver(nil))
	if p.roleResolver != nil {
		t.Fatal("nil role resolver option must not set the field")
	}
	got, err := p.Parse(context.Background(), "tok1")
	if err != nil {
		t.Fatalf("Parse should still succeed without a role resolver, got %v", err)
	}
	if got.Role != "admin" {
		t.Fatalf("without a resolver the token snapshot role must pass through, got %q", got.Role)
	}
}
