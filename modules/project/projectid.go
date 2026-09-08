package project

import (
	"fmt"

	"github.com/google/uuid"
)

// newProjectID mints a project_id.
//
// # Why canonical hyphenated UUID rather than util.GenerUUID
//
// util.GenerUUID is this repository's usual id generator and produces a v4 UUID
// with the hyphens stripped — 32 hex characters (octo-lib pkg/util/string.go).
// Every other id in this codebase is fine that way, because nothing outside the
// repository has an opinion about the shape.
//
// project_id does have such an opinion. It is shared verbatim with the Loop
// control plane, where the same value is the primary key of the corresponding
// workspace and is stored in a column typed as a UUID. That side accepts the
// unhyphenated form on input but renders it back canonically, so the two systems
// would agree on the value while disagreeing on its spelling — and the creation
// handshake compares them AS STRINGS to confirm both sides provisioned the same
// entity. A comparison that can never succeed is not a check.
//
// So the canonical form is generated here, at the only place a project_id comes
// into existence, rather than normalized at each boundary. Normalizing at
// boundaries means every future boundary has to remember to.
//
// # Existing rows
//
// Projects created before this change carry the 32-character form and keep it.
// Nothing validates the shape — the column is VARCHAR(40), which holds both, and
// project_id is opaque to every predicate that reads it — so the two coexist
// safely. What they cannot do is participate in the Loop handshake, which is a
// deployment question (whether any such rows exist) rather than a code one.
//
// # Error rather than panic
//
// uuid.NewString panics if the system entropy source fails. This runs inside a
// request handler that already returns an error, so it returns one: a failure to
// read entropy should refuse one create, not take down the process.
func newProjectID() (string, error) {
	u, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("project: generate project_id: %w", err)
	}
	return u.String(), nil
}
