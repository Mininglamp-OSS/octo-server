package common

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// snapshotFor builds a NoInfra SystemSettings whose snapshot holds exactly the
// given onboarding.* keys.
func welcomeSnapshot(t *testing.T, kv map[string]string) *SystemSettings {
	t.Helper()
	s := &SystemSettings{}
	snap := map[string]string{}
	for k, v := range kv {
		snap["onboarding."+k] = v
	}
	s.snapshot.Store(&snap)
	return s
}

func validWelcomeKV() map[string]string {
	return map[string]string{
		"space_welcome_enabled":       "1",
		"space_welcome_space_id":      "spc_x",
		"space_welcome_active_from":   "2026-07-16T00:00:00Z",
		"space_welcome_message_zh_cn": "欢迎加入",
		"space_welcome_message_en_us": "Welcome aboard",
	}
}

func TestSpaceWelcomeConfig_ReadsAllFiveKeys(t *testing.T) {
	s := welcomeSnapshot(t, validWelcomeKV())
	cfg := s.SpaceWelcomeConfig()
	assert.True(t, cfg.Enabled)
	assert.Equal(t, "spc_x", cfg.SpaceID)
	assert.Equal(t, "2026-07-16T00:00:00Z", cfg.ActiveFromRaw)
	assert.Equal(t, "欢迎加入", cfg.MessageZhCN)
	assert.Equal(t, "Welcome aboard", cfg.MessageEnUS)

	at, ok := cfg.ParsedActiveFrom()
	assert.True(t, ok)
	assert.Equal(t, time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC), at)
}

func TestSpaceWelcomeConfig_Defaults(t *testing.T) {
	s := welcomeSnapshot(t, map[string]string{})
	cfg := s.SpaceWelcomeConfig()
	assert.False(t, cfg.Enabled)
	assert.Empty(t, cfg.SpaceID)
	_, ok := cfg.ParsedActiveFrom()
	assert.False(t, ok)
}

// TestSpaceWelcomeConfig_SnapshotAtomicity swaps between two complete but
// distinct tuples while a reader loops. Every read must return one whole tuple,
// never a mix of the two — this is the property the single-snapshot accessor
// guarantees. Run under -race.
func TestSpaceWelcomeConfig_SnapshotAtomicity(t *testing.T) {
	s := &SystemSettings{}
	tupleA := map[string]string{
		"onboarding.space_welcome_enabled":       "1",
		"onboarding.space_welcome_space_id":      "spc_a",
		"onboarding.space_welcome_active_from":   "2026-01-01T00:00:00Z",
		"onboarding.space_welcome_message_zh_cn": "A-zh",
		"onboarding.space_welcome_message_en_us": "A-en",
	}
	tupleB := map[string]string{
		"onboarding.space_welcome_enabled":       "1",
		"onboarding.space_welcome_space_id":      "spc_b",
		"onboarding.space_welcome_active_from":   "2026-02-02T00:00:00Z",
		"onboarding.space_welcome_message_zh_cn": "B-zh",
		"onboarding.space_welcome_message_en_us": "B-en",
	}
	s.snapshot.Store(&tupleA)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		toggle := false
		for {
			select {
			case <-stop:
				return
			default:
				if toggle {
					s.snapshot.Store(&tupleA)
				} else {
					s.snapshot.Store(&tupleB)
				}
				toggle = !toggle
			}
		}
	}()

	for i := 0; i < 5000; i++ {
		cfg := s.SpaceWelcomeConfig()
		switch cfg.SpaceID {
		case "spc_a":
			assert.Equal(t, "A-zh", cfg.MessageZhCN)
			assert.Equal(t, "A-en", cfg.MessageEnUS)
			assert.Equal(t, "2026-01-01T00:00:00Z", cfg.ActiveFromRaw)
		case "spc_b":
			assert.Equal(t, "B-zh", cfg.MessageZhCN)
			assert.Equal(t, "B-en", cfg.MessageEnUS)
			assert.Equal(t, "2026-02-02T00:00:00Z", cfg.ActiveFromRaw)
		default:
			t.Fatalf("torn read: space_id=%q", cfg.SpaceID)
		}
	}
	close(stop)
	wg.Wait()
}

