package auth

// Boot-time resolution of the rollout mode.
//
// The floor itself still lives only in Redis (see session_rollout.go). MySQL
// holds one write-once marker row whose sole job is to answer a question Redis
// cannot answer about itself: when the floor is missing, was this deployment
// never initialised, or did it lose the floor to an RDB rollback? Those two
// need opposite reactions, so guessing is not an option.
//
// Losing the floor resolves UPWARD to enforce rather than restoring the old
// value. A user session token is disposable and re-login is an acceptable
// cost, so the safe answer is not the previous value but the strictest one.
// Restoring exactly is strictly worse in all three rollback shapes: with Redis
// wiped the tokens are gone anyway; rolled back past the floor's creation the
// resurrected permanent legacy tokens are the vulnerability itself; rolled back
// to some middle floor, restoring means continuing to accept a legacy set that
// came out of an inconsistent snapshot.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

const rolloutMarkerTable = "octo_session_rollout_marker"

// RolloutMarker records that this deployment has initialised (or adopted) a
// rollout floor at least once. It is write-once: initialized_floor is the value
// observed at adoption time and is audit only — never a source of truth for the
// current floor.
type RolloutMarker struct {
	InitializedAt    time.Time   `db:"initialized_at"`
	InitializedFloor SessionMode `db:"initialized_floor"`
}

// RolloutMarkerStore reads and stamps the singleton marker row.
type RolloutMarkerStore struct {
	db *dbr.Session
}

func NewRolloutMarkerStore(db *dbr.Session) *RolloutMarkerStore {
	if db == nil {
		panic("auth: NewRolloutMarkerStore requires a non-nil session")
	}
	return &RolloutMarkerStore{db: db}
}

// Load returns nil when the deployment has never initialised a floor.
func (s *RolloutMarkerStore) Load(ctx context.Context) (*RolloutMarker, error) {
	var marker RolloutMarker
	err := s.db.Select("initialized_at", "initialized_floor").
		From(rolloutMarkerTable).
		Where("singleton_id = ?", 1).
		LoadOneContext(ctx, &marker)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: load session rollout marker: %w", err)
	}
	return &marker, nil
}

// StampOnce records the marker if it is absent and is a no-op otherwise. It is
// the ONLY write to this table anywhere in the codebase: there is no UPDATE
// path, which is what makes the row write-once. A source guard enforces that.
func (s *RolloutMarkerStore) StampOnce(ctx context.Context, observedFloor SessionMode) error {
	floor := ""
	if observedFloor.valid() {
		floor = string(observedFloor)
	}
	_, err := s.db.InsertBySql(
		"INSERT INTO "+rolloutMarkerTable+" (singleton_id, initialized_floor) VALUES (?, ?)"+
			" ON DUPLICATE KEY UPDATE singleton_id = singleton_id",
		1, floor,
	).ExecContext(ctx)
	if err != nil {
		return fmt.Errorf("auth: stamp session rollout marker: %w", err)
	}
	return nil
}

// RolloutBootOutcome is the low-cardinality classification of what boot found.
// It is safe for a metric label.
type RolloutBootOutcome string

const (
	// RolloutBootFresh: no marker, no floor. A deployment that has never begun
	// the rollout. Starts at expand; the reconciler decides whether the
	// keyspace is empty (and may therefore go straight to enforce) using its
	// own rate-limited scan, so boot stays cheap.
	RolloutBootFresh RolloutBootOutcome = "fresh"
	// RolloutBootAdopted: no marker, floor present. Upgrading from #725 with a
	// rollout already under way. Adopt the floor, stamp the marker, change
	// nothing else.
	RolloutBootAdopted RolloutBootOutcome = "adopted"
	// RolloutBootRecovered: marker present, floor missing. Redis lost the
	// floor. Resolve upward to enforce and shout.
	RolloutBootRecovered RolloutBootOutcome = "rollback-recovered"
	// RolloutBootNormal: marker and floor both present.
	RolloutBootNormal RolloutBootOutcome = "normal"
)

// RolloutBoot is what a replica derives once at startup. Mode is the value the
// process runs at until the floor poller moves it.
type RolloutBoot struct {
	Outcome   RolloutBootOutcome
	Floor     SessionMode
	Mode      SessionMode
	MaxPerUID int
	// Warning is non-empty when an operator needs to know something: a
	// recovered rollback, or a legacy env mode still sitting above the floor.
	Warning string

	// Deployment-side knobs, echoed here so the server wiring reads one struct
	// rather than re-parsing the environment.
	AutoAdvance   bool
	CanaryAhead   bool
	ExpectWriters int
}

