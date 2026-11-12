package permissionbundle

// Profiles retain the inherited grammar and scalar domains. This checks their
// closed graph/phase/condition bounds. Source-valid input domains, full fixed
// program equivalence and renderer observations are separate support evidence;
// arbitrary model inputs are not substituted for validated operation truths.
func validatePackageProfile(p map[string]any, entries map[string]map[string]any) error {
	caps := map[string]bool{}
	for id := range entries {
		caps[id] = true
	}
	if !catalogProfileReferences(p, caps) {
		return packageError("profile references")
	}
	nodes := 0
	var condition func(map[string]any, int) bool
	condition = func(c map[string]any, depth int) bool {
		nodes++
		if nodes > 64 || depth > 4 {
			return false
		}
		if nested, ok := c["conditions"].([]any); ok {
			for _, v := range nested {
				if !condition(v.(map[string]any), depth+1) {
					return false
				}
			}
		}
		return true
	}
	branches := map[string]bool{}
	for _, v := range p["branches"].([]any) {
		b := v.(map[string]any)
		id := b["id"].(string)
		if branches[id] || !condition(b["condition"].(map[string]any), 1) {
			return packageError("branch/condition")
		}
		branches[id] = true
	}
	for _, phase := range []string{"prepare", "dispatch", "publish"} {
		previous := ""
		for _, v := range p[phase].([]any) {
			a := v.(map[string]any)
			id := a["id"].(string)
			if id <= previous || !condition(a["condition"].(map[string]any), 1) {
				return packageError("action order/condition")
			}
			previous = id
			target := a["target_template"].(map[string]any)
			binding := target["binding"].(map[string]any)
			if binding["kind"] == "capability" && (entries[binding["id"].(string)] == nil || a["capability"] != binding["id"]) {
				return packageError("action capability binding")
			}
			seenDest := map[string]bool{}
			for _, v := range target["variables"].([]any) {
				variable := v.(map[string]any)
				dest := string(canonical(variable["destination"]))
				if seenDest[dest] {
					return packageError("duplicate variable destination")
				}
				seenDest[dest] = true
				source := variable["source"].(map[string]any)
				if source["kind"] != "fixed_output" {
					continue
				}
				name, scope := source["name"].(string), source["scope"].(string)
				if (name == "portable_ref" || name == "portable_location" || name == "portable_download") != (scope == "portable_item") || (name == "page") != (scope == "page") {
					return packageError("fixed output scope")
				}
				if scope == "portable_item" && p["name"] != "portable" && p["name"] != "portable-bytes" {
					return packageError("item output contract")
				}
				if name == "result" && phase != "publish" {
					return packageError("forward result output")
				}
				if scope == "page" {
					found := false
					for _, v := range p["pagination"].([]any) {
						if v.(map[string]any)["action_id"] == id {
							found = true
						}
					}
					if !found {
						return packageError("undeclared page scope")
					}
				}
			}
		}
	}
	return nil
}
