package message

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/botpolicy"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

// queryActiveBotTypes is the single wire-classification path shared by
// conversation sync and sidebar sync. It mirrors botidentity's fail-closed
// lifecycle rules while batching the lookup for these list endpoints.
func queryActiveBotTypes(ctx *config.Context, uids []string) (map[string]string, error) {
	types := make(map[string]string)
	if ctx == nil || len(uids) == 0 {
		return types, nil
	}

	var appUIDs []string
	if _, err := ctx.DB().SelectBySql("SELECT uid FROM app_bot WHERE uid IN ? AND status=1", uids).Load(&appUIDs); err != nil {
		return nil, err
	}
	for _, uid := range appUIDs {
		types[uid] = "app_bot"
	}

	var robots []struct {
		UID        string `db:"robot_id"`
		Kind       string `db:"kind"`
		CreatorUID string `db:"creator_uid"`
	}
	if _, err := ctx.DB().SelectBySql(`SELECT robot_id,kind,creator_uid FROM robot r WHERE robot_id IN ?
		AND (r.kind='user' AND r.status=1 OR `+botpolicy.ActiveAvatarSQL("r")+`)`, uids).Load(&robots); err != nil {
		return nil, err
	}
	for _, row := range robots {
		if _, ambiguous := types[row.UID]; ambiguous {
			return nil, fmt.Errorf("ambiguous active Bot identity for uid %s", row.UID)
		}
		if row.Kind == string(botpolicy.Avatar) {
			types[row.UID] = "avatar"
		} else if row.CreatorUID != "" || spacepkg.IsSystemBot(row.UID) {
			types[row.UID] = "user_bot"
		}
	}
	return types, nil
}
