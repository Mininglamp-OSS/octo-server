package user

import (
	"errors"
	"fmt"
)

// MaxBatchUserUIDs caps the number of uids a single batch user-info request may
// carry. The batch endpoints exist to fold the per-uid anti-ghost-member lookups
// docs-backend used to issue (one call per group member + its bot) into a single
// request; the cap bounds the wrapped `IN (...)` query and the response size.
//
// The value is deliberately kept close to the largest realistic group fan-out
// (a ~38-member group plus its per-member bots is ~76 uids; this leaves headroom
// to roughly hundred-member groups) rather than a round, generous 1000. It is
// load-bearing for rate limiting: POST /v1/users/batch shares the process-level
// per-uid token bucket (`ratelimit:uid:{uid}`, see pkg/wkhttp.SharedUIDRateLimiter)
// with GET /v1/users/:uid, and that bucket charges a flat 1 token per request
// regardless of how many uids a batch resolves. A true per-uid-weighted charge
// (N tokens for N uids) would require hand-rolling a Redis counter, which
// .octospec/rules/rate-limit.md (load_bearing) forbids ("Do not hand-roll a Redis
// counter for generic HTTP throttling"), and the shared middleware exposes no
// allow-N primitive. The rule-compliant lever is therefore this cap: bounding it
// caps the identity-resolution amplification a single token can buy. Callers with
// larger fan-outs chunk their uid list across requests.
const MaxBatchUserUIDs = 200

// MaxBatchUIDLength bounds the length of any single uid in a batch request. Real
// uids are 32-hex; prefixed synthetic forms (app_<...>_bot, iwh_<...>) and the
// space-scoped s<digits>_<baseUID> form the bot leg strips stay comfortably under
// this. It is defense-in-depth so a single pathological multi-KB string cannot
// bloat the wrapped `IN (...)` query even within the request body cap.
const MaxBatchUIDLength = 64

// Batch-request validation sentinels. Each surface (human vs bot) maps these to
// its own localized error code, so the wire envelope stays per-surface while the
// rule itself lives in one place.
var (
	// ErrBatchUIDsEmpty — uids missing or empty.
	ErrBatchUIDsEmpty = errors.New("uids 不能为空")
	// ErrBatchUIDsTooMany — more than MaxBatchUserUIDs uids.
	ErrBatchUIDsTooMany = fmt.Errorf("uids 数量不能超过 %d", MaxBatchUserUIDs)
	// ErrBatchUIDEmpty — the list contains an empty-string uid.
	ErrBatchUIDEmpty = errors.New("uids 不能包含空字符串")
	// ErrBatchUIDDuplicate — the list contains a duplicate uid.
	ErrBatchUIDDuplicate = errors.New("uids 不能包含重复项")
	// ErrBatchUIDTooLong — a uid exceeds MaxBatchUIDLength.
	ErrBatchUIDTooLong = fmt.Errorf("单个 uid 长度不能超过 %d", MaxBatchUIDLength)
)

// BatchUsersRequest is the request body for the batch user-info endpoints.
type BatchUsersRequest struct {
	UIDs []string `json:"uids"`
}

// BatchUserDTO is the minimal, PII-free projection returned for a resolved user.
// It deliberately carries only identity-resolution fields — uid + status, plus
// the display name and robot flag. The contact/profile fields the full
// user.Resp exposes (phone / email / zone / avatar / timestamps / settings) are
// never included: these endpoints resolve target identities in bulk, they do not
// disclose profiles.
//
// Name and Robot are plain values, always emitted. An earlier revision typed them
// as *string / *int with `omitempty`, but the projection always set them to a
// non-nil address, so `omitempty` never fired and the fields were emitted
// unconditionally anyway. Plain fields make that always-present contract explicit
// (callers doing anti-ghost-member checks want the robot flag on every row) with
// identical wire output.
type BatchUserDTO struct {
	UID    string `json:"uid"`
	Status int    `json:"status"`
	Name   string `json:"name"`
	Robot  int    `json:"robot"`
}

// BatchUsersResponse is the batch user-info response. Both slices preserve the
// request's uid order; every requested uid appears in exactly one of them.
type BatchUsersResponse struct {
	Users       []BatchUserDTO `json:"users"`
	MissingUIDs []string       `json:"missing_uids"`
}

