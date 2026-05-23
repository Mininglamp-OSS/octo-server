// Module-side composer that wires (*Robot).groupService.GetMembers +
// (*Robot).db.exist into the pkg/mentionrewrite.ExpandAisToBotUIDs
// callback shape.
//
// Mininglamp-OSS/octo-server#144: see modules/message/mention_expand.go
// for the design rationale (leaf-package invariant + per-row best-
// effort error handling). The robot ingress uses rb.db.exist directly
// rather than going back through rb.IService.ExistRobot to keep the
// dependency tree obvious — *Robot already owns the robotDB, and the
// IService surface is for cross-module callers.
package robot

import (
	"go.uber.org/zap"
)

// fetchBotMemberUIDs enumerates the bot members of `groupNo` for the
// ExpandAisToBotUIDs chokepoint. See
// (*Message).fetchBotMemberUIDs / (*BotAPI).fetchBotMemberUIDs for
// the shared contract; this version exists to give modules/robot a
// hook free of any cross-module robot.IService indirection (which
// would just delegate to db.exist anyway).
func (rb *Robot) fetchBotMemberUIDs(groupNo string) ([]string, error) {
	members, err := rb.groupService.GetMembers(groupNo)
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
		ok, existErr := rb.db.exist(mem.UID)
		if existErr != nil {
			rb.Warn("ExistRobot lookup failed during mention.ais expansion; treating member as non-bot",
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
