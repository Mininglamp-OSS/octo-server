package aireasoningprocess_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
)

var reasoningStates = []struct {
	state   cardtmpl.State
	profile string
}{
	{"reasoning", "octo/v2"},
	{"answering", "octo/v2"},
	{"completed", "octo/v1"},
	{"stopped", "octo/v1"},
	{"error", "octo/v2"},
}

const (
	reasoningVersionV3 = aireasoningprocess.TemplateVersionV3
	reasoningRootV3    = aireasoningprocess.HandoffRootV3
	reasoningVersionV4 = aireasoningprocess.TemplateVersionV4
	reasoningRootV4    = aireasoningprocess.HandoffRootV4
)

var reasoningVersions = []struct {
	version string
	root    string
}{
	{aireasoningprocess.TemplateVersionV1, aireasoningprocess.HandoffRootV1},
	{aireasoningprocess.TemplateVersionV2, aireasoningprocess.HandoffRootV2},
	{reasoningVersionV3, reasoningRootV3},
	{reasoningVersionV4, reasoningRootV4},
}

// boundedReasoningVersions skips the pre-#667 V1, which has no bounds to assert.
var boundedReasoningVersions = reasoningVersions[1:]

// noSubmitReasoningVersions are the versions whose action surface is the local
// toggle only; V1/V2 still carry their frozen stop/retry Submit contracts.
var noSubmitReasoningVersions = reasoningVersions[2:]

func newRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	reg := cardtmpl.NewRegistry()
	reg.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV1)
	reg.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV2)
	reg.RegisterJSON(aireasoningprocess.Assets, reasoningRootV3)
	reg.RegisterJSON(aireasoningprocess.Assets, reasoningRootV4)
	reg.SetDefault(aireasoningprocess.TemplateID, reasoningVersionV4)
	reg.Freeze()
	return reg
}

func TestRegistersAllVersionsAndDefaultsToSimplifiedSuccessor(t *testing.T) {
	reg := newRegistry(t)
	noSubmit := make(map[string]struct{}, len(noSubmitReasoningVersions))
	for _, version := range noSubmitReasoningVersions {
		noSubmit[version.version] = struct{}{}
	}
	var versions []string
	for _, meta := range reg.List() {
		if meta.ID != aireasoningprocess.TemplateID {
			continue
		}
		versions = append(versions, meta.Version)
		if _, ok := noSubmit[meta.Version]; ok {
			if meta.ActionContract != nil {
				t.Fatalf("%s ActionContract = %+v, want nil", meta.Version, meta.ActionContract)
			}
			continue
		}
		if meta.ActionContract == nil || meta.ActionContract.Owner != "ai" ||
			meta.ActionContract.ActionType != "reasoning.control" {
			t.Fatalf("historical %s ActionContract = %+v, want {ai, reasoning.control}", meta.Version, meta.ActionContract)
		}
	}
	sort.Strings(versions)
	wantVersions := []string{
		aireasoningprocess.TemplateVersionV1,
		aireasoningprocess.TemplateVersionV2,
		reasoningVersionV3,
		reasoningVersionV4,
	}
	if !equalStrings(versions, wantVersions) {
		t.Fatalf("registered versions = %v, want %v", versions, wantVersions)
	}
	if aireasoningprocess.TemplateVersion != reasoningVersionV4 || aireasoningprocess.HandoffRoot != reasoningRootV4 {
		t.Fatalf("current aliases = %s / %s, want %s / %s",
			aireasoningprocess.TemplateVersion, aireasoningprocess.HandoffRoot, reasoningVersionV4, reasoningRootV4)
	}

	tmpl, err := reg.Lookup(aireasoningprocess.TemplateID, "")
	if err != nil {
		t.Fatalf("Lookup(default): %v", err)
	}
	if got := tmpl.Meta().Version; got != reasoningVersionV4 {
		t.Fatalf("default version = %q, want %q", got, reasoningVersionV4)
	}
}

