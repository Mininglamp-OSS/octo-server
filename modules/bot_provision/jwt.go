// Module bootstrap for bot_provision (package name kept for git history
// continuity from the previous auth_jwt module). JWT signing /
// verification / JWKS endpoints have been removed (plan 决策一+二 Phase 4).
// What's left is:
//
//   - bot_api.go: mintBot (web session) + botToken (daemon api_key)
//   - resolve.go: api_key → uid + space_id lookup with membership check
//
// Module should be renamed to e.g. "bot_api" in a follow-up cleanup; the
// rename touches internal/modules.go import path so it's left for later.
package bot_provision

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// BotProvision is the module entrypoint registered with octo-lib. Only ctx +
// logger are needed now — RSA key state was for JWT signing, all gone.
type BotProvision struct {
	ctx *config.Context
	log.Log
}

// New returns a configured module. No more loadOrGenerateKey since JWT
// signing was removed in 合并 plan 决策一+二 Phase 4.
func New(ctx *config.Context) *BotProvision {
	return &BotProvision{
		ctx: ctx,
		Log: log.NewTLog("BotProvision"),
	}
}
