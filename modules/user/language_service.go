package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/cache"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// LanguageCacheKeyPrefix is the Redis key prefix for the
// `user_language:{uid}` hot cache. Exported so external infra (e.g. ops
// scripts that need to invalidate a hot key) can construct the key without
// duplicating the literal.
const LanguageCacheKeyPrefix = "user_language:"

// LanguageCacheTTL bounds the staleness of cross-device language switches:
// after a PUT /v1/user/language the resolver writes through the cache, but
// if another node skipped that invalidation the next read converges within
// this window.
const LanguageCacheTTL = 5 * time.Minute

// negativeMarker is the sentinel stored when DB returns an empty preference.
// We need to distinguish "no entry in cache" (cache miss → query DB) from
// "user has no explicit preference" (negative cache hit → skip DB). A bare
// '-' is unambiguous because BCP 47 forbids a leading hyphen on any subtag.
const negativeMarker = "-"

// languageReader is the read surface LanguageService needs from the user DB
// layer; defined here (consumer side) so tests can stub it without spinning
// up dbr. Production code passes *DB which already satisfies the signature.
type languageReader interface {
	QueryLanguageByUID(uid string) (string, error)
}

// LanguageService resolves the authoritative language preference for a user.
// It satisfies both pkg/auth.LanguageResolver (consumed by CacheTokenParser)
// and pkg/i18n.UserLanguageResolver (documentation interface for the i18n
// contract).
//
// Lookup order: Redis `user_language:{uid}` → DB `user.language` → "".
// DB results are written back to Redis with a negative marker for empty
// values so subsequent requests for users without a preference don't
// hammer MySQL. SetLanguage handles cross-device invalidation by deleting
// the hot key on writes; absent a delete the entry expires within
// LanguageCacheTTL.
type LanguageService struct {
	db    languageReader
	cache cache.Cache
	ttl   time.Duration
}

// NewLanguageService constructs a LanguageService with the canonical 5-minute
// hot cache TTL. cache and db must be non-nil; nil is a programmer error and
// panics on construction rather than silently degrading at request time.
func NewLanguageService(db languageReader, c cache.Cache) *LanguageService {
	if db == nil {
		panic("user: NewLanguageService requires non-nil db reader")
	}
	if c == nil {
		panic("user: NewLanguageService requires non-nil cache")
	}
	return &LanguageService{db: db, cache: c, ttl: LanguageCacheTTL}
}

// Resolve returns the user's language preference or "" if none is set.
// Errors are returned to the caller; pkg/auth.CacheTokenParser specifically
// keeps the token snapshot on resolver error so an outage doesn't 5xx
// authentication.
func (s *LanguageService) Resolve(ctx context.Context, uid string) (string, error) {
	if uid == "" {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := LanguageCacheKeyPrefix + uid
	cached, cacheErr := s.cache.Get(key)
	if cacheErr == nil {
		switch cached {
		case "":
			// fall through to DB on cache miss
		case negativeMarker:
			return "", nil
		default:
			if _, ok := octoi18n.MatchSupportedLanguage(cached); ok {
				return cached, nil
			}
			// Cached value drifted from the supported language matrix
			// (e.g. matrix narrowed between releases). Drop the stale
			// entry and re-query DB instead of returning garbage.
			_ = s.cache.Delete(key)
		}
	}

	lang, dbErr := s.db.QueryLanguageByUID(uid)
	if dbErr != nil {
		return "", fmt.Errorf("user: read language from db: %w", dbErr)
	}
	normalized := normalizeLanguageOrEmpty(lang)
	s.writeCache(key, normalized)
	return normalized, nil
}

// SetLanguage validates the incoming preference, persists it to the DB and
// invalidates the hot cache so the next read on any node observes the new
// value within one round-trip. The empty string is accepted and clears the
// preference (back to "use OCTO_DEFAULT_LANGUAGE" semantics).
//
// The DB write is performed via the embedded reader's underlying *DB when
// available; tests that supply a stub reader without a writer get an
// explicit error rather than a silent drop.
func (s *LanguageService) SetLanguage(ctx context.Context, uid, lang string) error {
	if uid == "" {
		return errors.New("user: empty uid")
	}
	normalized := normalizeLanguageOrEmpty(lang)
	if lang != "" && normalized == "" {
		return fmt.Errorf("user: unsupported language %q", lang)
	}
	writer, ok := s.db.(languageWriter)
	if !ok {
		return errors.New("user: language service backend does not support writes")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writer.UpdateLanguageByUID(uid, normalized); err != nil {
		return fmt.Errorf("user: persist language: %w", err)
	}
	// Active invalidation: DEL beats waiting for TTL on cross-device switches.
	// Failure here is logged-only in callers; the worst case is up to TTL of
	// staleness on other nodes, which is acceptable for a UX-only preference.
	_ = s.cache.Delete(LanguageCacheKeyPrefix + uid)
	return nil
}

// languageWriter is the optional write surface; *DB satisfies it via
// UpdateLanguageByUID (added in this commit).
type languageWriter interface {
	UpdateLanguageByUID(uid, language string) error
}

func (s *LanguageService) writeCache(key, lang string) {
	value := lang
	if value == "" {
		value = negativeMarker
	}
	// Best-effort write — a Redis outage degrades to "no negative cache",
	// not a request failure.
	_ = s.cache.SetAndExpire(key, value, s.ttl)
}

// normalizeLanguageOrEmpty returns a supported language tag in canonical
// form, or "" if the input is empty / unsupported. Putting the gate here
// keeps both Resolve (read side) and SetLanguage (write side) consistent.
func normalizeLanguageOrEmpty(raw string) string {
	if raw == "" {
		return ""
	}
	if normalized, ok := octoi18n.MatchSupportedLanguage(raw); ok {
		return normalized
	}
	return ""
}
