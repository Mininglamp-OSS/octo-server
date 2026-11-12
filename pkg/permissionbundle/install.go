package permissionbundle

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

const (
	EnvPermissionBundleEnabled             = "OCTO_PERMISSION_BUNDLE_ENABLED"
	EnvPermissionBundleOrigin              = "OCTO_PERMISSION_BUNDLE_ORIGIN"
	EnvPermissionBundleCatalogDir          = "OCTO_PERMISSION_BUNDLE_CATALOG_DIR"
	EnvPermissionBundleCatalogVersion      = "OCTO_PERMISSION_BUNDLE_CATALOG_VERSION"
	EnvPermissionBundleCatalogDigest       = "OCTO_PERMISSION_BUNDLE_CATALOG_DIGEST"
	EnvPermissionBundleSlotWitnessPath     = "OCTO_PERMISSION_BUNDLE_SLOT_WITNESS_PATH"
	EnvPermissionBundleSlotWitnessSHA256   = "OCTO_PERMISSION_BUNDLE_SLOT_WITNESS_SHA256"
	EnvPermissionBundleAuthorityMode       = "OCTO_PERMISSION_BUNDLE_AUTHORITY_MODE"
	EnvPermissionBundleFactProviderMode    = "OCTO_PERMISSION_BUNDLE_FACT_PROVIDER_MODE"
	EnvPermissionBundleStoreAuthorityID    = "OCTO_PERMISSION_BUNDLE_STORE_AUTHORITY_ID"
	EnvPermissionBundleStoreIncarnation    = "OCTO_PERMISSION_BUNDLE_STORE_INCARNATION"
	EnvPermissionBundlePermitKeyHex        = "OCTO_PERMISSION_BUNDLE_PERMIT_KEY_HEX"
	EnvPermissionBundleDurablePermitLedger = "OCTO_PERMISSION_BUNDLE_DURABLE_PERMIT_LEDGER"
)

// InstallConfig configures explicit opt-in permission bundle runtime composition.
// When Enabled is false (default), InstallOptIn is a no-op and leaves
// RuntimeFor(ctx) == nil so permission policy and fact routes stay unmounted.
// When Enabled is true, missing or invalid authority, origin, catalog, or fact
// dependencies return an explicit error and never fabricate a ready runtime.
type InstallConfig struct {
	Enabled                   bool
	Session                   *dbr.Session
	Origin                    string
	Pin                       CatalogPin
	Releases                  Releases
	CatalogDir                string
	SlotWitnessPath           string
	SlotWitnessSHA256         string
	SlotValues                SlotValues
	AuthorityMode             string
	AuthorityDriver           ExternalAuthorityDriver
	Authority                 FencedAuthority
	Native                    NativeGuard
	FactProviderMode          string
	IMFactProvider            NativeIMFactProvider
	ServerMessageHooks        ServerMessageHooks
	ServerStoreAuthorityID    string
	ServerStoreIncarnation    string
	ServerPermitKey           []byte
	ServerDurablePermitLedger bool
	Facts                     NativeFactSource
	testNoLocks               bool
}

func parseOptInEnabled(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	default:
		return false, fmt.Errorf("%w: invalid %s=%q", ErrUnavailable, EnvPermissionBundleEnabled, raw)
	}
}

// InstallFromEnv reads explicit opt-in environment configuration and delegates
// to InstallFromEnvWithHooks without pre-bound server message hooks.
func InstallFromEnv(ctx *config.Context, authorityDriver ExternalAuthorityDriver, imProvider NativeIMFactProvider) error {
	return InstallFromEnvWithHooks(ctx, authorityDriver, imProvider, ServerMessageHooks{})
}

// InstallFromEnvWithHooks reads explicit opt-in environment configuration and
// composes the permission bundle runtime, binding ServerMessageFactProvider to
// serverHooks when OCTO_PERMISSION_BUNDLE_FACT_PROVIDER_MODE is
// octo_server_message_store_v1. Unset or false OCTO_PERMISSION_BUNDLE_ENABLED
// is a no-op.
func InstallFromEnvWithHooks(ctx *config.Context, authorityDriver ExternalAuthorityDriver, imProvider NativeIMFactProvider, serverHooks ServerMessageHooks) error {
	enabled, err := parseOptInEnabled(os.Getenv(EnvPermissionBundleEnabled))
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	var permitKey []byte
	if rawKey := strings.TrimSpace(os.Getenv(EnvPermissionBundlePermitKeyHex)); rawKey != "" {
		decoded, err := hex.DecodeString(rawKey)
		if err != nil {
			return fmt.Errorf("%w: invalid %s hex", ErrUnavailable, EnvPermissionBundlePermitKeyHex)
		}
		permitKey = decoded
	}
	durablePermits, err := parseOptInEnabled(os.Getenv(EnvPermissionBundleDurablePermitLedger))
	if err != nil {
		return fmt.Errorf("%w: invalid %s", ErrUnavailable, EnvPermissionBundleDurablePermitLedger)
	}
	return InstallOptIn(ctx, InstallConfig{
		Enabled:                   true,
		Origin:                    strings.TrimSpace(os.Getenv(EnvPermissionBundleOrigin)),
		Pin:                       CatalogPin{Version: strings.TrimSpace(os.Getenv(EnvPermissionBundleCatalogVersion)), Digest: strings.TrimSpace(os.Getenv(EnvPermissionBundleCatalogDigest))},
		CatalogDir:                strings.TrimSpace(os.Getenv(EnvPermissionBundleCatalogDir)),
		SlotWitnessPath:           strings.TrimSpace(os.Getenv(EnvPermissionBundleSlotWitnessPath)),
		SlotWitnessSHA256:         strings.TrimSpace(os.Getenv(EnvPermissionBundleSlotWitnessSHA256)),
		AuthorityMode:             strings.TrimSpace(os.Getenv(EnvPermissionBundleAuthorityMode)),
		AuthorityDriver:           authorityDriver,
		FactProviderMode:          strings.TrimSpace(os.Getenv(EnvPermissionBundleFactProviderMode)),
		IMFactProvider:            imProvider,
		ServerMessageHooks:        serverHooks,
		ServerStoreAuthorityID:    strings.TrimSpace(os.Getenv(EnvPermissionBundleStoreAuthorityID)),
		ServerStoreIncarnation:    strings.TrimSpace(os.Getenv(EnvPermissionBundleStoreIncarnation)),
		ServerPermitKey:           permitKey,
		ServerDurablePermitLedger: durablePermits,
	})
}

