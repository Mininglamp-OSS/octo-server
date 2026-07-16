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

	// ScopePlatform and ScopeSpace are the authoritative app_bot.scope values.
	ScopePlatform = "platform"
	ScopeSpace    = "space"
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
	UID        string
	Kind       Kind
	CreatorUID string
	AppScope   string
	AppSpaceID string
}

type activeKindStore interface {
	lookup(uid string) (identityRecord, error)
}

type identityRecord struct {
	UserBot    bool   `db:"user_bot"`
	CreatorUID string `db:"creator_uid"`
	AppBot     bool   `db:"app_bot"`
	AppScope   string `db:"app_scope"`
	AppSpaceID string `db:"app_space_id"`
}

type dbActiveKindStore struct {
	session *dbr.Session
}

// lookup reads both lifecycle authorities and their authorization metadata in
// one statement snapshot. This prevents kind and policy metadata from drifting
// across separate queries while an identity is unpublished or reconfigured.
func (s *dbActiveKindStore) lookup(uid string) (identityRecord, error) {
	var row identityRecord
	err := s.session.SelectBySql(`
		SELECT
			EXISTS(SELECT 1 FROM robot WHERE robot_id=? AND status=1) AS user_bot,
			COALESCE((SELECT creator_uid FROM robot WHERE robot_id=? AND status=1 LIMIT 1), '') AS creator_uid,
			EXISTS(SELECT 1 FROM app_bot WHERE uid=? AND status=1) AS app_bot,
			COALESCE((SELECT scope FROM app_bot WHERE uid=? AND status=1 LIMIT 1), '') AS app_scope,
			COALESCE((SELECT space_id FROM app_bot WHERE uid=? AND status=1 LIMIT 1), '') AS app_space_id`,
		uid, uid, uid, uid, uid,
	).LoadOne(&row)
	if err != nil {
		return identityRecord{}, err
	}
	return row, nil
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
	record, err := r.store.lookup(uid)
	if err != nil {
		return nil, fmt.Errorf("resolve bot identity %q: %w", uid, err)
	}
	if record.UserBot && record.AppBot {
		return nil, fmt.Errorf("%w: uid %q is active in robot and app_bot", ErrAmbiguousIdentity, uid)
	}
	if record.UserBot {
		return &Identity{UID: uid, Kind: KindUserBot, CreatorUID: record.CreatorUID}, nil
	}
	if record.AppBot {
		return &Identity{
			UID:        uid,
			Kind:       KindAppBot,
			AppScope:   record.AppScope,
			AppSpaceID: record.AppSpaceID,
		}, nil
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
