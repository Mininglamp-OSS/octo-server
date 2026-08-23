package common

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
)

// managerConsoleAccountsMissingEmail returns the usernames of accounts that can
// sign in to the management console but have no email address on file.
//
// "Can sign in" mirrors the gate in modules/user Manager.login exactly —
// role ∈ auth.ManagerConsoleRoles, status != UserDisable(0), is_destroy != 2 —
// so the guard and the runtime cannot disagree about who a 2FA switch affects.
// A destroy application still in its cooling period (is_destroy=1) can still log
// in today, so it counts here too.
//
// This lives in modules/common rather than modules/user because modules/user
// imports modules/common; the reverse edge would be an import cycle. pkg/auth
// depends on no module, so the role set is shared rather than duplicated.
func managerConsoleAccountsMissingEmail(ctx *config.Context) ([]string, error) {
	var usernames []string
	_, err := ctx.DB().Select("username").
		From("user").
		Where("role IN ?", auth.ManagerConsoleRoles).
		Where("status<>0").
		Where("is_destroy<>2").
		Where("email IS NULL OR TRIM(email)=''").
		OrderBy("username").
		Load(&usernames)
	if err != nil {
		return nil, err
	}
	// A console account seeded without a username would render as an empty
	// entry in the error detail, which tells the operator nothing. Fall back to
	// a placeholder so the count still reflects reality.
	for i, name := range usernames {
		if strings.TrimSpace(name) == "" {
			usernames[i] = "(unnamed account)"
		}
	}
	return usernames, nil
}
