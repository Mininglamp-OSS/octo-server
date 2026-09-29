package user

import (
	"strings"

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
	if !enabled || profile.Robot != 0 || spacepkg.IsSystemBot(profile.UID) || profile.IsDestroy == IsDestroyDone {
		return profile, nil
	}
	account, err := u.db.QueryByUID(profile.UID)
	if err != nil {
		return nil, err
	}
	if account == nil || account.Status != 1 || account.Robot != 0 || account.IsDestroy == IsDestroyDone {
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
