package card_template_catalog

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name:   "card_template_catalog",
			SQLDir: register.NewSQLFS(sqlFS),
		}
	})
}
