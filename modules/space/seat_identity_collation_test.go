package space

// The invariant this file pins:
//
//	Whatever identifier a seat transition hands to its tx steps and to the removal
//	outbox must be the bytes `space_member` STORES — never the bytes the caller sent.
//
// # Why this is a correctness property and not tidiness
//
// The epoch step (modules/project.bumpMemberEpochForSpaceMemberTx) takes that
// identifier and matches it against `octo_project_member`, which every migration in
// this repo pins to utf8mb4_general_ci. `space_member` is dump-imported and sits at
// utf8mb4_0900_ai_ci in production. Those two collations do NOT have the same
// equivalence classes, and the direction is the dangerous one: 0900_ai_ci (UCA 9.0.0
// primary weights) folds compatibility decompositions to their base character;
// general_ci is a legacy per-character table and does not.
//
// MEASURED on MySQL 8.0.33 against both production collations, stored `alice`:
//
//	U+FF41 FULLWIDTH a  vs `a`   -> 0900_ai_ci 1, general_ci 0   DIVERGENT
//	U+212A KELVIN SIGN  vs `K`   -> 0900_ai_ci 1, general_ci 0   DIVERGENT
//	U+2126 OHM SIGN     vs `Ω`   -> 0900_ai_ci 1, general_ci 0   DIVERGENT
//	U+00AA FEM ORDINAL  vs `a`   -> 0900_ai_ci 1, general_ci 0   DIVERGENT
//	U+017F LATIN long s vs `s`   -> 1 / 1   NOT divergent (both fold it)
//	U+00E9 e-acute      vs `e`   -> 1 / 1   NOT divergent (general_ci is
//	                                        accent-insensitive for Latin-1 too)
//	ASCII case A        vs `a`   -> 1 / 1   NOT divergent
//	U+2460 CIRCLED ONE  vs `1`   -> 0 / 0   neither folds it
//
// So a caller's spelling can close or reopen a Space seat and then enumerate NOTHING
// on the project side: the seat flips, member_epoch does not move, `_verify` answers
// the new value while `epochs` answers the old one, and a peer's staleness check
// agrees with itself. In the removal direction that is a revoked authorization that
// survives, unbounded — there is no async backstop either, because the outbox row
// carries the same drifted spelling and the cascade completes successfully as a
// documented no-op while the project seat is orphaned permanently.
//
// # Two families, and why examples cannot define this class
//
// Four reviewers reached for four different characters and three were wrong: KELVIN
// was reported both as divergent and as not divergent, and accents — the pair the
// previous fix's own comment rested on — turn out not to diverge at all. The class
// is "has a compatibility decomposition whose base is a Latin letter", which is not
// enumerable by intuition. That is the argument for making the identifier come from
// the database rather than pinning a list of characters, and it is why these cases
// exist to prove the mechanism rather than to enumerate the inputs.
//
// # Why the assertion is on the IDENTIFIER, not on member_epoch
//
// modules/space cannot see octo_* tables — modules/project imports this package, not
// the other way round. The epoch half is pinned in modules/project against the same
// drifted schema. Split deliberately: this half proves the identifier that leaves a
// seat transition is canonical, that half proves a canonical identifier enumerates
// correctly under drift. Neither is sufficient alone and both are cheap.
//
// # Why a separate database
//
// CI creates its schema with an explicit COLLATE utf8mb4_general_ci, so every table
// agrees there and this whole class is invisible — a green suite is structurally not
// evidence. `space` / `space_member` are shared fixtures, so CONVERTing them in place
// would wreck whatever case runs next under -shuffle=on.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const seatIdentityProbeDB = "octo_space_seat_identity_probe"

// The tables the seat transitions under test touch. `legacy` marks the ones that are
// dump-imported in production and therefore carry the drift.
var seatIdentityProbeTables = []struct {
	name   string
	legacy bool
}{
	{"space", true},
	{"space_member", true},
	{"space_member_removal_cleanup", false},
	{"space_join_apply", false},
}

