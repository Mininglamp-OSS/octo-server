package common

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
)

func scanLoginSettings(snapshot map[string]string) *SystemSettings {
	s := &SystemSettings{Log: log.NewTLog("SystemSettingsTest")}
	m := map[string]string{}
	for key, value := range snapshot {
		m[key] = value
	}
	s.snapshot.Store(&m)
	return s
}

func TestSystemSettings_ScanLoginEnabledDefaultsTrue(t *testing.T) {
	assert.True(t, scanLoginSettings(nil).ScanLoginEnabled())
}

func TestSystemSettings_ScanLoginEnabledHonoursDBSnapshot(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "0", want: false},
		{value: "1", want: true},
	} {
		s := scanLoginSettings(map[string]string{
			schemaKey("login", "scan_enabled"): tc.value,
		})
		assert.Equal(t, tc.want, s.ScanLoginEnabled(), "value=%s", tc.value)
	}
}
