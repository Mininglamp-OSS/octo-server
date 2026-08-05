package card_template_catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	runtimeCatalogControlEnv      = "OCTO_CARD_RUNTIME_CATALOG_CONTROL_ENABLED"
	runtimeCatalogNewSendEnv      = "OCTO_CARD_RUNTIME_CATALOG_NEW_SEND_ENABLED"
	runtimeCatalogCacheEntriesEnv = "OCTO_CARD_RUNTIME_CATALOG_CACHE_ENTRIES"
	runtimeCatalogCacheBytesEnv   = "OCTO_CARD_RUNTIME_CATALOG_CACHE_BYTES"
	runtimeCatalogCompileMSEnv    = "OCTO_CARD_RUNTIME_CATALOG_COMPILE_TIMEOUT_MS"
)

type runtimeCatalogGates struct {
	controlEnabled bool
	newSendEnabled bool
}

var (
	installedStoreMu sync.RWMutex
	installedStore   *store
	installedGates   runtimeCatalogGates
	installedReady   *runtimeCatalogReadiness
)

// InstallRuntimeCatalog is the composition-root entry point. It constructs the
// process's only runtime store/cache pair before module factories are created;
// the manager control plane later reuses the same store instance.
func InstallRuntimeCatalog(
	db *sql.DB,
	registry *cardtmpl.Registry,
	config cardtmpl.RuntimeCatalogConfig,
) (*cardtmpl.RuntimeCatalog, error) {
	if db == nil || registry == nil {
		return nil, errors.New("card template catalog: runtime dependencies are required")
	}
	gates, warnings := resolveRuntimeCatalogGates(os.Getenv)
	for _, warning := range warnings {
		log.Warn(warning)
	}
	config, err := runtimeCatalogConfigFromEnv(config, os.Getenv)
	if err != nil {
		return nil, err
	}
	readiness := newRuntimeCatalogReadiness()
	downstreamReady := config.CheckReady
	config.CheckReady = func() error {
		if err := readiness.Check(); err != nil {
			return err
		}
		if downstreamReady != nil {
			return downstreamReady()
		}
		return nil
	}
	config.AuthorizeDynamic = runtimeDynamicAuthorizer(gates, config.AuthorizeDynamic)
	config.Hooks = runtimeMetricHooks(config.Hooks, newCatalogMetrics(prometheus.DefaultRegisterer))
	runtimeStore := newStore(db)
	catalog, err := cardtmpl.NewRuntimeCatalog(registry, runtimeStore, config)
	if err != nil {
		return nil, err
	}
	setInstalledRuntimeStore(runtimeStore)
	setInstalledRuntimeGates(gates)
	setInstalledRuntimeReadiness(readiness)
	cardtmpl.SetDefaultCatalog(catalog)
	// Publish the resolver so consumers outside this module (the Bot capability
	// manifest, chiefly) read the same grant truth through the same snapshot
	// contract instead of growing a second one.
	cardtmpl.SetDefaultAuthorizationSource(&runtimeAuthorizationSource{
		store: runtimeStore, gates: gates,
	})
	return catalog, nil
}

// runtimeAuthorizationSource binds the store's readers to the deployment gate.
// It exists so that "new-send is dark" is answerable without a DB round trip:
// a gated-off deployment must keep its pre-PR-C static behaviour exactly, and
// that includes not acquiring a runtime DB dependency it did not have before.
type runtimeAuthorizationSource struct {
	store *store
	gates runtimeCatalogGates
}

func (s *runtimeAuthorizationSource) LoadAuthorization(
	ctx context.Context,
	query cardtmpl.RuntimeAuthorizationQuery,
) (cardtmpl.RuntimeAuthorization, error) {
	return s.store.LoadAuthorization(ctx, query)
}

func (s *runtimeAuthorizationSource) ListAuthorizedTemplates(
	ctx context.Context,
	principal cardtmpl.CatalogPrincipal,
	limit int,
) ([]cardtmpl.RuntimeAdvertisedTemplate, error) {
	return s.store.ListAuthorizedTemplates(ctx, principal, limit)
}

func (s *runtimeAuthorizationSource) NewSendEnabled() bool { return s.gates.newSendEnabled }

