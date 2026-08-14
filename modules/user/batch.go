package user

import (
	"errors"
	"fmt"
)

// MaxBatchUserUIDs caps the number of uids a single batch user-info request may
// carry. The batch endpoints exist to fold the per-uid anti-ghost-member lookups
// docs-backend used to issue (one call per group member + its bot) into a single
// request; the cap bounds the wrapped `IN (...)` query and the response size
// while staying well above the largest realistic group fan-out.
const MaxBatchUserUIDs = 1000

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
)

// BatchUsersRequest is the request body for the batch user-info endpoints.
type BatchUsersRequest struct {
	UIDs []string `json:"uids"`
}

// BatchUserDTO is the minimal, PII-free projection returned for a resolved user.
// It deliberately carries only identity-resolution fields — uid + status, plus
// the optional display name and robot flag. The contact/profile fields the full
// user.Resp exposes (phone / email / zone / avatar / timestamps / settings) are
// never included: these endpoints resolve target identities in bulk, they do not
// disclose profiles.
type BatchUserDTO struct {
	UID    string  `json:"uid"`
	Status int     `json:"status"`
	Name   *string `json:"name,omitempty"`
	Robot  *int    `json:"robot,omitempty"`
}

// BatchUsersResponse is the batch user-info response. Both slices preserve the
// request's uid order; every requested uid appears in exactly one of them.
type BatchUsersResponse struct {
	Users       []BatchUserDTO `json:"users"`
	MissingUIDs []string       `json:"missing_uids"`
}

// ValidateBatchUIDs enforces the batch request contract: a non-empty list of at
// most MaxBatchUserUIDs non-empty, unique uids. Returns one of the sentinels
// above so callers can branch (e.g. cap overflow → a limit-exceeded envelope).
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
// the enabled-only + request-order contract is enforced: it rebuilds the caller's
// order and drops any uid that has no row or whose user is not StatusEnable,
// reporting those in MissingUIDs. Treating a non-enabled user as missing mirrors
// the single-user GetUser gate (service.go) so a disabled/blacklisted account is
// never reported present. Callers must ValidateBatchUIDs first (this function
// assumes uids are already de-duplicated).
func BuildBatchUsersResponse(uids []string, resps []*Resp) *BatchUsersResponse {
	enabled := make(map[string]*Resp, len(resps))
	for _, r := range resps {
		if r == nil || r.Status != StatusEnable.Int() {
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
		name := r.Name
		robot := r.Robot
		out.Users = append(out.Users, BatchUserDTO{
			UID:    r.UID,
			Status: r.Status,
			Name:   &name,
			Robot:  &robot,
		})
	}
	return out
}
