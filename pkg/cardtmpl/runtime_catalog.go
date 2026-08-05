package cardtmpl

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	defaultRuntimeCacheEntries = 64
	defaultRuntimeCacheBytes   = 32 << 20
	hardRuntimeCacheEntries    = 256
	hardRuntimeCacheBytes      = 128 << 20
	defaultRuntimeCompileTime  = 10 * time.Second
)

var (
	ErrRuntimeCatalogUnavailable     = errors.New("cardtmpl: runtime catalog unavailable")
	ErrRuntimeCatalogDisabled        = errors.New("cardtmpl: runtime catalog template disabled")
	ErrRuntimeCatalogBlocked         = errors.New("cardtmpl: runtime catalog artifact blocked")
	ErrRuntimeCatalogNewSendDisabled = errors.New("cardtmpl: runtime catalog dynamic new-send disabled")
	ErrRuntimeCatalogNotAuthorized   = errors.New("cardtmpl: runtime catalog principal not authorized")
	ErrRuntimeCatalogIntegrity       = errors.New("cardtmpl: runtime catalog integrity failure")
)

type RuntimeActivationStatus string

const (
	RuntimeActivationActive   RuntimeActivationStatus = "active"
	RuntimeActivationDisabled RuntimeActivationStatus = "disabled"
)

type RuntimeActivation struct {
	Exists   bool
	Version  string
	Status   RuntimeActivationStatus
	Revision uint64
}

type RuntimeSource string

const (
	RuntimeSourceStatic  RuntimeSource = "static"
	RuntimeSourceDynamic RuntimeSource = "dynamic"
)

type RuntimeArtifactMeta struct {
	ID              ID
	Version         string
	Source          RuntimeSource
	Engine          string
	Hash            string
	Owner           string
	Visibility      string
	Protocol        string
	ContractVersion string
	Blocked         bool
}

type RuntimeArtifactStore interface {
	LoadActivation(context.Context, ID) (RuntimeActivation, error)
	LoadArtifactMeta(context.Context, ID, string) (RuntimeArtifactMeta, error)
	LoadArtifactBundle(context.Context, ID, string, string) (CanonicalBundle, error)
}

// RuntimeDynamicAuthorizeFunc decides one dynamic access. The grant argument
// comes from the same snapshot as the artifact metadata (see
// runtime_authorization.go); an implementation must never go read a grant of
// its own, because a second read is a second point in time.
type RuntimeDynamicAuthorizeFunc func(context.Context, CatalogAccess, RuntimeArtifactMeta, RuntimeGrant) error

type RuntimeCatalogHooks struct {
	OnResolve   func(RuntimeSource, string)
	OnCache     func(string)
	OnCacheSize func(entries, bytes int)
}

type RuntimeCatalogConfig struct {
	MaxCacheEntries  int
	MaxCacheBytes    int
	CompileTimeout   time.Duration
	CheckReady       func() error
	AuthorizeDynamic RuntimeDynamicAuthorizeFunc
	Hooks            RuntimeCatalogHooks
}

type RuntimeCatalog struct {
	static *StaticCatalog
	store  RuntimeArtifactStore
	// authorization is the single-snapshot resolver, present whenever the store
	// implements it. Production always does; see loadAuthorization for what a
	// store without it can and cannot do.
	authorization    RuntimeAuthorizationStore
	cache            *compiledArtifactCache
	compileTimeout   time.Duration
	compile          func(context.Context, Bundle, CompileLimits) (*CompiledArtifact, error)
	checkReady       func() error
	authorizeDynamic RuntimeDynamicAuthorizeFunc
	hooks            RuntimeCatalogHooks
	flights          singleflight.Group
}