// ValidateBatchUIDs enforces the batch request contract: a non-empty list of at
// most MaxBatchUserUIDs non-empty, unique uids, each at most MaxBatchUIDLength
// long. Returns one of the sentinels above so callers can branch (e.g. cap
// overflow → a limit-exceeded envelope).
//
// Duplicates are rejected outright (ErrBatchUIDDuplicate → 400) rather than
// silently de-duplicated. A caller assembling a "members + their bots" list can
// repeat a uid, but coalescing it server-side would hide a caller bug and make
// the response's uid count diverge from the request's; surfacing the duplicate
// keeps the contract strict and debuggable. Callers dedupe client-side.
func ValidateBatchUIDs(uids []string) error {
	if len(uids) == 0 {
		return ErrBatchUIDsEmpty
	}
	if len(uids) > MaxBatchUserUIDs {
		return ErrBatchUIDsTooMany
	}
	seen := make(map[string]struct{}, len(uids))
	for _, uid := range uids {
		if uid == "" {
			return ErrBatchUIDEmpty
		}
		if len(uid) > MaxBatchUIDLength {
			return ErrBatchUIDTooLong
		}
		if _, dup := seen[uid]; dup {
			return ErrBatchUIDDuplicate
		}
		seen[uid] = struct{}{}
	}
	return nil
}

// BuildBatchUsersResponse projects the rows returned by IService.GetUsers into
// the minimal batch DTO. GetUsers applies no status filter and returns rows in
// DB order (see service.go GetUsers → db.queryByUIDs), so this wrapper is where
// the liveness + request-order contract is enforced: it rebuilds the caller's
// order and drops any uid that has no row or whose account is not live,
// reporting those in MissingUIDs.
//
// Liveness is the repo's two-part predicate — Status == StatusEnable AND
// IsDestroy == IsDestroyNo (mirrors dashboardReaderGrantEligible in
// api_manager.go and IsBindable in oidc_bind.go). A destroyed account can still
// carry status=1 mid-teardown, so gating on Status alone would surface it as
// present; the IsDestroy check closes that so a destroyed uid always lands in
// MissingUIDs.
//
// This liveness gate is STRICTER than the human single-user read
// (GET /v1/users/:uid → GetUserDetail), which applies no status/destroy filter
// and will return a disabled or destroyed account. It matches the bot
// single-user read (GET /v1/bot/user/info → GetUser), which rejects any
// non-enabled account. Treating non-live accounts as missing is intentional:
// these batch endpoints must never let a caller confirm a ghost/destroyed
// member's identity.
//
// DB-only resolution (deliberate, see P2 decisions): GetUsers reads the user
// table directly and does NOT replicate the single-read synthetic fast paths —
// app_*_bot registry resolution (service.go GetUser → resolveAppBot) or iwh_
// webhook synthetic senders (api.go get → resolveWebhookChannel). Published app
// bots are dual-written to the user table so they resolve here in the common
// case; iwh_ synthetic senders are single-lookup constructs with no bulk
// use case. A caller needing those synthetic identities uses the single-read
// endpoint. Callers must ValidateBatchUIDs first (this function assumes uids are
// already de-duplicated).
func BuildBatchUsersResponse(uids []string, resps []*Resp) *BatchUsersResponse {
	enabled := make(map[string]*Resp, len(resps))
	for _, r := range resps {
		if r == nil || r.Status != StatusEnable.Int() || r.IsDestroy != IsDestroyNo {
			continue
		}
		enabled[r.UID] = r
	}
	out := &BatchUsersResponse{
		Users:       make([]BatchUserDTO, 0, len(uids)),
		MissingUIDs: make([]string, 0),
	}
	for _, uid := range uids {
		r, ok := enabled[uid]
		if !ok {
			out.MissingUIDs = append(out.MissingUIDs, uid)
			continue
		}
		out.Users = append(out.Users, BatchUserDTO{
			UID:    r.UID,
			Status: r.Status,
			Name:   r.Name,
			Robot:  r.Robot,
		})
	}
	return out
}
