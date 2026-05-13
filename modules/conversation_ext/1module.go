package conversation_ext

import (
	"embed"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

// ---------------------------------------------------------------------------
// Global singleton — same pattern as modules/user/db_pinned.go
// ---------------------------------------------------------------------------

var (
	globalConvExtService     *Service
	globalConvExtServiceOnce sync.Once
)

// InitGlobalConvExtService initialises the package-level *Service singleton.
// It is idempotent: repeated calls after the first are no-ops (sync.Once).
// Called from the module factory below so the singleton is ready before any
// handler or cascade-cleanup hook uses it.
func InitGlobalConvExtService(ctx *config.Context) {
	globalConvExtServiceOnce.Do(func() {
		globalConvExtService = NewService(ctx)
	})
}

// GetGlobalConvExtService returns the singleton *Service, or nil if
// InitGlobalConvExtService has not been called yet.
// External modules (group, thread, …) that inject cascade-cleanup hooks
// should call this to reach the service without importing anything else.
func GetGlobalConvExtService() *Service {
	return globalConvExtService
}

// ---------------------------------------------------------------------------
// Cascade-cleanup hook — stub for task #5
// ---------------------------------------------------------------------------

// CleanupHookFn is the signature for cascade-cleanup callbacks that other
// modules can register.  The concrete contract (e.g. "delete all ext rows
// when a user leaves a space") will be defined in task #5.
type CleanupHookFn func(uid, spaceID string) error

var (
	cleanupHooksMu sync.RWMutex
	cleanupHooks   []CleanupHookFn
)

// RegisterCleanupHook allows external modules to register a cascade-cleanup
// callback.  Stub implementation: nil is accepted silently (no-op); the full
// dispatch logic will be added in task #5.
func RegisterCleanupHook(fn CleanupHookFn) {
	if fn == nil {
		return
	}
	cleanupHooksMu.Lock()
	defer cleanupHooksMu.Unlock()
	cleanupHooks = append(cleanupHooks, fn)
}

// resetGlobalConvExtServiceOnce is a test-only helper that resets the
// sync.Once so individual tests can call InitGlobalConvExtService independently.
// Cannot be called from production code — the *testing.T parameter enforces this.
func resetGlobalConvExtServiceOnce(_ *testing.T) {
	globalConvExtServiceOnce = sync.Once{}
	globalConvExtService = nil
}

// ---------------------------------------------------------------------------
// Module registration
// ---------------------------------------------------------------------------

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		appCtx := ctx.(*config.Context)

		// Initialise the global singleton so it is available to other modules
		// (e.g. for cascade-cleanup hooks registered via RegisterCleanupHook)
		// before any HTTP request is served.
		InitGlobalConvExtService(appCtx)

		return register.Module{
			Name:    "conversation_ext",
			SQLDir:  register.NewSQLFS(sqlFS),
			Service: GetGlobalConvExtService(),
		}
	})
}
