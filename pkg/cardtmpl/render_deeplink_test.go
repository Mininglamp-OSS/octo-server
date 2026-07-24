package cardtmpl

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// deepLinkStub is a minimal Template returning a fixed DeepLink so the renderCore
// step-5 branch can be exercised directly — JSON-mode templates always return an
// empty DeepLink, so they cannot cover the non-empty/whitespace cases.
type deepLinkStub struct {
	meta     TemplateMeta
	deepLink string
}

func (s *deepLinkStub) Meta() TemplateMeta { return s.meta }
func (s *deepLinkStub) Build(_ context.Context, _ State, _ json.RawMessage, _ BuildEnv) (BuildResult, error) {
	return BuildResult{
		Body:     []any{map[string]any{"type": "TextBlock", "text": "hi"}},
		DeepLink: s.deepLink,
	}, nil
}
func (s *deepLinkStub) FallbackText(_ State, _ json.RawMessage, _ string) (string, error) {
	return "hi", nil
}

func permissiveObjectSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(`{"type":"object"}`), &doc); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	const url = "mem://stub.schema.json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatalf("add resource: %v", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return sch
}

func deepLinkStubMeta(t *testing.T) TemplateMeta {
	t.Helper()
	return TemplateMeta{
		ID:          "test.deeplink",
		Version:     "0.1.0",
		Protocol:    Protocol,
		Views:       map[ViewKey]ViewSpec{"v": {WireProfile: cardmsg.ProfileV1, States: []State{"shown"}}},
		InputSchema: permissiveObjectSchema(t),
		stateIndex:  map[State]ViewKey{"shown": "v"},
	}
}

// TestRenderCore_DeepLinkOptionalVsBlank pins the render step-5 contract after
// DeepLink was made optional (PR #654): an EMPTY DeepLink is a legitimate "no
// canonical URL" (the card renders, metadata.webUrl omitted), a NON-EMPTY but
// whitespace-only DeepLink is malformed and must fail-close (ErrRenderFailed)
// rather than being silently treated as absent, and a valid https DeepLink is
// injected as metadata.webUrl as before.
func TestRenderCore_DeepLinkOptionalVsBlank(t *testing.T) {
	meta := deepLinkStubMeta(t)
	fields := json.RawMessage(`{"x":1}`)

	// empty DeepLink → renders, webUrl omitted.
	payload, err := renderCore(context.Background(), &deepLinkStub{meta: meta, deepLink: ""}, meta, "shown", fields, BuildEnv{})
	if err != nil {
		t.Fatalf("empty DeepLink should render, got %v", err)
	}
	if _, has := payload["card"].(map[string]any)["metadata"].(map[string]any)["webUrl"]; has {
		t.Errorf("empty DeepLink must omit metadata.webUrl")
	}

	// whitespace-only DeepLink → fail-close (not silently absent).
	_, err = renderCore(context.Background(), &deepLinkStub{meta: meta, deepLink: "   "}, meta, "shown", fields, BuildEnv{})
	if err == nil {
		t.Fatal("whitespace-only DeepLink must fail-close, got nil")
	}
	if !strings.Contains(err.Error(), ErrRenderFailed.Error()) || !strings.Contains(err.Error(), "DeepLink") {
		t.Fatalf("want ErrRenderFailed on DeepLink, got %v", err)
	}

	// valid https DeepLink → injected as metadata.webUrl.
	payload, err = renderCore(context.Background(), &deepLinkStub{meta: meta, deepLink: "https://im.example.com/s/abc"}, meta, "shown", fields, BuildEnv{})
	if err != nil {
		t.Fatalf("valid https DeepLink should render, got %v", err)
	}
	if got := payload["card"].(map[string]any)["metadata"].(map[string]any)["webUrl"]; got != "https://im.example.com/s/abc" {
		t.Errorf("valid DeepLink must be injected as metadata.webUrl, got %v", got)
	}
}
