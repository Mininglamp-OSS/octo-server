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
	name string
	// marker is an ASCII byte the fixtures must contain; replacement is a code point
	// that utf8mb4_0900_ai_ci folds to it and utf8mb4_general_ci does not.
	//
	// A substitution rather than two literal spellings, because BOTH identity columns
	// have to be drifted and they are built differently — the uid is a constant here,
	// the space_id is composed per door.
	marker      byte
	replacement string
	storedUID   string
}{
	// The replacements are EXPLICIT \u escapes, never literal characters. These code
	// points are invisible or near-invisible in every editor and diff view, and a
	// copy-paste that silently degrades U+212A to ASCII "K" turns the case into
	// ordinary case drift — which BOTH collations fold, so the test still passes while
	// testing nothing. That happened once while writing this file and was caught only
	// by the control assertion below; the escapes are so it cannot happen again.
	{"fullwidth", 's', "\uFF53", "sidriftwide"},    // U+FF53 FULLWIDTH LATIN SMALL S
	{"letterlike", 'k', "\u212A", "sidriftkelvin"}, // U+212A KELVIN SIGN
}

// driftFirst substitutes the family's marker byte, and FAILS if the fixture does not
// contain it.
//
// The failure matters more than the substitution. A fixture renamed so that it no
// longer contains the marker would drift NOTHING, the door would be driven with the
// canonical spelling, and every assertion below would pass while testing the trivial
// case. That is the same vacuity this file's control assertion exists to catch, one
// level up.
func driftFirst(t *testing.T, s string, marker byte, replacement string) string {
	t.Helper()
	i := strings.IndexByte(s, marker)
	require.GreaterOrEqual(t, i, 0,
		"fixture %q must contain %q so a drifted spelling can be built from it; without it "+
			"the case drives the door with the canonical bytes and proves nothing", s, string(marker))
	return s[:i] + replacement + s[i+1:]
}

// assertProbeReproducesTheDrift is a control. Without it every case below could pass
// against a database with no drift at all, which is exactly how CI reads as green.
func assertProbeReproducesTheDrift(
	t *testing.T, sess *dbr.Session, storedSpace, storedUID, driftedSpace, driftedUID string,
) {
	t.Helper()
	// Both identity columns at once: the seat row must be reachable with BOTH drifted,
	// which is what the doors below are driven with.
	var loose []string
	_, err := sess.SelectBySql(
		"SELECT uid FROM space_member WHERE space_id = ? AND uid = ?",
		driftedSpace, driftedUID).Load(&loose)
	require.NoError(t, err)
	require.Equal(t, []string{storedUID}, loose,
		"the probe database must reproduce production's drift: under "+
			"utf8mb4_0900_ai_ci the drifted spelling has to MATCH the stored row. If this "+
			"fails the fixture is not drifted and every assertion below is vacuous.")

	// And each column independently must be invisible under the STRICTER collation —
	// which is where the epoch step and the cascade look. Asserted per column rather
	// than on the pair, because a fix on one axis and not the other is exactly the
	// state this file failed to detect when it drifted only the uid.
	for _, probe := range []struct {
		axis string
		sql  string
		args []interface{}
	}{
		{"space_id", "SELECT 1 FROM space_member WHERE space_id = ? COLLATE utf8mb4_general_ci AND uid = ?",
			[]interface{}{driftedSpace, storedUID}},
		{"uid", "SELECT 1 FROM space_member WHERE space_id = ? AND uid = ? COLLATE utf8mb4_general_ci",
			[]interface{}{storedSpace, driftedUID}},
	} {
		var strict []int
		_, err = sess.SelectBySql(probe.sql, probe.args...).Load(&strict)
		require.NoError(t, err)
		require.Empty(t, strict,
			"under octo_project_member's collation the drifted %s must NOT match — "+
				"that gap is the whole defect this file exists for", probe.axis)
	}
}

