package internal_membership

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/gocraft/dbr/v2"
)

// membershipStore is the narrow data surface these handlers need.
//
// An interface rather than a direct pkg/project call so the handlers' behavior —
// request-mode selection, response projection and fail-closed errors — is testable
// without MySQL. Database facts remain in pkg/project and are covered by its
// real-database HTTP tests.
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
	// UserProjects returns every activated project of spaceID that uid holds an
	// active seat in, with uid's role, ordered by the database row id
	// (`octo_project.id DESC`, not project_id).
	//
	// A successful empty answer must be a non-nil slice: the handler writes the
	// slice straight to the wire, and `null` would read as a failed lookup to a
	// caller whose contract says "no rows is []".
	UserProjects(ctx context.Context, spaceID, uid string) ([]projectpkg.UserProject, error)
	// ProjectMetadata returns the active, activated Project rows in spaceID whose
	// IDs match the request. Role is nil because no user membership was queried.
	ProjectMetadata(ctx context.Context, spaceID string, projectIDs []string) ([]projectpkg.ProjectMetadata, error)
	// Lifecycle returns the minimal active Project facts in the required active
	// Space scope. Archived Projects remain queryable.
	Lifecycle(ctx context.Context, spaceID, projectID string) (*projectpkg.ProjectLifecycle, error)
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

// UserProjects delegates to the read-only fact layer.
//
// Unlike Epochs, the transaction lives inside pkg/project rather than here: the
// qualification reads (account, Space, Space seat) and the project list must share
// one repeatable-read snapshot, and keeping them in one function is what makes it
// impossible to call the list against a different connection than the checks that
// gate it. See projectpkg.UserProjectsInSpace.
//
// The request's context is passed through, so a peer that hangs up releases the
// pooled connection instead of holding it to the five-second statement deadline.
func (s dbStore) UserProjects(ctx context.Context, spaceID, uid string) ([]projectpkg.UserProject, error) {
	return projectpkg.UserProjectsInSpace(ctx, s.ctx.DB(), spaceID, uid)
}

// ProjectMetadata delegates to the read-only metadata fact layer.
func (s dbStore) ProjectMetadata(ctx context.Context, spaceID string, projectIDs []string) ([]projectpkg.ProjectMetadata, error) {
	return projectpkg.ProjectMetadataInSpace(ctx, s.ctx.DB(), spaceID, projectIDs)
}

// Lifecycle delegates to the scoped lifecycle fact layer.
func (s dbStore) Lifecycle(ctx context.Context, spaceID, projectID string) (*projectpkg.ProjectLifecycle, error) {
	return projectpkg.ProjectLifecycleInSpace(ctx, s.ctx.DB(), spaceID, projectID)
}

const identityQueryTimeoutSeconds = 3

var ErrIdentityNotFound = errors.New("identity unavailable in Space")

type identityReader interface {
	Resolve(context.Context, IdentityRequest) (*IdentityContext, error)
}

type dbIdentityReader struct {
	ctx     *config.Context
	session *dbr.Session
}

// Resolve reads user/Space first, then an optional BF Bot in the same bounded
// snapshot. Indexable predicates precede byte-exact ID comparisons.
func (r *dbIdentityReader) Resolve(ctx context.Context, in IdentityRequest) (*IdentityContext, error) {
	session := r.session
	if session == nil && r.ctx != nil {
		session = r.ctx.DB()
	}
	if session == nil {
		return nil, errors.New("identity database unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, identityQueryTimeoutSeconds*time.Second)
	defer cancel()
	tx, err := session.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin identity snapshot: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	const userSpaceQuery = `
SELECT u.uid AS user_id, u.name AS user_name,
       s.space_id, s.name AS space_name, sm.role AS member_role
FROM user u
JOIN space s ON s.space_id = ? AND BINARY s.space_id = BINARY ? AND s.status = 1
JOIN space_member sm ON sm.space_id = ? AND BINARY sm.space_id = BINARY s.space_id
    AND sm.uid = ? AND BINARY sm.uid = BINARY u.uid AND sm.status = 1
WHERE u.uid = ? AND BINARY u.uid = BINARY ? AND u.status = 1 AND u.is_destroy <> 2`
	var row struct {
		UserID     string `db:"user_id"`
		UserName   string `db:"user_name"`
		SpaceID    string `db:"space_id"`
		SpaceName  string `db:"space_name"`
		MemberRole int    `db:"member_role"`
	}
	err = tx.SelectBySql(userSpaceQuery,
		in.SpaceID, in.SpaceID, in.SpaceID, in.UserID, in.UserID, in.UserID,
	).LoadOneContext(ctx, &row)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, ErrIdentityNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read user and Space: %w", err)
	}
	if row.MemberRole < 0 || row.MemberRole > 2 {
		return nil, errors.New("inconsistent identity membership")
	}
	out := &IdentityContext{
		User:       IdentityUser{UserID: row.UserID, Name: row.UserName},
		Space:      IdentitySpace{SpaceID: row.SpaceID, Name: row.SpaceName},
		Membership: IdentityMembership{Role: row.MemberRole},
	}
	if in.BotID == "" {
		return out, nil
	}

	// Bot creation/association guarantees the owner's Space; only ownership
	// and lifecycle are checked here. The user profile supplies the Bot name.
	const botQuery = `
SELECT b.robot_id AS bot_id, bu.name AS bot_name
FROM robot b
JOIN user bu ON bu.uid = ? AND BINARY bu.uid = BINARY b.robot_id
    AND bu.status = 1 AND bu.is_destroy <> 2
WHERE b.robot_id = ? AND BINARY b.robot_id = BINARY ?
    AND b.creator_uid = ? AND BINARY b.creator_uid = BINARY ? AND b.status = 1`
	var bot struct {
		BotID string `db:"bot_id"`
		Name  string `db:"bot_name"`
	}
	err = tx.SelectBySql(botQuery,
		in.BotID, in.BotID, in.BotID, in.UserID, in.UserID,
	).LoadOneContext(ctx, &bot)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, ErrIdentityNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read Bot: %w", err)
	}
	out.Bot = &IdentityBot{BotID: bot.BotID, Name: bot.Name, IsActive: true}
	return out, nil
}