func newSeatIdentityProbe(t *testing.T) *dbr.Session {
	t.Helper()
	addr := os.Getenv("OCTO_TEST_MYSQL_ADDR")
	if addr == "" {
		addr = testCtx.GetConfig().DB.MySQLAddr
	}
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected MySQL DSN shape")
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}

	admin, err := dbr.Open("mysql", addr[:slash+1]+opts, nil)
	require.NoError(t, err)
	adminSess := admin.NewSession(nil)
	exec := func(sess *dbr.Session, stmt string) {
		_, execErr := sess.UpdateBySql(stmt).Exec()
		require.NoError(t, execErr, stmt)
	}
	exec(adminSess, "DROP DATABASE IF EXISTS `"+seatIdentityProbeDB+"`")
	exec(adminSess, "CREATE DATABASE `"+seatIdentityProbeDB+
		"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")

	conn, err := dbr.Open("mysql", addr[:slash+1]+seatIdentityProbeDB+opts, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminSess.UpdateBySql("DROP DATABASE IF EXISTS `" + seatIdentityProbeDB + "`").Exec()
		_ = conn.Close()
		_ = admin.Close()
	})
	sess := conn.NewSession(nil)

	var sourceSchema string
	require.NoError(t, testCtx.DB().SelectBySql("SELECT DATABASE()").LoadOne(&sourceSchema))
	require.NotEmpty(t, sourceSchema)
	for _, table := range seatIdentityProbeTables {
		exec(sess, "CREATE TABLE `"+table.name+"` LIKE `"+sourceSchema+"`.`"+table.name+"`")
		if table.legacy {
			exec(sess, "ALTER TABLE `"+table.name+
				"` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
		}
	}
	return sess
}

// seatIdentityDrifts are spellings that match a stored seat under space_member's
// production collation and do NOT match it under octo_project_member's. Two families
// on purpose: one width form, one letterlike symbol, so a fix that special-cases
// fullwidth cannot pass.
var seatIdentityDrifts = []struct {
	name    string
	stored  string
	drifted string
}{
	// The drifted spellings are written as EXPLICIT \u escapes, never as literal
	// characters. These code points are invisible or near-invisible in every editor
	// and diff view, and a copy-paste that silently degrades U+212A to ASCII "K"
	// turns the case into ordinary case drift — which BOTH collations fold, so the
	// test still passes while testing nothing. That happened once while writing
	// this file and was caught only by the control assertion below; the escapes are
	// so it cannot happen silently again.
	{"fullwidth", "sidriftwide", "\uFF53idriftwide"},      // U+FF53 FULLWIDTH LATIN SMALL S
	{"letterlike", "sidriftkelvin", "sidrift\u212Aelvin"}, // U+212A KELVIN SIGN, for the k
}

// assertProbeReproducesTheDrift is a control. Without it every case below could pass
// against a database with no drift at all, which is exactly how CI reads as green.
func assertProbeReproducesTheDrift(t *testing.T, sess *dbr.Session, spaceID, stored, drifted string) {
	t.Helper()
	var loose []string
	_, err := sess.SelectBySql(
		"SELECT uid FROM space_member WHERE space_id = ? AND uid = ?", spaceID, drifted).Load(&loose)
	require.NoError(t, err)
	require.Equal(t, []string{stored}, loose,
		"the probe database must reproduce production's drift: under "+
			"utf8mb4_0900_ai_ci the drifted spelling has to MATCH the stored row. If this "+
			"fails the fixture is not drifted and every assertion below is vacuous.")

	var strict []int
	_, err = sess.SelectBySql(
		"SELECT 1 FROM space_member WHERE space_id = ? AND uid = ? COLLATE utf8mb4_general_ci",
		spaceID, drifted).Load(&strict)
	require.NoError(t, err)
	require.Empty(t, strict,
		"and under octo_project_member's collation the same spelling must NOT match — "+
			"that gap is the whole defect this file exists for")
}

