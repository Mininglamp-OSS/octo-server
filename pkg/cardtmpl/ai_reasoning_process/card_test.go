package aireasoningprocess_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
)

func newRegistry(t *testing.T) *cardtmpl.Registry {
	t.Helper()
	reg := cardtmpl.NewRegistry()
	// Successful RegisterJSON is itself the fail-close assertion: it runs the
	// sample self-check, which for the v2 views (active/error) verifies every
	// Action.Submit.data carries {owner: ai, action_type: reasoning.control}.
	reg.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRoot)
	reg.SetDefault(aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersion)
	reg.Freeze()
	return reg
}

func TestRegisters(t *testing.T) {
	reg := newRegistry(t)
	found := false
	for _, m := range reg.List() {
		if m.ID == aireasoningprocess.TemplateID {
			found = true
			if m.ActionContract == nil || m.ActionContract.Owner != "ai" ||
				m.ActionContract.ActionType != "reasoning.control" {
				t.Fatalf("ActionContract = %+v, want {ai, reasoning.control}", m.ActionContract)
			}
		}
	}
	if !found {
		t.Fatal("ai.reasoning-process not in Registry.List()")
	}
}

func TestRenderAllStates(t *testing.T) {
	reg := newRegistry(t)
	cases := []struct {
		state   cardtmpl.State
		profile string
	}{
		{"reasoning", "octo/v2"},
		{"answering", "octo/v2"},
		{"completed", "octo/v1"},
		{"stopped", "octo/v1"},
		{"error", "octo/v2"},
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			fields := readSample(t, string(tc.state))
			payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
				aireasoningprocess.TemplateVersion, tc.state, fields,
				cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
			if err != nil {
				t.Fatalf("Render(%s): %v", tc.state, err)
			}
			if got, _ := payload["profile"].(string); got != tc.profile {
				t.Fatalf("profile = %q, want %q", got, tc.profile)
			}
		})
	}
}

// TestConformance renders each sample and asserts the card node (minus the
// base-injected metadata) matches the shipped golden (minus its $schema),
// canonical JSON. Goldens carry the owner/action_type keys injected into every
// Submit.data, so this also locks that the rendered card carries them.
func TestConformance(t *testing.T) {
	reg := newRegistry(t)
	samples := []string{"reasoning", "answering", "completed", "stopped", "error"}
	for _, name := range samples {
		t.Run(name, func(t *testing.T) {
			fields := readSample(t, name)
			state := cardtmpl.State(mustState(t, fields))
			cardRaw, _, err := reg.RenderCard(context.Background(), aireasoningprocess.TemplateID,
				aireasoningprocess.TemplateVersion, state, fields,
				cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
			if err != nil {
				t.Fatalf("RenderCard(%s): %v", name, err)
			}
			var card map[string]any
			if err := json.Unmarshal(cardRaw, &card); err != nil {
				t.Fatalf("unmarshal card: %v", err)
			}
			delete(card, "metadata") // base-injected; not in golden

			golden := readGolden(t, name)
			delete(golden, "$schema") // authoring-tool artifact; not emitted by renderCore

			if g, w := canon(t, card), canon(t, golden); g != w {
				t.Fatalf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", name, g, w)
			}
		})
	}
}

func readSample(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("handoff", "ai.reasoning-process@0.1.0", "samples", name+".json"))
	if err != nil {
		t.Fatalf("read sample %s: %v", name, err)
	}
	return json.RawMessage(b)
}

func readGolden(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("handoff", "ai.reasoning-process@0.1.0", "goldens", name+".card.json"))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal golden %s: %v", name, err)
	}
	return m
}

func mustState(t *testing.T, fields json.RawMessage) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(fields, &m); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	s, _ := m["state"].(string)
	if s == "" {
		t.Fatal("sample missing state")
	}
	return s
}

func canon(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(sortKeys(v), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func sortKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		ks := make([]string, 0, len(x))
		for k := range x {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		m := make(map[string]any, len(x))
		for _, k := range ks {
			m[k] = sortKeys(x[k])
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = sortKeys(x[i])
		}
		return out
	default:
		return v
	}
}
