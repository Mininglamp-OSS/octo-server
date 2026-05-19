package common

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// Shared SystemSettings instance. EnsureSystemSettings is the single entry
// point — every caller (Common.New, NewManager, modules/user/*, modules/base/
// common.EmailService) goes through it so the in-memory snapshot is shared
// across the process. Otherwise the admin-write Reload would only update one
// instance and other modules would keep serving stale values.
var (
	sharedMu             sync.Mutex
	sharedSystemSettings *SystemSettings
)

// EnsureSystemSettings returns the process-wide SystemSettings instance,
// constructing it on first call. Safe to call from any goroutine.
//
// Failed initial Load is non-fatal: an empty-snapshot instance is stored
// and the background auto-reload (started here) will retry every
// reloadTTL. Until then all getters fall back to yaml — degraded mode,
// not a hard failure. A successful subsequent reload self-heals.
func EnsureSystemSettings(ctx *config.Context) *SystemSettings {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedSystemSettings != nil {
		return sharedSystemSettings
	}
	s := NewSystemSettings(ctx, newSystemSettingDB(ctx))
	if err := s.Load(); err != nil {
		s.Error("initial SystemSettings load failed; auto-reload will retry",
			zap.Error(err))
	}
	// Self-healing in case Load failed above, and multi-instance sync for
	// admin writes on peer servers. Lifetime tied to the process: context.
	// Background is intentional — server has no cancellation handle to
	// thread through here, and the goroutine is harmless to leak at
	// shutdown.
	s.StartAutoReload(context.Background())
	sharedSystemSettings = s
	return sharedSystemSettings
}

// (resetSharedSystemSettingsForTest was removed: octo-lib's
// register.GetModules caches the moduleList with sync.Once for the lifetime
// of a test binary, so the Manager's stored *SystemSettings is bound to
// the first ctx. Resetting the package-level singleton produces a fresh
// instance that the Manager does NOT see, which historically led to
// confusing test failures. Tests should instead reuse the singleton
// captured by NewManager and mutate state through it. See
// TestManagerSystemSetting_BoolEmptyValueResetsToYaml for the pattern.)

// defaultReloadTTL is how often the background goroutine pulls a fresh
// snapshot from system_setting. 60s is the agreed budget for multi-instance
// drift: an admin-side change becomes visible on every server within one TTL.
const defaultReloadTTL = 60 * time.Second

// SystemSettings is the read path for admin-tunable global config.
//
// Lookup model:
//   - Snapshot is an immutable map[string]string ("category.key" → value),
//     swapped atomically by Load / Reload. Readers go through atomic.Pointer
//     and never take a lock; SMTP send (high-frequency) does not block on
//     admin writes.
//   - Empty DB value means "not configured" and falls back to the matching
//     yaml field on *config.Config.
//   - Encrypted values are decrypted at snapshot-build time and cached in
//     plaintext form in the map; the high-frequency read path never calls
//     the cipher. Decryption failure logs an error and skips the entry, so
//     the getter falls back to yaml rather than serving a corrupt value.
type SystemSettings struct {
	ctx       *config.Context
	db        *systemSettingDB
	snapshot  atomic.Pointer[map[string]string]
	reloadTTL time.Duration
	log.Log
}

// NewSystemSettings builds a helper with an empty initial snapshot.
// Callers must invoke Load() once at startup before serving traffic;
// Reload() is safe to call at any time (admin write path uses it).
func NewSystemSettings(ctx *config.Context, db *systemSettingDB) *SystemSettings {
	s := &SystemSettings{
		ctx:       ctx,
		db:        db,
		reloadTTL: defaultReloadTTL,
		Log:       log.NewTLog("SystemSettings"),
	}
	empty := map[string]string{}
	s.snapshot.Store(&empty)
	return s
}

// Load reads every row from system_setting and atomically replaces the
// snapshot. Used at startup and by Reload (which is just an alias for
// "load now" with logging semantics).
func (s *SystemSettings) Load() error {
	rows, err := s.db.listAll()
	if err != nil {
		return err
	}
	next := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ValueType == settingTypeEncrypted {
			if row.Value == "" {
				continue // empty → fall back to yaml
			}
			plaintext, err := decryptKey(row.Value)
			if err != nil {
				s.Error("decrypt system_setting failed; falling back to yaml",
					zap.String("category", row.Category),
					zap.String("key", row.KeyName),
					zap.Error(err))
				continue
			}
			next[schemaKey(row.Category, row.KeyName)] = plaintext
			continue
		}
		next[schemaKey(row.Category, row.KeyName)] = row.Value
	}
	s.snapshot.Store(&next)
	return nil
}

