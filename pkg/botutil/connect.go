package botutil

import (
	"fmt"
	"os"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

// DefaultPluginPackage is the npm package an Agent runs (via `npx -y <pkg> ...`)
// to install the OpenClaw channel adapter and bind a Bot. It is the single
// backend-owned source of truth for the package name: the App Bot connect
// responses (modules/app_bot) and the BotFather connect prompts
// (modules/botfather) both resolve it through PluginPackage, so a package
// rename or canary rollout only touches the backend — never the frontend, and
// never the localized connect-guide prose.
const DefaultPluginPackage = "create-openclaw-octo"

// PluginPackageEnv overrides DefaultPluginPackage at runtime (rename / canary),
// so operators can switch the package without a code change or redeploy of the
// frontend.
const PluginPackageEnv = "OCTO_BOT_PLUGIN_PACKAGE"

// PluginPackage returns the configured OpenClaw plugin package name, honoring
// the OCTO_BOT_PLUGIN_PACKAGE override and falling back to DefaultPluginPackage.
func PluginPackage() string {
	if v := strings.TrimSpace(os.Getenv(PluginPackageEnv)); v != "" {
		return v
	}
	return DefaultPluginPackage
}

// DeriveAPIURL resolves the public Bot API entry for this deployment: the
// configured External.BaseURL, falling back to the direct-access host:port when
// BaseURL is unset. This is the value Agents pass as `--api-url` and the URL
// returned in Bot connect/register responses — NOT the admin-dashboard origin.
func DeriveAPIURL(cfg *config.Config) string {
	if u := strings.TrimSpace(cfg.External.BaseURL); u != "" {
		return u
	}
	return fmt.Sprintf("http://%s:8090", cfg.External.IP)
}