func NewRuntimeCatalog(
	registry *Registry,
	store RuntimeArtifactStore,
	config RuntimeCatalogConfig,
) (*RuntimeCatalog, error) {
	if registry == nil || store == nil {
		return nil, errors.New("cardtmpl: runtime catalog dependencies are required")
	}
	registry.mu.RLock()
	frozen := registry.frozen
	registry.mu.RUnlock()
	if !frozen {
		return nil, errors.New("cardtmpl: runtime catalog requires a frozen built-in Registry")
	}
	static, err := NewStaticCatalog(registry)
	if err != nil {
		return nil, err
	}
	entries, bytes, timeout, err := normalizeRuntimeCatalogConfig(config)
	if err != nil {
		return nil, err
	}
	authorization, _ := store.(RuntimeAuthorizationStore)
	return &RuntimeCatalog{
		static: static, store: store, authorization: authorization,
		cache: newCompiledArtifactCache(entries, bytes), compileTimeout: timeout,
		compile:          CompileJSONArtifact,
		checkReady:       config.CheckReady,
		authorizeDynamic: config.AuthorizeDynamic,
		hooks:            config.Hooks,
	}, nil
}

func normalizeRuntimeCatalogConfig(config RuntimeCatalogConfig) (int, int, time.Duration, error) {
	entries := config.MaxCacheEntries
	if entries == 0 {
		entries = defaultRuntimeCacheEntries
	}
	bytes := config.MaxCacheBytes
	if bytes == 0 {
		bytes = defaultRuntimeCacheBytes
	}
	timeout := config.CompileTimeout
	if timeout == 0 {
		timeout = defaultRuntimeCompileTime
	}
	if entries < 1 || entries > hardRuntimeCacheEntries || bytes < 1 || bytes > hardRuntimeCacheBytes ||
		timeout < 1 || timeout > defaultRuntimeCompileTime {
		return 0, 0, 0, errors.New("cardtmpl: runtime catalog cache/compile limits are invalid")
	}
	return entries, bytes, timeout, nil
}

func (c *RuntimeCatalog) Render(ctx context.Context, request CatalogRenderRequest) (map[string]any, error) {
	if c == nil || c.static == nil || c.store == nil {
		return nil, ErrRuntimeCatalogUnavailable
	}
	resolution, err := c.resolve(ctx, request.Access, request.ID, request.Version, followActivation)
	if err != nil {
		return nil, err
	}
	request.Version = resolution.version
	if resolution.serveStatic {
		payload, renderErr := c.static.Render(ctx, request)
		c.observeResolve(RuntimeSourceStatic, renderErr)
		return payload, renderErr
	}
	artifact, err := c.resolveDynamic(ctx, request.Access, resolution)
	if err != nil {
		return nil, err
	}
	return renderCore(ctx, artifact.Template, artifact.Meta, request.State, request.Fields, request.Env)
}

func (c *RuntimeCatalog) MetaExact(ctx context.Context, request CatalogExactRequest) (TemplateMeta, error) {
	if c == nil || c.static == nil || c.store == nil {
		return TemplateMeta{}, ErrRuntimeCatalogUnavailable
	}
	if strings.TrimSpace(request.Version) == "" {
		return TemplateMeta{}, fmt.Errorf("%w: explicit version is required", ErrTemplateUnknown)
	}
	resolution, err := c.resolve(ctx, request.Access, request.ID, request.Version, pinExactVersion)
	if err != nil {
		return TemplateMeta{}, err
	}
	if resolution.serveStatic {
		meta, metaErr := c.static.MetaExact(ctx, CatalogExactRequest{
			Access: request.Access, ID: request.ID, Version: resolution.version,
		})
		c.observeResolve(RuntimeSourceStatic, metaErr)
		return meta, metaErr
	}
	artifact, err := c.resolveDynamic(ctx, request.Access, resolution)
	if err != nil {
		return TemplateMeta{}, err
	}
	return artifact.Meta.Clone(), nil
}

func (c *RuntimeCatalog) MetaDefault(ctx context.Context, request CatalogDefaultRequest) (TemplateMeta, error) {
	if c == nil || c.static == nil || c.store == nil {
		return TemplateMeta{}, ErrRuntimeCatalogUnavailable
	}
	resolution, err := c.resolve(ctx, request.Access, request.ID, "", followActivation)
	if err != nil {
		return TemplateMeta{}, err
	}
	if resolution.serveStatic {
		var meta TemplateMeta
		var metaErr error
		if resolution.version == "" {
			meta, metaErr = c.static.MetaDefault(ctx, request)
		} else {
			meta, metaErr = c.static.MetaExact(ctx, CatalogExactRequest{
				Access: request.Access, ID: request.ID, Version: resolution.version,
			})
		}
		c.observeResolve(RuntimeSourceStatic, metaErr)
		return meta, metaErr
	}
	artifact, err := c.resolveDynamic(ctx, request.Access, resolution)
	if err != nil {
		return TemplateMeta{}, err
	}
	return artifact.Meta.Clone(), nil
}

