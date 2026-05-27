package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/cache"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// CacheTokenParser implements octo-lib's wkhttp.TokenParser using the shared
// pkg/auth codec. It supersedes octo-lib's legacyTokenParser so that octo-server
// can write v2 JSON envelopes while still decoding any legacy uid@name[@role]
// values left in cache from older binaries.
//
// Construct once at boot and register with WKHttp.SetTokenParser; the parser
// is safe for concurrent use as long as the underlying cache is.
type CacheTokenParser struct {
	Cache  cache.Cache
	Prefix string
}

// NewCacheTokenParser is a convenience constructor; nil cache is a programmer
// error and panics rather than silently degrading to a parser that fails every
// request.
func NewCacheTokenParser(c cache.Cache, prefix string) *CacheTokenParser {
	if c == nil {
		panic("auth: NewCacheTokenParser requires non-nil cache")
	}
	return &CacheTokenParser{Cache: c, Prefix: prefix}
}

// Parse implements wkhttp.TokenParser. The context is currently unused
// (cache.Cache lookups are synchronous) but kept on the signature for future
// upgrades — same rationale as octo-lib's legacy parser.
func (p *CacheTokenParser) Parse(_ context.Context, token string) (wkhttp.UserInfo, error) {
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
	return wkhttp.UserInfo{
		UID:      info.UID,
		Name:     info.Name,
		Role:     info.Role,
		Language: info.Language,
	}, nil
}
