package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/cache"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// LanguageResolver hydrates UserInfo.Language with the freshest user-language
// preference (Redis cache → DB → ""). It is intentionally a tiny interface
// shaped at the consumer side so pkg/auth does not need to import the i18n
// package or know about Redis / DB primitives. The concrete implementation
// lives in modules/user.
type LanguageResolver interface {
	Resolve(ctx context.Context, uid string) (string, error)
}

// CacheTokenParser implements octo-lib's wkhttp.TokenParser using the shared
// pkg/auth codec. It supersedes octo-lib's legacyTokenParser so that octo-server
// can write v2 JSON envelopes while still decoding any legacy uid@name[@role]
// values left in cache from older binaries.
//
// When a LanguageResolver is injected via WithLanguageResolver, Parse hits the
// resolver after Decode to upgrade the token-cache language snapshot to the
// authoritative value before octo-lib's AuthMiddleware stores UserInfo on the
// request context. Resolver failures are non-fatal — the decoded snapshot is
// preserved so a Redis/DB outage degrades to "stale language" rather than
// "authentication failure".
//
// Construct once at boot and register with WKHttp.SetTokenParser; the parser
// is safe for concurrent use as long as the underlying cache + resolver are.
type CacheTokenParser struct {
	Cache    cache.Cache
	Prefix   string
	resolver LanguageResolver
}

// ParserOption configures optional CacheTokenParser behaviour.
type ParserOption func(*CacheTokenParser)

// WithLanguageResolver wires a LanguageResolver into the parser; nil resolver
// is a no-op so callers can pass an interface value that may be unset in test
// environments without an extra guard.
func WithLanguageResolver(r LanguageResolver) ParserOption {
	return func(p *CacheTokenParser) {
		if r != nil {
			p.resolver = r
		}
	}
}

// NewCacheTokenParser is a convenience constructor; nil cache is a programmer
// error and panics rather than silently degrading to a parser that fails every
// request.
func NewCacheTokenParser(c cache.Cache, prefix string, opts ...ParserOption) *CacheTokenParser {
	if c == nil {
		panic("auth: NewCacheTokenParser requires non-nil cache")
	}
	p := &CacheTokenParser{Cache: c, Prefix: prefix}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Parse implements wkhttp.TokenParser. ctx is propagated to the optional
// LanguageResolver so resolver implementations can honour deadlines /
// cancellation set by the surrounding request.
func (p *CacheTokenParser) Parse(ctx context.Context, token string) (wkhttp.UserInfo, error) {
	if strings.TrimSpace(token) == "" {
		return wkhttp.UserInfo{}, wkhttp.ErrTokenMissing
	}
	raw, err := p.Cache.Get(p.Prefix + token)
	if err != nil {
		return wkhttp.UserInfo{}, fmt.Errorf("auth: load token from cache: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return wkhttp.UserInfo{}, wkhttp.ErrTokenNotFound
	}
	info, err := Decode(raw)
	if err != nil {
		if errors.Is(err, ErrEmptyToken) {
			return wkhttp.UserInfo{}, wkhttp.ErrTokenNotFound
		}
		return wkhttp.UserInfo{}, fmt.Errorf("%w: %v", wkhttp.ErrTokenInvalid, err)
	}
	language := info.Language
	if p.resolver != nil {
		// Resolver is the authoritative source per UserLanguageResolver's
		// documented contract:
		//   * rerr != nil  → keep the token-cache snapshot. Authentication
		//     must not 5xx because user_language cache is momentarily
		//     unreachable; the snapshot is the agreed last-resort fallback.
		//   * resolved == "" (no error) → user has no explicit preference
		//     right now (DB was cleared, negative-cache hit, or stored
		//     value normalised away). Drop the snapshot so EarlyMiddleware's
		//     Accept-Language / default decision wins downstream. Otherwise
		//     a token minted earlier with a real language tag would keep
		//     promoting LanguageSourceUser long after the user opted out
		//     — a stale-read regression worth a dedicated test, see
		//     parser_test.go::TestCacheTokenParserResolverEmptyClearsSnapshot.
		//   * resolved != ""  → use the fresh authoritative value.
		resolved, rerr := p.resolver.Resolve(ctx, info.UID)
		if rerr == nil {
			language = resolved
		}
	}
	return wkhttp.UserInfo{
		UID:      info.UID,
		Name:     info.Name,
		Role:     info.Role,
		Language: language,
	}, nil
}
