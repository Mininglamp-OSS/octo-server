package bot_api

import (
	"sync"
	"sync/atomic"
)

// AppBotRegistrySpec is the minimal spec needed by bot_api auth.
type AppBotRegistrySpec struct {
	UID     string
	Scope   string
	SpaceID string
}

// AppBotRegistryInterface is the App Bot auth registry: a token -> spec lookup
// plus the mutators the app_bot admin handlers call on publish/rotate/unpublish/
// delete. Two implementations satisfy it: AppBotRegistryAdapter (in-memory,
// single-process — used by unit tests) and RedisAppBotRegistry (shared write-
// through cache — used in production so revocations propagate across replicas;
// see issue #309).
//
// FindByToken returns nil on a miss AND on any backend error, so the caller
// (authAppBot) falls through to the authoritative DB lookup — auth must fail
// safe, never serve a stale spec when the backend is degraded.
type AppBotRegistryInterface interface {
	FindByToken(token string) *AppBotRegistrySpec
	// Add is an AUTHORITATIVE write (publish / rotate-new): it must establish the
	// spec, overwriting any prior value (e.g. a revocation tombstone on re-publish).
	Add(token string, spec *AppBotRegistrySpec)
	// Warm is a BEST-EFFORT, NON-BLOCKING/BOUNDED warm-up (auth-path repopulate +
	// startup load). It must never overwrite a concurrent revocation, and callers
	// may invoke it from a detached goroutine on the hot path — implementations
	// must keep it bounded (never an unbounded blocking write per call).
	Warm(token string, spec *AppBotRegistrySpec)
	Remove(token string)
	Update(oldToken, newToken string, spec *AppBotRegistrySpec)
}

// regHolder wraps the registry interface so the global can be stored through an
// atomic.Pointer of a single concrete type. Using atomic.Value directly would
// type-lock the slot to the first concrete implementation stored and panic if a
// later Store used a different one (or nil) — which a test that swaps the
// registry and then restores the previous (possibly nil) value needs to do.
type regHolder struct{ r AppBotRegistryInterface }

// appBotRegistry stores the global App Bot registry, set by the app_bot module
// on init. Lock-free reads on the bot-auth hot path; Store(nil-holder safe).
var appBotRegistry atomic.Pointer[regHolder]

// SetAppBotRegistry sets the global App Bot registry (called by app_bot module).
// A nil r is allowed (clears the slot back to "no registry").
func SetAppBotRegistry(r AppBotRegistryInterface) {
	appBotRegistry.Store(&regHolder{r: r})
}

// GetAppBotRegistry returns the global App Bot registry, or nil if unset/cleared.
func GetAppBotRegistry() AppBotRegistryInterface {
	h := appBotRegistry.Load()
	if h == nil {
		return nil
	}
	return h.r
}

// AppBotRegistryAdapter adapts an external registry to AppBotRegistryInterface.
// The app_bot module sets this on startup.
type AppBotRegistryAdapter struct {
	mu      sync.RWMutex
	byToken map[string]*AppBotRegistrySpec
}

// NewAppBotRegistryAdapter creates a new adapter.
func NewAppBotRegistryAdapter() *AppBotRegistryAdapter {
	return &AppBotRegistryAdapter{
		byToken: make(map[string]*AppBotRegistrySpec),
	}
}

// FindByToken looks up spec by token.
func (a *AppBotRegistryAdapter) FindByToken(token string) *AppBotRegistrySpec {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.byToken[token]
}

// Add adds a spec by token.
func (a *AppBotRegistryAdapter) Add(token string, spec *AppBotRegistrySpec) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.byToken[token] = spec
}

// Warm mirrors Add for the in-memory adapter: it is single-process, so the
// tombstone / SETNX semantics the shared Redis registry needs to close the
// cross-replica resurrection race don't apply here.
func (a *AppBotRegistryAdapter) Warm(token string, spec *AppBotRegistrySpec) {
	a.Add(token, spec)
}

// Remove removes a spec by token.
func (a *AppBotRegistryAdapter) Remove(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.byToken, token)
}

// Update atomically replaces a spec by old and new token.
func (a *AppBotRegistryAdapter) Update(oldToken, newToken string, spec *AppBotRegistrySpec) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.byToken, oldToken)
	a.byToken[newToken] = spec
}
