package cardactiondispatch

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ResolutionKind string

const (
	ResolutionCallback ResolutionKind = "callback"
	ResolutionBotPull  ResolutionKind = "bot_pull"
	ResolutionReject   ResolutionKind = "reject"
)

const (
	defaultTimeout     = 3 * time.Second
	defaultMaxAttempts = 5
	defaultBaseBackoff = time.Second
	defaultMaxBackoff  = time.Minute
	defaultMaxInFlight = 8
)

var (
	ownerPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	actionTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	senderPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	secretEnvPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

type RouteSpec struct {
	SenderUID      string
	Owner          string
	ActionType     string
	URL            string
	SecretEnv      string
	NotifyTokenEnv string
	Timeout        time.Duration
	MaxAttempts    int
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	MaxInFlight    int
}

type Route struct {
	SenderUID   string
	Owner       string
	ActionType  string
	URL         string
	Timeout     time.Duration
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	MaxInFlight int

	secret string
}

type Resolution struct {
	Kind  ResolutionKind
	Route *Route
}

type routeKey struct {
	senderUID  string
	owner      string
	actionType string
}

// NotifyCapability is the server-authoritative identity granted to one
// first-party notification caller. The bearer token never supplies owner or
// sender metadata; it resolves to this value at the ingress boundary.
type NotifyCapability struct {
	SenderUID string
	Owner     string
}

type notifyCredential struct {
	capability NotifyCapability
	tokenEnv   string
	token      string
}

type Registry struct {
	routes            map[routeKey]*Route
	internalSenders   map[string]struct{}
	notifyActions     map[routeKey]struct{}
	notifyCredentials []notifyCredential
	callbackSecrets   map[string]struct{}
}

func NewRegistry(specs []RouteSpec, getenv func(string) string) (*Registry, error) {
	if getenv == nil {
		return nil, errors.New("cardactiondispatch: getenv is required")
	}
	registry := &Registry{
		routes:          make(map[routeKey]*Route, len(specs)),
		internalSenders: make(map[string]struct{}),
		notifyActions:   make(map[routeKey]struct{}),
		callbackSecrets: make(map[string]struct{}, len(specs)),
	}
	credentialsByCapability := make(map[NotifyCapability]notifyCredential)
	tokenOwners := make(map[string]NotifyCapability)
	for i, input := range specs {
		spec := withRouteDefaults(input)
		if err := validateRouteSpec(spec, getenv); err != nil {
			return nil, fmt.Errorf("cardactiondispatch: route %d: %w", i, err)
		}
		key := routeKey{senderUID: spec.SenderUID, owner: spec.Owner, actionType: spec.ActionType}
		if _, exists := registry.routes[key]; exists {
			return nil, fmt.Errorf("cardactiondispatch: duplicate route for sender=%q owner=%q action_type=%q", spec.SenderUID, spec.Owner, spec.ActionType)
		}
		callbackSecret := getenv(spec.SecretEnv)
		registry.routes[key] = &Route{
			SenderUID:   spec.SenderUID,
			Owner:       spec.Owner,
			ActionType:  spec.ActionType,
			URL:         spec.URL,
			Timeout:     spec.Timeout,
			MaxAttempts: spec.MaxAttempts,
			BaseBackoff: spec.BaseBackoff,
			MaxBackoff:  spec.MaxBackoff,
			MaxInFlight: spec.MaxInFlight,
			secret:      callbackSecret,
		}
		registry.callbackSecrets[callbackSecret] = struct{}{}
		registry.internalSenders[spec.SenderUID] = struct{}{}
		if spec.NotifyTokenEnv != "" {
			capability := NotifyCapability{SenderUID: spec.SenderUID, Owner: spec.Owner}
			credential := notifyCredential{
				capability: capability,
				tokenEnv:   spec.NotifyTokenEnv,
				token:      getenv(spec.NotifyTokenEnv),
			}
			if existing, ok := credentialsByCapability[capability]; ok && existing.tokenEnv != credential.tokenEnv {
				return nil, fmt.Errorf("cardactiondispatch: route %d: inconsistent notify_token_env for sender/owner", i)
			}
			if owner, ok := tokenOwners[credential.token]; ok && owner != capability {
				return nil, fmt.Errorf("cardactiondispatch: route %d: notify token is bound to multiple owners", i)
			}
			credentialsByCapability[capability] = credential
			tokenOwners[credential.token] = capability
			registry.notifyActions[key] = struct{}{}
		}
	}
	for _, credential := range credentialsByCapability {
		if _, conflicts := registry.callbackSecrets[credential.token]; conflicts {
			return nil, errors.New("cardactiondispatch: notify token conflicts with a callback secret")
		}
	}
	registry.notifyCredentials = make([]notifyCredential, 0, len(credentialsByCapability))
	for _, credential := range credentialsByCapability {
		registry.notifyCredentials = append(registry.notifyCredentials, credential)
	}
	sort.Slice(registry.notifyCredentials, func(i, j int) bool {
		left, right := registry.notifyCredentials[i].capability, registry.notifyCredentials[j].capability
		if left.SenderUID == right.SenderUID {
			return left.Owner < right.Owner
		}
		return left.SenderUID < right.SenderUID
	})
	return registry, nil
}

func (r *Registry) Resolve(senderUID, owner, actionType string) Resolution {
	if r == nil {
		return Resolution{Kind: ResolutionBotPull}
	}
	if route, ok := r.routes[routeKey{senderUID: senderUID, owner: owner, actionType: actionType}]; ok {
		return Resolution{Kind: ResolutionCallback, Route: route}
	}
	if _, internal := r.internalSenders[senderUID]; internal {
		return Resolution{Kind: ResolutionReject}
	}
	return Resolution{Kind: ResolutionBotPull}
}

func (r *Registry) Route(senderUID, owner, actionType string) (*Route, bool) {
	if r == nil {
		return nil, false
	}
	route, ok := r.routes[routeKey{senderUID: senderUID, owner: owner, actionType: actionType}]
	return route, ok
}

// ResolveNotifyToken performs a constant-time comparison against every
// configured first-party notification capability. Tokens are unique across
// capabilities, so at most one result can match.
func (r *Registry) ResolveNotifyToken(token string) (NotifyCapability, bool) {
	if r == nil || token == "" {
		return NotifyCapability{}, false
	}
	matched := -1
	for i := range r.notifyCredentials {
		if subtle.ConstantTimeCompare([]byte(token), []byte(r.notifyCredentials[i].token)) == 1 {
			matched = i
		}
	}
	if matched < 0 {
		return NotifyCapability{}, false
	}
	return r.notifyCredentials[matched].capability, true
}

// CanNotify keeps a capability scoped to only the action types that explicitly
// declare its notify_token_env. A token for one owner cannot mint another
// owner's card or access a callback-only route.
func (r *Registry) CanNotify(capability NotifyCapability, actionType string) bool {
	if r == nil {
		return false
	}
	_, ok := r.notifyActions[routeKey{
		senderUID:  capability.SenderUID,
		owner:      capability.Owner,
		actionType: actionType,
	}]
	return ok
}

func (r *Registry) NotifyProducers() []NotifyCapability {
	if r == nil {
		return nil
	}
	producers := make([]NotifyCapability, len(r.notifyCredentials))
	for i := range r.notifyCredentials {
		producers[i] = r.notifyCredentials[i].capability
	}
	return producers
}

// ValidateNotifyTokenExclusions prevents a route-scoped approval token from
// accidentally inheriting a broader legacy/docs notify capability.
func (r *Registry) ValidateNotifyTokenExclusions(tokens ...string) error {
	if r == nil {
		return errors.New("cardactiondispatch: registry unavailable")
	}
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if _, ok := r.ResolveNotifyToken(token); ok {
			return errors.New("cardactiondispatch: action notify token conflicts with an existing notify token")
		}
		if _, ok := r.callbackSecrets[token]; ok {
			return errors.New("cardactiondispatch: callback secret conflicts with an existing notify token")
		}
	}
	return nil
}

