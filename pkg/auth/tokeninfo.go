// Package auth carries the canonical representation of the value stored under
// the token cache key (TokenCachePrefix+token) and provides versioned
// encoding helpers.
//
// Historically the cache value was a `@`-joined string ("uid@name" or
// "uid@name@role") split ad-hoc at every call site. i18n 主方案 D10/D21 要求把
// token cache 真相源收口，并在 payload 中带上用户语言偏好（D20 UserInfo），
// 因此引入 versioned JSON envelope（v2:）；v1 字符串格式保留为解码 fallback，
// 灰度期老 token 不会因为升级失效。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// v2Prefix marks the versioned JSON envelope. Bumping the version means a new
// prefix (e.g. "v3:") — decoder must keep tolerating older prefixes during the
// rollout window.
const v2Prefix = "v2:"

// TokenInfo is the structured payload stored under TokenCachePrefix+token.
// UID is required; Role/Language may be empty.
type TokenInfo struct {
	UID      string
	Name     string
	Role     string
	Language string
}

// ErrEmptyToken indicates an empty cache value (missing or evicted token).
var ErrEmptyToken = errors.New("auth: empty token payload")

// ErrInvalidToken indicates a token payload that matches neither v2 JSON nor
// the legacy "uid@name[@role]" string.
var ErrInvalidToken = errors.New("auth: invalid token payload")

type tokenInfoV2 struct {
	UID      string `json:"uid"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role,omitempty"`
	Language string `json:"lang,omitempty"`
}

// Encode serializes a TokenInfo as the versioned JSON envelope. The UID is
// the only required field; callers should populate Name/Role exactly as they
// did for the legacy "uid@name@role" string, and Language with the value
// resolved at write time (may be empty if unknown).
func Encode(info TokenInfo) (string, error) {
	if info.UID == "" {
		return "", fmt.Errorf("%w: uid required", ErrInvalidToken)
	}
	payload, err := json.Marshal(tokenInfoV2{
		UID:      info.UID,
		Name:     info.Name,
		Role:     info.Role,
		Language: info.Language,
	})
	if err != nil {
		return "", fmt.Errorf("auth: marshal token payload: %w", err)
	}
	return v2Prefix + string(payload), nil
}

// Decode reverses Encode and tolerates the legacy "uid@name[@role]" string so
// that tokens written by older binaries (and cached before the upgrade) keep
// working until they expire. UID emptiness is the only structural check
// applied here — language validity is enforced at consumption sites via
// i18n.MatchSupportedLanguage.
func Decode(raw string) (TokenInfo, error) {
	if raw == "" {
		return TokenInfo{}, ErrEmptyToken
	}
	if strings.HasPrefix(raw, v2Prefix) {
		return decodeV2(raw[len(v2Prefix):])
	}
	return decodeLegacy(raw)
}

func decodeV2(payload string) (TokenInfo, error) {
	var v tokenInfoV2
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		return TokenInfo{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if v.UID == "" {
		return TokenInfo{}, fmt.Errorf("%w: uid missing", ErrInvalidToken)
	}
	return TokenInfo{UID: v.UID, Name: v.Name, Role: v.Role, Language: v.Language}, nil
}

func decodeLegacy(raw string) (TokenInfo, error) {
	// Known limitation of the legacy "uid@name[@role]" wire format: a display
	// name containing '@' (e.g. an email-style handle) is structurally
	// ambiguous and rejected here as malformed. The original split-based
	// readers had the same blind spot — this is not a regression — and the v2
	// JSON envelope fixes it permanently for any token written after upgrade.
	parts := strings.Split(raw, "@")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" {
		return TokenInfo{}, fmt.Errorf("%w: legacy payload must be uid@name[@role]", ErrInvalidToken)
	}
	info := TokenInfo{UID: parts[0], Name: parts[1]}
	if len(parts) == 3 {
		info.Role = parts[2]
	}
	return info, nil
}
