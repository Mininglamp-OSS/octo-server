package aireasoningprocess_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
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

// D3: the bounded contract from #667 survives verbatim, with one deliberate
// relaxation (thought, see D3a). Asserted bound-by-bound against the frozen V3
// schema rather than by file comparison, because the two legitimately differ in
// `required`, descriptions, and examples.
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
		{"properties", "phases", "items", "properties", "actions", "maxItems"},
		{"properties", "phases", "items", "properties", "actions", "items", "properties", "tool", "maxLength"},
		{"properties", "phases", "items", "properties", "actions", "items", "properties", "detail", "maxLength"},
		// D7 restored the ${statusGlyph} binding, so this bound is load-bearing
		// again: the value now lands in TextBlock text, and a 2-rune cap is what
		// keeps it too short to forge markdown.
		{"properties", "phases", "items", "properties", "actions", "items", "properties", "statusGlyph", "maxLength"},
		{"properties", "phases", "items", "properties", "actions", "items", "properties", "statusGlyph", "minLength"},
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

	// The aggregate cap must be verbatim; V4 additionally declares truncateStrings,
	// which V3 has no equivalent for (see TestSimplifiedSuccessorTruncatesThought).
	v3Constraints, _ := v3["x-octo-constraints"].(map[string]any)
	v4Constraints, _ := v4["x-octo-constraints"].(map[string]any)
	if v3Constraints == nil || v4Constraints == nil {
		t.Fatal("both schemas must declare x-octo-constraints")
	}
	if got, want := canon(t, v4Constraints["aggregateArrayLimits"]),
		canon(t, v3Constraints["aggregateArrayLimits"]); got != want {
		t.Fatalf("aggregateArrayLimits drifted\ngot=%s\nwant=%s", got, want)
	}
	for key := range v3Constraints {
		if _, present := v4Constraints[key]; !present {
			t.Fatalf("V4 x-octo-constraints dropped %q", key)
		}
	}
	if _, present := v3Constraints["truncateStrings"]; present {
		t.Fatal("V3 must not declare truncateStrings; it is frozen with fail-close bounds")
	}

	// D3: read by no template, sent by no producer.
	items, _ := lookupPath(v4, []string{"properties", "phases", "items", "properties"})
	if properties, _ := items.(map[string]any); properties != nil {
		if _, exists := properties["phaseState"]; exists {
			t.Fatal("V4 schema must not carry phaseState")
		}
	}
}