// InstallOptIn installs the permission bundle runtime only when explicitly
// opted in and all configured dependencies pass verification.
func InstallOptIn(ctx *config.Context, cfg InstallConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if ctx == nil || ctx.Value(runtimeKey) != nil {
		return fmt.Errorf("%w: invalid or already-installed runtime context", ErrUnavailable)
	}
	if !ValidOrigin(cfg.Origin) {
		return fmt.Errorf("%w: opt-in permission bundle requires a valid https origin", ErrUnavailable)
	}
	if !validCatalogVersion(cfg.Pin.Version) || !validHex(cfg.Pin.Digest, 64) {
		return fmt.Errorf("%w: opt-in permission bundle requires an exact verified catalog pin", ErrUnavailable)
	}
	session := cfg.Session
	if session == nil && ctx.DB() != nil {
		session = ctx.DB()
	}
	if session == nil {
		return fmt.Errorf("%w: opt-in permission bundle requires an initialized SQL session", ErrUnavailable)
	}

	if cfg.AuthorityMode != "" || cfg.Authority == nil {
		mode := cfg.AuthorityMode
		if mode == "" && cfg.AuthorityDriver != nil {
			mode = AuthorityModeExternalFencedV1
		}
		if err := RejectUnfencedAuthorityMode(mode); err != nil {
			return err
		}
	}

	setupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	authority := cfg.Authority
	if authority == nil {
		if cfg.AuthorityDriver == nil {
			return fmt.Errorf("%w: opt-in permission bundle requires an external FencedAuthority passing RecoverAtLeast; unverified SQL pool or local writable state cannot fabricate readiness", ErrUnavailable)
		}
		var err error
		authority, err = NewFencedAuthorityAdapter(setupCtx, cfg.AuthorityDriver, cfg.Pin.Digest)
		if err != nil {
			return err
		}
	} else if fenced, ok := authority.(*FencedAuthorityAdapter); ok {
		if _, err := fenced.RecoverAtLeast(setupCtx); err != nil {
			return err
		}
	}

	releases := cfg.Releases
	if releases == nil {
		if cfg.CatalogDir == "" {
			return fmt.Errorf("%w: opt-in permission bundle requires a verified Releases registry or operator-owned CatalogDir", ErrUnavailable)
		}
		pkgInput, err := ReadCatalogDirectory(setupCtx, cfg.Pin, cfg.CatalogDir)
		if err != nil {
			return err
		}
		slots := cfg.SlotValues
		if slots == nil && cfg.SlotWitnessPath != "" {
			slots, _, err = ReadSlotWitnessFile(cfg.Pin, cfg.SlotWitnessPath, cfg.SlotWitnessSHA256)
			if err != nil {
				return err
			}
		}
		releases, err = NewCatalogRegistry(setupCtx, []SupportedSource{{
			Pin:    cfg.Pin,
			Format: CatalogPackageV2,
			Files:  pkgInput.Files,
		}}, nil, slots)
		if err != nil {
			return err
		}
	}

	nativeGuard := cfg.Native
	if nativeGuard == nil {
		nativeGuard = SQLNative{testNoLocks: cfg.testNoLocks}
	}

	facts := cfg.Facts
	imProvider := cfg.IMFactProvider
	factMode := strings.ToLower(strings.TrimSpace(cfg.FactProviderMode))
	if factMode != "" && factMode != "none" && factMode != "disabled" {
		if err := RejectPlainMessageSyncFactSource(cfg.FactProviderMode); err != nil {
			return err
		}
		if facts == nil && imProvider == nil && factMode == NativeFactProviderModeServerStoreV1 {
			serverProvider, err := NewServerMessageFactProvider(ServerMessageFactConfig{
				Session:             session,
				Hooks:               cfg.ServerMessageHooks,
				StoreAuthorityID:    cfg.ServerStoreAuthorityID,
				StoreIncarnation:    cfg.ServerStoreIncarnation,
				PermitKey:           cfg.ServerPermitKey,
				DurablePermitLedger: cfg.ServerDurablePermitLedger,
			})
			if err != nil {
				return err
			}
			imProvider = serverProvider
		}
		if facts == nil && imProvider == nil {
			return fmt.Errorf("%w: opt-in native fact mode %q requires a NativeIMFactProvider", ErrUnavailable, cfg.FactProviderMode)
		}
	}
	if facts == nil && imProvider != nil {
		sqlFacts, err := NewSQLNativeFactAdapter(authority, imProvider)
		if err != nil {
			return err
		}
		sqlFacts.testNoLocks = cfg.testNoLocks
		facts = sqlFacts
	}

	store := Store{
		Session:     session,
		Origin:      cfg.Origin,
		Authority:   authority,
		Native:      nativeGuard,
		Releases:    releases,
		testNoLocks: cfg.testNoLocks,
	}
	return Install(ctx, store, cfg.Pin, facts)
}