func TestRenderAllStatesForAllVersions(t *testing.T) {
	reg := newRegistry(t)
	for _, version := range reasoningVersions {
		for _, tc := range reasoningStates {
			t.Run(version.version+"/"+string(tc.state), func(t *testing.T) {
				fields := readSample(t, version.root, string(tc.state))
				payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
					version.version, tc.state, fields,
					cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
				if err != nil {
					t.Fatalf("Render(%s@%s): %v", tc.state, version.version, err)
				}
				if got, _ := payload["profile"].(string); got != tc.profile {
					t.Fatalf("profile = %q, want %q", got, tc.profile)
				}
			})
		}
	}
}

func TestConformanceForAllVersions(t *testing.T) {
	reg := newRegistry(t)
	for _, version := range reasoningVersions {
		for _, tc := range reasoningStates {
			name := string(tc.state)
			t.Run(version.version+"/"+name, func(t *testing.T) {
				fields := readSample(t, version.root, name)
				state := cardtmpl.State(mustState(t, fields))
				cardRaw, _, err := reg.RenderCard(context.Background(), aireasoningprocess.TemplateID,
					version.version, state, fields,
					cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
				if err != nil {
					t.Fatalf("RenderCard(%s@%s): %v", name, version.version, err)
				}
				var card map[string]any
				if err := json.Unmarshal(cardRaw, &card); err != nil {
					t.Fatalf("unmarshal card: %v", err)
				}
				delete(card, "metadata")

				golden := readGolden(t, version.root, name)
				delete(golden, "$schema")

				if g, w := canon(t, card), canon(t, golden); g != w {
					t.Fatalf("golden mismatch for %s@%s\n--- got ---\n%s\n--- want ---\n%s",
						name, version.version, g, w)
				}
			})
		}
	}
}

func TestHiddenControlsSuccessorArtifactContract(t *testing.T) {
	manifest := readAssetMap(t, reasoningRootV3+"/manifest.json")
	for key, want := range map[string]string{
		"id":                         string(aireasoningprocess.TemplateID),
		"version":                    reasoningVersionV3,
		"contractVersion":            "1.1.0",
		"protocol":                   "octo-card@1.0",
		"adaptiveCardVersion":        "1.5",
		"renderProfile":              "octo-chat@1.2.0-rc.1",
		"renderProfileCompatibility": "octo-chat/v1",
		"owner":                      "ai",
	} {
		if got, _ := manifest[key].(string); got != want {
			t.Fatalf("manifest.%s = %q, want %q", key, got, want)
		}
	}
	if _, exists := manifest["actionType"]; exists {
		t.Fatal("0.3.0 manifest must not advertise actionType")
	}

	if got, want := readAsset(t, reasoningRootV3+"/contract/data.schema.json"),
		readAsset(t, aireasoningprocess.HandoffRootV2+"/contract/data.schema.json"); !bytes.Equal(got, want) {
		t.Fatal("0.3.0 schema drifted from bounded 0.2.0 schema")
	}
	for _, relative := range []string{
		"templates/result.template.json",
		"samples/reasoning.json",
		"samples/answering.json",
		"samples/completed.json",
		"samples/stopped.json",
		"goldens/completed.card.json",
		"goldens/stopped.card.json",
	} {
		if got, want := readAsset(t, reasoningRootV3+"/"+relative),
			readAsset(t, aireasoningprocess.HandoffRootV2+"/"+relative); !bytes.Equal(got, want) {
			t.Fatalf("0.3.0 artifact %s drifted from 0.2.0", relative)
		}
	}

	legacyError := readAssetMap(t, aireasoningprocess.HandoffRootV2+"/samples/error.json")
	successorError := readAssetMap(t, reasoningRootV3+"/samples/error.json")
	if got, _ := successorError["errorMessage"].(string); got != "推理服务连接超时，已停止本次生成。已完成的过程会保留。" {
		t.Fatalf("0.3.0 errorMessage = %q", got)
	}
	successorError["errorMessage"] = legacyError["errorMessage"]
	if got, want := canon(t, successorError), canon(t, legacyError); got != want {
		t.Fatalf("0.3.0 error sample has changes beyond retry invitation\ngot=%s\nwant=%s", got, want)
	}
}

func TestHiddenControlsSuccessorHasOnlyLocalToggle(t *testing.T) {
	reg := newRegistry(t)
	tmpl, err := reg.Lookup(aireasoningprocess.TemplateID, reasoningVersionV3)
	if err != nil {
		t.Fatal(err)
	}
	meta := tmpl.Meta()
	if meta.ActionContract != nil {
		t.Fatalf("ActionContract = %+v, want nil", meta.ActionContract)
	}
	for _, view := range []cardtmpl.ViewKey{"active", "error"} {
		report, ok := meta.Interaction(view)
		if !ok {
			t.Fatalf("interaction report missing for %s", view)
		}
		if len(report.Actions) != 1 || report.Actions[0].ID != "reasoning_toggle" ||
			report.Actions[0].Type != "Action.ToggleVisibility" || len(report.Inputs) != 0 {
			t.Fatalf("%s interaction report = %+v", view, report)
		}
	}
	if _, ok := meta.Interaction("result"); ok {
		t.Fatal("octo/v1 result view must not have an interaction report")
	}

	for _, tc := range reasoningStates {
		t.Run(string(tc.state), func(t *testing.T) {
			payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
				reasoningVersionV3, tc.state, readSample(t, reasoningRootV3, string(tc.state)),
				cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{
				"Action.Submit", "reasoning_stop", "reasoning_retry",
				"stop_reasoning", "retry_reasoning", "可以重试",
			} {
				if bytes.Contains(encoded, []byte(forbidden)) {
					t.Fatalf("rendered payload contains unsupported control %q", forbidden)
				}
			}
			var actions []map[string]any
			collectActions(payload, &actions)
			if len(actions) != 1 {
				t.Fatalf("rendered actions = %+v, want one local toggle", actions)
			}
			action := actions[0]
			if action["type"] != "Action.ToggleVisibility" || action["id"] != "reasoning_toggle" {
				t.Fatalf("rendered action = %+v", action)
			}
			if got := canon(t, action["targetElements"]); got != canon(t, []any{"trace_panel", "collapsed_panel"}) {
				t.Fatalf("toggle targets = %s", got)
			}
		})
	}
}

func TestHiddenControlsSuccessorRejectsLegacySubmitIDs(t *testing.T) {
	reg := newRegistry(t)
	catalog, err := cardtmpl.NewStaticCatalog(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range noSubmitReasoningVersions {
		for _, tc := range []struct {
			state    cardtmpl.State
			actionID string
		}{
			{state: "reasoning", actionID: "reasoning_stop"},
			{state: "answering", actionID: "reasoning_stop"},
			{state: "error", actionID: "reasoning_retry"},
		} {
			t.Run(version.version+"/"+string(tc.state)+"/"+tc.actionID, func(t *testing.T) {
				payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
					version.version, tc.state, readSample(t, version.root, string(tc.state)),
					cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if _, found := cardmsg.SubmitAction(encoded, tc.actionID); found {
					t.Fatalf("legacy action %q resolved from %s %s frame", tc.actionID, version.version, tc.state)
				}
				_, err = catalog.ActionContext(context.Background(), cardtmpl.CatalogActionRequest{
					CatalogExactRequest: cardtmpl.CatalogExactRequest{
						Access: cardtmpl.CatalogAccess{Purpose: cardtmpl.CatalogPurposeActionContext},
						ID:     aireasoningprocess.TemplateID, Version: version.version,
					},
					ActionID: tc.actionID,
				})
				if !errors.Is(err, cardtmpl.ErrActionUnknown) {
					t.Fatalf("legacy action %q ActionContext error = %v, want ErrActionUnknown", tc.actionID, err)
				}
			})
		}
	}
}

func TestHiddenControlsSuccessorStaticExactRenderSurvivesRuntimeStoreOutage(t *testing.T) {
	reg := newRegistry(t)
	store := &unavailableRuntimeStore{}
	catalog, err := cardtmpl.NewRuntimeCatalog(reg, store, cardtmpl.RuntimeCatalogConfig{
		CheckReady: func() error { return cardtmpl.ErrRuntimeCatalogUnavailable },
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := catalog.Render(context.Background(), cardtmpl.CatalogRenderRequest{
		Access: cardtmpl.CatalogAccess{
			Purpose: cardtmpl.CatalogPurposeHistoricalEdit,
			Principal: cardtmpl.CatalogPrincipal{
				Kind: cardtmpl.CatalogPrincipalBot, ID: "bot-test", SpaceID: "space-x",
			},
		},
		ID: aireasoningprocess.TemplateID, Version: reasoningVersionV3,
		State: "completed", Fields: readSample(t, reasoningRootV3, "completed"),
		Env: cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"},
	})
	if err != nil {
		t.Fatalf("explicit static render during runtime store outage: %v", err)
	}
	if store.calls != 0 {
		t.Fatalf("explicit static render made %d runtime store calls", store.calls)
	}
	card := payload["card"].(map[string]any)
	metadata := card["metadata"].(map[string]any)
	octo := metadata["octo"].(map[string]any)
	template := octo["template"].(map[string]any)
	if got := template["version"]; got != reasoningVersionV3 {
		t.Fatalf("rendered template version = %v", got)
	}
}

func TestSuccessorFreeStringBounds(t *testing.T) {
	reg := newRegistry(t)
	setTop := func(key string) func(map[string]any, string) {
		return func(data map[string]any, value string) { data[key] = value }
	}
	tests := []struct {
		name string
		// limit is the ceiling shared by every bounded version. perVersion
		// overrides it for fields whose ceiling diverged across versions.
		limit      int
		perVersion func(*testing.T, string) int
		set        func(map[string]any, string)
	}{
		{name: "reasoningId", limit: 512, set: setTop("reasoningId")},
		{name: "title", limit: 64, set: setTop("title")},
		{name: "statusLabel", limit: 32, set: setTop("statusLabel")},
		{name: "timerText", limit: 128, set: setTop("timerText")},
		{name: "collapsedSummary", limit: 160, set: setTop("collapsedSummary")},
		{name: "progressText", limit: 160, set: setTop("progressText")},
		{name: "thought", perVersion: reasoningThoughtMax, set: setFirstThought},
		{name: "tool", limit: 81, set: setFirstActionField("tool")},
		{name: "detail", limit: 192, set: setFirstActionField("detail")},
		{name: "errorTitle", limit: 64, set: setTop("errorTitle")},
		{name: "errorMessage", limit: 121, set: setTop("errorMessage")},
	}
	units := []string{"x", "界", "😀"}
	for _, version := range boundedReasoningVersions {
		for i, tc := range tests {
			t.Run(version.version+"/"+tc.name, func(t *testing.T) {
				limit := tc.limit
				if tc.perVersion != nil {
					limit = tc.perVersion(t, version.version)
				}
				unit := units[i%len(units)]
				exact := readSampleMap(t, version.root, "reasoning")
				tc.set(exact, strings.Repeat(unit, limit))
				if err := renderData(reg, version.version, "reasoning", exact); err != nil {
					t.Fatalf("exact %d-rune value rejected: %v", limit, err)
				}

				over := readSampleMap(t, version.root, "reasoning")
				tc.set(over, strings.Repeat(unit, limit+1))
				if err := renderData(reg, version.version, "reasoning", over); !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
					t.Fatalf("%d-rune value error = %v, want ErrFieldsInvalid", limit+1, err)
				}
			})
		}
	}
}

func TestSuccessorArrayAndAggregateBounds(t *testing.T) {
	reg := newRegistry(t)
	tests := []struct {
		name         string
		actionCounts []int
		wantErr      bool
	}{
		{name: "six phases", actionCounts: []int{1, 1, 1, 1, 1, 1}},
		{name: "seven phases", actionCounts: []int{1, 1, 1, 1, 1, 1, 1}, wantErr: true},
		{name: "twelve actions in one phase", actionCounts: []int{12}},
		{name: "thirteen actions in one phase", actionCounts: []int{13}, wantErr: true},
		{name: "twelve actions across six phases", actionCounts: []int{2, 2, 2, 2, 2, 2}},
		{name: "aggregate thirteen with each phase under per-phase max", actionCounts: []int{3, 2, 2, 2, 2, 2}},
		{name: "aggregate fourteen with each phase under per-phase max", actionCounts: []int{4, 2, 2, 2, 2, 2}, wantErr: true},
	}
	for _, version := range boundedReasoningVersions {
		for _, tc := range tests {
			t.Run(version.version+"/"+tc.name, func(t *testing.T) {
				data := readSampleMap(t, version.root, "reasoning")
				data["phases"] = phasesWithActionCounts(tc.actionCounts...)
				err := renderData(reg, version.version, "reasoning", data)
				if tc.wantErr {
					if !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
						t.Fatalf("Render error = %v, want ErrFieldsInvalid", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("Render: %v", err)
				}
			})
		}
	}
}

func TestSuccessorWorstCaseRendersEveryView(t *testing.T) {
	reg := newRegistry(t)
	for _, version := range boundedReasoningVersions {
		for _, tc := range reasoningStates {
			t.Run(version.version+"/"+string(tc.state), func(t *testing.T) {
				data := readSampleMap(t, version.root, "reasoning")
				data["state"] = string(tc.state)
				data["reasoningId"] = strings.Repeat("r", 512)
				data["title"] = strings.Repeat("题", 64)
				data["statusLabel"] = strings.Repeat("状", 32)
				data["timerText"] = strings.Repeat("时", 128)
				data["collapsedSummary"] = strings.Repeat("摘", 160)
				data["progressText"] = strings.Repeat("进", 160)
				data["errorTitle"] = strings.Repeat("错", 64)
				data["errorMessage"] = strings.Repeat("误", 121)
				data["phases"] = worstCasePhases(reasoningThoughtMax(t, version.version))

				if err := renderData(reg, version.version, tc.state, data); err != nil {
					t.Fatalf("worst-case %s render: %v", tc.state, err)
				}
			})
		}
	}
}

func TestSuccessorPreservesVisualContractArtifacts(t *testing.T) {
	identical := []string{
		"templates/active.template.json",
		"templates/error.template.json",
		"templates/result.template.json",
		"reports/active.interaction.json",
		"reports/error.interaction.json",
	}
	for _, state := range []string{"reasoning", "answering", "completed", "stopped", "error"} {
		identical = append(identical, "samples/"+state+".json", "goldens/"+state+".card.json")
	}
	for _, relative := range identical {
		t.Run(relative, func(t *testing.T) {
			legacy := readAsset(t, aireasoningprocess.HandoffRootV1+"/"+relative)
			successor := readAsset(t, aireasoningprocess.HandoffRootV2+"/"+relative)
			if !bytes.Equal(legacy, successor) {
				t.Fatalf("successor artifact %s drifted from frozen 0.1.0", relative)
			}
		})
	}
}

func setFirstThought(data map[string]any, value string) {
	phase := data["phases"].([]any)[0].(map[string]any)
	phase["thought"] = value
}

func setFirstActionField(key string) func(map[string]any, string) {
	return func(data map[string]any, value string) {
		phase := data["phases"].([]any)[0].(map[string]any)
		action := phase["actions"].([]any)[0].(map[string]any)
		action[key] = value
	}
}

func phasesWithActionCounts(counts ...int) []any {
	phases := make([]any, 0, len(counts))
	for phaseIndex, actionCount := range counts {
		actions := make([]any, 0, actionCount)
		for actionIndex := 0; actionIndex < actionCount; actionIndex++ {
			actions = append(actions, map[string]any{
				"tool":        "tool",
				"detail":      "detail",
				"statusGlyph": "●",
				"statusTone":  "Good",
			})
		}
		phases = append(phases, map[string]any{
			"thought": "phase " + string(rune('a'+phaseIndex)),
			"actions": actions,
		})
	}
	return phases
}

func worstCasePhases(thoughtMax int) []any {
	phases := phasesWithActionCounts(3, 2, 2, 2, 2, 2)
	for _, rawPhase := range phases {
		phase := rawPhase.(map[string]any)
		phase["thought"] = strings.Repeat("思", thoughtMax)
		for _, rawAction := range phase["actions"].([]any) {
			action := rawAction.(map[string]any)
			action["tool"] = strings.Repeat("工", 81)
			action["detail"] = strings.Repeat("详", 192)
		}
	}
	return phases
}

// reasoningThoughtMax is the per-version `phases[].thought` ceiling. V2/V3 pinned
// it to the producer's observed output (280 + `…` = 281); V4 raised it to a
// platform product cap (4000 + `…` = 4001) that deliberately no longer tracks
// producer truncation, so the two must be asserted separately rather than
// through one shared constant. Keyed explicitly per version and fatal on an
// unknown one: a default would silently bless a future version that narrowed the
// bound back down, since the bounds table would still pass.
func reasoningThoughtMax(t *testing.T, version string) int {
	t.Helper()
	switch version {
	case aireasoningprocess.TemplateVersionV2, reasoningVersionV3:
		return 281
	case reasoningVersionV4:
		return 4001
	default:
		t.Fatalf("reasoningThoughtMax: unhandled version %q — add its ceiling explicitly", version)
		return 0
	}
}

func renderData(reg *cardtmpl.Registry, version string, state cardtmpl.State, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = reg.Render(context.Background(), aireasoningprocess.TemplateID,
		version, state, raw,
		cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
	return err
}

func readSampleMap(t *testing.T, root, name string) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(readSample(t, root, name), &data); err != nil {
		t.Fatalf("unmarshal sample %s: %v", name, err)
	}
	return data
}

func readSample(t *testing.T, root, name string) json.RawMessage {
	t.Helper()
	return json.RawMessage(readAsset(t, root+"/samples/"+name+".json"))
}

func readGolden(t *testing.T, root, name string) map[string]any {
	t.Helper()
	var golden map[string]any
	if err := json.Unmarshal(readAsset(t, root+"/goldens/"+name+".card.json"), &golden); err != nil {
		t.Fatalf("unmarshal golden %s: %v", name, err)
	}
	return golden
}

func readAssetMap(t *testing.T, name string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(readAsset(t, name), &value); err != nil {
		t.Fatalf("unmarshal asset %s: %v", name, err)
	}
	return value
}

func readAsset(t *testing.T, name string) []byte {
	t.Helper()
	b, err := aireasoningprocess.Assets.ReadFile(name)
	if err != nil {
		t.Fatalf("read asset %s: %v", name, err)
	}
	return b
}

func collectActions(value any, actions *[]map[string]any) {
	switch typed := value.(type) {
	case map[string]any:
		if kind, _ := typed["type"].(string); strings.HasPrefix(kind, "Action.") {
			*actions = append(*actions, typed)
		}
		for _, child := range typed {
			collectActions(child, actions)
		}
	case []any:
		for _, child := range typed {
			collectActions(child, actions)
		}
	}
}

type unavailableRuntimeStore struct {
	calls int
}

func (s *unavailableRuntimeStore) LoadActivation(context.Context, cardtmpl.ID) (cardtmpl.RuntimeActivation, error) {
	s.calls++
	return cardtmpl.RuntimeActivation{}, errors.New("runtime store unavailable")
}

func (s *unavailableRuntimeStore) LoadArtifactMeta(context.Context, cardtmpl.ID, string) (cardtmpl.RuntimeArtifactMeta, error) {
	s.calls++
	return cardtmpl.RuntimeArtifactMeta{}, errors.New("runtime store unavailable")
}

func (s *unavailableRuntimeStore) LoadArtifactBundle(context.Context, cardtmpl.ID, string, string) (cardtmpl.CanonicalBundle, error) {
	s.calls++
	return nil, errors.New("runtime store unavailable")
}

func mustState(t *testing.T, fields json.RawMessage) string {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(fields, &data); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	state, _ := data["state"].(string)
	if state == "" {
		t.Fatal("sample missing state")
	}
	return state
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func canon(t *testing.T, value any) string {
	t.Helper()
	b, err := json.MarshalIndent(sortKeys(value), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func sortKeys(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(typed))
		for _, key := range keys {
			out[key] = sortKeys(typed[key])
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = sortKeys(typed[i])
		}
		return out
	default:
		return value
	}
}
