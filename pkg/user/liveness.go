// Package user holds account predicates for callers that must not import
// modules/user.
//
// Same reason pkg/space exists: modules/user imports half the tree, so a
// predicate package that only needs a *dbr.Session belongs outside it. This
// package depends on dbr and nothing else.
package user

import (
	"strings"

	"github.com/gocraft/dbr/v2"
)

// statusEnable and isDestroyDone mirror modules/user's constants, which cannot be
// imported here without the dependency this package exists to avoid.
//
// Spelled as untyped constants next to the only statement that uses them, so a
// drift is visible in one place. modules/user.StatusEnable is the iota-1 of a
// Status type; modules/user.IsDestroyDone is 2.
const (
	statusEnable  = 1
	isDestroyDone = 2
)

// ActiveAccounts returns the subset of uids whose ACCOUNT is still live, keyed by
// the spelling the database holds.
//
// # The predicate, and why both columns
//
//	status = 1 AND is_destroy <> 2
//
// This is the repository's canonical "account still occupies its identity" gate —
// modules/user.BuildBatchUsersResponse applies exactly this pair, and its comment
// explains the half that is easy to get wrong: a COOLING-OFF account
// (is_destroy = 1, a reversible destroy request) can still send and receive, so it
// is live and must stay in the answer. Only the terminal is_destroy = 2 counts as
// gone. Gating on status alone would surface a fully destroyed account mid-teardown,
// since teardown can leave status = 1 for a while.
//
// # Why this is a SEPARATE query rather than a JOIN
//
// Callers hold rows from tables that may be in a different collation. `user` is
// one of the dump-imported tables that inherited utf8mb4_0900_ai_ci in production,
// while migration-created tables declare utf8mb4_general_ci — so
// `... JOIN user u ON u.uid = pm.uid` is MySQL error 1267 THERE while passing in
// CI, whose database is created general_ci. Measured on 8.0: the JOIN form errors,
// this single-table form does not.
//
// A JOIN rooted at a legacy table (space_member ⋈ user ⋈ space) is safe, because
// all three sides drifted together — the two existing precedents
// (modules/user.authVerifyAPIKey, modules/bot_provision.assertSpaceMember) have
// that shape. Callers rooted at an octo_* table must use this function instead.
//
// # Keyed by the DATABASE's spelling
//
// `uid` compares case-insensitively under either collation, so a row can come back
// spelled differently from the uid that was asked for. Callers that need to match
// their own strings fold both sides — see pkg/project.FoldID, which is what its
// two batch readers do.
//
// Absent from the map means "not a live account"; the map is never nil on success.
// Like pkg/space.ActiveMembers this takes a *dbr.Session, so it runs outside any
// caller transaction and proves nothing about state at COMMIT time.
func ActiveAccounts(session dbr.SessionRunner, uids []string) (map[string]bool, error) {
	live := make(map[string]bool, len(uids))
	lookup := make([]string, 0, len(uids))
	seen := make(map[string]bool, len(uids))
	for _, uid := range uids {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		lookup = append(lookup, uid)
	}
	if len(lookup) == 0 {
		return live, nil
	}

	var found []string
	_, err := session.SelectBySql(
		"SELECT uid FROM `user` "+
			"WHERE uid IN ("+strings.TrimSuffix(strings.Repeat("?,", len(lookup)), ",")+") "+
			"  AND status = ? AND is_destroy <> ?",
		append(toArgs(lookup), statusEnable, isDestroyDone)...,
	).Load(&found)
	if err != nil {
		return nil, err
	}
	for _, uid := range found {
		live[uid] = true
	}
	return live, nil
}

// toArgs widens a uid slice for dbr's variadic bind list.
//
// Placeholders are built explicitly rather than passing the slice as a single
// `IN ?` bind: dbr expands a slice there, but doing it by hand keeps the two
// trailing scalar binds unambiguous.
func toArgs(uids []string) []interface{} {
	args := make([]interface{}, 0, len(uids)+2)
	for _, uid := range uids {
		args = append(args, uid)
	}
	return args
}
