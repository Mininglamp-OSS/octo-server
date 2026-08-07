package aireasoningprocess_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
)

// V4 is the simplified-presentation successor (brief
// cardtmpl-reasoning-phase-tools-successor). Its action surface is identical to
// V3 — the local toggle only — so the assertions here are about the artifact
// contract, the bounded schema, and the node budget the richer per-phase markup
// now spends.

func TestSimplifiedSuccessorArtifactContract(t *testing.T) {
	manifest := readAssetMap(t, reasoningRootV4+"/manifest.json")
	for key, want := range map[string]string{
		"id":                         string(aireasoningprocess.TemplateID),
		"version":                    reasoningVersionV4,
		"contractVersion":            "1.2.0",
		"protocol":                   "octo-card@1.0",
		"adaptiveCardVersion":        "1.5",
		"renderProfile":              "octo-chat@1.2.0-rc.2",
		"renderProfileCompatibility": "octo-chat/v1",
		"owner":                      "ai",
	} {
		if got, _ := manifest[key].(string); got != want {
			t.Fatalf("manifest.%s = %q, want %q", key, got, want)
		}
	}
	if _, exists := manifest["actionType"]; exists {
		t.Fatal("0.4.0 manifest must not advertise actionType")
	}

	// D2: submit_actions is derived from the interaction reports at runtime; a
	// manifest that declares it reintroduces the hand-maintained list #681 refused.
	views, _ := manifest["views"].(map[string]any)
	if len(views) != 3 {
		t.Fatalf("views = %d, want 3", len(views))
	}
	for name, raw := range views {
		view, _ := raw.(map[string]any)
		if _, declared := view["submit_actions"]; declared {
			t.Fatalf("view %q declares submit_actions; it must stay derived", name)
		}
	}
}

// D5/D6: the handoff carries only what LoadJSONBundle consumes. A result report
// would be dropped silently here but fails `unreferenced` on a runtime-assembled
// bundle, and the render-profile package is front-end material no Go code reads.
func TestSimplifiedSuccessorShipsNoUnconsumedAssets(t *testing.T) {
	for _, forbidden := range []string{
		reasoningRootV4 + "/reports/result.interaction.json",
		reasoningRootV4 + "/render-profile/manifest.json",
		reasoningRootV4 + "/render-profile/styles.css",
	} {
		if _, err := aireasoningprocess.Assets.Open(forbidden); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s must not ship in the handoff (err = %v)", forbidden, err)
		}
	}
	entries, err := fs.ReadDir(aireasoningprocess.Assets, reasoningRootV4+"/reports")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 {
		t.Fatalf("reports = %v, want exactly active + error", names)
	}
}

// D3: the bounded contract from #667 survives verbatim. Asserted bound-by-bound
// against the frozen V3 schema rather than by file comparison, because the two
// legitimately differ in `required`, descriptions, and examples.
func TestSimplifiedSuccessorKeepsV3Bounds(t *testing.T) {
	v3 := readAssetMap(t, reasoningRootV3+"/contract/data.schema.json")
	v4 := readAssetMap(t, reasoningRootV4+"/contract/data.schema.json")

	for _, path := range [][]string{
		{"properties", "reasoningId", "maxLength"},
		{"properties", "title", "maxLength"},
		{"properties", "statusLabel", "maxLength"},
		{"properties", "timerText", "maxLength"},
		{"properties", "collapsedSummary", "maxLength"},
		{"properties", "progressText", "maxLength"},
		{"properties", "errorTitle", "maxLength"},
		{"properties", "errorMessage", "maxLength"},
		{"properties", "phases", "maxItems"},
		{"properties", "phases", "items", "properties", "thought", "maxLength"},
		{"properties", "phases", "items", "properties", "actions", "maxItems"},
		{"properties", "phases", "items", "properties", "actions", "items", "properties", "tool", "maxLength"},
		{"properties", "phases", "items", "properties", "actions", "items", "properties", "detail", "maxLength"},
	} {
		key := strings.Join(path, ".")
		want, ok := lookupPath(v3, path)
		if !ok {
			t.Fatalf("V3 schema missing %s", key)
		}
		got, ok := lookupPath(v4, path)
		if !ok {
			t.Fatalf("V4 schema dropped %s", key)
		}
		if canon(t, got) != canon(t, want) {
			t.Fatalf("%s = %v, want %v (V3 bounds are load-bearing)", key, got, want)
		}
	}

	if got, want := canon(t, v4["x-octo-constraints"]), canon(t, v3["x-octo-constraints"]); got != want {
		t.Fatalf("x-octo-constraints drifted\ngot=%s\nwant=%s", got, want)
	}

	// D3: read by no template, sent by no producer.
	items, _ := lookupPath(v4, []string{"properties", "phases", "items", "properties"})
	if properties, _ := items.(map[string]any); properties != nil {
		if _, exists := properties["phaseState"]; exists {
			t.Fatal("V4 schema must not carry phaseState")
		}
	}
}