// D3a: `thought` separates two ceilings. The schema's `maxLength` is the *accept*
// ceiling — a pathological payload is still refused, and an unbounded string would
// fail RequireBoundedSchema. The narrower *display* ceiling is declared in
// x-octo-constraints.truncateStrings and applied after validation, so a long
// summary renders truncated instead of failing the card. Callers asked for
// "<= 400 is fine, truncate above it, don't error".
func TestSimplifiedSuccessorTruncatesThought(t *testing.T) {
	reg := newRegistry(t)
	display := reasoningThoughtDisplayMax
	accept := reasoningThoughtMax(t, reasoningVersionV4)

	// The schema must declare both, and the display ceiling must sit under the
	// accept ceiling — otherwise the value is rejected before truncation applies.
	schema := readAssetMap(t, reasoningRootV4+"/contract/data.schema.json")
	thought, _ := lookupPath(schema, []string{"properties", "phases", "items", "properties", "thought"})
	thoughtSpec, _ := thought.(map[string]any)
	if thoughtSpec == nil {
		t.Fatal("thought missing from V4 schema")
	}
	if got := jsonNumber(t, thoughtSpec["maxLength"]); got != accept {
		t.Fatalf("thought accept ceiling = %d, want %d", got, accept)
	}
	constraints, _ := schema["x-octo-constraints"].(map[string]any)
	declared, _ := constraints["truncateStrings"].([]any)
	if len(declared) != 1 {
		t.Fatalf("truncateStrings = %v, want exactly the thought entry", declared)
	}
	entry, _ := declared[0].(map[string]any)
	if got, _ := entry["arrayField"].(string); got != "phases" {
		t.Fatalf("truncateStrings[0].arrayField = %q, want phases", got)
	}
	if got, _ := entry["field"].(string); got != "thought" {
		t.Fatalf("truncateStrings[0].field = %q, want thought", got)
	}
	if got := jsonNumber(t, entry["maxRunes"]); got != display {
		t.Fatalf("truncateStrings[0].maxRunes = %d, want %d", got, display)
	}
	ellipsis, _ := entry["ellipsis"].(string)
	if ellipsis == "" {
		t.Fatal("truncateStrings[0].ellipsis must be set so a cut is visible to the reader")
	}

	renderedThoughtRunes := func(t *testing.T, input string) (int, bool) {
		t.Helper()
		data := readSampleMap(t, reasoningRootV4, "reasoning")
		setFirstThought(data, input)
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
			reasoningVersionV4, "reasoning", raw, cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
		if err != nil {
			t.Fatalf("input of %d runes rejected: %v", len([]rune(input)), err)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		// The fixture rune repeats, so the longest run in the frame is the thought.
		longest, run := 0, 0
		for _, r := range string(encoded) {
			if r == '思' {
				run++
				if run > longest {
					longest = run
				}
				continue
			}
			run = 0
		}
		return longest, strings.Contains(string(encoded), "思"+ellipsis)
	}

	// At or below the display ceiling: untouched, no ellipsis.
	for _, n := range []int{1, display - 1, display} {
		got, cut := renderedThoughtRunes(t, strings.Repeat("思", n))
		if got != n || cut {
			t.Fatalf("input %d runes rendered %d runes (truncated=%v), want %d untouched", n, got, cut, n)
		}
	}

	// Above it, up to the accept ceiling: clamped to exactly the display ceiling
	// including the ellipsis, and never rejected.
	for _, n := range []int{display + 1, display * 2, accept} {
		got, cut := renderedThoughtRunes(t, strings.Repeat("思", n))
		if !cut {
			t.Fatalf("input %d runes was not truncated; the reader gets no cut marker", n)
		}
		if want := display - len([]rune(ellipsis)); got != want {
			t.Fatalf("input %d runes rendered %d runes of text, want %d (+%q = %d total)",
				n, got, want, ellipsis, display)
		}
	}

	// Past the accept ceiling it is still refused — truncation is not a licence for
	// unbounded input.
	data := readSampleMap(t, reasoningRootV4, "reasoning")
	setFirstThought(data, strings.Repeat("思", accept+1))
	if err := renderData(reg, reasoningVersionV4, "reasoning", data); !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
		t.Fatalf("input of %d runes error = %v, want ErrFieldsInvalid", accept+1, err)
	}

	// V2/V3 are frozen fail-close: no truncation, so the accept ceiling is the
	// display ceiling and one rune over is rejected.
	for _, version := range []string{aireasoningprocess.TemplateVersionV2, reasoningVersionV3} {
		if reasoningThoughtTruncates(version) {
			t.Fatalf("%s must not truncate", version)
		}
		root := aireasoningprocess.HandoffRootV2
		if version == reasoningVersionV3 {
			root = reasoningRootV3
		}
		frozen := readSampleMap(t, root, "reasoning")
		setFirstThought(frozen, strings.Repeat("思", reasoningThoughtMax(t, version)+1))
		if err := renderData(reg, version, "reasoning", frozen); !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
			t.Fatalf("%s over-limit error = %v, want ErrFieldsInvalid", version, err)
		}
	}
}