func (c *RuntimeCatalog) FallbackText(ctx context.Context, request CatalogFallbackRequest) (string, error) {
	if c == nil || c.static == nil || c.store == nil {
		return "", ErrRuntimeCatalogUnavailable
	}
	resolution, err := c.resolve(ctx, request.Access, request.ID, request.Version, followActivation)
	if err != nil {
		return "", err
	}
	if resolution.serveStatic {
		template, lookupErr := c.static.registry.Lookup(request.ID, resolution.version)
		if lookupErr != nil {
			if resolution.version == "" && errors.Is(lookupErr, ErrTemplateUnknown) {
				return "", fmt.Errorf("%w: %s has no default", ErrTemplateUnknown, request.ID)
			}
			return "", lookupErr
		}
		text, fallbackErr := template.FallbackText(request.State, request.Fields, request.Lang)
		c.observeResolve(RuntimeSourceStatic, fallbackErr)
		return text, fallbackErr
	}
	artifact, err := c.resolveDynamic(ctx, request.Access, resolution)
	if err != nil {
		return "", err
	}
	return artifact.Template.FallbackText(request.State, request.Fields, request.Lang)
}

func (c *RuntimeCatalog) ActionContext(ctx context.Context, request CatalogActionRequest) (CatalogActionContext, error) {
	if c == nil || c.static == nil || c.store == nil {
		return CatalogActionContext{}, ErrRuntimeCatalogUnavailable
	}
	// An action callback answers a card that already exists, so it reads the
	// stored exact version and must never follow the activation pointer (D4);
	// flipping the pointer cannot retarget a button on a card already sent.
	resolution, err := c.resolve(ctx, request.Access, request.ID, request.Version, pinExactVersion)
	if err != nil {
		return CatalogActionContext{}, err
	}
	if resolution.serveStatic {
		request.Version = resolution.version
		result, actionErr := c.static.ActionContext(ctx, request)
		c.observeResolve(RuntimeSourceStatic, actionErr)
		return result, actionErr
	}
	artifact, err := c.resolveDynamic(ctx, request.Access, resolution)
	if err != nil {
		return CatalogActionContext{}, err
	}
	view, err := actionViewFromMeta(artifact.Meta, request.ActionID)
	if err != nil {
		return CatalogActionContext{}, err
	}
	return CatalogActionContext{View: view, Meta: artifact.Meta.Clone()}, nil
}

// versionMode says whether a read is allowed to follow the activation pointer.
type versionMode bool

const (
	// followActivation resolves an absent version from the activation pointer.
	// Only new sends may do this.
	followActivation versionMode = true
	// pinExactVersion forbids consulting the pointer, so an absent version can
	// only be answered by the frozen Registry's own default.
	pinExactVersion versionMode = false
)

// runtimeResolution is what at most one DB snapshot decided for one request.
type runtimeResolution struct {
	id      ID
	version string
	// serveStatic routes the request to the frozen Registry. Static reads never
	// consult a business grant: a static exact is code-reviewed policy, not
	// producer-granted state (D6 row 3).
	serveStatic bool
	auth        RuntimeAuthorization
}

