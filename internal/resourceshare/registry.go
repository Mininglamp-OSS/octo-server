package resourceshare

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"reflect"
	"regexp"
	"strings"

	jose "github.com/go-jose/go-jose/v3"
)

const maxVerificationKeys = 8

var (
	providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	keyIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

type Provider struct {
	spec      ProviderSpec
	keys      map[string]VerificationKey
	templates map[TemplateRef]struct{}
}

func (p *Provider) ID() ProviderID {
	if p == nil {
		return ""
	}
	return p.spec.ID
}

type Registry struct {
	providers map[ProviderID]*Provider
}

func (r *Registry) EnabledProviderCount() int {
	if r == nil {
		return 0
	}
	count := 0
	for _, provider := range r.providers {
		if provider.spec.Enabled {
			count++
		}
	}
	return count
}

func NewRegistry(specs []ProviderSpec) (*Registry, error) {
	registry := &Registry{providers: make(map[ProviderID]*Provider, len(specs))}
	for _, input := range specs {
		if _, duplicate := registry.providers[input.ID]; duplicate {
			return nil, providerConfigError(input.ID, "duplicate provider")
		}
		if !providerIDPattern.MatchString(string(input.ID)) {
			return nil, providerConfigError(input.ID, "invalid provider id")
		}

		if !input.Enabled {
			registry.providers[input.ID] = &Provider{spec: ProviderSpec{ID: input.ID}}
			continue
		}

		spec, keys, templates, err := validateAndCopyProvider(input)
		if err != nil {
			return nil, err
		}
		registry.providers[spec.ID] = &Provider{spec: spec, keys: keys, templates: templates}
	}
	return registry, nil
}

func (r *Registry) Provider(id ProviderID) (*Provider, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: registry unavailable", ErrProviderNotFound)
	}
	provider, ok := r.providers[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, id)
	}
	if !provider.spec.Enabled {
		return nil, fmt.Errorf("%w: %s", ErrProviderDisabled, id)
	}
	return provider, nil
}

func validateAndCopyProvider(input ProviderSpec) (ProviderSpec, map[string]VerificationKey, map[TemplateRef]struct{}, error) {
	if !providerIDPattern.MatchString(input.ResourceType) {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "invalid resource type")
	}
	if !validHTTPSIssuer(input.Issuer) {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "invalid issuer")
	}
	if !validBoundedString(input.Audience, 1, 256) {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "invalid audience")
	}
	if input.IntentVersion != PlatformIntentVersion {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "unsupported intent version")
	}
	if len(input.VerificationKeys) == 0 || len(input.VerificationKeys) > maxVerificationKeys {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "invalid verification key count")
	}

	keys := make(map[string]VerificationKey, len(input.VerificationKeys))
	keySlice := make([]VerificationKey, 0, len(input.VerificationKeys))
	for _, inputKey := range input.VerificationKeys {
		if !keyIDPattern.MatchString(inputKey.KeyID) {
			return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "invalid key id")
		}
		if _, duplicate := keys[inputKey.KeyID]; duplicate {
			return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "duplicate key id")
		}
		if inputKey.Algorithm != jose.ES256 {
			return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "unsupported verification algorithm")
		}
		publicKey, ok := copyP256PublicKey(inputKey.PublicKey)
		if !ok {
			return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "invalid verification key")
		}
		key := VerificationKey{KeyID: inputKey.KeyID, Algorithm: inputKey.Algorithm, PublicKey: publicKey}
		keys[key.KeyID] = key
		keySlice = append(keySlice, key)
	}

	if len(input.Templates) == 0 {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "missing templates")
	}
	templates := make(map[TemplateRef]struct{}, len(input.Templates))
	templateSlice := make([]TemplateRef, 0, len(input.Templates))
	for _, template := range input.Templates {
		if !providerIDPattern.MatchString(template.ID) || template.Version <= 0 {
			return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "invalid template")
		}
		if _, duplicate := templates[template]; duplicate {
			return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "duplicate template")
		}
		templates[template] = struct{}{}
		templateSlice = append(templateSlice, template)
	}

	if !validProviderLimits(input.Limits) {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "invalid provider limits")
	}
	if input.ValidateClaims == nil {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "missing claims validator")
	}
	if input.BuildDeepLink == nil {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "missing deep link builder")
	}
	if input.RenderCard == nil {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "missing card renderer")
	}
	if isNilInterface(input.Adapter) {
		return ProviderSpec{}, nil, nil, providerConfigError(input.ID, "missing provider adapter")
	}

	input.VerificationKeys = keySlice
	input.Templates = templateSlice
	return input, keys, templates, nil
}

func providerConfigError(id ProviderID, reason string) error {
	return fmt.Errorf("%w: provider=%q reason=%s", ErrProviderConfig, id, reason)
}

func validHTTPSIssuer(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func validProviderLimits(limits ProviderLimits) bool {
	return limits.MaxClaimsBytes > 0 && limits.MaxClaimsBytes <= PlatformMaxClaimsBytes &&
		limits.MaxTargets > 0 && limits.MaxTargets <= PlatformMaxTargets &&
		limits.MaxIntentLifetime > 0 && limits.MaxIntentLifetime <= PlatformMaxIntentLifetime &&
		limits.ClockSkew >= 0 && limits.ClockSkew <= PlatformMaxClockSkew &&
		validRateBudget(limits.TargetBudget)
}

func validRateBudget(budget RateBudget) bool {
	return budget.RatePerSecond > 0 && !math.IsNaN(budget.RatePerSecond) &&
		!math.IsInf(budget.RatePerSecond, 0) && budget.Burst > 0
}

func copyP256PublicKey(input interface{}) (*ecdsa.PublicKey, bool) {
	key, ok := input.(*ecdsa.PublicKey)
	if !ok || key == nil || key.Curve != elliptic.P256() || key.X == nil || key.Y == nil || !key.Curve.IsOnCurve(key.X, key.Y) {
		return nil, false
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).Set(key.X), Y: new(big.Int).Set(key.Y)}, true
}

func isNilInterface(value interface{}) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func validBoundedString(value string, minBytes, maxBytes int) bool {
	return len(value) >= minBytes && len(value) <= maxBytes && strings.TrimSpace(value) == value
}
