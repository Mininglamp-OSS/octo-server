package resourceshare

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	jose "github.com/go-jose/go-jose/v3"
	"github.com/gowebpki/jcs"
)

const (
	maxActorUIDBytes       = 128
	maxSpaceIDBytes        = 128
	maxResourceIDBytes     = 256
	maxRevisionBytes       = 128
	maxTargetIDBytes       = 128
	minNonceBytes          = 16
	maxNonceBytes          = 128
	minIdempotencyKeyBytes = 8
	maxIdempotencyKeyBytes = 128
)

func (r *Registry) VerifyIntent(ctx context.Context, compact string, now time.Time) (*VerifiedIntent, error) {
	return r.verifyIntent(ctx, compact, now, false)
}

// VerifyIntentForRetry performs the same signature, schema and provider checks
// as VerifyIntent but returns an authenticated Expired marker after the normal
// clock-skew window. Callers may use it only to read durable terminal results;
// it does not authorize new or resumed delivery work.
func (r *Registry) VerifyIntentForRetry(ctx context.Context, compact string, now time.Time) (*VerifiedIntent, error) {
	return r.verifyIntent(ctx, compact, now, true)
}

func (r *Registry) verifyIntent(ctx context.Context, compact string, now time.Time, allowExpired bool) (*VerifiedIntent, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrIntentInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if now.IsZero() || len(compact) == 0 || len(compact) > PlatformMaxCompactIntentBytes ||
		compact != strings.TrimSpace(compact) || strings.Count(compact, ".") != 2 {
		return nil, fmt.Errorf("%w: malformed compact JWS", ErrIntentInvalid)
	}

	signed, err := jose.ParseSigned(compact)
	if err != nil || len(signed.Signatures) != 1 {
		return nil, fmt.Errorf("%w: malformed JWS", ErrIntentSignature)
	}
	raw := signed.UnsafePayloadWithoutVerification()
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical payload: %v", ErrIntentInvalid, err)
	}

	var selector struct {
		Provider ProviderID `json:"provider"`
	}
	if err := json.Unmarshal(canonical, &selector); err != nil || selector.Provider == "" {
		return nil, fmt.Errorf("%w: provider selector", ErrIntentInvalid)
	}
	provider, err := r.Provider(selector.Provider)
	if err != nil {
		return nil, err
	}

	signature := signed.Signatures[0]
	if !emptyJOSEHeader(signature.Unprotected) || signature.Protected.JSONWebKey != nil ||
		signature.Protected.Nonce != "" || len(signature.Protected.ExtraHeaders) != 0 {
		return nil, fmt.Errorf("%w: unsupported JOSE header", ErrIntentSignature)
	}
	verificationKey, ok := provider.keys[signature.Protected.KeyID]
	if !ok || signature.Protected.Algorithm != string(verificationKey.Algorithm) {
		return nil, fmt.Errorf("%w: untrusted key or algorithm", ErrIntentSignature)
	}
	verifiedRaw, err := signed.Verify(verificationKey.PublicKey)
	if err != nil || !bytes.Equal(raw, verifiedRaw) {
		return nil, fmt.Errorf("%w: verification failed", ErrIntentSignature)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	intent, err := decodeIntentStrict(verifiedRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrIntentInvalid, err)
	}
	expired, err := provider.validateIntent(intent, now, allowExpired)
	if err != nil {
		return nil, err
	}

	fingerprint := sha256.Sum256(canonical)
	return &VerifiedIntent{ProviderID: provider.spec.ID, Intent: intent, Fingerprint: fingerprint, Expired: expired}, nil
}

func decodeIntentStrict(raw []byte) (Intent, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var intent Intent
	if err := decoder.Decode(&intent); err != nil {
		return Intent{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return Intent{}, errors.New("multiple JSON values")
		}
		return Intent{}, err
	}
	return intent, nil
}

