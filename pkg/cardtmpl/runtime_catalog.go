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

type RuntimeDynamicAuthorizeFunc func(context.Context, CatalogAccess, RuntimeArtifactMeta) error

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
	static           *StaticCatalog
	store            RuntimeArtifactStore
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
	return &RuntimeCatalog{
		static: static, store: store,
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
	explicit := request.Version != ""
	version, err := c.resolveVersion(ctx, request.ID, request.Version)
	if err != nil {
		return nil, err
	}
	if version == "" {
		payload, err := c.static.Render(ctx, request)
		c.observeResolve(RuntimeSourceStatic, err)
		return payload, err
	}
	if _, err := c.static.registry.Lookup(request.ID, version); err == nil {
		if explicit {
			if err := c.rejectIntegrity(); err != nil {
				c.observeResolve(RuntimeSourceStatic, err)
				return nil, err
			}
		}
		request.Version = version
		payload, renderErr := c.static.Render(ctx, request)
		c.observeResolve(RuntimeSourceStatic, renderErr)
		return payload, renderErr
	} else if !errors.Is(err, ErrTemplateUnknown) {
		return nil, err
	}
	request.Version = version
	artifact, err := c.resolveDynamic(ctx, request.Access, request.ID, version)
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
	if meta, err := c.static.MetaExact(ctx, request); err == nil {
		if err := c.rejectIntegrity(); err != nil {
			c.observeResolve(RuntimeSourceStatic, err)
			return TemplateMeta{}, err
		}
		c.observeResolve(RuntimeSourceStatic, nil)
		return meta, nil
	} else if !errors.Is(err, ErrTemplateUnknown) {
		return TemplateMeta{}, err
	}
	artifact, err := c.resolveDynamic(ctx, request.Access, request.ID, request.Version)
	if err != nil {
		return TemplateMeta{}, err
	}
	return artifact.Meta.Clone(), nil
}

func (c *RuntimeCatalog) MetaDefault(ctx context.Context, request CatalogDefaultRequest) (TemplateMeta, error) {
	if c == nil || c.static == nil || c.store == nil {
		return TemplateMeta{}, ErrRuntimeCatalogUnavailable
	}
	version, err := c.resolveVersion(ctx, request.ID, "")
	if err != nil {
		return TemplateMeta{}, err
	}
	if version == "" {
		meta, metaErr := c.static.MetaDefault(ctx, request)
		c.observeResolve(RuntimeSourceStatic, metaErr)
		return meta, metaErr
	}
	return c.MetaExact(ctx, CatalogExactRequest{Access: request.Access, ID: request.ID, Version: version})
}

func (c *RuntimeCatalog) FallbackText(ctx context.Context, request CatalogFallbackRequest) (string, error) {
	if c == nil || c.static == nil || c.store == nil {
		return "", ErrRuntimeCatalogUnavailable
	}
	explicit := request.Version != ""
	version, err := c.resolveVersion(ctx, request.ID, request.Version)
	if err != nil {
		return "", err
	}
	if template, lookupErr := c.static.registry.Lookup(request.ID, version); lookupErr == nil {
		if explicit {
			if err := c.rejectIntegrity(); err != nil {
				c.observeResolve(RuntimeSourceStatic, err)
				return "", err
			}
		}
		text, fallbackErr := template.FallbackText(request.State, request.Fields, request.Lang)
		c.observeResolve(RuntimeSourceStatic, fallbackErr)
		return text, fallbackErr
	} else if !errors.Is(lookupErr, ErrTemplateUnknown) {
		return "", lookupErr
	}
	if version == "" {
		return "", fmt.Errorf("%w: %s has no default", ErrTemplateUnknown, request.ID)
	}
	artifact, err := c.resolveDynamic(ctx, request.Access, request.ID, version)
	if err != nil {
		return "", err
	}
	return artifact.Template.FallbackText(request.State, request.Fields, request.Lang)
}

func (c *RuntimeCatalog) ActionContext(ctx context.Context, request CatalogActionRequest) (CatalogActionContext, error) {
	if c == nil || c.static == nil || c.store == nil {
		return CatalogActionContext{}, ErrRuntimeCatalogUnavailable
	}
	if _, err := c.static.registry.Lookup(request.ID, request.Version); err == nil {
		if err := c.rejectIntegrity(); err != nil {
			c.observeResolve(RuntimeSourceStatic, err)
			return CatalogActionContext{}, err
		}
		result, actionErr := c.static.ActionContext(ctx, request)
		c.observeResolve(RuntimeSourceStatic, actionErr)
		return result, actionErr
	} else if !errors.Is(err, ErrTemplateUnknown) {
		return CatalogActionContext{}, err
	}
	artifact, err := c.resolveDynamic(ctx, request.Access, request.ID, request.Version)
	if err != nil {
		return CatalogActionContext{}, err
	}
	view, err := actionViewFromMeta(artifact.Meta, request.ActionID)
	if err != nil {
		return CatalogActionContext{}, err
	}
	return CatalogActionContext{View: view, Meta: artifact.Meta.Clone()}, nil
}

func (c *RuntimeCatalog) resolveDynamic(
	ctx context.Context,
	access CatalogAccess,
	id ID,
	version string,
) (artifact *CompiledArtifact, err error) {
	defer func() { c.observeResolve(RuntimeSourceDynamic, err) }()
	if err := c.requireReady(); err != nil {
		return nil, err
	}
	if c.authorizeDynamic == nil {
		return nil, ErrRuntimeCatalogNotAuthorized
	}
	meta, err := c.store.LoadArtifactMeta(ctx, id, version)
	if err != nil {
		return nil, classifyRuntimeStoreError("load artifact metadata", err)
	}
	if err := validateRuntimeArtifactMeta(id, version, meta); err != nil {
		return nil, err
	}
	if meta.Blocked {
		return nil, fmt.Errorf("%w: %s@%s", ErrRuntimeCatalogBlocked, id, version)
	}
	if err := c.authorizeDynamic(ctx, access, meta); err != nil {
		if errors.Is(err, ErrRuntimeCatalogNotAuthorized) || errors.Is(err, ErrRuntimeCatalogNewSendDisabled) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: authorize dynamic artifact: %w", ErrRuntimeCatalogUnavailable, err)
	}
	return c.loadCompiled(ctx, meta)
}

func (c *RuntimeCatalog) resolveVersion(ctx context.Context, id ID, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if err := c.requireReady(); err != nil {
		return "", err
	}
	activation, err := c.store.LoadActivation(ctx, id)
	if err != nil {
		return "", classifyRuntimeStoreError("load activation", err)
	}
	if !activation.Exists {
		return "", nil
	}
	switch activation.Status {
	case RuntimeActivationDisabled:
		return "", fmt.Errorf("%w: %s revision %d", ErrRuntimeCatalogDisabled, id, activation.Revision)
	case RuntimeActivationActive:
		if strings.TrimSpace(activation.Version) == "" {
			return "", fmt.Errorf("%w: active row has no version", ErrRuntimeCatalogIntegrity)
		}
		return activation.Version, nil
	default:
		return "", fmt.Errorf("%w: unknown activation status %q", ErrRuntimeCatalogIntegrity, activation.Status)
	}
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

func classifyRuntimeStoreError(operation string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, ErrTemplateUnknown), errors.Is(err, ErrRuntimeCatalogIntegrity):
		return fmt.Errorf("cardtmpl: %s: %w", operation, err)
	default:
		return fmt.Errorf("%w: %s: %v", ErrRuntimeCatalogUnavailable, operation, err)
	}
}
