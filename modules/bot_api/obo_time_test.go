package bot_api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOBOUTCFromColumnIsOffsetInvariant(t *testing.T) {
	want := time.Date(2026, 10, 1, 4, 5, 6, 123000000, time.UTC)
	locations := []*time.Location{
		time.UTC,
		time.FixedZone("UTC+8", 8*60*60),
		time.FixedZone("UTC-4", -4*60*60),
	}
	for _, location := range locations {
		t.Run(location.String(), func(t *testing.T) {
			scanned := time.Date(2026, 10, 1, 4, 5, 6, 123000000, location)
			require.Equal(t, want, oboUTCFromColumn(scanned))
		})
	}
}

func TestNormalizeGenericGrantRowTimestamps(t *testing.T) {
	local := time.FixedZone("UTC+8", 8*60*60)
	revokedAt := time.Date(2026, 9, 30, 3, 0, 0, 0, local)
	expiresAt := time.Date(2026, 10, 1, 4, 0, 0, 0, local)
	grant := &genericGrantRow{RevokedAt: &revokedAt, ExpiresAt: &expiresAt}

	normalizeGenericGrantRowTimestamps(grant)

	require.Equal(t, time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC), *grant.RevokedAt)
	require.Equal(t, time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC), *grant.ExpiresAt)
	require.Nil(t, normalizeGenericGrantRowTimestamps(nil))
}
