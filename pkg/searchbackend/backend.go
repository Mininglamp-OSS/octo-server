// Package searchbackend exposes the single, explicit OCTO_SEARCH_BACKEND
// three-state switch that governs every search surface in octo-server.
//
// There are three surfaces that can return message search results:
//
//  1. modules/messages_search — POST /v1/messages/_search* (OpenSearch).
//  2. modules/message         — POST /v1/message/search (legacy WuKongIM→Zinc).
//  3. modules/search          — POST /v1/search/global (legacy global search,
//     whose MESSAGE portion is WuKongIM→Zinc; friend/group portions are MySQL).
//
// Before this package each surface decided independently whether it was on
// (the new path always tried OS; the legacy paths keyed off
// ZincSearch.SearchOn), there was no "all off" state, and an es deployment
// whose OpenSearch was unreachable had no contract forbidding a silent
// fall-through to Zinc. This package makes the choice explicit and shared.
//
// Two independent surfaces, three explicit declarations + the unset default:
//
//	OCTO_SEARCH_BACKEND   _search* (ES)   legacy Zinc message search
//	──────────────────────────────────────────────────────────────────
//	es                    on              off   (ES is authoritative)
//	zinc                  off (rollback)  on    (read from Zinc again)
//	disabled              off             off   (uniform SEARCH_DISABLED)
//	(unset / unknown)     on              = ZincSearch.SearchOn  (current behaviour, unchanged)
//
// `es` MUST NOT fall through to Zinc: an es deployment whose OpenSearch is down
// surfaces UPSTREAM_UNAVAILABLE, never a Zinc result. `zinc` is the rollback
// target (read stale-free from Zinc while OS is repaired). `disabled` turns
// everything off and the process must still boot with no search backend
// reachable. There is deliberately NO connectivity auto-detection or automatic
// degradation: the backend is whatever the operator declared, full stop.
package searchbackend

import (
	"os"
	"strings"
)

// envKey is the single environment variable that selects the backend.
const envKey = "OCTO_SEARCH_BACKEND"

// Declared is the raw three-state declaration (plus the unset default), kept
// for logging / appconfig diagnostics. Resolve folds it into a Mode.
type Declared string

const (
	DeclaredES       Declared = "es"
	DeclaredZinc     Declared = "zinc"
	DeclaredDisabled Declared = "disabled"
	DeclaredDefault  Declared = "" // unset / unknown
)

// Mode is the resolved two-axis search posture. Each surface consults exactly
// the axis it owns, so the "unset default" case (ES on + legacy following the
// Zinc toggle) is representable without collapsing two surfaces onto one enum.
type Mode struct {
	// ESServe reports whether the OpenSearch _search* endpoints should serve.
	ESServe bool
	// LegacyZinc reports whether the legacy WuKongIM/Zinc MESSAGE search path
	// (/v1/message/search and the message portion of /v1/search/global) should
	// run.
	LegacyZinc bool
	// Declared is the raw declaration this Mode came from (diagnostics only).
	Declared Declared
}

// Resolve returns the search Mode selected by OCTO_SEARCH_BACKEND, given the
// legacy ZincSearch.SearchOn toggle (used only to preserve current behaviour in
// the unset/default case). Pure and tiny so all surfaces can reuse it without
// import cycles.
func Resolve(zincSearchOn bool) Mode {
	switch Declared(strings.ToLower(strings.TrimSpace(os.Getenv(envKey)))) {
	case DeclaredES:
		// ES authoritative; legacy Zinc message search off (no fall-through).
		return Mode{ESServe: true, LegacyZinc: false, Declared: DeclaredES}
	case DeclaredZinc:
		// Rollback: read from Zinc, ES _search* off.
		return Mode{ESServe: false, LegacyZinc: true, Declared: DeclaredZinc}
	case DeclaredDisabled:
		return Mode{ESServe: false, LegacyZinc: false, Declared: DeclaredDisabled}
	default:
		// Unset / unknown: preserve the exact pre-switch behaviour — the new
		// _search* endpoints always queried OS, and the legacy endpoints
		// followed ZincSearch.SearchOn. Never silently flip search off.
		return Mode{ESServe: true, LegacyZinc: zincSearchOn, Declared: DeclaredDefault}
	}
}

// SearchEnabled reports whether ANY search surface is live (advertised to
// clients via appconfig.search_enabled). False only when both surfaces are off
// (the disabled backend).
func (m Mode) SearchEnabled() bool { return m.ESServe || m.LegacyZinc }
