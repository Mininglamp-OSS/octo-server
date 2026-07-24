package cardtmpl

import (
	"fmt"
	"testing"
)

func fmtSprint(v any) string { return fmt.Sprint(v) }

// TestJSONTemplate_Meta asserts the generic jsonTemplate exposes the assembled
// meta (id/version/views) after RegisterJSON.
func TestJSONTemplate_Meta(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterJSON(jsonCardTestData, "testdata/test.jsoncard@0.1.0")
	reg.Freeze()
	tmpl, err := reg.Lookup("test.jsoncard", "0.1.0")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	m := tmpl.Meta()
	if m.ID != "test.jsoncard" || m.Version != "0.1.0" {
		t.Fatalf("meta id/version = %s@%s", m.ID, m.Version)
	}
	if _, ok := m.Views["main"]; !ok {
		t.Fatalf("meta.Views missing 'main': %+v", m.Views)
	}
	if v, ok := m.ViewFor("shown"); !ok || v != "main" {
		t.Fatalf("ViewFor(shown) = %q,%v", v, ok)
	}
}
