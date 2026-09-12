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
// actually matched holds.
//
// Resolving FIRST is not merely more expensive, which is what this comment used to
// say. It would break an argument documented in another module. The SELECT below is
// plain and non-locking, so on the current ordering it is not the transaction's first
// consistency read on any of these paths — the seat UPDATE has already run. That is
// exactly what leg 2 of modules/project/db.go's read-view note depends on: under
// REPEATABLE READ the consistent-read view is assigned at the first CONSISTENCY read,
// and every statement before this one on the removal path is a locking read or a DML.
//
// Move the resolve ahead of the write and, on the two doors whose first statement IS
// the seat UPDATE (modules/space/db.go reactivateMember, and
// atomicReactivateMemberIfNotFullOnce whenever maxUsers == 0 — the normal case for an
// unlimited Space), the read view gets pinned before any lock is taken.
// bumpMemberEpochForSpaceMemberTx's enumeration would then be able to miss a
// concurrently-committed admission, bump nothing, and leave a cached denial agreeing
// with an unchanged epoch.
//
// Resolving after also avoids re-opening the question of what to do when the resolve
// misses but the write would have hit.
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
	// BOTH identity columns, not just the uid.
	//
	// A seat row is identified by a PAIR, and the epoch step matches on the pair:
	// `WHERE space_id = ? AND uid = ?` against octo_project_member. Canonicalising one
	// column and passing the caller's bytes for the other leaves the signal exactly as
	// dead — the enumeration finds nothing, member_epoch does not move, and the outbox
	// row carries the drifted spelling so the async cascade completes as a successful
	// no-op. The first version of this function returned the caller's space_id verbatim
	// and was reviewed twice before that was caught, because the door matrix drifted
	// only the uid and was therefore blind to the other axis by construction.
	//
	// Nothing about the space_id makes it safer than the uid. It is a server-generated
	// identifier, but the CALLER supplies it on every remove/kick/leave/re-invite
	// request, and every ASCII character an id can contain has a fullwidth form that
	// utf8mb4_0900_ai_ci folds and utf8mb4_general_ci does not — measured on MySQL
	// 8.0.33 for digits, hex letters and the underscore alike. So a drifted space_id
	// passes the Space middleware (which joins space_member and space, both loose),
	// flips the seat, and reaches here.
	var stored []struct {
		SpaceID string `db:"space_id"`
		UID     string `db:"uid"`
	}
	if _, err := tx.SelectBySql(
		"SELECT space_id, uid FROM space_member WHERE space_id = ? AND uid = ?",
		spaceID, callerUID,
	).Load(&stored); err != nil {
		return SeatRef{}, fmt.Errorf("space: resolve seat identity: %w", err)
	}
	if len(stored) == 0 {
		return SeatRef{}, fmt.Errorf("%w: space_id=%s", ErrSeatNotFound, spaceID)
	}
	// More than one row is impossible through the unique index on (space_id, uid) —
	// under EITHER collation, since both are case- and accent-insensitive and the
	// index enforces its own. Taking the first is therefore not a choice.
	return SeatRef{spaceID: stored[0].SpaceID, uid: stored[0].UID}, nil
}

// SpaceRef is a `space` row identified by the bytes the DATABASE stores.
//
// SeatRef's argument, one table over. The two Space-DISBAND doors are the only
// removal-outbox producers that do not have a seat to resolve — they enqueue for the
// whole roster at once — so they took the space_id as a plain string, and both of
// them passed `c.Param("space_id")` straight through.
//
// The consequence is the same one SeatRef exists to prevent, arrived at from the
// other column. The `uid` on those outbox rows IS canonical (lockActiveMemberUIDsTx
// reads it out of space_member); the space_id was not. The async cascade then queries
// octo_project_member on the PAIR, under utf8mb4_general_ci, which does not fold the
// fullwidth forms utf8mb4_0900_ai_ci matched on the way in — so it enumerates zero
// project seats, marks the job `done`, and every project seat in that Space stays
// status = 1 forever, with the I1 scan reporting a violation nothing can repair.
//
// It grants nothing — the actor is disbanding their own Space, and both the Space
// middleware and ProjectEpochsInSpace's IsActiveSpace fold deny against a disbanded
// Space — so the cost is orphaned rows and permanent reconcile noise rather than an
// escalation. It is here as a TYPE anyway, rather than as two rebinds, because two
// rebinds is what rounds 13 through 15 tried three times: fix the doors you can see,
// and the guard stays blind to the door you cannot. Both spelling guards missed these
// two by construction — one scans checkSpaceActive call sites and disbandSpace has
// none, the other drives a table of manager doors that forceDisband is not in.
//
// There is no conversion from string, so the next batch-enqueue door does not
// compile until it has resolved the row.
type SpaceRef struct {
	spaceID string
}

// SpaceID is the space_id exactly as the `space` row stores it.
func (r SpaceRef) SpaceID() string { return r.spaceID }

// IsZero reports whether this ref was never resolved.
func (r SpaceRef) IsZero() bool { return r.spaceID == "" }

// ErrSpaceRowNotFound means the `space` row a disband had just written could not be
// read back in the same transaction.
var ErrSpaceRowNotFound = errors.New("space: space row not found for canonicalisation")

// ResolveSpaceIDTx reads a Space's canonical space_id back inside the caller's
// transaction.
//
// # Call it while the row is already locked
//
// The read is a LOCKING one, and both properties matter:
//
//   - it must not be the transaction's first CONSISTENCY read. Under REPEATABLE READ
//     that is where the read view gets assigned, and the disband transactions do
//     plain SELECTs nowhere after this point today — but a helper is called by code
//     that has not been written yet. FOR UPDATE assigns no view.
//   - it must add no lock-order edge. Both callers have just executed
//     `UPDATE space ... WHERE space_id = ?`, so the X lock on this row is already
//     held and this statement waits for nothing. A caller that has NOT locked the row
//     first would be introducing a new edge into a graph that three review rounds
//     found cycles in — so do not call it from anywhere else without redoing that
//     analysis.
//
// A miss is an error. This is only called after a write that was supposed to match
// the row, so zero rows means it vanished under a lock this transaction holds.
// Rolling back leaves the Space undisbanded, which is recoverable; committing the
// disband with a roster of poisoned outbox rows is not.
func ResolveSpaceIDTx(tx *dbr.Tx, spaceID string) (SpaceRef, error) {
	if spaceID == "" {
		return SpaceRef{}, fmt.Errorf("%w: empty space_id", ErrSpaceRowNotFound)
	}
	var stored []string
	if _, err := tx.SelectBySql(
		"SELECT space_id FROM space WHERE space_id = ? FOR UPDATE", spaceID,
	).Load(&stored); err != nil {
		return SpaceRef{}, fmt.Errorf("space: resolve space identity: %w", err)
	}
	if len(stored) == 0 {
		return SpaceRef{}, fmt.Errorf("%w: space_id=%s", ErrSpaceRowNotFound, spaceID)
	}
	return SpaceRef{spaceID: stored[0]}, nil
}
