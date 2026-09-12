package internal_membership

import (
	"context"
	"database/sql"

	"github.com/Mininglamp-OSS/octo-lib/config"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
)

// membershipStore is the narrow data surface these handlers need.
//
// An interface rather than a direct pkg/project call so the handlers' behaviour
// — answer shape, ordering, the non-member/unknown-project folding, the
// fail-closed 500 — is testable without MySQL. Every value crossing this
// boundary is an authorization fact, so "the happy path is only covered by an
// integration suite that needs a database" is not good enough: those are exactly
// the assertions that must run on every change.
//
// Same shape as modules/internal_resolve's identityResolver, for the same
// reason.
type membershipStore interface {
	// Epochs returns member_epoch per ACTIVE project in spaceID. A project
	// absent from the map does not exist, is disbanded, or is in another Space.
	Epochs(ctx context.Context, spaceID string, projectIDs []string) (map[string]int64, error)
	// Memberships returns the project's member_epoch and the role of each uid
	// holding an active seat. Epoch 0 with an empty map means the project is
	// not an active project of spaceID.
	Memberships(ctx context.Context, spaceID, projectID string, uids []string) (int64, map[string]int, error)
}

// dbStore is the production implementation, delegating to pkg/project.
//
// It resolves the session per call rather than caching one at construction:
// config.Context owns the pool's lifecycle, and holding a session across a
// reconnect is how a module ends up querying a closed handle.
type dbStore struct {
	ctx *config.Context
}

// Epochs answers from one bounded, read-only transaction, exactly like Memberships.
//
// # Why it is not two autocommit reads any more
//
// The previous shape ran ProjectEpochsInSpace straight on the process-wide session,
// and the interface comment justified that by saying Epochs "borrows and returns a
// connection each" read, so it does not need Memberships' deadline. That answered the
// wrong half of the question. It covered the HOLD and said nothing about the WAIT: the
// session's Timeout is unset process-wide, so dbr reaches sql.DB.QueryContext with
// context.Background() and the acquisition of each connection was unbounded. This is
// the POLLING half of the peer integration — the half most likely to arrive in a
// burst, with the limiter deliberately sized loose at 20 rps / burst 400 — so it is
// the worse place to leave that open, not the safer one.
//
// A transaction rather than a ctx-bearing read on the session, for two reasons:
//
//   - it bounds both halves with the mechanism already proven next door. BeginTx(ctx)
//     bounds the pool wait and is cancellable when the peer hangs up; tx.Timeout bounds
//     each statement once the connection is held.
//   - ProjectEpochsInSpace issues TWO reads (the octo_project select and then
//     space.IsActiveSpace), and on the session they could straddle a commit — a live
//     project row read beside a Space that has since been disbanded, or the reverse.
//     One repeatable-read snapshot makes the pair describe one instant, which is the
//     same property ProjectMemberships opens a transaction for.
//
// The cost is holding one pooled connection across two point reads instead of
// borrowing it twice. That is strictly less connection time than the two acquisitions
// it replaces whenever the pool is contended, which is the only case that matters.
func (s dbStore) Epochs(ctx context.Context, spaceID string, projectIDs []string) (map[string]int64, error) {
	tx, err := s.ctx.DB().BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, err
	}
	// Rollback rather than commit: nothing here writes, and rolling back releases the
	// read view just as commit would.
	defer tx.RollbackUnlessCommitted()
	tx.Timeout = projectpkg.EpochsReadTimeout()
	return projectpkg.ProjectEpochsInSpace(tx, spaceID, projectIDs)
}

func (s dbStore) Memberships(ctx context.Context, spaceID, projectID string, uids []string) (int64, map[string]int, error) {
	return projectpkg.ProjectMemberships(ctx, s.ctx.DB(), spaceID, projectID, uids)
}
