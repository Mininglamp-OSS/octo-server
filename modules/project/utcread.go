package project

import (
	"database/sql"
	"time"
)

// Reading DATETIME columns back correctly, which this module got wrong twice in
// two different ways.
//
// # Why a helper exists at all
//
// Every DATETIME column in this module stores a UTC wall clock: the migrations
// forbid CURRENT_TIMESTAMP defaults and every writer passes time.Now().UTC(),
// which dbr interpolates as UTC text. So the bytes in the column are right.
//
// Read-back is where it goes wrong, and the two failures are independent:
//
//  1. THE VALUE DOES NOT ARRIVE. dbr cannot scan a bare column into a
//     []time.Time: it returns len(rows) entries of the ZERO time, with n set and
//     err nil. Silent. Both age gauges in this module were written that way, so
//     both computed now.Sub(year zero) — about 2.5 million hours — on every
//     deployment, in every timezone. The test that was supposed to catch it
//     asserted the age was positive, which a garbage-huge number is.
//     sql.NullTime scans correctly (and carries NULL, which a bare time.Time
//     cannot), so it is the only shape these reads may use.
//
//  2. THE VALUE ARRIVES IN THE WRONG LOCATION. With parseTime=true and loc=Local
//     in the DSN, the driver tags the parsed UTC wall clock with the PROCESS
//     location, and a later .UTC() then shifts the instant by the full offset.
//     Measured against MySQL 8.0.46 on a column holding 10:45:00 UTC:
//
//     TZ=Asia/Shanghai    -> .UTC() yields 02:45:00Z; a 5-minute queue reads 8h5m
//     TZ=America/New_York -> .UTC() yields 14:45:00Z; the age goes negative and
//     clamps to zero, so a stuck backlog reads fresh forever
//
//     The second direction is the dangerous one: the alert never fires.
//
// This is documented repository history, not a hypothesis.
// modules/space/member_removal_metrics.go carries the -28799 seconds it shipped
// with, and modules/notification/db.go rebuilds wall-clock components in UTC for
// exactly this reason. The comment on oldestPendingLifecycleEventAge cited that
// precedent and still got it wrong, because it defended the SQL side while the
// Go-side read-back is where loc=Local actually bites.
//
// The default test DSN carries no loc=, so the driver defaults to UTC and both
// paths are the identity — which is why a suite can be green and a deployment
// broken. See TestUTCFromColumnIsOffsetInvariant.

// utcFromColumn reinterprets a DATETIME read-back as the UTC wall clock it
// actually is.
//
// It rebuilds the value from its components rather than calling .UTC(), which is
// the whole point: the components are the bytes MySQL returned and are correct;
// the LOCATION attached to them by the driver is what is wrong. .UTC() trusts
// that location and converts, which is precisely the shift.
//
// Same construction as modules/notification's normalizePauseRecord.
func utcFromColumn(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// firstUTCFromColumn is the read shape the two "oldest row" queries share:
// scan into sql.NullTime (never []time.Time) and reinterpret as UTC.
//
// Returns ok=false for no rows AND for a NULL, which the callers treat the same
// way — there is nothing to measure the age of.
func firstUTCFromColumn(rows []sql.NullTime) (time.Time, bool) {
	if len(rows) == 0 || !rows[0].Valid {
		return time.Time{}, false
	}
	return utcFromColumn(rows[0].Time), true
}
