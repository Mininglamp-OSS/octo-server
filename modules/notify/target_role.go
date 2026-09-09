package notify

import (
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"go.uber.org/zap"
)

// Role-targeted delivery for POST /v1/internal/notify.
//
// # Why this exists
//
// The plugin-review approval card has to reach a Space's owners/admins. The
// first design gave octo-marketplace a cross-tenant roster endpoint
// (GET /v1/internal/spaces/:space_id/admins) so it could name the recipients
// itself. That endpoint leaked verified legal names across tenants and turned a
// single shared token into a Space-existence oracle; it has been deleted (see
// modules/space/api_internal.go).
//
// The fix is to stop moving the roster at all. octo-server already owns
// space_member and already delivers the card, so a producer can say "deliver to
// this Space's admins" and never learn who they are before the fact. It learns
// afterwards only which uids were actually delivered to — which it needs
// anyway, and which is strictly less than the roster (no names, no roles, no
// members who were filtered out for reasons other than delivery).
//
// # Who is allowed to use it
//
// target_role is NOT available to every holder of an internal notify
// credential. POST /v1/internal/notify admits three credential classes
// (internalAuthMiddleware): the legacy NOTIFY_INTERNAL_TOKEN, the docs
// OCTO_DOCS_NOTIFY_TOKEN, and the per-route action tokens loaded from
// OCTO_CARD_ACTION_ROUTES. Only the third — the action capability that carries
// the marketplace plugin-review ApprovalCard — may set target_role.
//
// The reason is that `delivered` is an answer about who the Space's admins are.
// Handing that to the legacy or docs credential would re-create, on a different
// endpoint, exactly the cross-tenant roster capability this change deleted: one
// shared token, up to 200 admin uids per call, any space_id. The gate lives in
// notifyCapabilityAllows (api.go) alongside the existing Card / DocsCard /
// ApprovalCard capability rules, because "which credential may ask for what" is
// one question and it should have one answer site.
//
// # Backward compatibility
//
// This is purely additive. Every existing producer (docs-backend per
// .octospec/tasks/card-message-internal-dispatch/docs-notify-contract.md, the
// smart-summary card pilot per summary-notify-contract.md, and the generic
// action-card producers) sends `targets` and no `target_role`. For those
// requests validateTargeting takes the same branch the old
// binding:"required" tag did, resolveTargets is a no-op, and every downstream
// path (dedup, actor exclusion, memberCache.verify, the 200 cap, the
// delivered/filtered response) runs on the identical slice. There is no
// in-repo constructor of NotifyReq outside tests, so the wire contract is the
// whole compatibility surface.

const (
	// TargetRoleSpaceAdmin selects the Space's active owners and admins
	// (space_member status=1 AND role>=1) of an active Space, robots excluded.
	// It is the only accepted target_role value; an unknown value is a 400
	// rather than a silent fallback, so a typo can never widen or narrow the
	// audience unnoticed.
	TargetRoleSpaceAdmin = "space_admin"

	// maxNotifyTargets is the per-request recipient cap already enforced on the
	// explicit `targets` path (deliverNotification, deliverCardNotification,
	// deliverDocsCardNotification, deliverApprovalCardNotification all reject
	// len(Targets) > 200). Role-resolved recipients are held to the same
	// ceiling, but TRUNCATED rather than rejected: the caller does not choose
	// the size of a Space's admin set, so failing a legitimate notification
	// because an org has 201 admins would be a denial of service the producer
	// cannot fix. Truncation is logged (see resolveRoleTargets).
	maxNotifyTargets = 200
)

// errTargetingInvalid is returned by validateTargeting. It is mapped to
// errcode.ErrSharedParamInvalid at the HTTP boundary; the message is for logs
// only and never reaches a client.
var errTargetingInvalid = errors.New("notify: exactly one of targets / target_role must be set")

// roleTargeted reports whether the request asks octo-server to resolve its own
// recipients. Single definition so the capability gate
// (notifyCapabilityAllows), the pre-resolution validation in sendNotify and
// resolveRoleTargets can never disagree about what "this request uses
// target_role" means — they trim identically.
//
// /notify/batch deliberately does NOT use this. Its rule is "an entry carrying
// the field at all is refused", so it compares the raw value: a whitespace-only
// "   " would be trimmed to "unset" here and slip past a rejection that exists
// precisely to keep role resolution off the batch path. See sendNotifyBatch.
func roleTargeted(req *NotifyReq) bool {
	return req != nil && strings.TrimSpace(req.TargetRole) != ""
}