// recordSeatIdentities installs recording tx steps and returns the identifiers they
// were handed. Registration is latest-wins by name with no unregister, so both
// registries are restored with no-ops on cleanup.
func recordSeatIdentities(t *testing.T, removals, rejoins *[]string) {
	t.Helper()
	const name = "seat_identity_probe"
	// ONE registry, and the direction arrives as data. That is also what this
	// recording step pins: a door that flips a seat must reach the single step with
	// the right Opened value — there is no second registry it could be missing from.
	RegisterSeatTransitionTxStep(name, func(_ *dbr.Tx, tr SeatTransition) error {
		if tr.Opened {
			*rejoins = append(*rejoins, tr.Seat.UID())
		} else {
			*removals = append(*removals, tr.Seat.UID())
		}
		return nil
	})
	t.Cleanup(func() {
		RegisterSeatTransitionTxStep(name, func(*dbr.Tx, SeatTransition) error { return nil })
	})
}

func seedProbeSeat(t *testing.T, sess *dbr.Session, spaceID, uid string, status int) {
	t.Helper()
	_, err := sess.InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) "+
			"VALUES (?, ?, 0, ?, NOW(), NOW())", spaceID, uid, status).Exec()
	require.NoError(t, err)
}

func seedProbeSpace(t *testing.T, sess *dbr.Session, spaceID string) {
	t.Helper()
	_, err := sess.InsertBySql(
		"INSERT INTO `space` (space_id, name, creator, status, created_at, updated_at) "+
			"VALUES (?, ?, 'probeop', 1, NOW(), NOW())", spaceID, spaceID).Exec()
	require.NoError(t, err)
}

func probeSeatStatus(t *testing.T, sess *dbr.Session, spaceID, uid string) int {
	t.Helper()
	var status []int
	_, err := sess.SelectBySql(
		"SELECT status FROM space_member WHERE space_id = ? AND uid = ?", spaceID, uid).Load(&status)
	require.NoError(t, err)
	require.Len(t, status, 1, "the fixture seat must exist")
	return status[0]
}

// TestSeatTransitionsHandTheStoredSpellingToTheirTxSteps drives every seat
// transition with a drifted spelling over a canonically-spelled seat and asserts the
// identifier that leaves the transaction is the stored one.
//
// Each case first proves the transition actually took (the seat flipped), so a door
// that silently did nothing cannot pass by handing over no identifier at all.
func TestSeatTransitionsHandTheStoredSpellingToTheirTxSteps(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)
	sess := newSeatIdentityProbe(t)

	for _, drift := range seatIdentityDrifts {
		t.Run(drift.name, func(t *testing.T) {
			doors := []struct {
				name string
				// seat is the status the fixture seat starts in.
				seat int
				// rejoin marks a transition that REOPENS a seat; the others close one.
				rejoin bool
				drive  func(t *testing.T, spaceID, uid string)
			}{
				{
					name: "upsertMembers", seat: 0, rejoin: true,
					drive: func(t *testing.T, spaceID, uid string) {
						require.NoError(t, newManagerDB(sess).upsertMembers(spaceID, []string{uid}))
					},
				},
				{
					name: "reactivateMember", seat: 0, rejoin: true,
					drive: func(t *testing.T, spaceID, uid string) {
						require.NoError(t, (&DB{session: sess}).reactivateMember(spaceID, uid, 0))
					},
				},
				{
					name: "atomicReactivateMemberIfNotFull", seat: 0, rejoin: true,
					drive: func(t *testing.T, spaceID, uid string) {
						require.NoError(t, (&DB{session: sess}).atomicReactivateMemberIfNotFull(spaceID, uid, 100))
					},
				},
				{
					name: "approveJoinApplyAtomic", seat: 0, rejoin: true,
					drive: func(t *testing.T, spaceID, uid string) {
						db := &DB{session: sess}
						applyID, aerr := db.upsertJoinApply(&spaceJoinApplyModel{SpaceId: spaceID, UID: uid})
						require.NoError(t, aerr)
						outcome, _, aerr := db.approveJoinApplyAtomic(applyID, "probeop", spaceID, 100)
						require.NoError(t, aerr)
						require.Equal(t, approveOK, outcome)
					},
				},
				{
					name: "removeMemberLocked", seat: 1,
					drive: func(t *testing.T, spaceID, uid string) {
						removed, rerr := removeMemberLocked(
							sess, spaceID, uid, 999, "probeop", MemberRemoveReasonKicked)
						require.NoError(t, rerr)
						require.True(t, removed, "the fixture seat must actually have closed")
					},
				},
				{
					name: "removeMembersForce", seat: 1,
					drive: func(t *testing.T, spaceID, uid string) {
						removed, rerr := newManagerDB(sess).removeMembersForce(
							spaceID, []string{uid}, "probeop")
						require.NoError(t, rerr)
						require.NotEmpty(t, removed, "the fixture seat must actually have closed")
					},
				},
				{
					name: "closeOneSeatAndEnqueue", seat: 1,
					drive: func(t *testing.T, spaceID, uid string) {
						closed, cerr := closeOneSeatAndEnqueueTx(
							sess, spaceID, uid, "probeop", MemberRemoveReasonForceRemoved)
						require.NoError(t, cerr)
						require.True(t, closed, "the fixture seat must actually have closed")
					},
				},
			}

			for i, door := range doors {
				t.Run(door.name, func(t *testing.T) {
					// Short and positional: space_id is VARCHAR(40) and the door names
					// are longer than that once the family prefix is on them.
					spaceID := fmt.Sprintf("sid-%s-%d", drift.name, i)
					seedProbeSpace(t, sess, spaceID)
					seedProbeSeat(t, sess, spaceID, drift.stored, door.seat)
					assertProbeReproducesTheDrift(t, sess, spaceID, drift.stored, drift.drifted)

					var removals, rejoins []string
					recordSeatIdentities(t, &removals, &rejoins)

					door.drive(t, spaceID, drift.drifted)

					// The transition really happened: without this a door that no-ops
					// hands over nothing and would pass on an empty slice.
					want := 0
					if door.rejoin {
						want = 1
					}
					require.Equal(t, want, probeSeatStatus(t, sess, spaceID, drift.stored),
						"the seat must have flipped — the drifted spelling matches it under "+
							"space_member's collation, which is precisely why the epoch step "+
							"must be told about it")

					got := removals
					if door.rejoin {
						got = rejoins
					}
					require.Len(t, got, 1, "the transition must run exactly one tx step")
					assert.Equal(t, drift.stored, got[0],
						"the tx step must be handed the spelling space_member STORES, not the "+
							"one the caller sent. It matches that identifier against "+
							"octo_project_member under utf8mb4_general_ci, where the caller's "+
							"drifted spelling enumerates NOTHING — the seat moves and "+
							"member_epoch does not, so a peer's cached decision keeps agreeing "+
							"with an epoch that never changed.")
				})
			}
		})
	}
}

