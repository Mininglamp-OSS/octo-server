package bot_api

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// getMentionPref handles GET /v1/bot/groups/:group_no/mention_pref.
//
// Adapter-facing read of the per-group no-@ decision (octo-server#237 + YUJ-2996).
// robot_id is taken from the authBot context — query is NOT trusted.
// Membership gate mirrors getGroupInfo (groups.go:60-94).
//
// Two permission axes, AND-combined:
//   - no_mention: bot owner intent (bot_mention_pref, no record → 0)
//   - group_allow_no_mention: group-level switch owned by the group
//     creator/manager (group.allow_no_mention, no group row → default 1)
//   - effective = (no_mention==1 && group_allow_no_mention==1)
//
// When effective is false the bot must be @mentioned in this group.
func (ba *BotAPI) getMentionPref(c *wkhttp.Context) {
	robotID := getRobotIDFromContext(c)
	if robotID == "" {
		c.ResponseError(errors.New("robot_id not found"))
		return
	}
	groupNo := c.Param("group_no")

	var count int
	err := ba.db.session.SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		groupNo, robotID,
	).LoadOne(&count)
	if err != nil || count == 0 {
		c.ResponseError(errors.New("bot is not a member of this group"))
		return
	}

	var noMention int
	err = ba.db.session.Select("no_mention").From("bot_mention_pref").
		Where("robot_id=? AND group_no=?", robotID, groupNo).LoadOne(&noMention)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		ba.Error("查询 bot_mention_pref 失败", zap.Error(err),
			zap.String("robot_id", robotID), zap.String("group_no", groupNo))
		c.ResponseError(errors.New("查询失败"))
		return
	}

	// group_allow_no_mention: 群级总开关；无群记录回退默认 1（允许），零回归。
	groupAllow := 1
	err = ba.db.session.Select("allow_no_mention").From("`group`").
		Where("group_no=?", groupNo).LoadOne(&groupAllow)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			groupAllow = 1
		} else {
			ba.Error("查询 group.allow_no_mention 失败", zap.Error(err),
				zap.String("robot_id", robotID), zap.String("group_no", groupNo))
			c.ResponseError(errors.New("查询失败"))
			return
		}
	}

	effective := computeEffectiveNoMention(noMention, groupAllow)

	c.Response(map[string]interface{}{
		"no_mention":             noMention,
		"group_allow_no_mention": groupAllow,
		"effective":              effective,
	})
}

// computeEffectiveNoMention AND-combines the two permission axes: the bot
// owner's intent (noMention) and the group-level switch (groupAllow). The bot
// may answer without an @mention only when BOTH are 1. Extracted as a pure
// function so the 4-combination truth table is unit-testable without a DB.
func computeEffectiveNoMention(noMention, groupAllow int) bool {
	return noMention == 1 && groupAllow == 1
}
