package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/require"
)

// The hooks must expose this module's own helpers, not copies. This test needs
// no database: it only checks that the exported hooks delegate to the same
// sharding and visibility functions the message APIs use.
func TestServerMessageFactHooksUseModuleHelpers(t *testing.T) {
	cfg := config.New()
	cfg.TablePartitionConfig.MessageTableCount = 8
	cfg.TablePartitionConfig.MessageUserEditTableCount = 4
	cfg.TablePartitionConfig.ChannelOffsetTableCount = 4
	ctx := config.NewContext(cfg)
	h := NewServerMessageFactHooks(ctx)

	db := &DB{ctx: ctx}
	ue := &messageUserExtraDB{ctx: ctx}
	off := &channelOffsetDB{ctx: ctx}
	for _, id := range []string{"a", "group-1", "u1@u2", "0123456789abcdef0123456789abcdef____123456789012345"} {
		require.Equal(t, db.getTable(id), h.MessageTable(id), id)
		require.Equal(t, ue.getTable(id), h.MessageUserExtraTable(id), id)
		require.Equal(t, off.getTable(id), h.ChannelOffsetTable(id), id)
	}

	payload := []byte(`{"type":1,"visibles":["u1"],"space_id":"s1"}`)
	require.True(t, h.PayloadVisibleTo(payload, "u1"))
	require.False(t, h.PayloadVisibleTo(payload, "u2"))
	require.True(t, h.PayloadVisibleTo([]byte(`{"type":1}`), "u2"))
	require.Equal(t, "s1", h.PayloadSpaceID(payload))
	require.Equal(t, "", h.PayloadSpaceID([]byte(`not json`)))
	require.Equal(t, "", h.PayloadSpaceID([]byte(`{"space_id":7}`)))
	require.True(t, h.IsSystemUID("u_10000"))
	require.False(t, h.IsSystemUID("someone"))
}
