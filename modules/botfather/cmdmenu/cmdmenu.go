// Package cmdmenu owns BotFather's own command menu — the /newbot, /help, ...
// entries written to robot.bot_commands and rendered by clients as the chat
// command picker (#335).
//
// The menu is server-owned copy (unlike user bots' commands, which are creator
// content set via POST /v1/bot/setCommands and are never localized), so its
// descriptions render through the shared msgtmpl catalog. The stored blob can
// only carry ONE language, therefore localization happens in two layers:
//
//   - startup (botfather.registerBotFatherCommands) writes the blob in the
//     deployment default language — the floor served by paths that genuinely
//     have no request context (admin robot detail, future context-less reads);
//   - every request-bearing read of the blob overrides it per request via
//     JSON(i18n.OutboundLanguage(ctx)), for the BotFather UID only: channel
//     info extra.bot_commands, /v1/robot/commands, /v1/users/:uid, and the
//     batch user-detail fill (user.Service.GetUserDetails) behind friend
//     sync / friend search / search / conversation enrichment.
//
// This package is deliberately a LEAF (msgtmpl + pkg/i18n only): botfather
// imports user, so the read-side modules (user, channel, robot) can only share
// this catalog through a package that imports neither.
package cmdmenu

import (
	"embed"
	"encoding/json"
	"fmt"

	"github.com/Mininglamp-OSS/octo-server/modules/base/common/msgtmpl"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// BotFatherUID is the BotFather system bot's UID. The canonical constant lives
// here (not in the botfather package, which aliases it) so the read-side
// override call sites can match it without an import cycle.
const BotFatherUID = "botfather"

// Command is one menu entry in the robot.bot_commands storage/wire shape.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// templatesFS holds the per-language menu-description templates.
// Layout: templates/{lang}/menu.tmpl, one {{define "menu_*"}} block per entry.
//
//go:embed templates
var templatesFS embed.FS

// catalog enforces the msgtmpl completeness matrix at init: a supported
// language missing any menu key fails startup loudly.
var catalog = msgtmpl.MustNew(templatesFS, "templates")

// entries is the menu in display order; key names the {{define}} block.
var entries = []struct{ command, key string }{
	{"/install", "menu_install"},
	{"/quickstart", "menu_quickstart"},
	{"/newbot", "menu_newbot"},
	{"/mybots", "menu_mybots"},
	{"/connect", "menu_connect"},
	{"/disconnect", "menu_disconnect"},
	{"/setname", "menu_setname"},
	{"/setdescription", "menu_setdescription"},
	{"/deletebot", "menu_deletebot"},
	{"/token", "menu_token"},
	{"/revoke", "menu_revoke"},
	{"/approve", "menu_approve"},
	{"/reject", "menu_reject"},
	{"/pending", "menu_pending"},
	{"/help", "menu_help"},
	{"/cancel", "menu_cancel"},
}

// The menu is static (no template params), so every language is pre-rendered
// once at init; a render/marshal failure here is an asset defect and must fail
// loud at startup, never surface as a half-built menu at request time.
var (
	commandsByLang = map[string][]Command{}
	jsonByLang     = map[string]string{}
)

func init() {
	for _, lang := range octoi18n.SupportedLanguages() {
		menu := make([]Command, 0, len(entries))
		for _, e := range entries {
			desc, err := catalog.Render(e.key, lang, nil)
			if err != nil {
				panic(fmt.Sprintf("cmdmenu: render %s (%s): %v", e.key, lang, err))
			}
			menu = append(menu, Command{Command: e.command, Description: desc})
		}
		raw, err := json.Marshal(menu)
		if err != nil {
			panic(fmt.Sprintf("cmdmenu: marshal menu (%s): %v", lang, err))
		}
		commandsByLang[lang] = menu
		jsonByLang[lang] = string(raw)
	}
}

// normalize maps lang onto the supported matrix, falling back to the source
// language for an unsupported/empty tag — the same contract as msgtmpl.Render.
func normalize(lang string) string {
	if norm, ok := octoi18n.MatchSupportedLanguage(lang); ok {
		if _, present := jsonByLang[norm]; present {
			return norm
		}
	}
	return octoi18n.SourceLanguage
}

// Commands returns the menu with descriptions rendered in lang.
func Commands(lang string) []Command {
	src := commandsByLang[normalize(lang)]
	out := make([]Command, len(src))
	copy(out, src)
	return out
}

// JSON returns the menu serialized in the robot.bot_commands storage/wire
// shape, rendered in lang.
func JSON(lang string) string {
	return jsonByLang[normalize(lang)]
}
