// Module-side composer that wires (*IncomingWebhook).groupService.GetMembers +
// (*IncomingWebhook).robotService.ExistRobot into the
// pkg/mentionrewrite.ExpandAisToBotUIDs callback shape.
//
// See modules/message/mention_expand.go and modules/bot_api/mention_expand.go
// for the design rationale — the leaf helper in pkg/mentionrewrite stays free
// of any modules/* dependency to preserve the import-cycle-free invariant
// called out in pkg/mentionrewrite/rewrite.go, so each ingress chokepoint owns
// its own thin composer. This is the incoming-webhook ingress version; reusing
// the SAME GetMembers + ExistRobot pair guarantees `@所有 AI` from a webhook
// resolves to the identical bot set as the user / bot-API ingresses (parity).
package incomingwebhook

import (
	"go.uber.org/zap"
)

// fetchBotMemberUIDs enumerates the bot members of groupNo for the
// ExpandAisToBotUIDs chokepoint. Contract is identical to
// (*Message).fetchBotMemberUIDs / (*BotAPI).fetchBotMemberUIDs — see those
// files for the best-effort / per-row error rationale: a single failed
// ExistRobot lookup is treated as "not a bot" rather than aborting the whole
// expansion, and a non-nil error degrades the expansion to a no-op upstream
// (pkg/mentionrewrite/expand_ais.go clause 5) instead of dropping the message.
func (w *IncomingWebhook) fetchBotMemberUIDs(groupNo string) ([]string, error) {
	members, err := w.groupService.GetMembers(groupNo)
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
		ok, existErr := w.robotService.ExistRobot(mem.UID)
		if existErr != nil {
			w.Warn("ExistRobot lookup failed during mention.ais expansion; treating member as non-bot",
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