// resolve performs at most one primary-DB snapshot and decides static versus
// dynamic from it. The static branches below deliberately return before any
// store call so that a deployment with the runtime gates off — or one whose DB
// is briefly unreachable — keeps serving frozen static cards exactly as it did
// before the runtime catalog existed.
func (c *RuntimeCatalog) resolve(
	ctx context.Context,
	access CatalogAccess,
	id ID,
	version string,
	mode versionMode,
) (runtimeResolution, error) {
	if version != "" || mode == pinExactVersion {
		if _, err := c.static.registry.Lookup(id, version); err == nil {
			// A persistent startup integrity failure (a static/dynamic key
			// collision) still fails closed; a transient DB outage does not.
			if err := c.rejectIntegrity(); err != nil {
				c.observeResolve(RuntimeSourceStatic, err)
				return runtimeResolution{}, err
			}
			return runtimeResolution{id: id, version: version, serveStatic: true}, nil
		} else if !errors.Is(err, ErrTemplateUnknown) {
			return runtimeResolution{}, err
		}
		if version == "" {
			return runtimeResolution{}, fmt.Errorf("%w: %s has no default", ErrTemplateUnknown, id)
		}
	}
	if err := c.requireReady(); err != nil {
		c.observeResolve(RuntimeSourceDynamic, err)
		return runtimeResolution{}, err
	}
	auth, err := c.loadAuthorization(ctx, RuntimeAuthorizationQuery{
		ID: id, Version: version, Principal: access.Principal,
	})
	if err != nil {
		c.observeResolve(RuntimeSourceDynamic, err)
		return runtimeResolution{}, err
	}
	if auth.Version == "" {
		// No activation row at all: the ID belongs entirely to the frozen
		// Registry, if it knows it.
		return runtimeResolution{id: id, serveStatic: true}, nil
	}
	if _, err := c.static.registry.Lookup(id, auth.Version); err == nil {
		return runtimeResolution{id: id, version: auth.Version, serveStatic: true, auth: auth}, nil
	} else if !errors.Is(err, ErrTemplateUnknown) {
		return runtimeResolution{}, err
	}
	return runtimeResolution{id: id, version: auth.Version, auth: auth}, nil
}

// loadAuthorization is the single point where an authorization decision's
// inputs enter the process.
func (c *RuntimeCatalog) loadAuthorization(
	ctx context.Context,
	query RuntimeAuthorizationQuery,
) (RuntimeAuthorization, error) {
	if c.authorization != nil {
		auth, err := c.authorization.LoadAuthorization(ctx, query)
		if err != nil {
			return RuntimeAuthorization{}, classifyRuntimeStoreError("load authorization", err)
		}
		if err := c.verifyAuthorization(query, auth); err != nil {
			return RuntimeAuthorization{}, err
		}
		return auth, nil
	}
	// A store that predates the resolver (test fakes, and any embedding that
	// never had grants) can still answer version resolution and metadata, but
	// it returns no grant — so every dynamic business purpose is denied
	// downstream. A split read therefore cannot manufacture an *allow*, which
	// is the only direction that matters for the single-snapshot invariant.
	auth := RuntimeAuthorization{Version: query.Version}
	if query.Version == "" {
		activation, err := c.store.LoadActivation(ctx, query.ID)
		if err != nil {
			return RuntimeAuthorization{}, classifyRuntimeStoreError("load activation", err)
		}
		version, err := ActivationVersion(query.ID, activation)
		if err != nil {
			return RuntimeAuthorization{}, err
		}
		auth.Activation, auth.Version = activation, version
		if version == "" {
			return auth, nil
		}
	}
	meta, err := c.store.LoadArtifactMeta(ctx, query.ID, auth.Version)
	if err != nil {
		return RuntimeAuthorization{}, classifyRuntimeStoreError("load artifact metadata", err)
	}
	auth.Artifact = meta
	return auth, nil
}

// verifyAuthorization re-derives the parts of the receipt the catalog can check
// itself. A store that answered for a different template, followed the pointer
// when it was told not to, or disagreed with ActivationVersion has produced an
// incoherent snapshot, and an incoherent snapshot is an integrity failure — not
// something to paper over with a second read.
func (c *RuntimeCatalog) verifyAuthorization(query RuntimeAuthorizationQuery, auth RuntimeAuthorization) error {
	if query.Version != "" {
		if auth.Version != query.Version || auth.Activation.Exists {
			return fmt.Errorf("%w: pinned authorization snapshot for %s@%s is incoherent",
				ErrRuntimeCatalogIntegrity, query.ID, query.Version)
		}
		return nil
	}
	expected, err := ActivationVersion(query.ID, auth.Activation)
	if err != nil {
		return err
	}
	if auth.Version != expected {
		return fmt.Errorf("%w: authorization snapshot for %s resolved %q against activation %q",
			ErrRuntimeCatalogIntegrity, query.ID, auth.Version, expected)
	}
	return nil
}