func TestValidateSpaceWelcomeCombination(t *testing.T) {
	activeSpace := func(string) (bool, error) { return true, nil }
	longMsg := make([]rune, spaceWelcomeMessageMaxRunes+1)
	for i := range longMsg {
		longMsg[i] = 'x'
	}

	cases := []struct {
		name      string
		mutate    func(*SpaceWelcomeConfig)
		checker   func(string) (bool, error)
		wantField string
	}{
		{"disabled always ok", func(c *SpaceWelcomeConfig) { c.Enabled = false; c.SpaceID = "" }, nil, ""},
		{"valid", nil, activeSpace, ""},
		{"missing space_id", func(c *SpaceWelcomeConfig) { c.SpaceID = "  " }, activeSpace, "space_welcome_space_id"},
		{"bad time", func(c *SpaceWelcomeConfig) { c.ActiveFromRaw = "not-a-time" }, activeSpace, "space_welcome_active_from"},
		{"empty zh", func(c *SpaceWelcomeConfig) { c.MessageZhCN = "   " }, activeSpace, "space_welcome_message_zh_cn"},
		{"empty en", func(c *SpaceWelcomeConfig) { c.MessageEnUS = "" }, activeSpace, "space_welcome_message_en_us"},
		{"oversize zh", func(c *SpaceWelcomeConfig) { c.MessageZhCN = string(longMsg) }, activeSpace, "space_welcome_message_zh_cn"},
		{"space not active", nil, func(string) (bool, error) { return false, nil }, "space_welcome_space_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := SpaceWelcomeConfig{
				Enabled:       true,
				SpaceID:       "spc_x",
				ActiveFromRaw: "2026-07-16T00:00:00Z",
				MessageZhCN:   "欢迎",
				MessageEnUS:   "Welcome",
			}
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			field, err := ValidateSpaceWelcomeCombination(cfg, tc.checker)
			assert.NoError(t, err)
			assert.Equal(t, tc.wantField, field)
		})
	}
}

func TestValidateSpaceWelcomeCombination_SpaceCheckError(t *testing.T) {
	cfg := SpaceWelcomeConfig{
		Enabled: true, SpaceID: "spc_x", ActiveFromRaw: "2026-07-16T00:00:00Z",
		MessageZhCN: "欢迎", MessageEnUS: "Welcome",
	}
	_, err := ValidateSpaceWelcomeCombination(cfg, func(string) (bool, error) {
		return false, assert.AnError
	})
	assert.Error(t, err, "infra error must surface, not be masked as a validation field")
}

// TestValidateSpaceWelcomeCombination_Prospective covers the two directions the
// prospective-merge write path must get right, by validating the merged tuple
// directly (the handler builds the same merge inline).
func TestValidateSpaceWelcomeCombination_Prospective(t *testing.T) {
	activeSpace := func(string) (bool, error) { return true, nil }

	// (1) Valid current snapshot; a patch that alone looks fine but whose merge
	// is invalid (blanks the zh message) must be rejected.
	current := SpaceWelcomeConfig{
		Enabled: true, SpaceID: "spc_x", ActiveFromRaw: "2026-07-16T00:00:00Z",
		MessageZhCN: "欢迎", MessageEnUS: "Welcome",
	}
	merged := current
	merged.MessageZhCN = "" // incoming patch clears zh
	field, err := ValidateSpaceWelcomeCombination(merged, activeSpace)
	assert.NoError(t, err)
	assert.Equal(t, "space_welcome_message_zh_cn", field, "merge that breaks the composite must be rejected")

	// (2) Invalid current snapshot (enabled but no space_id); a patch that adds a
	// valid space_id repairs the composite and must be accepted.
	broken := SpaceWelcomeConfig{
		Enabled: true, SpaceID: "", ActiveFromRaw: "2026-07-16T00:00:00Z",
		MessageZhCN: "欢迎", MessageEnUS: "Welcome",
	}
	repaired := broken
	repaired.SpaceID = "spc_x" // incoming patch supplies the space
	field, err = ValidateSpaceWelcomeCombination(repaired, activeSpace)
	assert.NoError(t, err)
	assert.Empty(t, field, "patch that repairs the composite must be accepted")
}