func (p *Provider) validateIntent(intent Intent, now time.Time, allowExpired bool) (bool, error) {
	invalid := func(reason string) (bool, error) { return false, fmt.Errorf("%w: %s", ErrIntentInvalid, reason) }
	if intent.Provider != p.spec.ID || intent.Version != p.spec.IntentVersion {
		return invalid("provider or version mismatch")
	}
	if intent.Issuer != p.spec.Issuer || intent.Audience != p.spec.Audience {
		return invalid("issuer or audience mismatch")
	}
	if !validIdentifier(intent.ActorUID, 1, maxActorUIDBytes) || !validIdentifier(intent.SpaceID, 1, maxSpaceIDBytes) {
		return invalid("actor or space missing")
	}
	if !validOpaque(intent.Nonce, minNonceBytes, maxNonceBytes) ||
		!validOpaque(intent.IdempotencyKey, minIdempotencyKeyBytes, maxIdempotencyKeyBytes) {
		return invalid("nonce or idempotency key invalid")
	}
	if intent.IssuedAt <= 0 || intent.ExpiresAt <= intent.IssuedAt {
		return invalid("invalid intent timestamps")
	}
	if time.Unix(intent.IssuedAt, 0).After(now.Add(p.spec.Limits.ClockSkew)) {
		return invalid("intent issued in future")
	}
	expired := time.Unix(intent.ExpiresAt, 0).Before(now.Add(-p.spec.Limits.ClockSkew))
	if expired && !allowExpired {
		return invalid("intent expired")
	}
	if intent.ExpiresAt-intent.IssuedAt > int64(p.spec.Limits.MaxIntentLifetime/time.Second) {
		return invalid("intent lifetime too long")
	}
	if intent.Resource.Type != p.spec.ResourceType ||
		!validIdentifier(intent.Resource.ID, 1, maxResourceIDBytes) ||
		!validIdentifier(intent.Resource.Revision, 1, maxRevisionBytes) {
		return invalid("resource mismatch")
	}
	if _, ok := p.templates[intent.Template]; !ok {
		return invalid("template unsupported")
	}
	if len(intent.Claims) == 0 || len(intent.Claims) > p.spec.Limits.MaxClaimsBytes {
		return invalid("claims size invalid")
	}
	canonicalClaims, err := jcs.Transform(intent.Claims)
	if err != nil || len(canonicalClaims) == 0 || canonicalClaims[0] != '{' {
		return invalid("claims must be a canonicalizable object")
	}
	if err := p.spec.ValidateClaims(intent.Claims); err != nil {
		return invalid("claims rejected")
	}
	if err := validateTargets(intent.ActorUID, intent.Targets, p.spec.Limits.MaxTargets); err != nil {
		return invalid("targets invalid")
	}
	return expired, nil
}

func validateTargets(actorUID string, targets []Target, maxTargets int) error {
	if len(targets) == 0 || len(targets) > maxTargets {
		return errors.New("target count invalid")
	}
	previous := ""
	for _, target := range targets {
		key, err := canonicalTargetKey(actorUID, target)
		if err != nil {
			return err
		}
		if previous != "" && key <= previous {
			return errors.New("targets must be unique and canonical")
		}
		previous = key
	}
	return nil
}

func canonicalTargetKey(actorUID string, target Target) (string, error) {
	switch target.Kind {
	case TargetDM:
		if target.GroupNo != "" || target.ShortID != "" ||
			!validIdentifier(target.PeerUID, 1, maxTargetIDBytes) || target.PeerUID == actorUID {
			return "", errors.New("invalid dm target")
		}
		return string(TargetDM) + "\x00" + target.PeerUID, nil
	case TargetGroup:
		if target.PeerUID != "" || target.ShortID != "" || !validIdentifier(target.GroupNo, 1, maxTargetIDBytes) {
			return "", errors.New("invalid group target")
		}
		return string(TargetGroup) + "\x00" + target.GroupNo, nil
	case TargetThread:
		if target.PeerUID != "" || !validIdentifier(target.GroupNo, 1, maxTargetIDBytes) ||
			!validIdentifier(target.ShortID, 1, maxTargetIDBytes) {
			return "", errors.New("invalid thread target")
		}
		return string(TargetThread) + "\x00" + target.GroupNo + "\x00" + target.ShortID, nil
	default:
		return "", errors.New("unsupported target kind")
	}
}

func validIdentifier(value string, minBytes, maxBytes int) bool {
	if !validBoundedString(value, minBytes, maxBytes) || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validOpaque(value string, minBytes, maxBytes int) bool {
	if !validIdentifier(value, minBytes, maxBytes) {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') &&
			r != '-' && r != '_' && r != '.' && r != '~' {
			return false
		}
	}
	return true
}

func emptyJOSEHeader(header jose.Header) bool {
	return header.KeyID == "" && header.JSONWebKey == nil && header.Algorithm == "" &&
		header.Nonce == "" && len(header.ExtraHeaders) == 0
}

func ClassifyReplay(stored *IntentFingerprint, candidate IntentFingerprint) (ReplayDisposition, error) {
	if candidate == (IntentFingerprint{}) {
		return 0, fmt.Errorf("%w: empty fingerprint", ErrIntentInvalid)
	}
	if stored == nil {
		return ReplayFirstUse, nil
	}
	if subtle.ConstantTimeCompare(stored[:], candidate[:]) == 1 {
		return ReplayRetry, nil
	}
	return 0, ErrIntentReplay
}
