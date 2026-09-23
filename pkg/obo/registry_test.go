package obo

import "testing"

func TestActionRegistryKeepsBuiltInsAndAllowsConfiguredActions(t *testing.T) {
	r, err := ParseActionRegistry(`{"task.read":["ALL"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Allows("project.read") || !r.AllowsScope("project.read", "ALL") || !r.Allows("task.read") || r.Allows("task.write") {
		t.Fatal("Action allowlist not enforced")
	}
	defaults, err := ParseActionRegistry("")
	if err != nil || !defaults.Allows("project.member.read") || defaults.Allows("task.read") {
		t.Fatalf("unexpected default registry: %v, %v", defaults, err)
	}
}

func TestActionRegistryRejectsAmbiguousConfig(t *testing.T) {
	for _, raw := range []string{
		`{"task.read":[]}`,
		`{"task.read":["PROJECT"]}`,
		`{"task.read":["ALL"],"task.read":["ALL"]}`,
		`{" task.read":["ALL"]}`,
		`{"project.read":["ALL"]}`,
		`{"task.read":{"scope":"ALL"}}`,
		`[]`,
	} {
		if _, err := ParseActionRegistry(raw); err == nil {
			t.Fatalf("accepted invalid registry %s", raw)
		}
	}
}

func TestScopeMatcherRequiresExplicitALLOnBothSides(t *testing.T) {
	if !MatchALL("project.read", []string{"ALL"}) {
		t.Fatal("registered Action with ALL binding denied")
	}
	if MatchALL("unregistered", []string{"ALL"}) || MatchALL("project.read", nil) {
		t.Fatal("missing Action or binding accepted")
	}
}

func TestActionRegistryDoesNotAliasBuiltInScopeMaps(t *testing.T) {
	r, err := ParseActionRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	r.actions["project.read"]["FUTURE"] = struct{}{}
	if _, present := localActions["project.read"]["FUTURE"]; present {
		t.Fatal("registry mutation leaked into built-in Action policy")
	}
}