// ResolveRolloutBoot decides the starting mode without ever failing the
// process. Nothing here panics: the defect this change exists to remove was a
// missing Redis key taking the whole deployment down.
//
// legacyMode and legacyMaxPerUID come from the deprecated OCTO_AUTH_SESSION_MODE
// and OCTO_AUTH_SESSION_MAX_PER_UID. They are honoured for one release so an
// upgrade cannot silently loosen a deployment that is mid-canary: #725 requires
// deploying one phase above the floor and confirming before advancing, so a
// deployment in Phase D runs bounded on a revoke floor. Deriving mode purely
// from the floor would drop that reader back to revoke and re-admit permanent
// and over-max legacy tokens.
func ResolveRolloutBoot(
	ctx context.Context,
	store *RedisSessionStore,
	markers *RolloutMarkerStore,
	legacyMode SessionMode,
	legacyMaxPerUID int,
) (RolloutBoot, error) {
	if store == nil {
		return RolloutBoot{}, errors.New("auth: resolve rollout boot requires a session store")
	}
	control, controlErr := store.RolloutControl(ctx)

	var marker *RolloutMarker
	if markers != nil {
		var markerErr error
		marker, markerErr = markers.Load(ctx)
		if markerErr != nil {
			return RolloutBoot{}, markerErr
		}
	}

	// An unreadable control record is handled exactly like a missing one, and
	// the marker decides which way it resolves. Falling back to expand here
	// would be a fail-OPEN: expand does not consult legacy deny markers
	// (checksLegacyDeny requires v3-write or above), so a transient Redis error
	// on a revoke-floor deployment would let already-revoked legacy bearers
	// back in. Resolving upward is invariant 6, and it applies to "cannot read"
	// just as much as to "not there".
	if controlErr != nil {
		if marker == nil {
			// Never initialised: there is nothing to protect, and expand is the
			// behaviour this deployment already had.
			return RolloutBoot{
				Outcome: RolloutBootFresh,
				Mode:    SessionModeExpand,
				Warning: fmt.Sprintf("session rollout floor unreadable (%v) and no floor was ever established; holding at %s", controlErr, SessionModeExpand),
			}, nil
		}
		return RolloutBoot{
			Outcome:   RolloutBootRecovered,
			Floor:     SessionModeEnforce,
			Mode:      SessionModeEnforce,
			MaxPerUID: legacyMaxPerUID,
			Warning:   fmt.Sprintf("session rollout floor unreadable (%v) but this deployment established one; resolving upward to enforce rather than loosening", controlErr),
		}, nil
	}

	boot := RolloutBoot{Mode: SessionModeExpand}
	switch {
	case marker == nil && control == nil:
		boot.Outcome = RolloutBootFresh
	case marker == nil && control != nil:
		boot.Outcome = RolloutBootAdopted
		boot.Floor = control.ModeFloor
		boot.Mode = control.ModeFloor
		boot.MaxPerUID = control.MaxPerUID
	case marker != nil && control == nil:
		boot.Outcome = RolloutBootRecovered
		boot.Floor = SessionModeEnforce
		boot.Mode = SessionModeEnforce
		boot.Warning = fmt.Sprintf(
			"session rollout floor is missing but this deployment initialised one at %s; "+
				"treating it as Redis data loss and resolving upward to enforce",
			marker.InitializedAt.UTC().Format(time.RFC3339))
	default:
		boot.Outcome = RolloutBootNormal
		boot.Floor = control.ModeFloor
		boot.Mode = control.ModeFloor
		boot.MaxPerUID = control.MaxPerUID
	}

	// §3.5②: never loosen on upgrade.
	if legacyMode.valid() && legacyMode.rank() > boot.Mode.rank() {
		if boot.Warning == "" {
			boot.Warning = fmt.Sprintf(
				"deprecated %s=%s is above the rollout floor %s and is being honoured for this release "+
					"(equivalent to %s=1); remove it once the floor catches up",
				sessionModeEnv, legacyMode, boot.Floor, sessionCanaryAheadEnv)
		}
		boot.Mode = legacyMode
	}

	// §3.5③: the cap moves from env into the control record. A deployment at
	// v3-write or above necessarily has the env set, because #725's policy
	// refused to start without it, so this one-time carry cannot come up empty.
	if boot.MaxPerUID <= 0 {
		boot.MaxPerUID = legacyMaxPerUID
	}
	return boot, nil
}
