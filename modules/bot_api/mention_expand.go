// Module-side composer that wires bot_api.groupService.GetMembers +
// bot_api.robotService.ExistRobot into the
// pkg/mentionrewrite.ExpandAisToBotUIDs callback shape.
//
// See modules/message/mention_expand.go for the design rationale —
// the leaf helper stays free of any modules/* dependency to preserve
// the import-cycle-free invariant called out in
// pkg/mentionrewrite/rewrite.go. Each ingress chokepoint owns its
// own thin composer.
package bot_api

import (
	"go.uber.org/zap"
)

// fetchBotMemberUIDs enumerates the bot members of `groupNo` for the
// ExpandAisToBotUIDs chokepoint. Contract is identical to
// (*Message).fetchBotMemberUIDs — see that file for the
// best-effort / per-row error rationale.
func (ba *BotAPI) fetchBotMemberUIDs(groupNo string) ([]string, error) {
	members, err := ba.groupService.GetMembers(groupNo)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(members))
	for _, mem := range members {
		if mem == nil || mem.UID == "" {
			continue
		}
		ok, existErr := ba.robotService.ExistRobot(mem.UID)
		if existErr != nil {
			ba.Warn("ExistRobot lookup failed during mention.ais expansion; treating member as non-bot",
				zap.String("group_no", groupNo),
				zap.String("uid", mem.UID),
				zap.Error(existErr))
			continue
		}
		if ok {
			out = append(out, mem.UID)
		}
	}
	return out, nil
}