func (c *RuntimeCatalog) resolveDynamic(
	ctx context.Context,
	access CatalogAccess,
	resolution runtimeResolution,
) (artifact *CompiledArtifact, err error) {
	defer func() { c.observeResolve(RuntimeSourceDynamic, err) }()
	if c.authorizeDynamic == nil {
		return nil, ErrRuntimeCatalogNotAuthorized
	}
	meta := resolution.auth.Artifact
	if err := validateRuntimeArtifactMeta(resolution.id, resolution.version, meta); err != nil {
		return nil, err
	}
	if meta.Blocked {
		return nil, fmt.Errorf("%w: %s@%s", ErrRuntimeCatalogBlocked, resolution.id, resolution.version)
	}
	if err := c.authorizeDynamic(ctx, access, meta, resolution.auth.Grant); err != nil {
		if errors.Is(err, ErrRuntimeCatalogNotAuthorized) || errors.Is(err, ErrRuntimeCatalogNewSendDisabled) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: authorize dynamic artifact: %w", ErrRuntimeCatalogUnavailable, err)
	}
	return c.loadCompiled(ctx, meta)
}

func (c *RuntimeCatalog) loadCompiled(ctx context.Context, meta RuntimeArtifactMeta) (*CompiledArtifact, error) {
	key := meta.Engine + ":" + meta.Hash
	if artifact, ok := c.cache.get(key); ok {
		c.observeCache("hit")
		if err := validateCompiledArtifact(meta, artifact); err != nil {
			return nil, err
		}
		return artifact, nil
	}
	c.observeCache("miss")
	resultCh := c.flights.DoChan(key, func() (any, error) {
		if artifact, ok := c.cache.get(key); ok {
			if err := validateCompiledArtifact(meta, artifact); err != nil {
				return nil, err
			}
			return artifact, nil
		}
		compileCtx, cancel := context.WithTimeout(context.Background(), c.compileTimeout)
		defer cancel()
		canonical, err := c.store.LoadArtifactBundle(compileCtx, meta.ID, meta.Version, meta.Hash)
		if err != nil {
			return nil, classifyRuntimeStoreError("load artifact bundle", err)
		}
		bundle, err := DecodeBundleJSON(canonical)
		if err != nil {
			return nil, fmt.Errorf("%w: decode stored bundle: %w", ErrRuntimeCatalogIntegrity, err)
		}
		compiler := c.compile
		if compiler == nil {
			compiler = CompileJSONArtifact
		}
		artifact, err := compiler(compileCtx, bundle, DefaultCompileLimits())
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, ErrArtifactCompileBusy) {
				return nil, fmt.Errorf("%w: compile stored bundle: %w", ErrRuntimeCatalogUnavailable, err)
			}
			return nil, fmt.Errorf("%w: compile stored bundle: %w", ErrRuntimeCatalogIntegrity, err)
		}
		if err := validateCompiledArtifact(meta, artifact); err != nil {
			return nil, err
		}
		evicted := c.cache.add(key, artifact)
		for i := 0; i < evicted; i++ {
			c.observeCache("evict")
		}
		if c.hooks.OnCacheSize != nil {
			entries, bytes := c.cache.size()
			c.hooks.OnCacheSize(entries, bytes)
		}
		return artifact, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-resultCh:
		if result.Shared {
			c.observeCache("shared")
		}
		if result.Err != nil {
			return nil, result.Err
		}
		artifact, ok := result.Val.(*CompiledArtifact)
		if !ok || artifact == nil {
			return nil, fmt.Errorf("%w: invalid compiled cache value", ErrRuntimeCatalogIntegrity)
		}
		if err := validateCompiledArtifact(meta, artifact); err != nil {
			return nil, err
		}
		return artifact, nil
	}
}

func (c *RuntimeCatalog) requireReady() error {
	if c == nil || c.checkReady == nil {
		return nil
	}
	err := c.checkReady()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrRuntimeCatalogIntegrity), errors.Is(err, ErrRuntimeCatalogUnavailable),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return fmt.Errorf("%w: readiness check: %v", ErrRuntimeCatalogUnavailable, err)
	}
}

// CheckReady reports whether startup reconciliation has established a safe
// authoritative view for default and dynamic catalog paths. It performs no IO
// and is intended for the process readiness probe composed by main.
func (c *RuntimeCatalog) CheckReady() error {
	return c.requireReady()
}

