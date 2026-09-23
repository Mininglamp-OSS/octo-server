package bot_api

import "time"

// oboUTCFromColumn reinterprets a UTC DATETIME wall clock read through a
// loc=Local connection. The components are correct; only the attached location
// is wrong, so converting with Time.UTC would shift the persisted value.
func oboUTCFromColumn(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(),
		value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), time.UTC)
}

func oboUTCTimePtrFromColumn(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := oboUTCFromColumn(*value)
	return &utc
}

func normalizeGenericGrantRowTimestamps(grant *genericGrantRow) *genericGrantRow {
	if grant == nil {
		return nil
	}
	grant.RevokedAt = oboUTCTimePtrFromColumn(grant.RevokedAt)
	grant.ExpiresAt = oboUTCTimePtrFromColumn(grant.ExpiresAt)
	return grant
}

func normalizeOBOGrantModelTimestamps(grant *oboGrantModel) *oboGrantModel {
	if grant == nil {
		return nil
	}
	grant.RevokedAt = oboUTCTimePtrFromColumn(grant.RevokedAt)
	grant.ExpiresAt = oboUTCTimePtrFromColumn(grant.ExpiresAt)
	return grant
}

func normalizeOBOGrantModelsTimestamps(grants []*oboGrantModel) []*oboGrantModel {
	for _, grant := range grants {
		normalizeOBOGrantModelTimestamps(grant)
	}
	return grants
}