// D3a: `thought` is the one bound V4 deliberately widens. V2/V3 pinned it to the
// producer's observed output (280 + `…` = 281); V4 sets a platform product cap
// instead. Assert the exact new value and the direction — a future edit that
// narrows it below V3, or that quietly drops the bound entirely, is a contract
// regression the fail-close publish path would otherwise be the only thing to
// notice.
func TestSimplifiedSuccessorWidensThoughtBound(t *testing.T) {
	thoughtPath := []string{"properties", "phases", "items", "properties", "thought"}
	v3, _ := lookupPath(readAssetMap(t, reasoningRootV3+"/contract/data.schema.json"), thoughtPath)
	v4, _ := lookupPath(readAssetMap(t, reasoningRootV4+"/contract/data.schema.json"), thoughtPath)
	v3Spec, _ := v3.(map[string]any)
	v4Spec, _ := v4.(map[string]any)
	if v3Spec == nil || v4Spec == nil {
		t.Fatal("thought missing from V3 or V4 schema")
	}

	v3Max, v4Max := jsonNumber(t, v3Spec["maxLength"]), jsonNumber(t, v4Spec["maxLength"])
	if want := reasoningThoughtMax(t, reasoningVersionV4); v4Max != want {
		t.Fatalf("V4 thought maxLength = %d, want %d", v4Max, want)
	}
	if v3Max != reasoningThoughtMax(t, reasoningVersionV3) {
		t.Fatalf("V3 thought maxLength = %d, want it frozen at %d",
			v3Max, reasoningThoughtMax(t, reasoningVersionV3))
	}
	if v4Max <= v3Max {
		t.Fatalf("V4 thought maxLength = %d must be a widening of V3's %d", v4Max, v3Max)
	}
	// Still bounded: an unbounded string fails DefaultCompileLimits' fail-close
	// publish path, and trust-boundary requires an explicit ceiling. Assert the
	// lower bound's *value* against V3, not just its presence — `minLength: 0`
	// would satisfy a presence check while admitting the empty string.
	if v4Spec["maxLength"] == nil {
		t.Fatal("thought must keep an explicit upper bound")
	}
	if v3Min, v4Min := jsonNumber(t, v3Spec["minLength"]), jsonNumber(t, v4Spec["minLength"]); v4Min != v3Min {
		t.Fatalf("thought minLength = %d, want V3's %d (only the ceiling moves)", v4Min, v3Min)
	}

	// The widened ceiling must actually render at the worst case, not just
	// validate: 6 phases each carrying a max-length thought.
	reg := newRegistry(t)
	for _, tc := range reasoningStates {
		data := readSampleMap(t, reasoningRootV4, "reasoning")
		data["state"] = string(tc.state)
		data["errorTitle"] = "Generation failed"
		data["errorMessage"] = "upstream timed out"
		data["phases"] = worstCasePhases(v4Max)
		if err := renderData(reg, reasoningVersionV4, tc.state, data); err != nil {
			t.Fatalf("%s at the widened thought ceiling: %v", tc.state, err)
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
			data["phases"] = worstCasePhases(reasoningThoughtMax(t, reasoningVersionV4))
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

// V4's toggle ids are data-driven (`reasoning_tools_toggle_action_${$index}`),
// so an interaction report can only honestly declare the actions that exist in
// every frame. The engine never notices the difference — assertInteractionReport
// compares Action.Submit and Input ids only, and the Bot capability skips
// non-Submit entries — so an indexed id in the report would be a published lie
// that no gate catches. Assert both halves: nothing index-suffixed is declared,
// and everything declared really is present in every state.
func TestSimplifiedSuccessorReportDeclaresOnlyDataIndependentActions(t *testing.T) {
	reg := newRegistry(t)
	tmpl, err := reg.Lookup(aireasoningprocess.TemplateID, reasoningVersionV4)
	if err != nil {
		t.Fatal(err)
	}
	meta := tmpl.Meta()

	indexed := regexp.MustCompile(`_\d+$`)
	viewStates := map[cardtmpl.ViewKey][]string{
		"active": {"reasoning", "answering"},
		"error":  {"error"},
	}
	for view, states := range viewStates {
		report, ok := meta.Interaction(view)
		if !ok {
			t.Fatalf("interaction report missing for %s", view)
		}
		if len(report.Inputs) != 0 {
			t.Fatalf("%s declares inputs %+v, want none", view, report.Inputs)
		}
		if len(report.Actions) == 0 {
			t.Fatalf("%s declares no actions", view)
		}
		for _, action := range report.Actions {
			if indexed.MatchString(action.ID) {
				t.Fatalf("%s declares data-driven id %q; a report cannot enumerate per-phase toggles",
					view, action.ID)
			}
		}

		// Every declared id must appear in every state this view serves, at every
		// phase count the schema admits.
		for _, state := range states {
			for phases := 1; phases <= 6; phases++ {
				data := readSampleMap(t, reasoningRootV4, "reasoning")
				data["state"] = state
				data["errorTitle"] = "Generation failed"
				data["errorMessage"] = "upstream timed out"
				counts := make([]int, phases)
				for i := range counts {
					counts[i] = 1
				}
				data["phases"] = phasesWithActionCounts(counts...)

				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
					reasoningVersionV4, cardtmpl.State(state), raw,
					cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
				if err != nil {
					t.Fatal(err)
				}
				var rendered []map[string]any
				collectActions(payload, &rendered)
				present := make(map[string]struct{}, len(rendered))
				for _, action := range rendered {
					if id, _ := action["id"].(string); id != "" {
						present[id] = struct{}{}
					}
				}
				for _, action := range report.Actions {
					if _, ok := present[action.ID]; !ok {
						t.Fatalf("%s report declares %q, absent from %s with %d phase(s)",
							view, action.ID, state, phases)
					}
				}
			}
		}
	}
}

// Acceptance B3: the runtime publish path applies RequireOwner /
// RequireProtocol / RequireBoundedSchema, all of which static registration
// skips. Compiling under both profiles is what proves the manifest and schema
// corrections actually landed, rather than only satisfying the laxer path.
func TestSimplifiedSuccessorCompilesUnderRuntimePublishLimits(t *testing.T) {
	bundle, err := cardtmpl.LoadJSONBundle(aireasoningprocess.Assets, reasoningRootV4,
		cardtmpl.CatalogDescriptor{
			Engine:     cardtmpl.JSONTemplateEngineV1,
			Visibility: cardtmpl.CatalogVisibilityPublic,
		})
	if err != nil {
		t.Fatalf("LoadJSONBundle: %v", err)
	}
	artifact, err := cardtmpl.CompileJSONArtifact(context.Background(), bundle, cardtmpl.DefaultCompileLimits())
	if err != nil {
		t.Fatalf("compile under DefaultCompileLimits: %v", err)
	}
	if artifact.Owner != "ai" {
		t.Fatalf("compiled owner = %q, want ai", artifact.Owner)
	}
	if artifact.ContractVersion != "1.2.0" {
		t.Fatalf("compiled contractVersion = %q, want 1.2.0", artifact.ContractVersion)
	}
}

// Acceptance D1: the frozen-out controls must not reappear anywhere in the
// shipped artifact or in a rendered frame.
func TestSimplifiedSuccessorCarriesNoLegacyControlVocabulary(t *testing.T) {
	forbidden := []string{
		"Action.Submit", "reasoning_stop", "reasoning_retry",
		"stop_reasoning", "retry_reasoning", "可以重试", "phaseState",
	}
	for _, relative := range []string{
		"templates/active.template.json", "templates/result.template.json",
		"templates/error.template.json",
		"reports/active.interaction.json", "reports/error.interaction.json",
		"contract/data.schema.json",
		"goldens/reasoning.card.json", "goldens/answering.card.json",
		"goldens/completed.card.json", "goldens/stopped.card.json",
		"goldens/error.card.json",
	} {
		content := string(readAsset(t, reasoningRootV4+"/"+relative))
		for _, needle := range forbidden {
			if strings.Contains(content, needle) {
				t.Fatalf("%s contains %q", relative, needle)
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
		for _, needle := range forbidden {
			if strings.Contains(string(encoded), needle) {
				t.Fatalf("rendered %s frame contains %q", tc.state, needle)
			}
		}
	}
}

// Acceptance D5: `result` is octo/v1 and ships no interaction report, so the
// wire profile — not the golden — is what forbids Action.Submit there. Prove the
// golden cannot launder a forged control: mutate the result template to carry a
// Submit AND update the two result goldens to match it, so the golden gate would
// pass. Compilation must still fail, because selfCheckCompiledJSON runs
// cardmsg.Validate before checkArtifactGoldens is ever reached.
func TestSimplifiedSuccessorResultFrameRejectsSubmitEvenWithMatchingGolden(t *testing.T) {
	bundle, err := cardtmpl.LoadJSONBundle(aireasoningprocess.Assets, reasoningRootV4,
		cardtmpl.CatalogDescriptor{
			Engine:     cardtmpl.JSONTemplateEngineV1,
			Visibility: cardtmpl.CatalogVisibilityPublic,
		})
	if err != nil {
		t.Fatalf("LoadJSONBundle: %v", err)
	}
	const forged = `[{"type":"Action.Submit","id":"forged_submit","title":"probe"}]`
	injectTopLevelActions := func(t *testing.T, doc json.RawMessage) json.RawMessage {
		t.Helper()
		var card map[string]any
		if err := json.Unmarshal(doc, &card); err != nil {
			t.Fatalf("unmarshal document: %v", err)
		}
		if _, exists := card["actions"]; exists {
			t.Fatal("result frame already declares top-level actions; this probe assumes none")
		}
		card["actions"] = json.RawMessage(forged)
		mutated, err := json.Marshal(card)
		if err != nil {
			t.Fatalf("marshal document: %v", err)
		}
		return mutated
	}

	bundle.Templates["result"] = injectTopLevelActions(t, bundle.Templates["result"])
	for _, state := range []string{"completed", "stopped"} {
		golden, ok := bundle.Goldens[state]
		if !ok {
			t.Fatalf("golden %q missing from bundle (keys drifted)", state)
		}
		bundle.Goldens[state] = injectTopLevelActions(t, golden)
	}

	if _, err := cardtmpl.CompileJSONArtifact(context.Background(), bundle,
		cardtmpl.DefaultCompileLimits()); err == nil {
		t.Fatal("octo/v1 result view accepted Action.Submit with a matching golden")
	} else if got := err.Error(); !strings.Contains(got, "cardmsg.Validate") ||
		!strings.Contains(got, "Action.Submit") || !strings.Contains(got, "octo/v1") {
		t.Fatalf("error = %q, want an octo/v1 cardmsg profile rejection "+
			"(a golden mismatch here would mean the frame check is not the guard)", got)
	}
}

// D3a persistence ceiling — the contract must not admit what the store cannot
// hold. `cardmsg.MaxPayloadBytes` (512 KiB) is the only size gate a rendered
// frame passes, but it is not the smallest one downstream: the authoritative
// bot-edit write puts the whole marshalled frame into
// `message_extra.content_edit` (`internal/carddispatch/mutation.go`), and that
// column's width is discovered only at INSERT time — as `Data too long for
// column` under STRICT_TRANS_TABLES, or a silent truncation into invalid JSON
// without it.
//
// So this asserts the invariant directly: at the schema's own ceiling, under the
// worst-case *byte* encoding, the frame still fits the column. Two things make
// that non-trivial and are covered explicitly:
//
//   - JSON Schema counts code points while the column counts bytes, and Go
//     escapes `<`, `>`, `&`, U+2028 and U+2029 to six bytes each. The CJK
//     fixture the other tests use is therefore NOT the byte worst case, and
//     neither is escaping `thought` alone — every free string has to escape.
//   - the column width is read out of the migration rather than hand-copied, so
//     widening it to MEDIUMTEXT is observable here instead of leaving the
//     ceiling silently over-conservative.
func TestSimplifiedSuccessorCeilingIsPersistenceSafe(t *testing.T) {
	columnBytes := contentEditColumnBytes(t)
	reg := newRegistry(t)

	// escapeAll drives every free string to 6 bytes per code point, not just
	// `thought`: the other fields sit in the same frame and inflate the baseline
	// with it (measured: 30,036 B of baseline as CJK vs 42,024 B escaped).
	worstFrameBytes := func(t *testing.T, thoughtLen int, escapeAll bool) int {
		t.Helper()
		filler := func(n int, cjk string) string {
			if escapeAll {
				return strings.Repeat("<", n)
			}
			return strings.Repeat(cjk, n)
		}
		var maxSeen int
		for _, tc := range reasoningStates {
			data := readSampleMap(t, reasoningRootV4, "reasoning")
			data["state"] = string(tc.state)
			data["reasoningId"] = strings.Repeat("r", 512)
			data["title"] = filler(64, "题")
			data["statusLabel"] = filler(32, "状")
			data["timerText"] = filler(128, "时")
			data["collapsedSummary"] = filler(160, "摘")
			data["progressText"] = filler(160, "进")
			data["errorTitle"] = filler(64, "错")
			data["errorMessage"] = filler(121, "误")
			phases := phasesWithActionCounts(3, 2, 2, 2, 2, 2)
			for _, rawPhase := range phases {
				phase := rawPhase.(map[string]any)
				phase["thought"] = filler(thoughtLen, "思")
				for _, rawAction := range phase["actions"].([]any) {
					action := rawAction.(map[string]any)
					action["tool"] = filler(81, "工")
					action["detail"] = filler(192, "详")
				}
			}
			data["phases"] = phases
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
				reasoningVersionV4, tc.state, raw, cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
			if err != nil {
				t.Fatalf("thought=%d escapeAll=%v state=%s: %v", thoughtLen, escapeAll, tc.state, err)
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > maxSeen {
				maxSeen = len(encoded)
			}
		}
		return maxSeen
	}

	ceiling := reasoningThoughtMax(t, reasoningVersionV4)
	cjk := worstFrameBytes(t, ceiling, false)
	escaped := worstFrameBytes(t, ceiling, true)

	// The invariant. If this fails, the advertised contract accepts frames the
	// authoritative write cannot persist — a bot can send schema-valid data and
	// get a 500 with the card frozen mid-stream.
	if escaped > columnBytes {
		t.Fatalf("frame at the schema ceiling (thought=%d, every free string escaped) = %d B, "+
			"over message_extra.content_edit's %d B: the contract would admit payloads the "+
			"authoritative write cannot store. Lower the ceiling, gate the frame at the "+
			"persistence boundary, or widen the column — and update D3a with it",
			ceiling, escaped, columnBytes)
	}
	// Escaping only `thought` understates the maximum; guard the fixture choice
	// so a future edit cannot quietly reintroduce a CJK-only "worst case".
	if escaped <= cjk {
		t.Fatalf("escape-heavy frame (%d B) should exceed the CJK frame (%d B): Go escapes "+
			"<, >, &, U+2028, U+2029 to 6 bytes, so CJK is not the byte worst case", escaped, cjk)
	}
	if escaped > cardmsg.MaxPayloadBytes {
		t.Fatalf("escape-heavy frame = %d B exceeds cardmsg.MaxPayloadBytes (%d)", escaped, cardmsg.MaxPayloadBytes)
	}
	t.Logf("ceiling %d cp/phase: %d B CJK / %d B fully escaped, both within the %d B column (%.0f%% used)",
		ceiling, cjk, escaped, columnBytes, float64(escaped)/float64(columnBytes)*100)
}

// contentEditColumnBytes derives the persistence ceiling from the migrations that
// declare or alter the column, so that widening it (the MEDIUMTEXT follow-up D3a
// anticipates) changes this test's behaviour instead of leaving a hand-copied
// constant behind. The whole migration directory is scanned in filename order and
// the last declaration wins, because a widening would land in a *new* migration
// rather than by editing the original one.
func contentEditColumnBytes(t *testing.T) int {
	t.Helper()
	const migrationDir = "../../../modules/message/sql"
	entries, err := os.ReadDir(migrationDir)
	if err != nil {
		t.Fatalf("read %s: %v", migrationDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names) // migrations are date-prefixed, so name order is apply order
	widths := map[string]int{
		"TINYTEXT":   255,
		"TEXT":       65535,
		"MEDIUMTEXT": 16777215,
		"LONGTEXT":   4294967295,
	}
	re := regexp.MustCompile(`(?i)content_edit\s+(TINYTEXT|TEXT|MEDIUMTEXT|LONGTEXT)`)
	width := 0
	var declaredIn, declaredAs string
	for _, name := range names {
		raw, err := os.ReadFile(migrationDir + "/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if match := re.FindSubmatch(raw); match != nil {
			declaredAs = strings.ToUpper(string(match[1]))
			w, ok := widths[declaredAs]
			if !ok {
				t.Fatalf("unknown column type %q for content_edit in %s", declaredAs, name)
			}
			width, declaredIn = w, name
		}
	}
	if width == 0 {
		t.Fatalf("no content_edit text-column declaration found under %s", migrationDir)
	}
	t.Logf("content_edit is %s (%d B) per %s", declaredAs, width, declaredIn)
	return width
}

// D4a: the chevrons are inline `data:image/svg+xml` icons, not a third-party fetch.
// The shape this replaced pulled them from api.iconify.design, which made every
// viewer of every reasoning card hit an external host — and because rendered cards
// are persisted immutably, that reference would have been permanent. Pin the
// outcome: no outbound runtime URL at all, in the templates or the goldens.
//
// `cardmsg`'s checkURL only constrains the scheme, and a template-authored URL is
// server-authored, so nothing else here would notice an external reference coming
// back.
func TestSimplifiedSuccessorHasNoOutboundRuntimeDependency(t *testing.T) {
	urlPattern := regexp.MustCompile(`https?://[^"\s]+`)
	for _, relative := range []string{
		"templates/active.template.json", "templates/result.template.json",
		"templates/error.template.json",
		"goldens/reasoning.card.json", "goldens/answering.card.json",
		"goldens/completed.card.json", "goldens/stopped.card.json",
		"goldens/error.card.json",
	} {
		content := string(readAsset(t, reasoningRootV4+"/"+relative))
		for _, found := range urlPattern.FindAllString(content, -1) {
			switch {
			case strings.HasPrefix(found, "http://adaptivecards.io/schemas/"):
				// $schema meta-identifier, never fetched.
			case strings.HasPrefix(found, "http://www.w3.org/2000/svg"):
				// SVG namespace inside the inline data URI — an identifier, not a fetch.
			default:
				t.Fatalf("%s references outbound URL %q; V4 must carry no runtime "+
					"dependency outside the deployment (D4a)", relative, found)
			}
		}
	}

	// The icons must actually be inline, and must survive the render path — which
	// accepts them only because cardtmpl passes cardmsg.AllowInlineImageData().
	reg := newRegistry(t)
	for _, tc := range reasoningStates {
		payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
			reasoningVersionV4, tc.state, readSample(t, reasoningRootV4, string(tc.state)),
			cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
		if err != nil {
			t.Fatalf("render %s: %v", tc.state, err)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), "data:image/svg+xml,") {
			t.Fatalf("rendered %s frame carries no inline icon", tc.state)
		}
	}

	// The frozen predecessors never had one; keep that true.
	for _, root := range []string{aireasoningprocess.HandoffRootV1, aireasoningprocess.HandoffRootV2, reasoningRootV3} {
		for _, relative := range []string{
			"templates/active.template.json", "templates/result.template.json",
			"templates/error.template.json",
		} {
			if strings.Contains(string(readAsset(t, root+"/"+relative)), "api.iconify.design") {
				t.Fatalf("%s/%s gained an outbound icon reference", root, relative)
			}
		}
	}
}
func jsonNumber(t *testing.T, raw any) int {
	t.Helper()
	switch value := raw.(type) {
	case float64:
		return int(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			t.Fatalf("bound %v is not an integer: %v", raw, err)
		}
		return int(parsed)
	default:
		t.Fatalf("bound %v has unexpected type %T", raw, raw)
		return 0
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
