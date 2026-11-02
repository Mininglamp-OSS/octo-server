package user

import (
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-server/pkg/mcppolicy"
)

func (r *authVerifyBotReq) UnmarshalJSON(data []byte) error {
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