// Reload is the admin-write hook: after the manager API upserts new values
// it calls this so the change is visible on this instance immediately
// (other instances pick it up within reloadTTL).
func (s *SystemSettings) Reload() error {
	return s.Load()
}

// StartAutoReload kicks off a goroutine that re-loads the snapshot every
// reloadTTL until ctx is canceled. Intended to be called once at startup
// (with a long-lived context). Errors are logged but do not stop the loop.
//
// Production callers pass context.Background() — the goroutine therefore
// runs for the lifetime of the process and shuts down with it. The
// ctx.Done() arm exists to make this swappable: if a server-shutdown
// context is ever plumbed through, no code change is needed here. The
// defer ticker.Stop() is reached only on that future cancellation; with
// context.Background() it is unreachable but kept so the function stays
// correct under either invocation.
func (s *SystemSettings) StartAutoReload(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(s.reloadTTL)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Load(); err != nil {
					s.Error("auto-reload system_setting failed", zap.Error(err))
				}
			}
		}
	}()
}

// ----- generic getters -----

func (s *SystemSettings) lookup(category, key string) (string, bool) {
	// Defensive: NewSystemSettings always seeds a non-nil map, but a
	// zero-value SystemSettings literal (e.g. tests that bypass the
	// constructor) would crash here without this guard.
	snapPtr := s.snapshot.Load()
	if snapPtr == nil {
		return "", false
	}
	v, ok := (*snapPtr)[schemaKey(category, key)]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func (s *SystemSettings) getBool(category, key string, fallback bool) bool {
	v, ok := s.lookup(category, key)
	if !ok {
		return fallback
	}
	switch v {
	case "1", "true", "TRUE":
		return true
	case "0", "false", "FALSE":
		return false
	default:
		return fallback
	}
}

func (s *SystemSettings) getString(category, key string, fallback string) string {
	v, ok := s.lookup(category, key)
	if !ok {
		return fallback
	}
	return v
}

func (s *SystemSettings) getInt(category, key string, fallback int) int {
	v, ok := s.lookup(category, key)
	if !ok {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func (s *SystemSettings) getEncrypted(category, key string, fallback string) string {
	// Encrypted values are stored decrypted in the snapshot, so a plain
	// lookup is sufficient. The dedicated method exists so callers — and
	// readers — can see the difference between "stored as encrypted" and
	// "stored as string".
	return s.getString(category, key, fallback)
}

// ----- typed getters (the 7 settings shipped this iteration) -----

// RegisterOff returns whether registration is globally disabled.
// DB value wins over cfg.Register.Off when set.
func (s *SystemSettings) RegisterOff() bool {
	return s.getBool("register", "off", s.ctx.GetConfig().Register.Off)
}

// RegisterOnlyChina returns whether only China-region phone numbers may register.
func (s *SystemSettings) RegisterOnlyChina() bool {
	return s.getBool("register", "only_china", s.ctx.GetConfig().Register.OnlyChina)
}

// RegisterUsernameOn returns whether username-based registration is enabled.
func (s *SystemSettings) RegisterUsernameOn() bool {
	return s.getBool("register", "username_on", s.ctx.GetConfig().Register.UsernameOn)
}

// RegisterEmailOn returns whether email-based registration / login is enabled.
func (s *SystemSettings) RegisterEmailOn() bool {
	return s.getBool("register", "email_on", s.ctx.GetConfig().Register.EmailOn)
}

// SupportEmail returns the From address used by the SMTP sender.
func (s *SystemSettings) SupportEmail() string {
	return s.getString("support", "email", s.ctx.GetConfig().Support.Email)
}

// SupportEmailSmtp returns the SMTP host:port endpoint.
func (s *SystemSettings) SupportEmailSmtp() string {
	return s.getString("support", "email_smtp", s.ctx.GetConfig().Support.EmailSmtp)
}

// SupportEmailPwd returns the (decrypted) SMTP password. If the stored
// ciphertext fails to decrypt at Load time, the snapshot omits the key and
// this getter returns the yaml fallback.
func (s *SystemSettings) SupportEmailPwd() string {
	return s.getEncrypted("support", "email_pwd", s.ctx.GetConfig().Support.EmailPwd)
}
