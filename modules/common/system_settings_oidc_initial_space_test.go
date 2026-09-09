package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSystemSettings_OIDCInitialSpaceID_NoInfra pins the read side of
// space.oidc_initial_space_id (task oidc-auto-join-initial-space). Drives the
// snapshot directly, no infra.
//
// The three properties that matter to the consumer:
//   - an unset key means "feature off", not "" as a space_id to look up;
//   - the value is trimmed, so a space_id pasted out of the admin console with
//     trailing whitespace still resolves instead of silently missing on every
//     lookup while reading as configured in the GET response;
//   - a whitespace-only value collapses to off rather than to a lookup for a
//     Space whose id is a space character.
func TestSystemSettings_OIDCInitialSpaceID_NoInfra(t *testing.T) {
	cases := []struct {
		name     string
		snapshot map[string]string
		want     string
	}{
		{
			name:     "unset means off",
			snapshot: map[string]string{},
			want:     "",
		},
		{
			name:     "explicit empty means off",
			snapshot: map[string]string{"space.oidc_initial_space_id": ""},
			want:     "",
		},
		{
			name:     "whitespace only collapses to off",
			snapshot: map[string]string{"space.oidc_initial_space_id": "   \t\n"},
			want:     "",
		},
		{
			name:     "value served verbatim",
			snapshot: map[string]string{"space.oidc_initial_space_id": "sp_abc123"},
			want:     "sp_abc123",
		},
		{
			name:     "surrounding whitespace trimmed",
			snapshot: map[string]string{"space.oidc_initial_space_id": "  sp_abc123\n"},
			want:     "sp_abc123",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &SystemSettings{}
			snap := tc.snapshot
			s.snapshot.Store(&snap)
			assert.Equal(t, tc.want, s.OIDCInitialSpaceID())
		})
	}
}

// TestSystemSettingSchema_OIDCInitialSpaceIDRegistered pins that the key is in
// the schema, because the schema — not the getter — is what the manager write
// path consults: an unregistered (category, key) is rejected with "未知的配置项",
// so a getter without a schema row would leave the setting permanently
// unwritable through the admin API while looking implemented in code.
//
// The Effective hook is exercised too: it is what GET's effective_value renders,
// and a hook that reads a different key than the getter would report a value the
// consumer never sees.
func TestSystemSettingSchema_OIDCInitialSpaceIDRegistered(t *testing.T) {
	def := findSchemaDef("space", "oidc_initial_space_id")
	if !assert.NotNil(t, def, "space.oidc_initial_space_id must be registered in systemSettingSchema") {
		return
	}
	assert.Equal(t, settingTypeString, def.Type)
	assert.NotEmpty(t, def.Description, "admin console renders this as the field help text")

	s := &SystemSettings{}
	snap := map[string]string{"space.oidc_initial_space_id": " sp_effective "}
	s.snapshot.Store(&snap)
	assert.Equal(t, "sp_effective", def.Effective(s),
		"effective_value must agree with the getter the consumer reads")
}
