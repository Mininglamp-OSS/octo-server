package user

import (
	"encoding/json"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/mcppolicy"
	"github.com/Mininglamp-OSS/octo-server/pkg/permissionbundle"
)

func (r *authVerifyBotReq) UnmarshalJSON(data []byte) error {
	var selector map[string]json.RawMessage
	if err := json.Unmarshal(data, &selector); err != nil {
		return err
	}
	for key := range selector {
		if strings.EqualFold(key, "identity_version") {
			req, err := permissionbundle.ParseManagedIdentity(data)
			if err != nil {
				return err
			}
			*r = authVerifyBotReq{BotToken: req.BotToken, IncludeMCPPolicy: req.IncludePolicy, IdentityVersion: 2}
			return nil
		}
	}
	type legacy authVerifyBotReq
	var req legacy
	if err := json.Unmarshal(data, &req); err != nil {
		return err
	}
	include, err := mcppolicy.VerifyInclude(data)
	if err != nil {
		return err
	}
	*r = authVerifyBotReq(req)
	r.IncludeMCPPolicy = include
	return nil
}