// TestRemovalOutboxCarriesTheStoredSpelling covers the async half. The cascade
// re-reads Space membership with whatever this row holds (matching under the loose
// collation, so it proceeds), then enumerates project seats with it and finds none,
// and completes SUCCESSFULLY — a permanently orphaned seat with nothing left to
// repair the epoch.
func TestRemovalOutboxCarriesTheStoredSpelling(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)
	sess := newSeatIdentityProbe(t)

	for _, drift := range seatIdentityDrifts {
		t.Run(drift.name, func(t *testing.T) {
			spaceID := "oid-" + drift.name
			seedProbeSpace(t, sess, spaceID)
			seedProbeSeat(t, sess, spaceID, drift.stored, 1)
			assertProbeReproducesTheDrift(t, sess, spaceID, drift.stored, drift.drifted)

			var removals, rejoins []string
			recordSeatIdentities(t, &removals, &rejoins)

			removed, rerr := removeMemberLocked(
				sess, spaceID, drift.drifted, 999, "probeop", MemberRemoveReasonKicked)
			require.NoError(t, rerr)
			require.True(t, removed)

			var queued []string
			_, err := sess.SelectBySql(
				"SELECT uid FROM space_member_removal_cleanup WHERE space_id = ?", spaceID).Load(&queued)
			require.NoError(t, err)
			require.Len(t, queued, 1)
			assert.Equal(t, drift.stored, queued[0],
				"the cleanup outbox must carry the spelling space_member stores. With the "+
					"caller's spelling the cascade's project-seat enumeration returns zero rows "+
					"and the work order completes as a no-op, so the async path is not a "+
					"backstop for the synchronous gap — it has the identical gap.")
		})
	}
}
