// Package botidentity resolves the authoritative lifecycle identity behind a
// bot UID. It deliberately reads only the robot and app_bot tables: user.robot
// is presentation metadata and is not an authorization source.
package botidentity

import (
	"errors"
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

// Kind identifies which authoritative bot table owns a UID.
type Kind string

const (
	KindUserBot Kind = "user_bot"
	KindAppBot  Kind = "app_bot"
)

var (
	// ErrAmbiguousIdentity indicates a broken cross-table uniqueness invariant.
	// Callers must fail closed rather than silently choosing one bot kind.
	ErrAmbiguousIdentity = errors.New("ambiguous bot identity")
	// ErrResolverUnavailable indicates a construction or wiring failure.
	ErrResolverUnavailable = errors.New("bot identity resolver unavailable")
)

// Identity is the minimum authoritative metadata consumers need.
type Identity struct {
	UID  string
	Kind Kind
}

type activeKindStore interface {
	activeKinds(uid string) (userBot bool, appBot bool, err error)
}

type dbActiveKindStore struct {
	session *dbr.Session
}

// activeKinds reads both lifecycle authorities in one statement. MySQL gives
// the two EXISTS predicates one statement snapshot, so an ambiguous result is
// detected consistently without a precedence rule or a two-query race.
func (s *dbActiveKindStore) activeKinds(uid string) (bool, bool, error) {
	var row struct {
		UserBot int `db:"user_bot"`
		AppBot  int `db:"app_bot"`
	}
	err := s.session.SelectBySql(`
		SELECT
			EXISTS(SELECT 1 FROM robot WHERE robot_id=? AND status=1) AS user_bot,
			EXISTS(SELECT 1 FROM app_bot WHERE uid=? AND status=1) AS app_bot`,
		uid, uid,
	).LoadOne(&row)
	if err != nil {
		return false, false, err
	}
	return row.UserBot == 1, row.AppBot == 1, nil
}

// Resolver performs live authoritative lookups. It intentionally has no
// package-level cache; consumers that need presentation caching own it locally.
type Resolver struct {
	store activeKindStore
}

func New(ctx *config.Context) *Resolver {
	if ctx == nil {
		return &Resolver{}
	}
	return &Resolver{store: &dbActiveKindStore{session: ctx.DB()}}
}

// Resolve returns nil when uid is not an active bot identity. Lookup and
// invariant errors are preserved so callers cannot accidentally fail open.
func (r *Resolver) Resolve(uid string) (*Identity, error) {
	if uid == "" {
		return nil, nil
	}
	if r == nil || r.store == nil {
		return nil, ErrResolverUnavailable
	}
	userBot, appBot, err := r.store.activeKinds(uid)
	if err != nil {
		return nil, fmt.Errorf("resolve bot identity %q: %w", uid, err)
	}
	if userBot && appBot {
		return nil, fmt.Errorf("%w: uid %q is active in robot and app_bot", ErrAmbiguousIdentity, uid)
	}
	if userBot {
		return &Identity{UID: uid, Kind: KindUserBot}, nil
	}
	if appBot {
		return &Identity{UID: uid, Kind: KindAppBot}, nil
	}
	return nil, nil
}

// Active is a boolean convenience that preserves all lookup and invariant
// errors. It must not be used to collapse an error into an unqualified false.
func (r *Resolver) Active(uid string) (bool, error) {
	identity, err := r.Resolve(uid)
	if err != nil {
		return false, err
	}
	return identity != nil, nil
}
