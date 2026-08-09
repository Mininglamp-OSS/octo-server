package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
)

// TokenRecord is an atomic snapshot of a token payload and its Redis PTTL.
// TTL follows Redis semantics: -2 means missing and -1 means persistent.
type TokenRecord struct {
	Payload string
	TTL     time.Duration
}

type TokenRecordReader interface {
	ReadToken(ctx context.Context, key string) (TokenRecord, error)
}

type SessionGenerationResolver interface {
	CurrentGeneration(ctx context.Context, uid string) (string, error)
}

type TokenValidator struct {
	reader             TokenRecordReader
	prefix             string
	now                func() time.Time
	generationResolver SessionGenerationResolver
}

func WithSessionGenerationResolver(resolver SessionGenerationResolver) ValidatorOption {
	return func(v *TokenValidator) {
		if resolver != nil {
			v.generationResolver = resolver
		}
	}
}

type ValidatorOption func(*TokenValidator)

func WithValidatorClock(now func() time.Time) ValidatorOption {
	return func(v *TokenValidator) {
		if now != nil {
			v.now = now
		}
	}
}

func NewTokenValidator(reader TokenRecordReader, prefix string, opts ...ValidatorOption) *TokenValidator {
	if reader == nil {
		panic("auth: NewTokenValidator requires non-nil reader")
	}
	v := &TokenValidator{reader: reader, prefix: prefix, now: time.Now}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Validate is the canonical token-read policy. Legacy v1/v2 persistent keys
// remain readable in Release A so migration can be observed before apply. Any
// v3 key is strict immediately: finite Redis TTL plus absolute payload expiry.
func (v *TokenValidator) Validate(ctx context.Context, token string) (TokenInfo, error) {
	if strings.TrimSpace(token) == "" {
		return TokenInfo{}, wkhttp.ErrTokenMissing
	}
	record, err := v.reader.ReadToken(ctx, v.prefix+token)
	if err != nil {
		metrics.ObserveSessionValidationReject("store_error")
		return TokenInfo{}, fmt.Errorf("auth: load token record: %w", err)
	}
	if record.TTL == -2 || strings.TrimSpace(record.Payload) == "" {
		metrics.ObserveSessionValidationReject("missing")
		return TokenInfo{}, wkhttp.ErrTokenNotFound
	}
	info, err := Decode(record.Payload)
	if err != nil {
		if errors.Is(err, ErrEmptyToken) {
			metrics.ObserveSessionValidationReject("missing")
			return TokenInfo{}, wkhttp.ErrTokenNotFound
		}
		metrics.ObserveSessionValidationReject("decode_invalid")
		return TokenInfo{}, fmt.Errorf("%w: %v", wkhttp.ErrTokenInvalid, err)
	}
	if info.IsV3() {
		if record.TTL <= 0 {
			metrics.ObserveSessionValidationReject("v3_non_finite_ttl")
			return TokenInfo{}, fmt.Errorf("%w: v3 token has non-finite redis ttl", wkhttp.ErrTokenInvalid)
		}
		if v.now().Unix() >= info.ExpiresAt {
			metrics.ObserveSessionValidationReject("v3_expired")
			return TokenInfo{}, fmt.Errorf("%w: v3 token absolute deadline reached", wkhttp.ErrTokenInvalid)
		}
		if v.generationResolver == nil {
			metrics.ObserveSessionValidationReject("v3_generation_unavailable")
			return TokenInfo{}, fmt.Errorf("%w: v3 generation validator unavailable", wkhttp.ErrTokenInvalid)
		}
		generation, err := v.generationResolver.CurrentGeneration(ctx, info.UID)
		if err != nil {
			metrics.ObserveSessionValidationReject("generation_store_error")
			return TokenInfo{}, fmt.Errorf("auth: load session generation: %w", err)
		}
		if generation == "" || generation != info.SessionGeneration {
			metrics.ObserveSessionValidationReject("generation_mismatch")
			return TokenInfo{}, fmt.Errorf("%w: v3 session generation mismatch", wkhttp.ErrTokenInvalid)
		}
		return info, nil
	}
	if record.TTL == 0 || record.TTL < -2 {
		metrics.ObserveSessionValidationReject("invalid_ttl")
		return TokenInfo{}, fmt.Errorf("%w: invalid redis ttl", wkhttp.ErrTokenInvalid)
	}
	if record.TTL == -1 {
		metrics.ObservePersistentSession()
	}
	return info, nil
}