// validateTargeting enforces "exactly one of targets / target_role", plus the
// role vocabulary.
//
// Hand-rolled rather than expressed with binding tags: gin's validator has no
// clean way to say "exactly one of these two", and `required_without` pairs
// would still let a caller set both. Both-set is rejected rather than
// resolved by precedence — a producer that sends both has a bug, and silently
// honouring one of them is how the wrong people get an approval card.
func validateTargeting(req *NotifyReq) error {
	if req == nil {
		return errTargetingInvalid
	}
	role := strings.TrimSpace(req.TargetRole)
	hasTargets := len(req.Targets) > 0
	hasRole := role != ""

	switch {
	case hasTargets && hasRole:
		return errTargetingInvalid
	case !hasTargets && !hasRole:
		return errTargetingInvalid
	case hasRole && role != TargetRoleSpaceAdmin:
		return errTargetingInvalid
	default:
		return nil
	}
}

// resolveRoleTargets materializes req.Targets from the role selector, in place,
// so every downstream delivery path keeps operating on a concrete uid slice and
// needs no changes at all.
//
// It returns handled=true when the request is complete and the caller should
// respond immediately with resp — that is the zero-admin case, which must be a
// SUCCESS with an empty `delivered`, not an error: "this Space currently has no
// human admins" is a legitimate state of the world, and turning it into a 500
// would make the producer retry forever.
//
// Contract details worth keeping:
//   - Recipients come from space.ActiveAdminUIDs, the single owner of the
//     space_member predicates (status=1, role>=1, INNER JOIN space ON
//     s.status=1, robots excluded). modules/notify does not restate them.
//   - The actor is excluded BEFORE the cap is applied, not after. The delivery
//     paths drop req.ActorUID themselves, so truncating first would spend one
//     of the 200 slots on a uid that is then thrown away — a Space with 201
//     admins whose 200th-or-earlier admin triggered the notification would get
//     199 deliveries while an eligible 201st admin sat just past the cut. The
//     over-fetch of one row exists precisely so removing the (at most one)
//     actor cannot shrink the delivered set.
//   - Over-fetch by one so hitting the cap is detectable, then truncate with a
//     WARN naming the space_id. Silent truncation on a notification fan-out is
//     how "the approver never got the card" becomes unexplainable.
//   - The zero-admin WARN also names the space_id, per the same reasoning: the
//     producer sees `delivered: []` and cannot tell why on its own.
func (n *Notify) resolveRoleTargets(req *NotifyReq) (resp *NotifyResp, handled bool, err error) {
	if !roleTargeted(req) {
		return nil, false, nil
	}
	uids, queryErr := space.ActiveAdminUIDs(n.db, req.SpaceID, maxNotifyTargets+1)
	if queryErr != nil {
		return nil, false, queryErr
	}
	// Actor exclusion first — see the contract note above. Doing it here rather
	// than relying solely on the delivery path also keeps the truncation WARN
	// honest: `resolved` now counts recipients that could actually be delivered
	// to, which is what an operator reading the log is trying to learn.
	if req.ActorUID != "" {
		eligible := make([]string, 0, len(uids))
		for _, uid := range uids {
			if uid != req.ActorUID {
				eligible = append(eligible, uid)
			}
		}
		uids = eligible
	}
	if len(uids) > maxNotifyTargets {
		n.Warn("notify role targeting truncated at policy limit",
			zap.String("space_id", req.SpaceID),
			zap.String("target_role", req.TargetRole),
			zap.Int("limit", maxNotifyTargets),
			zap.Int("resolved", len(uids)))
		uids = uids[:maxNotifyTargets]
	}
	if len(uids) == 0 {
		n.Warn("notify role targeting resolved to zero recipients",
			zap.String("space_id", req.SpaceID),
			zap.String("target_role", req.TargetRole),
			zap.String("service", req.Service),
			zap.String("event", req.Event))
		// Empty-but-non-nil for both, matching the shape every other
		// zero-recipient return in this package uses, so a consumer decoding
		// `delivered` never gets a JSON null. An empty `filtered` alongside an
		// empty `delivered` is how the producer distinguishes "no admins" from
		// "every admin was filtered out".
		return &NotifyResp{Delivered: []string{}, Filtered: map[string]string{}}, true, nil
	}
	req.Targets = uids
	return nil, false, nil
}
