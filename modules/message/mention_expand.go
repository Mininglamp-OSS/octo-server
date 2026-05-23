// Module-side composer that wires (*Message).groupService.GetMembers
// + (*Message).robotService.ExistRobot into the
// pkg/mentionrewrite.ExpandAisToBotUIDs callback shape.
//
// Mininglamp-OSS/octo-server#144: the leaf helper in
// pkg/mentionrewrite stays free of any modules/* dependency to
// preserve the leaf invariant called out in
// pkg/mentionrewrite/rewrite.go (the historical
// `robot → message → robot` import cycle). Each ingress chokepoint
// owns its own thin composer instead. This is the message-ingress
// version; modules/bot_api/mention_expand.go and the equivalent
// inline helper in modules/robot/api.go are its peers.
package message

import (
	"go.uber.org/zap"
)

// fetchBotMemberUIDs enumerates the bot members of `groupNo` for the
// ExpandAisToBotUIDs chokepoint. Returns an empty slice (no error)
// when the lookup succeeds but the group has no bot members, so the
// helper can no-op cleanly. A non-nil error degrades the expansion
// to a no-op upstream (see pkg/mentionrewrite/expand_ais.go clause 5)
// — we MUST NOT drop the inbound message just because the bot-set
// lookup failed.
//
// Implementation note: a single failed ExistRobot lookup for one
// member is treated as "this member is not a bot" (best effort)
// rather than aborting the whole expansion. Aborting on a transient
// per-row error would silently disable broadcast expansion for an
// entire group whenever one robot row is corrupt — the legacy
// adapter would then miss the @所有 AI, which is the exact bug
// Mininglamp-OSS/octo-server#144 is fixing. Logging at warn keeps
// the failure observable without spamming on the happy path.
func (m *Message) fetchBotMemberUIDs(groupNo string) ([]string, error) {
	members, err := m.groupService.GetMembers(groupNo)
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
		ok, existErr := m.robotService.ExistRobot(mem.UID)
		if existErr != nil {
			m.Warn("ExistRobot lookup failed during mention.ais expansion; treating member as non-bot",
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
