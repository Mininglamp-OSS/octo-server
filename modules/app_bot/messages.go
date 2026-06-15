package app_bot

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/common/msgtmpl"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

//go:embed templates
var templatesFS embed.FS

// appBotMessages is the App Bot outbound success-message catalog — the apply
// flow's HTTP success "message" bodies. MustNew enforces at startup that every
// supported language defines every key, so a missing translation is a build/
// asset defect surfaced on boot, not a runtime blank.
var appBotMessages = msgtmpl.MustNew(templatesFS, "templates")

// localizedMessage renders a success-response "message" in the request's
// negotiated language (the same resolution path the error envelope uses), so a
// success body and any error on the same endpoint stay in one language rather
// than diverging. A render miss is logged and yields "" — the completeness
// matrix makes that a bug, not an expected path.
func (ab *AppBot) localizedMessage(c *wkhttp.Context, key string) string {
	lang := octoi18n.LanguageOrDefault(c.Request.Context(), octoi18n.DefaultLanguage)
	s, err := appBotMessages.Render(key, lang, nil)
	if err != nil {
		ab.Error("render app_bot success message failed",
			zap.String("key", key), zap.String("lang", lang), zap.Error(err))
		return ""
	}
	return s
}
