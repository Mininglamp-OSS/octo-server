package aireasoningprocess_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

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

var reasoningVersions = []struct {
	version string
	root    string
}{
	{aireasoningprocess.TemplateVersionV1, aireasoningprocess.HandoffRootV1},
	{aireasoningprocess.TemplateVersionV2, aireasoningprocess.HandoffRootV2},
}

func newRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	reg := cardtmpl.NewRegistry()
	reg.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV1)
	reg.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV2)
	reg.SetDefault(aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV2)
	reg.Freeze()
	return reg
}

func TestRegistersBothVersionsAndDefaultsToSuccessor(t *testing.T) {
	reg := newRegistry(t)
	var versions []string
	for _, meta := range reg.List() {
		if meta.ID != aireasoningprocess.TemplateID {
			continue
		}
		versions = append(versions, meta.Version)
		if meta.ActionContract == nil || meta.ActionContract.Owner != "ai" ||
			meta.ActionContract.ActionType != "reasoning.control" {
			t.Fatalf("%s ActionContract = %+v, want {ai, reasoning.control}", meta.Version, meta.ActionContract)
		}
	}
	sort.Strings(versions)
	wantVersions := []string{aireasoningprocess.TemplateVersionV1, aireasoningprocess.TemplateVersionV2}
	if !equalStrings(versions, wantVersions) {
		t.Fatalf("registered versions = %v, want %v", versions, wantVersions)
	}

	tmpl, err := reg.Lookup(aireasoningprocess.TemplateID, "")
	if err != nil {
		t.Fatalf("Lookup(default): %v", err)
	}
	if got := tmpl.Meta().Version; got != aireasoningprocess.TemplateVersionV2 {
		t.Fatalf("default version = %q, want %q", got, aireasoningprocess.TemplateVersionV2)
	}
}

func TestRenderAllStatesForBothVersions(t *testing.T) {
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

func TestConformanceForBothVersions(t *testing.T) {
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

func TestSuccessorFreeStringBounds(t *testing.T) {
	reg := newRegistry(t)
	setTop := func(key string) func(map[string]any, string) {
		return func(data map[string]any, value string) { data[key] = value }
	}
	tests := []struct {
		name  string
		limit int
		set   func(map[string]any, string)
	}{
		{name: "reasoningId", limit: 512, set: setTop("reasoningId")},
		{name: "title", limit: 64, set: setTop("title")},
		{name: "statusLabel", limit: 32, set: setTop("statusLabel")},
		{name: "timerText", limit: 128, set: setTop("timerText")},
		{name: "collapsedSummary", limit: 160, set: setTop("collapsedSummary")},
		{name: "progressText", limit: 160, set: setTop("progressText")},
		{name: "thought", limit: 281, set: setFirstThought},
		{name: "tool", limit: 81, set: setFirstActionField("tool")},
		{name: "detail", limit: 192, set: setFirstActionField("detail")},
		{name: "errorTitle", limit: 64, set: setTop("errorTitle")},
		{name: "errorMessage", limit: 121, set: setTop("errorMessage")},
	}
	units := []string{"x", "界", "😀"}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unit := units[i%len(units)]
			exact := readSampleMap(t, aireasoningprocess.HandoffRootV2, "reasoning")
			tc.set(exact, strings.Repeat(unit, tc.limit))
			if err := renderData(reg, "reasoning", exact); err != nil {
				t.Fatalf("exact %d-rune value rejected: %v", tc.limit, err)
			}

			over := readSampleMap(t, aireasoningprocess.HandoffRootV2, "reasoning")
			tc.set(over, strings.Repeat(unit, tc.limit+1))
			if err := renderData(reg, "reasoning", over); !errors.Is(err, cardtmpl.ErrFieldsInvalid) {
				t.Fatalf("%d-rune value error = %v, want ErrFieldsInvalid", tc.limit+1, err)
			}
		})
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
		{name: "aggregate thirteen with each phase under per-phase max", actionCounts: []int{3, 2, 2, 2, 2, 2}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := readSampleMap(t, aireasoningprocess.HandoffRootV2, "reasoning")
			data["phases"] = phasesWithActionCounts(tc.actionCounts...)
			err := renderData(reg, "reasoning", data)
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

func TestSuccessorWorstCaseRendersEveryView(t *testing.T) {
	reg := newRegistry(t)
	for _, tc := range reasoningStates {
		t.Run(string(tc.state), func(t *testing.T) {
			data := readSampleMap(t, aireasoningprocess.HandoffRootV2, "reasoning")
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

			if err := renderData(reg, tc.state, data); err != nil {
				t.Fatalf("worst-case %s render: %v", tc.state, err)
			}
		})
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

func worstCasePhases() []any {
	phases := phasesWithActionCounts(2, 2, 2, 2, 2, 2)
	for _, rawPhase := range phases {
		phase := rawPhase.(map[string]any)
		phase["thought"] = strings.Repeat("思", 281)
		for _, rawAction := range phase["actions"].([]any) {
			action := rawAction.(map[string]any)
			action["tool"] = strings.Repeat("工", 81)
			action["detail"] = strings.Repeat("详", 192)
		}
	}
	return phases
}

func renderData(reg *cardtmpl.Registry, state cardtmpl.State, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = reg.Render(context.Background(), aireasoningprocess.TemplateID,
		aireasoningprocess.TemplateVersionV2, state, raw,
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

func readAsset(t *testing.T, name string) []byte {
	t.Helper()
	b, err := aireasoningprocess.Assets.ReadFile(name)
	if err != nil {
		t.Fatalf("read asset %s: %v", name, err)
	}
	return b
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
