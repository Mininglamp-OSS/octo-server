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
	"strings"
	"time"

	"github.com/gocraft/dbr/v2"
)

const defaultRolloutMarkerTable = "octo_session_rollout_marker"

// rolloutMarkerTable is a var only so a test can simulate "the migration has
// not run yet" without dropping a table the whole test database shares.
var rolloutMarkerTable = defaultRolloutMarkerTable

// defaultRecoveryMaxPerUID bounds sessions when no cap can be recovered from
// the floor record or the deprecated environment. A cap is an upper bound: too
// low means a few users hit it, too high means a weaker bound. Neither is the
// class of regression this path guards against, whereas refusing to apply the
// recovered mode leaves the process in a LOWER one, which is.
const defaultRecoveryMaxPerUID = 20

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

	// The marker is consulted ONLY when the floor cannot answer. When the floor
	// read succeeds there is nothing to disambiguate, and letting a marker error
	// veto a floor we already read is how the first boot of this artifact ended
	// up denying every legacy session: the marker table is created by a
	// modules/user migration that runs several hundred lines after the session
	// store is built, so on that boot it does not exist yet.
	if controlErr == nil && control != nil {
		boot := RolloutBoot{
			Outcome:   RolloutBootNormal,
			Floor:     control.ModeFloor,
			Mode:      control.ModeFloor,
			MaxPerUID: control.MaxPerUID,
		}
		// Best effort, and cosmetic: it only picks the metric label. Stamping is
		// the reconciler's job so it can be retried once the table exists.
		if markers != nil {
			if marker, err := markers.Load(ctx); err != nil || marker == nil {
				boot.Outcome = RolloutBootAdopted
			}
		} else {
			boot.Outcome = RolloutBootAdopted
		}
		applyBootOverrides(&boot, legacyMode, legacyMaxPerUID)
		return boot, nil
	}

	var marker *RolloutMarker
	if markers != nil {
		loaded, markerErr := markers.Load(ctx)
		switch {
		case markerErr == nil:
			marker = loaded
		case isMissingMarkerTable(markerErr):
			// Literally true rather than a workaround: with no table, no marker
			// was ever stamped, so this deployment never recorded an
			// initialisation.
			marker = nil
		default:
			// The floor could not answer and neither can the marker, so nothing
			// can be decided safely. MySQL is already a hard boot dependency
			// (migrations run during startup), so this resolves itself or the
			// process does not come up at all.
			return RolloutBoot{}, markerErr
		}
	}

	boot := RolloutBoot{}
	switch {
	case marker == nil:
		// Never initialised: nothing to protect, and expand is what this
		// deployment already had.
		boot.Outcome = RolloutBootFresh
		boot.Mode = SessionModeExpand
		if controlErr != nil {
			boot.Warning = fmt.Sprintf(
				"session rollout floor unreadable (%v) and no floor was ever established; holding at %s",
				controlErr, SessionModeExpand)
		}
	default:
		boot.Outcome = RolloutBootRecovered
		boot.Floor = SessionModeEnforce
		boot.Mode = SessionModeEnforce
		// The cap has to survive the loss of the floor that carried it. Falling
		// back to zero here made recovery unappliable, which left the process at
		// expand — the fail-open this whole path exists to prevent.
		boot.MaxPerUID = firstPositive(legacyMaxPerUID, defaultRecoveryMaxPerUID)
		reason := "is missing"
		if controlErr != nil {
			reason = fmt.Sprintf("is unreadable (%v)", controlErr)
		}
		boot.Warning = fmt.Sprintf(
			"session rollout floor %s but this deployment initialised one at %s; "+
				"treating it as Redis data loss and resolving upward to enforce",
			reason, marker.InitializedAt.UTC().Format(time.RFC3339))
	}
	applyBootOverrides(&boot, legacyMode, legacyMaxPerUID)
	return boot, nil
}

// applyBootOverrides carries the deprecated environment forward.
func applyBootOverrides(boot *RolloutBoot, legacyMode SessionMode, legacyMaxPerUID int) {
	// §3.5②: never loosen on upgrade. #725 requires running one phase above the
	// floor and confirming before advancing, so a deployment in Phase D sits at
	// bounded on a revoke floor; deriving purely from the floor would drop that
	// reader back to revoke.
	if legacyMode.valid() && legacyMode.rank() > boot.Mode.rank() {
		if boot.Warning == "" {
			boot.Warning = fmt.Sprintf(
				"deprecated %s=%s is above the rollout floor %s and is being honoured for this release "+
					"(equivalent to %s=1); remove it once the floor catches up",
				sessionModeEnv, legacyMode, boot.Floor, sessionCanaryAheadEnv)
		}
		boot.Mode = legacyMode
	}
	// Defaulted unconditionally, not just for a mode that already writes v3.
	// A process that boots at expand has no cap either, and the floor rises
	// under it minutes later — at which point the reconciler asks for a cap
	// nobody has and blocks on "max_per_uid is not configured" forever. Boot
	// cannot know which mode this process will end up running.
	if boot.MaxPerUID <= 0 {
		boot.MaxPerUID = legacyMaxPerUID
	}
	if boot.MaxPerUID <= 0 {
		boot.MaxPerUID = defaultRecoveryMaxPerUID
	}
}

// StrictRolloutBoot is what a caller uses when resolution itself failed. It
// goes through the same overrides as every other outcome: hand-building the
// struct at the call site skipped the cap fallback and produced enforce with no
// cap, which fences every login permanently and writes no floor to recover
// from.
func StrictRolloutBoot(reason error, legacyMode SessionMode, legacyMaxPerUID int) RolloutBoot {
	boot := RolloutBoot{
		Outcome: RolloutBootRecovered,
		Floor:   SessionModeEnforce,
		Mode:    SessionModeEnforce,
	}
	applyBootOverrides(&boot, legacyMode, legacyMaxPerUID)
	boot.Warning = fmt.Sprintf(
		"session rollout state unresolvable at boot (%v); resolving upward to %s", reason, boot.Mode)
	return boot
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// isMissingMarkerTable reports the "table does not exist" case, which is the
// state of every database on the first boot of this artifact.
func isMissingMarkerTable(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "Error 1146") ||
		strings.Contains(message, "doesn't exist") ||
		strings.Contains(message, "does not exist")
}