// rejectIntegrity keeps explicit static reads available during a transient DB
// outage while still failing closed after startup proves a static/dynamic
// source collision or another persistent catalog invariant violation.
func (c *RuntimeCatalog) rejectIntegrity() error {
	if c == nil || c.checkReady == nil {
		return nil
	}
	err := c.checkReady()
	if errors.Is(err, ErrRuntimeCatalogIntegrity) {
		return err
	}
	return nil
}

func (c *RuntimeCatalog) observeResolve(source RuntimeSource, err error) {
	if c != nil && c.hooks.OnResolve != nil {
		c.hooks.OnResolve(source, runtimeCatalogResult(err))
	}
}

func (c *RuntimeCatalog) observeCache(result string) {
	if c != nil && c.hooks.OnCache != nil {
		c.hooks.OnCache(result)
	}
}

func runtimeCatalogResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrTemplateUnknown):
		return "unknown"
	case errors.Is(err, ErrRuntimeCatalogDisabled), errors.Is(err, ErrRuntimeCatalogNewSendDisabled):
		return "disabled"
	case errors.Is(err, ErrRuntimeCatalogBlocked):
		return "blocked"
	case errors.Is(err, ErrRuntimeCatalogNotAuthorized):
		return "unauthorized"
	case errors.Is(err, ErrRuntimeCatalogIntegrity):
		return "integrity"
	case errors.Is(err, ErrRuntimeCatalogUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "unavailable"
	default:
		return "error"
	}
}

func validateRuntimeArtifactMeta(id ID, version string, meta RuntimeArtifactMeta) error {
	decodedHash, hashErr := hex.DecodeString(meta.Hash)
	validHash := hashErr == nil && len(decodedHash) == 32 && strings.ToLower(meta.Hash) == meta.Hash
	validVisibility := meta.Visibility == CatalogVisibilityPublic || meta.Visibility == CatalogVisibilityPrivate
	if meta.ID != id || meta.Version != version || meta.Source != RuntimeSourceDynamic ||
		meta.Engine != JSONTemplateEngineV1 || !validHash || strings.TrimSpace(meta.Owner) == "" ||
		!validVisibility || meta.Protocol != Protocol {
		return fmt.Errorf("%w: invalid metadata for %s@%s", ErrRuntimeCatalogIntegrity, id, version)
	}
	return nil
}

func validateCompiledArtifact(meta RuntimeArtifactMeta, artifact *CompiledArtifact) error {
	if artifact == nil || artifact.Meta.ID != meta.ID || artifact.Meta.Version != meta.Version ||
		artifact.Engine != meta.Engine || artifact.Hash != meta.Hash || artifact.Owner != meta.Owner ||
		artifact.Visibility != meta.Visibility || artifact.Meta.Protocol != meta.Protocol ||
		artifact.ContractVersion != meta.ContractVersion {
		return fmt.Errorf("%w: compiled metadata mismatch for %s@%s",
			ErrRuntimeCatalogIntegrity, meta.ID, meta.Version)
	}
	return nil
}

// classifyRuntimeStoreError turns a store failure into a typed catalog error.
// Anything it does not recognise becomes "unavailable", which is the right
// default for a raw driver error but the wrong answer for a verdict the store
// already reached.
//
// The distinction is load-bearing downstream: `disabled` means an operator
// deliberately turned this template off and callers withhold it quietly, while
// `unavailable` means we could not tell and callers surface an outage. Since
// PR-C moved activation resolution inside the snapshot, a disabled pointer is
// produced by the store rather than by the catalog — so every typed error the
// catalog owns has to pass through intact instead of only the two that happened
// to be listed when the store could not produce the others.
func classifyRuntimeStoreError(operation string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, ErrTemplateUnknown), errors.Is(err, ErrRuntimeCatalogIntegrity),
		errors.Is(err, ErrRuntimeCatalogDisabled), errors.Is(err, ErrRuntimeCatalogBlocked),
		errors.Is(err, ErrRuntimeCatalogNotAuthorized), errors.Is(err, ErrRuntimeCatalogNewSendDisabled),
		errors.Is(err, ErrRuntimeCatalogUnavailable):
		return fmt.Errorf("cardtmpl: %s: %w", operation, err)
	default:
		return fmt.Errorf("%w: %s: %v", ErrRuntimeCatalogUnavailable, operation, err)
	}
}
