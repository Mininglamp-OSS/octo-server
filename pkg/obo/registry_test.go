package obo

import "testing"

func TestActionRegistryAllowsOnlyConfiguredActions(t *testing.T) {
	r, err := ParseActionRegistry(`{"all":["ALL"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Allows("all") || !r.AllowsScope("all", "ALL") ||
		r.AllowsScope("all", "PROJECT") || r.Allows("project.read") {
		t.Fatal("Action allowlist not enforced")
	}
	empty, err := ParseActionRegistry("")
	if err != nil || empty.Allows("project.read") || empty.Allows("all") {
		t.Fatalf("unexpected empty registry: %v, %v", empty, err)
	}
}

func TestActionRegistryRejectsAmbiguousConfig(t *testing.T) {
	for _, raw := range []string{
		`{"task.read":[]}`,
		`{"task.read":["PROJECT"]}`,
		`{"task.read":["ALL"],"task.read":["ALL"]}`,
		`{" task.read":["ALL"]}`,
		`{"task.read":{"scope":"ALL"}}`,
		`[]`,
	} {
		if _, err := ParseActionRegistry(raw); err == nil {
			t.Fatalf("accepted invalid registry %s", raw)
		}
	}
}

func TestActionRegistriesDoNotShareScopeMaps(t *testing.T) {
	first, err := ParseActionRegistry(`{"all":["ALL"]}`)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseActionRegistry(`{"all":["ALL"]}`)
	if err != nil {
		t.Fatal(err)
	}
	first.actions["all"]["FUTURE"] = struct{}{}
	if second.AllowsScope("all", "FUTURE") {
		t.Fatal("registry mutation leaked into another registry")
	}
}
