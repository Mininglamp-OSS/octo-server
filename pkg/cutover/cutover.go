// Package cutover is the shared control plane for one-way data-plane
// cutovers: a DB-authoritative singleton state row that selects between a
// legacy and a new allocator/write path, flipped exactly once by an operator
// after validating a watermark ("cutover floor") against observed evidence.
//
// It exists because the same control plane was hand-written three times —
// #627 (internal/msgextraseq, octo_message_extra_version_state), #697
// (pkg/botevent, octo_bot_event_seq_state, whose migration comment says
// "deliberately isomorphic to #627"), and #725/#733 (pkg/auth session
// rollout) — each with its own singleton read, FOR UPDATE compare-and-set
// flip, floor validation, and expected-mode deployment guard. This package
// collects the parts that were literal duplicates. What stays in each domain
// is exactly what differs between them: which evidence sources bound the
// floor, what happens after the flip (e.g. #697's Redis mirror publication),
// and every runtime read path.
//
// The session rollout (#733) is deliberately NOT built on this package: it is
// a five-phase evidence-gated ladder with an automatic reconciler, not a
// two-state flip. It shares the conventions documented in
// docs/cutover-framework.md (singleton table shape, guard-env naming, the
// flip-then-arm ordering invariant) but not this code.
//
// Everything here fails closed: a missing state row refuses to flip, a
// malformed guard value refuses to assert, a floor outside its validated
// bounds refuses to activate. There is deliberately no Unflip — every domain
// using this package documents rollback as a coordinated procedure or forbids
// it outright, because returning to the legacy allocator re-opens the exact
// id-below-live-cursor loss the cutover exists to close.
package cutover

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
)

// SingletonID is the fixed primary key every cutover state table uses. The
// table template (docs/cutover-framework.md) enforces it with a CHECK
// constraint so a second row cannot exist.
const SingletonID = 1

// Mode values shared by every two-state cutover domain. The seeded value is
// inactive so a deploy is inert until an operator flips.
const (
	ModeInactive = 0
	ModeActive   = 1
)

// ErrStateMissing means the domain's singleton state row does not exist —
// either the row is absent or the table itself has not been migrated yet
// (MySQL 1146). The two are collapsed on purpose: both mean "the migration has
// not seeded the authority here", and an operator flip refuses either way. It
// must never be confused with an unreachable database, whose safe answer is the
// opposite once the new allocator has issued ids.
//
// What the collapse does NOT promise is that a domain's runtime reader treats
// the two alike. Each domain owns that: pkg/botevent maps both to its legacy
// path, while internal/msgextraseq's FOR SHARE reader defaults to legacy only
// for a missing row and lets a missing table fail every write closed. Where the
// difference is "writes are flowing" versus "every write is erroring", an
// operator surface has to be able to say which — so ErrStateTableMissing
// carries it, rather than each domain re-deriving it with another query.
var ErrStateMissing = errors.New("cutover: state row is missing")

// ErrStateTableMissing is the subset of ErrStateMissing where the TABLE itself
// is absent (MySQL 1146) rather than merely unseeded.
//
// It wraps ErrStateMissing, so every `errors.Is(err, ErrStateMissing)` caller
// keeps working and no domain has to handle it — but the fact is preserved for
// the ones that care instead of being detected here and thrown away. Deriving
// it at the point where the driver error is already in hand is what stops each
// domain from bolting on its own information_schema probe to recover it.
var ErrStateTableMissing = fmt.Errorf("%w: the table itself does not exist", ErrStateMissing)

// State is a domain's decoded singleton state row.
type State struct {
	// Mode selects the allocator: ModeInactive (legacy) or ModeActive.
	Mode int
	// Epoch counts flips. Domains give it extra meaning (#697 stamps it into
	// the Redis mirror value); here it is only carried and incremented.
	Epoch uint64
	// Floor is the watermark validated at activation: every id the legacy
	// path could still have issued lies at or below it.
	Floor int64
}

// Active reports whether the new path is the authoritative one.
func (s State) Active() bool { return s.Mode == ModeActive }

// ReadState reads a domain's singleton state row under the caller's deadline.
//
// table is interpolated into the statement, so it must be a compile-time
// constant owned by the domain package (msgextraseq.StateTable,
// botevent.StateTable) — never a value derived from configuration or input.
// The same applies to FlipSpec.Table.
// A missing row and a missing table both return ErrStateMissing (see its doc);
// any other failure is surfaced as-is so callers can distinguish "not migrated
// yet" from "authority unreachable" — opposite safe answers post-cutover.
func ReadState(deadline context.Context, db *dbr.Session, table string) (State, error) {
	if db == nil {
		return State{}, errors.New("cutover: nil db session, cannot read state")
	}
	var row struct {
		Mode  int    `db:"mode"`
		Epoch uint64 `db:"epoch"`
		Floor int64  `db:"cutover_floor"`
	}
	count, err := db.SelectBySql(
		"SELECT `mode`, `epoch`, `cutover_floor` FROM `"+table+"` WHERE `singleton_id`=?",
		SingletonID,
	).LoadContext(deadline, &row)
	if err != nil {
		if isMissingTable(err) {
			return State{}, ErrStateTableMissing
		}
		return State{}, fmt.Errorf("cutover: read %s: %w", table, err)
	}
	if count == 0 {
		return State{}, ErrStateMissing
	}
	return State{Mode: row.Mode, Epoch: row.Epoch, Floor: row.Floor}, nil
}

// isMissingTable reports whether err is MySQL's "table doesn't exist" (1146),
// matched on the driver's error number so it survives a server locale change.
func isMissingTable(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1146
}