// D3/D8: timerText is no longer rendered, but the property must survive. The
// root is additionalProperties:false and the producer sends the field
// unconditionally, so deleting it would reject every current payload.
func TestSimplifiedSuccessorKeepsTimerTextOptionalButAccepted(t *testing.T) {
	schema := readAssetMap(t, reasoningRootV4+"/contract/data.schema.json")
	properties, _ := schema["properties"].(map[string]any)
	if _, exists := properties["timerText"]; !exists {
		t.Fatal("timerText property was deleted; additionalProperties:false would reject producer payloads")
	}
	required, _ := schema["required"].([]any)
	for _, name := range required {
		if name == "timerText" {
			t.Fatal("timerText must be optional in V4")
		}
	}

	reg := newRegistry(t)
	// Carrying the field must validate...
	withTimer := readSampleMap(t, reasoningRootV4, "completed")
	if _, exists := withTimer["timerText"]; !exists {
		t.Fatal("completed sample should exercise the retained property")
	}
	if err := renderData(reg, reasoningVersionV4, "completed", withTimer); err != nil {
		t.Fatalf("payload carrying timerText rejected: %v", err)
	}
	// ...and omitting it must also validate.
	without := readSampleMap(t, reasoningRootV4, "completed")
	delete(without, "timerText")
	if err := renderData(reg, reasoningVersionV4, "completed", without); err != nil {
		t.Fatalf("payload omitting timerText rejected: %v", err)
	}
}

// D7: the glyph is the only per-call success/failure marker in the tool rows, so
// it stays data-driven rather than frozen to a template constant.
func TestSimplifiedSuccessorBindsStatusGlyph(t *testing.T) {
	for _, view := range []string{"active", "result", "error"} {
		raw := string(readAsset(t, reasoningRootV4+"/templates/"+view+".template.json"))
		if !strings.Contains(raw, "${statusGlyph}") {
			t.Fatalf("%s template does not bind ${statusGlyph}", view)
		}
	}

	reg := newRegistry(t)
	data := readSampleMap(t, reasoningRootV4, "reasoning")
	const marker = "✗"
	phase := data["phases"].([]any)[0].(map[string]any)
	phase["actions"].([]any)[0].(map[string]any)["statusGlyph"] = marker

	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
		reasoningVersionV4, "reasoning", raw, cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), marker) {
		t.Fatal("producer statusGlyph did not reach the rendered card")
	}
}

// D8: the header simplification is intentional. Locking it keeps a later edit
// from quietly reinstating the line and the badge without a version bump.
func TestSimplifiedSuccessorDropsHeaderTimerAndSemanticIDs(t *testing.T) {
	for _, view := range []string{"active", "result", "error"} {
		raw := string(readAsset(t, reasoningRootV4+"/templates/"+view+".template.json"))
		if strings.Contains(raw, "${timerText}") {
			t.Fatalf("%s template still binds ${timerText}; D8 removed the header line", view)
		}
		for _, prefix := range []string{"octo-surface-", "octo-badge-"} {
			if strings.Contains(raw, prefix) {
				t.Fatalf("%s template declares an %s id; D8b keeps the primitives out", view, prefix)
			}
		}
	}

	reg := newRegistry(t)
	for _, tc := range reasoningStates {
		payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
			reasoningVersionV4, tc.state, readSample(t, reasoningRootV4, string(tc.state)),
			cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, prefix := range []string{"octo-surface-", "octo-badge-"} {
			if strings.Contains(string(encoded), prefix) {
				t.Fatalf("rendered %s card contains %s", tc.state, prefix)
			}
		}
	}
}