func runtimeCatalogConfigFromEnv(
	config cardtmpl.RuntimeCatalogConfig,
	getenv func(string) string,
) (cardtmpl.RuntimeCatalogConfig, error) {
	if getenv == nil {
		return config, nil
	}
	if raw := strings.TrimSpace(getenv(runtimeCatalogCacheEntriesEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 256 {
			return config, fmt.Errorf("%s must be between 1 and 256", runtimeCatalogCacheEntriesEnv)
		}
		config.MaxCacheEntries = value
	}
	if raw := strings.TrimSpace(getenv(runtimeCatalogCacheBytesEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 128<<20 {
			return config, fmt.Errorf("%s must be between 1 and %d", runtimeCatalogCacheBytesEnv, 128<<20)
		}
		config.MaxCacheBytes = value
	}
	if raw := strings.TrimSpace(getenv(runtimeCatalogCompileMSEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 10_000 {
			return config, fmt.Errorf("%s must be between 1 and 10000", runtimeCatalogCompileMSEnv)
		}
		config.CompileTimeout = time.Duration(value) * time.Millisecond
	}
	return config, nil
}

func runtimeMetricHooks(existing cardtmpl.RuntimeCatalogHooks, metrics *catalogMetrics) cardtmpl.RuntimeCatalogHooks {
	return cardtmpl.RuntimeCatalogHooks{
		OnResolve: func(source cardtmpl.RuntimeSource, result string) {
			if existing.OnResolve != nil {
				existing.OnResolve(source, result)
			}
			metrics.observeResolve(string(source), result)
		},
		OnCache: func(result string) {
			if existing.OnCache != nil {
				existing.OnCache(result)
			}
			metrics.observeCache(result)
		},
		OnCacheSize: func(entries, bytes int) {
			if existing.OnCacheSize != nil {
				existing.OnCacheSize(entries, bytes)
			}
			metrics.setCacheSize(entries, bytes)
		},
	}
}

func installedRuntimeStore() *store {
	installedStoreMu.RLock()
	defer installedStoreMu.RUnlock()
	return installedStore
}

func setInstalledRuntimeStore(store *store) {
	installedStoreMu.Lock()
	defer installedStoreMu.Unlock()
	installedStore = store
}

func installedRuntimeGates() runtimeCatalogGates {
	installedStoreMu.RLock()
	defer installedStoreMu.RUnlock()
	return installedGates
}

func setInstalledRuntimeGates(gates runtimeCatalogGates) {
	installedStoreMu.Lock()
	defer installedStoreMu.Unlock()
	installedGates = gates
}

func installedRuntimeReadiness() *runtimeCatalogReadiness {
	installedStoreMu.RLock()
	defer installedStoreMu.RUnlock()
	return installedReady
}

func setInstalledRuntimeReadiness(readiness *runtimeCatalogReadiness) {
	installedStoreMu.Lock()
	defer installedStoreMu.Unlock()
	installedReady = readiness
}

func resolveRuntimeCatalogGates(getenv func(string) string) (runtimeCatalogGates, []string) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	control, controlOK := strictRuntimeBool(getenv(runtimeCatalogControlEnv))
	newSend, newSendOK := strictRuntimeBool(getenv(runtimeCatalogNewSendEnv))
	warnings := make([]string, 0, 2)
	if !controlOK {
		warnings = append(warnings, runtimeCatalogControlEnv+" is missing or invalid; dynamic activation remains disabled")
	}
	if !newSendOK {
		warnings = append(warnings, runtimeCatalogNewSendEnv+" is missing or invalid; dynamic new-send remains disabled")
	}
	return runtimeCatalogGates{controlEnabled: control, newSendEnabled: newSend}, warnings
}

func strictRuntimeBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// runtimeDynamicAuthorizer turns one snapshot into one allow/deny. Every input
// it reads — purpose, owner, grant — was captured at the same instant by the
// resolver; the function performs no IO of its own, which is what keeps the
// decision linearizable against a concurrent revoke or block.
//
// The checks are ordered cheapest-and-most-global first so that a deployment
// with the new-send gate closed never reveals anything about grants, and an
// unapproved owner is rejected before any per-principal state is considered.
func runtimeDynamicAuthorizer(
	gates runtimeCatalogGates,
	downstream cardtmpl.RuntimeDynamicAuthorizeFunc,
) cardtmpl.RuntimeDynamicAuthorizeFunc {
	return func(
		ctx context.Context,
		access cardtmpl.CatalogAccess,
		meta cardtmpl.RuntimeArtifactMeta,
		grant cardtmpl.RuntimeGrant,
	) error {
		if access.Purpose == cardtmpl.CatalogPurposeNewSend && !gates.newSendEnabled {
			return cardtmpl.ErrRuntimeCatalogNewSendDisabled
		}
		if !isApprovedRuntimeOwner(meta.Owner) {
			return cardtmpl.ErrRuntimeCatalogNotAuthorized
		}
		// A grantable principal is required: `system` and blank identities are
		// control-plane concepts, never business producers (D2), and the
		// resolver reports them as ungranted rather than erroring.
		if !grant.Allows(access.Purpose) {
			return cardtmpl.ErrRuntimeCatalogNotAuthorized
		}
		if downstream == nil {
			return nil
		}
		return downstream(ctx, access, meta, grant)
	}
}
