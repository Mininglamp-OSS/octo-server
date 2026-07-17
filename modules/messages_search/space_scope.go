package messages_search

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// resolveP2PSpaceScope returns the spaceID that should be applied as a term
// filter to the OS DSL for a p2p (DM) search. The bool return is "continue"
// — false means a response has already been written and the handler must
// abort.
//
// Rules (P0 fix for cross-Space DM disclosure):
//   - channel_type != p2p → ("", true). Group/thread already encode the
//     parent Space in their channel_id (and the membership gate enforces
//     active membership), so we don't add a redundant spaceId filter that
//     would only mask indexer-mapping mismatches.
//   - p2p && spaceID resolved → (spaceID, true). The DSL builder MUST
//     attach the term filter so the search is scoped to that Space.
//   - p2p && spaceID empty && RequireSpaceID=true → fail-closed.
//     respondNotFound (resource=channel) so we don't leak whether the peer
//     exists in any Space, and return false to abort the handler. This gate
//     applies ONLY to space-scoped principals (user / uk); a space-less
//     principal (as-bot / OBO, see Principal.RequiresSpaceScope) proceeds with
//     ("", true) regardless of RequireSpaceID — it has no Space to resolve and
//     its DM reachability is bounded by the per-principal readable predicate,
//     not a spaceId term (YUJ-57).
//   - p2p && spaceID empty && RequireSpaceID=false → ("", true) with a
//     WARN log. Operational escape hatch only; intended for the rollout
//     window before the v1.9 indexer is writing `payload.space_id` and the
//     existing corpus is backfilled. The DSL skips the filter, which means
//     legacy index docs without `spaceId` are visible — accept that risk
//     deliberately by flipping the env, never by accident.
func (h *Handler) resolveP2PSpaceScope(c *wkhttp.Context, channelType uint8, loginUID string) (string, bool) {
	if channelType != channelTypePerson {
		return "", true
	}
	// spaceID 走 principal（决策十）：真人主体等价于 GetSpaceID(c)；bot 路由不挂
	// SpaceMiddleware 故为空；uk 取 api_key_space_id。The principal is the single
	// source so the p2p Space scope stays consistent across all four subjects.
	p := h.principal(c)
	spaceID := p.SpaceID()
	if spaceID != "" {
		return spaceID, true
	}
	// YUJ-57: space-less principals (as-bot / OBO) legitimately carry no Space,
	// so the fail-close gate must not apply to them — otherwise a global search
	// whose scope collapses to a single DM (routed through this p2p gate via the
	// fast path) would 404 for a bot even after resolveGlobalScope let it in.
	// Their DM reachability is bounded by the per-principal readable predicate,
	// not a spaceId term, so we proceed with spaceID="".
	if p.RequiresSpaceScope() {
		if h.cfg.RequireSpaceID {
			respondNotFound(c, "channel")
			return "", false
		}
		h.Warn("messages_search: p2p search without spaceID; OCTO_SEARCH_REQUIRE_SPACE_ID=false escape hatch active",
			zap.String("uid", loginUID))
	}
	return "", true
}
