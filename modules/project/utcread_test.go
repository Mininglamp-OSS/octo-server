package project

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two DATETIME read-back defects, pinned so neither can come back.
//
// Neither is visible through the ordinary suite: the default test DSN carries no
// loc=, so the driver defaults to UTC and the shift is the identity, and the
// zero-time load produces a number that passes any "is the age positive" check.
// So these tests construct the hazardous inputs directly rather than hoping the
// environment supplies them.

// TestUTCFromColumnIsOffsetInvariant is the loc=Local half.
//
// With parseTime=true&loc=Local the driver hands back the column's UTC wall clock
// TAGGED with the process location. utcFromColumn must reinterpret those
// components as UTC; .UTC() would convert them, which is the bug.
func TestUTCFromColumnIsOffsetInvariant(t *testing.T) {
	// The stored fact: 10:45:00 UTC, as every writer in this module produces.
	const wallClock = "2026-09-08 10:45:00.000"
	want := time.Date(2026, 9, 8, 10, 45, 0, 0, time.UTC)

	for _, zone := range []string{"UTC", "Asia/Shanghai", "America/New_York", "Pacific/Kiritimati"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			require.NoError(t, err, "tzdata must be present; a skip here would hide the bug")
			// Exactly what the driver builds under loc=<zone>: the right components,
			// the wrong location.
			asDriverParsesIt, err := time.ParseInLocation("2006-01-02 15:04:05.000", wallClock, loc)
			require.NoError(t, err)

			got := utcFromColumn(asDriverParsesIt)
			assert.True(t, got.Equal(want),
				"utcFromColumn must reinterpret the components as UTC, not convert them: "+
					"got %s, want %s (offset %s)", got, want, asDriverParsesIt.Format("-07:00"))

			// And the thing the production code used to do, shown failing, so the
			// assertion above cannot be read as "any implementation passes".
			if zone != "UTC" {
				assert.False(t, asDriverParsesIt.UTC().Equal(want),
					"precondition: .UTC() on a driver-tagged value SHIFTS it — that is the bug")
			}
		})
	}
}

// TestAgeIsOffsetInvariantAcrossZones drives the two age helpers' arithmetic the
// way production does, through the same read shape, under every zone.
func TestAgeIsOffsetInvariantAcrossZones(t *testing.T) {
	const wallClock = "2026-09-08 10:45:00.000"
	now := time.Date(2026, 9, 8, 10, 50, 0, 0, time.UTC) // five minutes later

	for _, zone := range []string{"UTC", "Asia/Shanghai", "America/New_York"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			require.NoError(t, err)
			parsed, err := time.ParseInLocation("2006-01-02 15:04:05.000", wallClock, loc)
			require.NoError(t, err)

			oldestUTC, ok := firstUTCFromColumn([]sql.NullTime{{Time: parsed, Valid: true}})
			require.True(t, ok)
			assert.Equal(t, 5*time.Minute, now.Sub(oldestUTC),
				"a five-minute-old queue must read five minutes in every process timezone; "+
					"on Asia/Shanghai the unfixed code read 8h5m, and on America/New_York it "+
					"went negative and clamped to zero — a stuck backlog reading fresh forever")
		})
	}
}

// TestFirstUTCFromColumnHandlesAbsence: no rows and a NULL are the same answer —
// there is nothing to measure the age of — and neither may be reported as an age.
func TestFirstUTCFromColumnHandlesAbsence(t *testing.T) {
	if _, ok := firstUTCFromColumn(nil); ok {
		t.Error("no rows must report absent")
	}
	if _, ok := firstUTCFromColumn([]sql.NullTime{{Valid: false}}); ok {
		t.Error("a NULL must report absent, not the zero time")
	}
}

// TestNoBareTimeSliceScans is the source guard for the silent half.
//
// dbr fills a []time.Time from a bare column with len(rows) ZERO times, n set and
// err nil. Both age gauges in this module were written that way and reported
// about 2.5 million hours on every deployment; the test that should have caught
// it asserted the age was positive, which a garbage-huge number is. No behaviour
// test can catch the shape's return, so the shape itself is banned.
func TestNoBareTimeSliceScans(t *testing.T) {
	for _, file := range moduleSourceFiles(t) {
		src := readStripped(t, file)
		for _, banned := range []string{"[]time.Time", "[]*time.Time"} {
			if strings.Contains(src, banned) {
				t.Errorf("modules/project/%s declares %s. dbr scans a bare DATETIME column "+
					"into it as the ZERO time with no error, so the value silently never "+
					"arrives. Use []sql.NullTime and firstUTCFromColumn — see utcread.go.",
					file, banned)
			}
		}
	}
}