func withRouteDefaults(spec RouteSpec) RouteSpec {
	if spec.Timeout == 0 {
		spec.Timeout = defaultTimeout
	}
	if spec.MaxAttempts == 0 {
		spec.MaxAttempts = defaultMaxAttempts
	}
	if spec.BaseBackoff == 0 {
		spec.BaseBackoff = defaultBaseBackoff
	}
	if spec.MaxBackoff == 0 {
		spec.MaxBackoff = defaultMaxBackoff
	}
	if spec.MaxInFlight == 0 {
		spec.MaxInFlight = defaultMaxInFlight
	}
	return spec
}

func validateRouteSpec(spec RouteSpec, getenv func(string) string) error {
	if !senderPattern.MatchString(spec.SenderUID) || strings.HasPrefix(spec.SenderUID, "iwh_") {
		return errors.New("invalid sender_uid")
	}
	if !ownerPattern.MatchString(spec.Owner) {
		return errors.New("invalid owner")
	}
	if !actionTypePattern.MatchString(spec.ActionType) {
		return errors.New("invalid action_type")
	}
	if _, err := validateCallbackURL(spec.URL); err != nil {
		return err
	}
	if !secretEnvPattern.MatchString(spec.SecretEnv) {
		return errors.New("invalid secret_env")
	}
	if len(getenv(spec.SecretEnv)) < 32 {
		return errors.New("callback secret must contain at least 32 bytes")
	}
	if spec.NotifyTokenEnv != "" {
		if !secretEnvPattern.MatchString(spec.NotifyTokenEnv) {
			return errors.New("invalid notify_token_env")
		}
		notifyToken := getenv(spec.NotifyTokenEnv)
		if len(notifyToken) < 32 {
			return errors.New("notify token must contain at least 32 bytes")
		}
		if spec.NotifyTokenEnv == spec.SecretEnv || notifyToken == getenv(spec.SecretEnv) {
			return errors.New("notify token must differ from callback secret")
		}
	}
	if spec.Timeout < 100*time.Millisecond || spec.Timeout > 10*time.Second {
		return errors.New("timeout must be between 100ms and 10s")
	}
	if spec.MaxAttempts < 1 || spec.MaxAttempts > 10 {
		return errors.New("max_attempts must be between 1 and 10")
	}
	if spec.BaseBackoff < 100*time.Millisecond || spec.BaseBackoff > time.Minute {
		return errors.New("base_backoff must be between 100ms and 1m")
	}
	if spec.MaxBackoff < spec.BaseBackoff || spec.MaxBackoff > 10*time.Minute {
		return errors.New("max_backoff must be between base_backoff and 10m")
	}
	if spec.MaxInFlight < 1 || spec.MaxInFlight > 100 {
		return errors.New("max_in_flight must be between 1 and 100")
	}
	return nil
}

func validateCallbackURL(raw string) (string, error) {
	if strings.TrimSpace(raw) != raw || raw == "" {
		return "", errors.New("invalid callback URL")
	}
	// Fragments are stripped by url.Parse *before* setting the Fragment field
	// when they are empty (e.g. "https://host/path#"), so checking u.Fragment
	// alone lets a trailing '#' through. Reject on the raw string first.
	if strings.ContainsRune(raw, '#') {
		return "", errors.New("callback URL must not contain a fragment")
	}
	u, err := url.Parse(raw)
	// Hostname() strips the ":port" so that URLs like "http://:8080/path" are
	// rejected as host-less. Host alone contains the port and would pass.
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.Opaque != "" {
		return "", errors.New("callback URL must be an absolute http(s) URL")
	}
	if u.User != nil {
		return "", errors.New("callback URL must not contain credentials")
	}
	if u.Fragment != "" {
		return "", errors.New("callback URL must not contain a fragment")
	}
	// ForceQuery covers the "trailing ?" form where RawQuery is empty but the
	// separator was present, which url.String would preserve on the wire.
	if u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("callback URL must not contain a query")
	}
	return u.String(), nil
}
