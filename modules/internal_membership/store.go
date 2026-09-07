package internal_membership

import (
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
	Epochs(spaceID string, projectIDs []string) (map[string]int64, error)
	// Memberships returns the project's member_epoch and the role of each uid
	// holding an active seat. Epoch 0 with an empty map means the project is
	// not an active project of spaceID.
	Memberships(spaceID, projectID string, uids []string) (int64, map[string]int, error)
}

// dbStore is the production implementation, delegating to pkg/project.
//
// It resolves the session per call rather than caching one at construction:
// config.Context owns the pool's lifecycle, and holding a session across a
// reconnect is how a module ends up querying a closed handle.
type dbStore struct {
	ctx *config.Context
}

func (s dbStore) Epochs(spaceID string, projectIDs []string) (map[string]int64, error) {
	return projectpkg.ProjectEpochsInSpace(s.ctx.DB(), spaceID, projectIDs)
}

func (s dbStore) Memberships(spaceID, projectID string, uids []string) (int64, map[string]int, error) {
	return projectpkg.ProjectMemberships(s.ctx.DB(), spaceID, projectID, uids)
}
