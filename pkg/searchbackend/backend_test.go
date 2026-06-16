package searchbackend

import (
	"os"
	"testing"
)

func withEnv(t *testing.T, val string, unset bool) {
	t.Helper()
	prev, had := os.LookupEnv(envKey)
	if unset {
		os.Unsetenv(envKey)
	} else {
		os.Setenv(envKey, val)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(envKey, prev)
		} else {
			os.Unsetenv(envKey)
		}
	})
}

// V5/V6 foundation — explicit declaration, no auto-detect. Each declared value
// resolves to the right two-axis posture regardless of ZincSearch.SearchOn.
func TestResolve_ExplicitValues(t *testing.T) {
	cases := []struct {
		env        string
		zincOn     bool
		wantES     bool
		wantLegacy bool
	}{
		{"es", true, true, false},    // es: ES on, NO Zinc fallthrough
		{"es", false, true, false},
		{"ES", false, true, false},   // case-insensitive
		{" es ", false, true, false}, // trimmed
		{"zinc", false, false, true}, // rollback: ES off, Zinc on
		{"zinc", true, false, true},
		{"disabled", true, false, false},
		{"disabled", false, false, false},
	}
	for _, tc := range cases {
		withEnv(t, tc.env, false)
		m := Resolve(tc.zincOn)
		if m.ESServe != tc.wantES || m.LegacyZinc != tc.wantLegacy {
			t.Fatalf("Resolve(%q, zinc=%v) = {ES:%v Legacy:%v}, want {ES:%v Legacy:%v}",
				tc.env, tc.zincOn, m.ESServe, m.LegacyZinc, tc.wantES, tc.wantLegacy)
		}
	}
}

// Default-value semantics (§7-#9 / codex P2): unset MUST preserve current
// deployment behaviour — the new _search* endpoints keep serving from ES, and
// the legacy Zinc message path follows ZincSearch.SearchOn. It must NEVER turn
// the ES endpoints off just because Zinc is on.
func TestResolve_UnsetPreservesCurrentBehaviour(t *testing.T) {
	withEnv(t, "", true)
	if m := Resolve(false); !m.ESServe || m.LegacyZinc {
		t.Fatalf("unset + zincOff: want {ES:true Legacy:false}, got %+v", m)
	}
	withEnv(t, "", true)
	if m := Resolve(true); !m.ESServe || !m.LegacyZinc {
		t.Fatalf("unset + zincOn: ES must STAY on AND legacy on, got %+v", m)
	}
}

func TestResolve_UnknownFallsBackToCurrentBehaviour(t *testing.T) {
	withEnv(t, "elasticsearch", false)
	if m := Resolve(false); !m.ESServe || m.LegacyZinc {
		t.Fatalf("unknown + zincOff: want {ES:true Legacy:false}, got %+v", m)
	}
	withEnv(t, "off", false)
	if m := Resolve(true); !m.ESServe || !m.LegacyZinc {
		t.Fatalf("unknown + zincOn: want {ES:true Legacy:true}, got %+v", m)
	}
}

func TestSearchEnabled(t *testing.T) {
	if !(Mode{ESServe: true}).SearchEnabled() {
		t.Fatal("ES-only mode must be search-enabled")
	}
	if !(Mode{LegacyZinc: true}).SearchEnabled() {
		t.Fatal("Zinc-only mode must be search-enabled")
	}
	if (Mode{}).SearchEnabled() {
		t.Fatal("both-off (disabled) mode must NOT be search-enabled")
	}
}
