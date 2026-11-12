package message

import (
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/permissionbundle"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

// NewServerMessageFactHooks exposes this module's actual message storage
// sharding, per-user extra/offset sharding, visibles whitelist and DM space-tag
// helpers to the permission server-only message facts provider, so the provider
// does not carry divergent copies of those rules.
//
// It does not open any database connection; the provider receives its own
// session. Group/thread membership is read from the native tables by the
// provider using the same predicates as requireGroupMember (active, not
// blacklisted, group Normal or Disband for reads).
func NewServerMessageFactHooks(ctx *config.Context) permissionbundle.ServerMessageHooks {
	db := &DB{ctx: ctx}
	userExtra := &messageUserExtraDB{ctx: ctx}
	offset := &channelOffsetDB{ctx: ctx}
	return permissionbundle.ServerMessageHooks{
		MessageTable:          db.getTable,
		MessageUserExtraTable: userExtra.getTable,
		ChannelOffsetTable:    offset.getTable,
		PayloadVisibleTo:      visiblesAllows,
		PayloadSpaceID: func(payload []byte) string {
			var m map[string]interface{}
			if err := json.Unmarshal(payload, &m); err != nil {
				return ""
			}
			return extractPayloadSpaceID(m)
		},
		IsSystemUID: spacepkg.IsSystemBot,
	}
}
