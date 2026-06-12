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

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
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
// already has its api_key) + space membership + active user.
//
// Behavior mirrors the now-removed handleDaemon (formerly BotFather IM
// `/daemon` command):
//   1. Resolve caller uid + space_id
//   2. Verify caller is an active member of an active space + active user
//      (D11 撤销链路审计: token cache 路径 + DB SQL gate 双层防护)
//   3. getOrCreate user_api_key for (uid, space_id)
//   4. Derive server / fleet / matter URLs from server config
//   5. Pre-render install + start command strings for display
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

	// query 优先 / header 备选 — 跟 SpaceMiddleware (pkg/space/middleware.go)
	// 顺序对齐, 减少认知负担.
	spaceID := strings.TrimSpace(c.Query("space_id"))
	if spaceID == "" {
		spaceID = strings.TrimSpace(c.GetHeader("X-Space-Id"))
	}
	if spaceID == "" {
		spaceID = strings.TrimSpace(c.GetHeader("X-Space-ID"))
	}
	if spaceID == "" {
		c.ResponseErrorWithStatus(errors.New("space_id required (query ?space_id= or header X-Space-Id)"), http.StatusBadRequest)
		return
	}

	// C1 fix (review round 1): 显式 SpaceMember 校验 + active user 二重 gate.
	// CheckMembership 已 join space.status=1 + space_member.status=1 但
	// **没** join user.status=1 (D11 撤销链路: liftBanUser 走 redis token
	// cache 路径关 banned user 的 session, 但 onboarding 是 lazy-create
	// api_key, 一旦 row 写下去后续 verify-api-key 会显式查 user.status=1
	// 堵住, 但当下这次写入仍会"成功" — 给 banned user lazy-create 一行
	// dead row 是数据污染, 这里加 belt-and-braces).
	ok, err := spacepkg.CheckMembership(bf.ctx.DB().NewSession(nil), spaceID, uid)
	if err != nil {
		bf.Error("runtime-onboarding: check membership", zap.Error(err))
		c.ResponseErrorWithStatus(errors.New("internal error"), http.StatusInternalServerError)
		return
	}
	if !ok {
		c.ResponseErrorWithStatus(errors.New("not a member of this space"), http.StatusForbidden)
		return
	}
	// user.status=1 二重 gate (D11): banned user 应被堵在这一层, 不能落
	// 库 lazy-create dead api_key row.
	var userActive int
	if qerr := bf.ctx.DB().NewSession(nil).
		SelectBySql("SELECT COUNT(*) FROM user WHERE uid=? AND status=1", uid).
		LoadOne(&userActive); qerr != nil {
		bf.Error("runtime-onboarding: check user status", zap.Error(qerr))
		c.ResponseErrorWithStatus(errors.New("internal error"), http.StatusInternalServerError)
		return
	}
	if userActive == 0 {
		c.ResponseErrorWithStatus(errors.New("user not active"), http.StatusForbidden)
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
	if strings.Contains(serverURL, "://:") || strings.Contains(fleetURL, "://:") || strings.Contains(matterURL, "://:") {
		// External.BaseURL + External.IP 都空 — 拼出来是 'http://:8090'
		// broken URL, 给前端展示也是误导, 直接报 500 + log 提示运维.
		bf.Error("runtime-onboarding: server URL config missing (External.BaseURL + External.IP both empty)",
			zap.String("server", serverURL), zap.String("fleet", fleetURL), zap.String("matter", matterURL))
		c.ResponseErrorWithStatus(errors.New("server URL not configured"), http.StatusInternalServerError)
		return
	}

	// commands.start 是 standalone 可复制可跑的 multi-line block: 含 3 个
	// OCTO_*_URL export 让 daemon-cli 能拿到 server/fleet/matter, 末尾才是
	// octo-daemon start 行. caller 直接复制 commands.start 就跑得起来,
	// 不用再去 env_vars 字段二次拼接 (env_vars 单独保留供想 set 到 shell rc
	// 文件而非 inline 的场景).
	startBlock := fmt.Sprintf(
		"export OCTO_SERVER_URL=%s\nexport OCTO_FLEET_URL=%s\nexport OCTO_MATTER_URL=%s\nocto-daemon start --api-key %s --api-url %s",
		serverURL, fleetURL, matterURL, apiKey, fleetURL,
	)

	resp := runtimeOnboardingResp{
		APIKey:    apiKey,
		SpaceID:   spaceID,
		ServerURL: serverURL,
		FleetURL:  fleetURL,
		MatterURL: matterURL,
		Commands: onboardingCmds{
			Install: "go install github.com/Mininglamp-OSS/octo-daemon-cli@latest",
			Start:   startBlock,
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
