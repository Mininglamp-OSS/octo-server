// Runtime onboarding endpoint — replaces the BotFather `/daemon` IM command.
//
// Web `/runtimes` page calls GET /v1/runtime-onboarding to get the daemon
// install + start commands for the logged-in user. Returns the user's
// api_key (lazy-create on first call), derived service URLs, and pre-rendered
// CLI command strings for direct display in the CreateRuntimeModal.
//
// The previous `/daemon` IM-side command (modules/botfather/command.go's
// handleDaemon) was removed in the same PR — onboarding is web-only now.

package botfather

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// runtimeOnboardingResp is the JSON shape consumed by web's
// CreateRuntimeModal. Callers should treat the `commands.start` string as
// the canonical start command — env_vars are exposed for the case where
// the user wants to set them in their shell rc file rather than inlining
// per-invocation, but the command string is sufficient on its own.
type runtimeOnboardingResp struct {
	APIKey    string            `json:"api_key"`
	SpaceID   string            `json:"space_id"`
	ServerURL string            `json:"server_url"`
	FleetURL  string            `json:"fleet_url"`
	MatterURL string            `json:"matter_url"`
	Commands  onboardingCmds    `json:"commands"`
	EnvVars   map[string]string `json:"env_vars"`
}

type onboardingCmds struct {
	Install string `json:"install"`
	Start   string `json:"start"`
}

// runtimeOnboarding handles GET /v1/runtime-onboarding.
//
// Auth: session token (web caller only — daemon never calls this; daemon
// already has its api_key).
//
// Behavior mirrors the now-removed handleDaemon (formerly BotFather IM
// `/daemon` command):
//   1. Resolve caller uid + space_id
//   2. getOrCreate user_api_key for (uid, space_id)
//   3. Derive server / fleet / matter URLs from server config
//   4. Pre-render install + start command strings for display
//
// space_id source: the request must carry a verified space context (either
// via X-Space-Id header set by middleware or query param ?space_id=). If
// neither is present, return 400 — no implicit "first space" fallback,
// since that would let the caller end up with an api_key bound to a space
// they didn't intend.
func (bf *BotFather) runtimeOnboarding(c *wkhttp.Context) {
	uid := c.GetLoginUID()
	if uid == "" {
		c.ResponseErrorWithStatus(errors.New("login required"), http.StatusUnauthorized)
		return
	}

	spaceID := strings.TrimSpace(c.GetHeader("X-Space-Id"))
	if spaceID == "" {
		spaceID = strings.TrimSpace(c.Query("space_id"))
	}
	if spaceID == "" {
		c.ResponseErrorWithStatus(errors.New("space_id required (header X-Space-Id or query ?space_id=)"), http.StatusBadRequest)
		return
	}

	// getOrCreate user_api_key for (uid, space_id). Mirrors the lazy-create
	// behavior the IM /daemon command had — first onboarding access seeds
	// the row, subsequent calls reuse it. INSERT failure on race is
	// tolerated by re-querying (two concurrent web tabs could both miss
	// the SELECT then both INSERT, but the (uid, space_id) unique
	// constraint guarantees only one wins; the loser re-reads).
	apiKey, err := bf.getOrCreateUserAPIKey(uid, spaceID)
	if err != nil {
		bf.Error("runtime-onboarding: get/create api_key", zap.Error(err))
		c.ResponseErrorWithStatus(errors.New("failed to allocate api_key"), http.StatusInternalServerError)
		return
	}

	serverURL, fleetURL, matterURL := bf.deriveOnboardingURLs()

	resp := runtimeOnboardingResp{
		APIKey:    apiKey,
		SpaceID:   spaceID,
		ServerURL: serverURL,
		FleetURL:  fleetURL,
		MatterURL: matterURL,
		Commands: onboardingCmds{
			Install: "go install github.com/Mininglamp-OSS/octo-daemon-cli@latest",
			Start:   fmt.Sprintf("octo-daemon start --api-key %s --api-url %s", apiKey, fleetURL),
		},
		EnvVars: map[string]string{
			"OCTO_SERVER_URL": serverURL,
			"OCTO_FLEET_URL":  fleetURL,
			"OCTO_MATTER_URL": matterURL,
		},
	}
	c.JSON(http.StatusOK, resp)
}

// getOrCreateUserAPIKey looks up the (uid, space_id) api_key row, or
// creates one with a freshly-generated `uk_<32hex>` token if missing.
// Extracted from the now-deleted handleDaemon so both the new HTTP
// endpoint and any future caller can share the same lazy-init semantics.
func (bf *BotFather) getOrCreateUserAPIKey(uid, spaceID string) (string, error) {
	existing, err := bf.db.queryUserAPIKeyByUIDAndSpaceID(uid, spaceID)
	if err != nil {
		return "", fmt.Errorf("query api_key: %w", err)
	}
	if existing != nil {
		return existing.APIKey, nil
	}
	hex, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate api_key: %w", err)
	}
	apiKey := UserAPIKeyPrefix + hex
	if err := bf.db.insertUserAPIKey(uid, apiKey, spaceID); err != nil {
		// Race: another concurrent caller may have inserted in between.
		// Re-query — if a row is present now, the other side won the
		// race and we return their key. If still empty, the INSERT
		// failed for some other reason (e.g. DB error) and we surface
		// that.
		if again, qerr := bf.db.queryUserAPIKeyByUIDAndSpaceID(uid, spaceID); qerr == nil && again != nil {
			return again.APIKey, nil
		}
		return "", fmt.Errorf("insert api_key: %w", err)
	}
	return apiKey, nil
}

// deriveOnboardingURLs returns the (server, fleet, matter) URLs the daemon
// should hit. server URL comes from config (External.BaseURL, falling back
// to External.IP); fleet/matter ports are derived by swapping the trailing
// :port suffix. Mirrors the now-deleted handleDaemon's URL block.
//
// In production, octo-deployment overrides these via reverse proxy /
// docker-compose env, but the dev-local default works out of the box for
// users running all 3 services on the same host with default ports.
func (bf *BotFather) deriveOnboardingURLs() (server, fleet, matter string) {
	cfg := bf.ctx.GetConfig()
	server = cfg.External.BaseURL
	if strings.TrimSpace(server) == "" {
		server = fmt.Sprintf("http://%s:8090", cfg.External.IP)
	}
	// daemon constructs /v1/daemon/... paths itself — the base URL must
	// not end in /api or a trailing slash.
	server = strings.TrimSuffix(server, "/api")
	server = strings.TrimSuffix(server, "/")
	fleet = deriveServiceURL(server, ":8092")
	matter = deriveServiceURL(server, ":8080")
	return
}
