package aireasoningprocess_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
)

func TestFullBleedSuccessorContractAndAssets(t *testing.T) {
	manifest := readAssetMap(t, reasoningRootV5+"/manifest.json")
	for key, want := range map[string]string{
		"id":                         string(aireasoningprocess.TemplateID),
		"version":                    reasoningVersionV5,
		"contractVersion":            "1.2.0",
		"protocol":                   "octo-card@1.0",
		"adaptiveCardVersion":        "1.5",
		"renderProfile":              "octo-chat@1.2.0-rc.3",
		"renderProfileCompatibility": "octo-chat/v1",
		"owner":                      "ai",
	} {
		if got, _ := manifest[key].(string); got != want {
			t.Fatalf("manifest.%s = %q, want %q", key, got, want)
		}
	}
	if _, exists := manifest["actionType"]; exists {
		t.Fatal("0.4.1 manifest must not advertise actionType")
	}
	views, _ := manifest["views"].(map[string]any)
	if len(views) != 3 {
		t.Fatalf("views = %d, want 3", len(views))
	}
	for name, raw := range views {
		view, _ := raw.(map[string]any)
		if _, exists := view["submit_actions"]; exists {
			t.Fatalf("view %q declares submit_actions; capability must stay derived", name)
		}
	}

	for _, relative := range []string{
		"contract/data.schema.json",
		"reports/active.interaction.json",
		"reports/error.interaction.json",
		"samples/reasoning.json",
		"samples/answering.json",
		"samples/completed.json",
		"samples/stopped.json",
		"samples/error.json",
	} {
		if got, want := readAsset(t, reasoningRootV5+"/"+relative), readAsset(t, reasoningRootV4+"/"+relative); !bytes.Equal(got, want) {
			t.Fatalf("0.4.1 %s drifted from 0.4.0", relative)
		}
	}

	for _, view := range []string{"active", "result", "error"} {
		v4 := readAssetMap(t, reasoningRootV4+"/templates/"+view+".template.json")
		v5 := readAssetMap(t, reasoningRootV5+"/templates/"+view+".template.json")
		root := fullBleedRoot(t, v5, "template "+view)
		delete(root, "bleed")
		if got, want := canon(t, v5), canon(t, v4); got != want {
			t.Fatalf("0.4.1 %s template changed beyond root bleed\ngot=%s\nwant=%s", view, got, want)
		}
	}

	for _, state := range []string{"reasoning", "answering", "completed", "stopped", "error"} {
		v4 := readGolden(t, reasoningRootV4, state)
		v5 := readGolden(t, reasoningRootV5, state)
		root := fullBleedRoot(t, v5, "golden "+state)
		delete(root, "bleed")
		if got, want := canon(t, v5), canon(t, v4); got != want {
			t.Fatalf("0.4.1 %s golden changed beyond root bleed\ngot=%s\nwant=%s", state, got, want)
		}
	}

	for _, forbidden := range []string{
		reasoningRootV5 + "/reports/result.interaction.json",
		reasoningRootV5 + "/render-profile/manifest.json",
	} {
		if _, err := aireasoningprocess.Assets.Open(forbidden); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s must not be shipped (err = %v)", forbidden, err)
		}
	}
}

func TestFullBleedSuccessorSurvivesProductionPersistence(t *testing.T) {
	reg := newRegistry(t)
	for _, tc := range reasoningStates {
		payload, err := reg.Render(context.Background(), aireasoningprocess.TemplateID,
			reasoningVersionV5, tc.state, readSample(t, reasoningRootV5, string(tc.state)),
			cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-x"})
		if err != nil {
			t.Fatalf("render %s: %v", tc.state, err)
		}
		payload["card_seq"] = 2
		payload["space_id"] = "space-x"
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := carddispatch.NormalizeFrameForPersistence(string(raw)); err != nil {
			t.Fatalf("%s V5 frame rejected by persistence normalization: %v", tc.state, err)
		}
	}
}

func fullBleedRoot(t *testing.T, card map[string]any, label string) map[string]any {
	t.Helper()
	body, _ := card["body"].([]any)
	if len(body) == 0 {
		t.Fatalf("%s has no body", label)
	}
	root, _ := body[0].(map[string]any)
	if root == nil {
		t.Fatalf("%s root body element is %T, want object", label, body[0])
	}
	if bleed, _ := root["bleed"].(bool); !bleed {
		t.Fatalf("%s root bleed = %v, want true", label, root["bleed"])
	}
	return root
}
