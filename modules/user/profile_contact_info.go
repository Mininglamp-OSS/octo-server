package user

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

// profileContactResp is an HTTP-only projection. Explicit empty strings mean
// "not filled in" to profile cards; omitted fields mean withheld. Keep these
// fields out of UserDetailResp, which is also used by sync and IM data sources.
type profileContactResp struct {
	*UserDetailResp
	Phone            string `json:"phone"`
	Email            string `json:"email"`
	Zone             string `json:"zone,omitempty"`
	PhoneCountryCode string `json:"phone_country_code,omitempty"`
}

// profileWithContactInfo must only be called after the full-profile visibility
// check in User.get. The authoritative row is read only for the opted-in HTTP
// path, so neither the shared service nor a minimal profile can acquire peer PII.
func (u *User) profileWithContactInfo(profile *UserDetailResp, enabled bool) (any, error) {
	if !enabled || profile.Robot != 0 || spacepkg.IsSystemBot(profile.UID) || profileContactCategoryRestricted(profile.Category) || profile.IsDestroy == IsDestroyDone {
		return profile, nil
	}
	account, err := u.db.QueryByUID(profile.UID)
	if err != nil {
		return nil, err
	}
	// Recheck category on the fresh row as well: the account may have changed
	// since GetUserDetail. Manager roles are only available on this row, not on
	// the shared profile DTO. A visible full profile does not authorize sharing
	// a system/service account's contacts or a manager's authentication email.
	if account == nil || account.Status != 1 || account.Robot != 0 ||
		profileContactCategoryRestricted(account.Category) || auth.IsManagerConsoleRole(account.Role) || account.IsDestroy == IsDestroyDone {
		return profile, nil
	}
	return &profileContactResp{
		UserDetailResp:   profile,
		Phone:            account.Phone,
		Email:            account.Email,
		Zone:             account.Zone,
		PhoneCountryCode: strings.TrimPrefix(strings.TrimPrefix(account.Zone, "+"), "00"),
	}, nil
}

func profileContactCategoryRestricted(category string) bool {
	return category == CategorySystem || category == CategoryCustomerService
}