// recordSeatIdentities installs recording tx steps and returns the identifiers they
// were handed. Registration is latest-wins by name with no unregister, so both
// registries are restored with no-ops on cleanup.
func recordSeatIdentities(t *testing.T, removals, rejoins *[]SeatRef) {
	t.Helper()
	const name = "seat_identity_probe"
	// ONE registry, and the direction arrives as data. That is also what this
	// recording step pins: a door that flips a seat must reach the single step with
	// the right Opened value — there is no second registry it could be missing from.
	//
	// The WHOLE SeatRef is recorded, not just its uid. Recording one column is how the
	// earlier version of this file stayed green while the space_id axis was open: the
	// assertion could only see what the fixture bothered to capture.
	RegisterSeatTransitionTxStep(name, func(_ *dbr.Tx, tr SeatTransition) error {
		if tr.Opened {
			*rejoins = append(*rejoins, tr.Seat)
		} else {
			*removals = append(*removals, tr.Seat)
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
					// The "sk-" prefix carries EVERY family's marker byte on purpose, so
					// the space_id can be drifted by any of them. driftFirst fails loudly
					// if that ever stops being true — it already caught a fixture name
					// that had no marker, which would have driven the door with the
					// canonical space_id and proved nothing.
					//
					// Short and positional because space_id is VARCHAR(40) and the door
					// names are longer than that once a family prefix is on them.
					spaceID := fmt.Sprintf("sk-%s-%d", drift.name, i)
					driftedSpace := driftFirst(t, spaceID, drift.marker, drift.replacement)
					driftedUID := driftFirst(t, drift.storedUID, drift.marker, drift.replacement)

					seedProbeSpace(t, sess, spaceID)
					seedProbeSeat(t, sess, spaceID, drift.storedUID, door.seat)
					assertProbeReproducesTheDrift(
						t, sess, spaceID, drift.storedUID, driftedSpace, driftedUID)

					var removals, rejoins []SeatRef
					recordSeatIdentities(t, &removals, &rejoins)

					// BOTH identity columns drifted. A seat row has two, and the epoch
					// step matches on both — `WHERE space_id = ? AND uid = ?` against
					// octo_project_member — so canonicalising one and not the other
					// leaves the signal just as dead. Driving both at once is what makes
					// this matrix able to fail on either axis.
					door.drive(t, driftedSpace, driftedUID)

					// The transition really happened: without this a door that no-ops
					// hands over nothing and would pass on an empty slice.
					want := 0
					if door.rejoin {
						want = 1
					}
					require.Equal(t, want, probeSeatStatus(t, sess, spaceID, drift.storedUID),
						"the seat must have flipped — the drifted spelling matches it under "+
							"space_member's collation, which is precisely why the epoch step "+
							"must be told about it")

					got := removals
					if door.rejoin {
						got = rejoins
					}
					require.Len(t, got, 1, "the transition must run exactly one tx step")
					const why = "the tx step must be handed the %s that space_member STORES, not " +
						"the one the caller sent. It matches that identifier against " +
						"octo_project_member under utf8mb4_general_ci, where the caller's drifted " +
						"spelling enumerates NOTHING — the seat moves and member_epoch does not, " +
						"so a peer's cached decision keeps agreeing with an epoch that never changed."
					assert.Equal(t, drift.storedUID, got[0].UID(), why, "uid")
					assert.Equal(t, spaceID, got[0].SpaceID(), why, "space_id")
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
			spaceID := "sk-out-" + drift.name
			driftedSpace := driftFirst(t, spaceID, drift.marker, drift.replacement)
			driftedUID := driftFirst(t, drift.storedUID, drift.marker, drift.replacement)

			seedProbeSpace(t, sess, spaceID)
			seedProbeSeat(t, sess, spaceID, drift.storedUID, 1)
			assertProbeReproducesTheDrift(
				t, sess, spaceID, drift.storedUID, driftedSpace, driftedUID)

			var removals, rejoins []SeatRef
			recordSeatIdentities(t, &removals, &rejoins)

			removed, rerr := removeMemberLocked(
				sess, driftedSpace, driftedUID, 999, "probeop", MemberRemoveReasonKicked)
			require.NoError(t, rerr)
			require.True(t, removed)

			// Read the row back by INSERT ORDER, not by a predicate on either identity
			// column. A predicate would be asking the drifted question: this table is
			// pinned general_ci like octo_project_member, so `WHERE space_id = <stored>`
			// silently returns nothing when the row carries drifted bytes — the failure
			// would read as "no work order was written" rather than "the work order is
			// unusable", which is a different and less accurate finding.
			var row struct {
				SpaceID string `db:"space_id"`
				UID     string `db:"uid"`
			}
			require.NoError(t, sess.SelectBySql(
				"SELECT space_id, uid FROM space_member_removal_cleanup ORDER BY id DESC LIMIT 1",
			).LoadOne(&row))

			const why = "the cleanup outbox must carry the %s that space_member stores. With " +
				"the caller's spelling the cascade re-checks Space membership under the LOOSE " +
				"collation (matches, so it proceeds), enumerates project seats under the STRICT " +
				"one (zero rows), and completes SUCCESSFULLY as a documented no-op — the seat is " +
				"orphaned permanently and the async path is not a backstop for the synchronous " +
				"gap, it has the identical gap."
			assert.Equal(t, drift.storedUID, row.UID, why, "uid")
			assert.Equal(t, spaceID, row.SpaceID, why, "space_id")
		})
	}
}
