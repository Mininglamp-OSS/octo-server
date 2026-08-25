package featuregate

import (
	"os"
	"strings"
	"testing"
)

// TestFeatureGateAPINoLegacyResponseError pins that every handler in this module
// goes through the localized error envelope (httperr.ResponseErrorL* via the
// gate* helpers in api_i18n.go) instead of octo-lib's raw error responses.
//
// This guard did not exist when the framework was first written, which is
// precisely why its four manager handlers shipped with
// `c.ResponseError(c.CheckLoginRoleIsSuperAdmin())` — a raw, unlocalized
// rejection. Comments are stripped first so commented-out breadcrumbs (and the
// explanatory note in api_i18n.go about the legacy shape) do not trip the guard;
// m.Error(...) / m.Warn(...) zap LOG calls are not responses and stay allowed.
func TestFeatureGateAPINoLegacyResponseError(t *testing.T) {
	files := []string{"api_manager.go", "api_flags.go", "api_i18n.go", "manager.go"}
	banned := []string{".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(", "AbortWithStatusJSON("}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			var clean strings.Builder
			for _, line := range strings.Split(string(data), "\n") {
				if idx := strings.Index(line, "//"); idx >= 0 {
					line = line[:idx]
				}
				clean.WriteString(line)
				clean.WriteByte('\n')
			}
			cleaned := clean.String()
			for _, b := range banned {
				if strings.Contains(cleaned, b) {
					t.Fatalf("modules/featuregate/%s must use the gate* helpers in api_i18n.go "+
						"(httperr.ResponseErrorLWithStatus + a registered errcode) instead of legacy %s", f, b)
				}
			}
		})
	}
}

// TestHandlerFilesAreGuarded fails when a new api*.go lands without being added
// to the guard list above. Without it the guard silently stops covering new
// handlers — the failure mode is invisible, which is the worst kind.
func TestHandlerFilesAreGuarded(t *testing.T) {
	guarded := map[string]struct{}{
		"api_manager.go": {}, "api_flags.go": {}, "api_i18n.go": {}, "manager.go": {},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read module dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "api") || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := guarded[name]; !ok {
			t.Fatalf("%s is a handler file but is missing from TestFeatureGateAPINoLegacyResponseError's file list", name)
		}
	}
}
