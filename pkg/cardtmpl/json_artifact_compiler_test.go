package cardtmpl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCompileJSONArtifactProducesStableCanonicalArtifact(t *testing.T) {
	bundle := validJSONArtifactBundle()
	first, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
	if err != nil {
		t.Fatalf("CompileJSONArtifact: %v", err)
	}
	if first.Meta.ID != "test.runtime-card" || first.Meta.Version != "1.0.0" {
		t.Fatalf("compiled identity = %s@%s", first.Meta.ID, first.Meta.Version)
	}
	if len(first.Hash) != 64 {
		t.Fatalf("hash length = %d, want 64", len(first.Hash))
	}
	if len(first.Bundle) == 0 {
		t.Fatal("canonical bundle is empty")
	}

	// Formatting and map insertion order are not part of the immutable identity.
	secondBundle := validJSONArtifactBundle()
	var formattedSchema bytes.Buffer
	if err := json.Indent(&formattedSchema, secondBundle.Schema, "", "  "); err != nil {
		t.Fatalf("format schema fixture: %v", err)
	}
	secondBundle.Schema = append(json.RawMessage(nil), formattedSchema.Bytes()...)
	secondBundle.Templates = map[string]json.RawMessage{
		"main": secondBundle.Templates["main"],
	}
	second, err := CompileJSONArtifact(context.Background(), secondBundle, DefaultCompileLimits())
	if err != nil {
		t.Fatalf("CompileJSONArtifact reformatted: %v", err)
	}
	if first.Hash != second.Hash || string(first.Bundle) != string(second.Bundle) {
		t.Fatalf("canonical artifact drifted: first=%s second=%s", first.Hash, second.Hash)
	}

	fields := json.RawMessage(`{"title":"hello","groups":[{"items":["a"]}]}`)
	card, err := first.Template.Build(context.Background(), "shown", fields, BuildEnv{Lang: "en-US"})
	if err != nil {
		t.Fatalf("compiled template Build: %v", err)
	}
	if len(card.Body) != 1 {
		t.Fatalf("compiled card body len = %d, want 1", len(card.Body))
	}
}

