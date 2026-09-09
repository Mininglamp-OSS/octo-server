package space

import (
	"github.com/gocraft/dbr/v2"
)

// Cross-module read used by modules/notify's role-targeted delivery mode
// (NotifyReq.target_role == "space_admin").
//
// # Why this lives here and is exported
//
// modules/notify must be able to answer "who are this Space's owners/admins?"
// without the caller naming them, and without anyone outside octo-server ever
// receiving that list. Three options were considered:
//
//  1. Copy the SQL into modules/notify. Rejected: the space_member isolation
//     predicates (status=1, INNER JOIN space ON s.status=1, the bot filter) are
//     exactly the kind of thing that drifts once it exists twice, and each
//     drift is a silent authorization bug. modules/notify already carries one
//     hand-rolled space_member query (memberCache.refresh in space_verify.go);
//     a second, subtler one is how the invariant gets lost.
//  2. Move the query into a neutral leaf package (pkg/space). Rejected:
//     pkg/space is a dependency-free helper package for channel-id parsing and
//     membership middleware; giving it a dbr session and space_member reads
//     would make it a second home for the same schema modules/space owns.
//  3. Export it from modules/space and have modules/notify call it. Chosen.
//
// There is no import cycle: modules/notify already depends on modules/space
// transitively (notify -> modules/user -> modules/space, and notify ->
// modules/group -> modules/space), while modules/space depends on neither
// modules/notify nor modules/user. The Go compiler is the ultimate enforcer of
// that direction, but a cycle error is a confusing way to learn it, so
// TestSpaceDoesNotImportNotifyOrUser (route_wiring_test.go) fails first with an
// explanation.
//
// The function takes a *dbr.Session rather than a *DB so callers do not need a
// modules/space DB handle (modules/notify holds ctx.DB() directly), and so it
// stays trivially testable.

// ActiveAdminUIDs returns the uids of a Space's active owners and admins:
// space_member rows with status=1 AND role>=1, in an ACTIVE space, excluding
// bot identities. Ordered (role DESC, created_at ASC, uid ASC) and capped at
// `limit` rows so callers get deterministic, bounded output.
//
// Every predicate is load-bearing:
//
//   - status=1 AND role>=1 — the reviewer set. A removed admin (status=0) or a
//     plain member (role=0) must never receive an approval card, because the
//     recipient's uid is subsequently treated as an authorized approver by the
//     consumer.
//
//   - INNER JOIN space ON s.status=1 — disbanding a Space only flips
//     space.status; its space_member rows stay status=1. Without this join a
//     disbanded org's former admins would keep receiving cards. Same hardening
//     as queryUserSpaceContext (modules/user/api.go, v3.3.1 §A.1) and
//     queryActiveMemberRole (api_internal.go).
//
//   - the robot LEFT JOIN … IS NULL — bots are ordinary space_member rows.
//     queryMembers (db.go:179-201) already filters bots the viewer does not
//     own; here there is no viewer, and delivering an approval card to a bot
//     would put a machine uid into the approver set. So exclude every uid that
//     has a robot row, regardless of robot.status: a soft-deleted bot is still
//     not a person.
//
//     App bots (the app_bot table) need no filter here: createBot persists the
//     bot's scope in app_bot.space_id and deliberately does NOT insert the App
//     Bot uid into space_member (see modules/app_bot/app_bot.go:1122-1126), so
//     an app bot cannot appear in this result at all.
//
// A caller that wants to detect truncation should over-fetch by one and
// compare against its own cap; this function does not log, because the useful
// log line (which Space, which cap) belongs to the caller.
func ActiveAdminUIDs(session *dbr.Session, spaceID string, limit uint64) ([]string, error) {
	if session == nil || spaceID == "" || limit == 0 {
		return nil, nil
	}
	var uids []string
	_, err := session.SelectBySql(`
		SELECT sm.uid
		FROM space_member sm
		INNER JOIN space s ON s.space_id=sm.space_id AND s.status=1
		LEFT JOIN robot r ON r.robot_id=sm.uid
		WHERE sm.space_id=? AND sm.status=1 AND sm.role>=1
			AND r.robot_id IS NULL
		ORDER BY sm.role DESC, sm.created_at ASC, sm.uid ASC
		LIMIT ?
	`, spaceID, limit).Load(&uids)
	if err != nil {
		return nil, err
	}
	return uids, nil
}
