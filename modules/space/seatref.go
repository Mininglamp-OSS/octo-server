package space

import (
	"errors"
	"fmt"

	"github.com/gocraft/dbr/v2"
)

// SeatRef is a `space_member` row identified by the bytes the DATABASE stores.
//
// # Why this is a type and not a pair of strings
//
// Everything downstream of a seat transition — the synchronous tx steps, the removal
// outbox, and through the outbox the async cascade — matches this identifier against
// a table with a DIFFERENT collation. `octo_project_member` is pinned
// utf8mb4_general_ci by every migration in this repo; `space_member` is dump-imported
// and sits at utf8mb4_0900_ai_ci in production. Those equivalence classes are not the
// same, and the gap is not symmetric: 0900_ai_ci (UCA 9.0.0 primary weights) folds
// compatibility decompositions to their base letter, general_ci does not.
//
// Measured on MySQL 8.0.33 with both production collations, stored `alice`: U+FF41
// FULLWIDTH a, U+212A KELVIN SIGN, U+2126 OHM SIGN and U+00AA FEMININE ORDINAL all
// match under 0900_ai_ci and do NOT match under general_ci. (Accents match under
// both, and ASCII case matches under both — those are NOT instances, though earlier
// rounds of this branch asserted they were.)
//
// So a caller's spelling can flip a Space seat and then enumerate nothing on the
// project side: the seat moves, member_epoch does not, and a peer's cached decision
// keeps agreeing with an epoch that never changed. In the removal direction that is a
// revoked authorization surviving without bound, and the async cascade is not a
// backstop because the outbox row carries the same drifted spelling and completes
// successfully as a no-op.
//
// The previous four fixes in this class each moved ONE hop of the decision into the
// database — a name, then an affected-rows convention, then a locking read, then a
// predicated write — and each left the next hop in Go. A struct with unexported
// fields ends the sequence rather than advancing it: there is no conversion from
// `string`, so a caller who has not resolved the identifier against `space_member`
// cannot construct one and the code does not compile. That property is what the
// source guards could not express, because the failure mode was always a call site
// nobody had thought of yet.
type SeatRef struct {
	spaceID string
	uid     string
}

// SpaceID is the Space this seat belongs to.
func (r SeatRef) SpaceID() string { return r.spaceID }

// UID is the uid exactly as `space_member` stores it.
func (r SeatRef) UID() string { return r.uid }

// IsZero reports whether this ref was never resolved.
func (r SeatRef) IsZero() bool { return r.spaceID == "" || r.uid == "" }

// ErrSeatNotFound means the row a write had just matched could not be read back in
// the same transaction.
var ErrSeatNotFound = errors.New("space: seat row not found for canonicalisation")

// ResolveSeatTx reads a seat's canonical uid back inside the caller's transaction.
//
// # Call it AFTER the write, not before
//
// The point is not to validate the uid — it is to learn which bytes the row the write
// actually matched holds. Resolving first and writing with the resolved spelling would
// work too, but it costs an extra statement on paths that already have the row locked
// and it re-opens the question of what to do when the resolve misses but the write
// would have hit.
//
// # Status is deliberately not in the predicate
//
// Callers use this on both transitions: after a removal the row reads status = 0,
// after a reactivation it reads 1. Filtering on either would make the helper wrong for
// half its callers in a way that fails OPEN — an unresolvable seat would look like "no
// seat" rather than an error.
//
// # A miss is an error, and that is the safe direction
//
// This is only ever called where a write has just reported affecting a row, so zero
// rows here means the row vanished under a lock the transaction is still holding —
// i.e. an invariant violation, not a normal outcome. Returning an error rolls the
// transition back, which leaves the seat as it was and the peer's cached decision
// still correct. Falling back to the caller's spelling would instead commit the seat
// change and skip the invalidation, which is exactly the defect this type exists to
// make unrepresentable.
func ResolveSeatTx(tx *dbr.Tx, spaceID, callerUID string) (SeatRef, error) {
	if spaceID == "" || callerUID == "" {
		return SeatRef{}, fmt.Errorf("%w: empty space_id or uid", ErrSeatNotFound)
	}
	var stored []string
	if _, err := tx.SelectBySql(
		"SELECT uid FROM space_member WHERE space_id = ? AND uid = ?", spaceID, callerUID,
	).Load(&stored); err != nil {
		return SeatRef{}, fmt.Errorf("space: resolve seat uid: %w", err)
	}
	if len(stored) == 0 {
		return SeatRef{}, fmt.Errorf("%w: space_id=%s", ErrSeatNotFound, spaceID)
	}
	// More than one row is impossible through the unique index on (space_id, uid) —
	// under EITHER collation, since both are case- and accent-insensitive and the
	// index enforces its own. Taking the first is therefore not a choice.
	return SeatRef{spaceID: spaceID, uid: stored[0]}, nil
}