func TestCompileJSONArtifactReturnsTypedErrorsWithoutPanicking(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		properties string
		want       string
	}{
		{
			name:       "unknown constraint key",
			constraint: `{"aggregateArrayLimits":[{"parentArray":"groups","childArray":"items","maxTotalItems":2}],"unknown":true}`,
			want:       "unknown",
		},
		{name: "empty constraint list", constraint: `{"aggregateArrayLimits":[]}`, want: "aggregateArrayLimits"},
		{
			name:       "blank parent",
			constraint: `{"aggregateArrayLimits":[{"parentArray":" ","childArray":"items","maxTotalItems":2}]}`,
			want:       "parentArray",
		},
		{
			name:       "untrimmed child",
			constraint: `{"aggregateArrayLimits":[{"parentArray":"groups","childArray":" items","maxTotalItems":2}]}`,
			want:       "childArray",
		},
		{
			name:       "non-positive limit",
			constraint: `{"aggregateArrayLimits":[{"parentArray":"groups","childArray":"items","maxTotalItems":0}]}`,
			want:       "maxTotalItems",
		},
		{
			name: "duplicate target",
			constraint: `{"aggregateArrayLimits":[` +
				`{"parentArray":"groups","childArray":"items","maxTotalItems":2},` +
				`{"parentArray":"groups","childArray":"items","maxTotalItems":3}]}`,
			want: "duplicate",
		},
		{
			name:       "missing parent",
			constraint: validAggregateConstraint,
			properties: `{"title":{"type":"string","maxLength":32}}`,
			want:       "parentArray",
		},
		{
			name:       "parent wrong type",
			constraint: validAggregateConstraint,
			properties: `{"title":{"type":"string","maxLength":32},"groups":{"type":"object","additionalProperties":false}}`,
			want:       "array of objects",
		},
		{
			name:       "missing child",
			constraint: validAggregateConstraint,
			properties: `{"title":{"type":"string","maxLength":32},"groups":{"type":"array","maxItems":2,"items":{"type":"object","additionalProperties":false,"properties":{}}}}`,
			want:       "childArray",
		},
		{
			name:       "child wrong type",
			constraint: validAggregateConstraint,
			properties: `{"title":{"type":"string","maxLength":32},"groups":{"type":"array","maxItems":2,"items":{"type":"object","additionalProperties":false,"properties":{"items":{"type":"string","maxLength":8}}}}}`,
			want:       "must be an array",
		},
		{
			name:       "invalid child sub-schema",
			constraint: validAggregateConstraint,
			properties: `{"title":{"type":"string","maxLength":32},"groups":{"type":"array","maxItems":2,"items":{"type":"object","additionalProperties":false,"properties":{"items":{"type":7}}}}}`,
			want:       "schema",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := validJSONArtifactBundle()
			properties := tt.properties
			if properties == "" {
				properties = validArtifactProperties
			}
			bundle.Schema = json.RawMessage(fmt.Sprintf(
				`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":false,"required":["title"],"properties":%s,"x-octo-constraints":%s}`,
				properties,
				tt.constraint,
			))

			var panicValue any
			var err error
			func() {
				defer func() { panicValue = recover() }()
				_, err = CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
			}()
			if panicValue != nil {
				t.Fatalf("runtime compiler panicked: %v", panicValue)
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
			var validationErr *ArtifactValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error %T = %v, want ArtifactValidationError", err, err)
			}
			if validationErr.Document != "schema" {
				t.Fatalf("validation document = %q, want schema", validationErr.Document)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestCompileJSONArtifactRejectsAmbiguousJSON(t *testing.T) {
	tests := []struct {
		name string
		doc  func(*Bundle) *json.RawMessage
		bad  json.RawMessage
	}{
		{
			name: "duplicate key",
			doc:  func(bundle *Bundle) *json.RawMessage { return &bundle.Manifest },
			bad:  json.RawMessage(`{"id":"test.runtime-card","id":"other","version":"1.0.0"}`),
		},
		{
			name: "trailing token",
			doc:  func(bundle *Bundle) *json.RawMessage { return &bundle.Schema },
			bad:  json.RawMessage(`{} {}`),
		},
		{
			name: "non-finite numeric range",
			doc:  func(bundle *Bundle) *json.RawMessage { return &bundle.Schema },
			bad:  json.RawMessage(`{"type":"object","additionalProperties":false,"maximum":1e10000}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := validJSONArtifactBundle()
			*tt.doc(&bundle) = tt.bad
			_, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
			var validationErr *ArtifactValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %v, want typed validation error", err)
			}
		})
	}
}

func TestRegisterGoTemplateRejectsJSONOnlyConstraints(t *testing.T) {
	reg := NewRegistry()
	var panicValue any
	func() {
		defer func() { panicValue = recover() }()
		reg.Register(&goConstraintTemplate{}, jsonCardTestData, "testdata/test.jsoncard@0.1.0")
	}()
	if panicValue == nil {
		t.Fatal("Go-authored template silently accepted x-octo-constraints")
	}
	if !strings.Contains(fmt.Sprint(panicValue), "x-octo-constraints") {
		t.Fatalf("panic = %v, want x-octo-constraints", panicValue)
	}
}

const (
	validAggregateConstraint = `{"aggregateArrayLimits":[{"parentArray":"groups","childArray":"items","maxTotalItems":2}]}`
	validArtifactProperties  = `{"title":{"type":"string","maxLength":32},"groups":{"type":"array","maxItems":2,"items":{"type":"object","additionalProperties":false,"properties":{"items":{"type":"array","maxItems":2,"items":{"type":"string","maxLength":8}}}}}}`
)

func validJSONArtifactBundle() Bundle {
	return Bundle{
		Catalog: CatalogDescriptor{
			Engine:     JSONTemplateEngineV1,
			Visibility: CatalogVisibilityPrivate,
		},
		Manifest: json.RawMessage(`{
			"schemaVersion":2,
			"id":"test.runtime-card",
			"name":"Runtime compiler test",
			"version":"1.0.0",
			"contractVersion":"1.0.0",
			"protocol":"octo-card@1.0",
			"adaptiveCardVersion":"1.5",
			"owner":"ai",
			"dataSchema":"contract/data.schema.json",
			"views":{"main":{"wireProfile":"octo/v1","states":["shown"],"template":"templates/main.template.json","samples":["samples/shown.json"]}}
		}`),
		Schema: json.RawMessage(fmt.Sprintf(
			`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":false,"required":["title"],"properties":%s,"x-octo-constraints":%s}`,
			validArtifactProperties,
			validAggregateConstraint,
		)),
		Templates: map[string]json.RawMessage{
			"main": json.RawMessage(`{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"${title}","wrap":true}]}`),
		},
		Samples: map[string]json.RawMessage{
			"shown": json.RawMessage(`{"title":"hello","groups":[{"items":["a"]}]}`),
		},
		Goldens: map[string]json.RawMessage{
			"shown": json.RawMessage(`{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"hello","wrap":true}]}`),
		},
	}
}

type goConstraintTemplate struct {
	meta TemplateMeta
}

func (t *goConstraintTemplate) SetMeta(meta TemplateMeta) { t.meta = meta }
func (t *goConstraintTemplate) Meta() TemplateMeta        { return t.meta.Clone() }
func (t *goConstraintTemplate) Build(context.Context, State, json.RawMessage, BuildEnv) (BuildResult, error) {
	return BuildResult{Body: []any{map[string]any{"type": "TextBlock", "text": "test"}}}, nil
}
func (t *goConstraintTemplate) FallbackText(State, json.RawMessage, string) (string, error) {
	return "test", nil
}
