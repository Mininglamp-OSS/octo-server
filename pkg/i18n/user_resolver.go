package i18n

import "context"

// UserLanguageResolver returns the authoritative language preference for an
// authenticated user. Implementations are expected to consult a hot cache
// (e.g. Redis `user_language:{uid}`) first and fall back to the persistent
// store (`user.language` column) on cache miss, then write the result back
// to the cache.
//
// Contract:
//   - An empty return string means "no explicit preference"; callers must
//     then fall back to the language already negotiated by EarlyMiddleware
//     (Accept-Language / default).
//   - A non-nil error means the lookup itself failed (cache + DB both
//     unreachable). Callers should keep going with the snapshot value
//     already carried by the token rather than 5xx-ing the request.
//   - Implementations are responsible for input/output validation; if a
//     stored value isn't a supported language tag, returning "" is the
//     safe default.
//
// The resolver is hooked into pkg/auth.CacheTokenParser so the resolved
// language ends up on wkhttp.UserInfo.Language, which LanguageFromContext
// then merges into the i18n language decision (LanguageSourceUser priority).
type UserLanguageResolver interface {
	Resolve(ctx context.Context, uid string) (string, error)
}