// The per-phase markup is the whole point of V4 and it is what spends the node
// budget. Pin both ends: the worst case the schema still admits must render, and
// anything past the aggregate cap must be refused before expansion.
func TestSimplifiedSuccessorNodeBudgetCeiling(t *testing.T) {
	reg := newRegistry(t)
	for _, tc := range reasoningStates {
		t.Run("worst-case/"+string(tc.state), func(t *testing.T) {
			data := readSampleMap(t, reasoningRootV4, "reasoning")
			data["state"] = string(tc.state)
			data["reasoningId"] = strings.Repeat("r", 512)
			data["title"] = strings.Repeat("题", 64)
			data["statusLabel"] = strings.Repeat("状", 32)
			data["timerText"] = strings.Repeat("时", 128)
			data["collapsedSummary"] = strings.Repeat("摘", 160)
			data["progressText"] = strings.Repeat("进", 160)
			data["errorTitle"] = strings.Repeat("错", 64)
			data["errorMessage"] = strings.Repeat("误", 121)
			data["phases"] = worstCasePhases()
			if err := renderData(reg, reasoningVersionV4, tc.state, data); err != nil {
				t.Fatalf("worst case admitted by the schema must render: %v", err)
			}
		})
	}

	// One past the aggregate cap: refused by the schema, never reaching the
	// node walker. This is the guard that keeps a future markup change from
	// silently pushing production payloads over cardmsg.MaxNodes.
	over := readSampleMap(t, reasoningRootV4, "reasoning")
	over["phases"] = phasesWithActionCounts(4, 2, 2, 2, 2, 2) // 14 aggregate
	if err := renderData(reg, reasoningVersionV4, "reasoning", over); !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
		t.Fatalf("14 aggregate actions error = %v, want ErrFieldsInvalid", err)
	}
}

// Toggle ids are data-driven (`${$index}`), so jsontmpl's static target check
// skips them by design and cardmsg's whole-card resolveTargetRefs is the only
// thing left enforcing that no chevron points at a missing element.
func TestSimplifiedSuccessorToggleTargetsResolveForEveryPhaseCount(t *testing.T) {
	reg := newRegistry(t)
	for phases := 1; phases <= 6; phases++ {
		for _, tc := range reasoningStates {
			data := readSampleMap(t, reasoningRootV4, "reasoning")
			data["state"] = string(tc.state)
			// The state-conditional `required` blocks want these present for the
			// error view; harmless for the others.
			data["errorTitle"] = "Generation failed"
			data["errorMessage"] = "upstream timed out"
			counts := make([]int, phases)
			for i := range counts {
				counts[i] = 2
			}
			data["phases"] = phasesWithActionCounts(counts...)
			if err := renderData(reg, reasoningVersionV4, tc.state, data); err != nil {
				t.Fatalf("%d phases / %s: %v", phases, tc.state, err)
			}
		}
	}
}

// The consumer selects a template by view shape, not by version
// (openclaw-channel-octo `selectReasoningProcessTemplate` deliberately has no
// local allowlist). If this shape drifts, the plugin silently stops sending
// reasoning cards, so it is asserted here rather than discovered in an E2E.
func TestSimplifiedSuccessorMatchesConsumerCompatibilityShape(t *testing.T) {
	reg := newRegistry(t)
	tmpl, err := reg.Lookup(aireasoningprocess.TemplateID, reasoningVersionV4)
	if err != nil {
		t.Fatal(err)
	}
	meta := tmpl.Meta()
	want := map[cardtmpl.ViewKey]struct {
		wireProfile string
		states      []string
	}{
		"active": {"octo/v2", []string{"answering", "reasoning"}},
		"result": {"octo/v1", []string{"completed", "stopped"}},
		"error":  {"octo/v2", []string{"error"}},
	}
	if len(meta.Views) != len(want) {
		t.Fatalf("views = %d, want %d", len(meta.Views), len(want))
	}
	for view, expected := range want {
		spec, ok := meta.Views[view]
		if !ok {
			t.Fatalf("view %q missing", view)
		}
		if spec.WireProfile != expected.wireProfile {
			t.Fatalf("view %q wire profile = %q, want %q", view, spec.WireProfile, expected.wireProfile)
		}
		states := make([]string, 0, len(spec.States))
		for _, state := range spec.States {
			states = append(states, string(state))
		}
		sort.Strings(states)
		if !equalStrings(states, expected.states) {
			t.Fatalf("view %q states = %v, want %v", view, states, expected.states)
		}
		// The consumer only tolerates Submit ids it implements; V4 advertises none.
		if report, ok := meta.Interaction(view); ok {
			for _, action := range report.Actions {
				if action.Type == "Action.Submit" {
					t.Fatalf("view %q advertises Submit %q", view, action.ID)
				}
			}
		}
	}
	if meta.ActionContract != nil {
		t.Fatalf("ActionContract = %+v, want nil", meta.ActionContract)
	}
}

func lookupPath(root map[string]any, path []string) (any, bool) {
	var current any = root
	for _, key := range path {
		node, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = node[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
