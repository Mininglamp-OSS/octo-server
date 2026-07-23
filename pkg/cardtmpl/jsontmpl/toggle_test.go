package jsontmpl

import "testing"

func TestValidateToggleTargets_OK(t *testing.T) {
	tmpl := parseJSON(t, `{
		"body":[
			{"type":"Container","id":"panel_a","items":[]},
			{"type":"Container","id":"panel_b","items":[
				{"type":"ActionSet","actions":[
					{"type":"Action.ToggleVisibility","targetElements":["panel_a","panel_b"]}
				]}
			]}
		]
	}`)
	if err := ValidateToggleTargets(tmpl); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidateToggleTargets_DanglingTarget(t *testing.T) {
	tmpl := parseJSON(t, `{
		"body":[
			{"type":"Container","id":"panel_a","items":[
				{"type":"ActionSet","actions":[
					{"type":"Action.ToggleVisibility","targetElements":["panel_a","ghost_panel"]}
				]}
			]}
		]
	}`)
	err := ValidateToggleTargets(tmpl)
	if err == nil {
		t.Fatal("expected error for dangling toggle target 'ghost_panel'")
	}
}

func TestValidateToggleTargets_TargetObjectForm(t *testing.T) {
	// AC allows {elementId, isVisible} target objects — the reasoning card uses
	// these in its interaction reports; verify the object form is checked too.
	tmpl := parseJSON(t, `{
		"body":[
			{"type":"Container","id":"trace_panel","items":[]},
			{"type":"ActionSet","actions":[
				{"type":"Action.ToggleVisibility","targetElements":[{"elementId":"trace_panel"},{"elementId":"missing"}]}
			]}
		]
	}`)
	if err := ValidateToggleTargets(tmpl); err == nil {
		t.Fatal("expected error for missing elementId 'missing'")
	}
}

func TestValidateToggleTargets_DataDrivenSkipped(t *testing.T) {
	// A ${...} target cannot be resolved statically; it must not be flagged.
	tmpl := parseJSON(t, `{
		"body":[
			{"type":"ActionSet","actions":[
				{"type":"Action.ToggleVisibility","targetElements":["${dynamicId}"]}
			]}
		]
	}`)
	if err := ValidateToggleTargets(tmpl); err != nil {
		t.Fatalf("data-driven target must be skipped, got %v", err)
	}
}
