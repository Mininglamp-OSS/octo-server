package cardtmpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDecodeBundleJSONStrictRoundTrip(t *testing.T) {
	raw, err := json.Marshal(validJSONArtifactBundle())
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	bundle, err := DecodeBundleJSON(raw)
	if err != nil {
		t.Fatalf("DecodeBundleJSON: %v", err)
	}
	if bundle.Catalog.Engine != JSONTemplateEngineV1 || len(bundle.Templates) != 1 {
		t.Fatalf("decoded bundle = %+v", bundle)
	}

	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "duplicate root key", raw: []byte(`{"catalog":{},"catalog":{}}`)},
		{name: "unknown root key", raw: []byte(`{"unknown":true}`)},
		{name: "catalog unknown key", raw: []byte(`{"catalog":{"engine":"octo-json-template/v1","visibility":"private","extra":true}}`)},
		{name: "non-object root", raw: []byte(`[]`)},
		{name: "oversize", raw: []byte(strings.Repeat(" ", maxArtifactBundleBytes+1))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeBundleJSON(tt.raw)
			var validationErr *ArtifactValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %v, want ArtifactValidationError", err)
			}
		})
	}
}

func TestDecodeStrictJSONRejectsAmbiguousEnvelope(t *testing.T) {
	type envelope struct {
		Bundle json.RawMessage `json:"bundle"`
	}
	tests := []struct {
		name string
		raw  []byte
		out  any
		ok   bool
	}{
		{name: "valid", raw: []byte(`{"bundle":{"catalog":{}}}`), out: &envelope{}, ok: true},
		{name: "unknown field", raw: []byte(`{"bundle":{},"actor_uid":"spoof"}`), out: &envelope{}},
		{name: "duplicate field", raw: []byte(`{"bundle":{},"bundle":{}}`), out: &envelope{}},
		{name: "trailing token", raw: []byte(`{"bundle":{}} true`), out: &envelope{}},
		{name: "nil target", raw: []byte(`{}`), out: nil},
		{name: "oversize", raw: []byte(strings.Repeat(" ", maxArtifactBundleBytes+1)), out: &envelope{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DecodeStrictJSON(tt.raw, tt.out)
			if tt.ok {
				if err != nil {
					t.Fatalf("DecodeStrictJSON: %v", err)
				}
				return
			}
			var validationErr *ArtifactValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %v, want ArtifactValidationError", err)
			}
		})
	}
}

func TestCompileJSONArtifactRejectsGovernanceAndDocumentDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Bundle)
		want   string
	}{
		{
			name: "unknown engine",
			mutate: func(bundle *Bundle) {
				bundle.Catalog.Engine = "unknown/v1"
			},
			want: "engine",
		},
		{
			name: "unknown visibility",
			mutate: func(bundle *Bundle) {
				bundle.Catalog.Visibility = "internal"
			},
			want: "visibility",
		},
		{
			name: "external schema ref",
			mutate: func(bundle *Bundle) {
				bundle.Schema = json.RawMessage(`{"type":"object","additionalProperties":false,"$ref":"https://example.com/schema.json"}`)
			},
			want: "$ref",
		},
		{
			name: "unbounded string",
			mutate: func(bundle *Bundle) {
				bundle.Schema = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"title":{"type":"string"}}}`)
			},
			want: "unbounded",
		},
		{
			name: "unbounded unconstrained property",
			mutate: func(bundle *Bundle) {
				bundle.Schema = artifactSchemaWithProperties(`{"title":{},"groups":{"type":"array","maxItems":2,"items":{"type":"object","additionalProperties":false,"properties":{"items":{"type":"array","maxItems":2,"items":{"type":"string","maxLength":8}}}}}}`)
			},
			want: "bounded type",
		},
		{
			name: "unbounded nullable string",
			mutate: func(bundle *Bundle) {
				bundle.Schema = artifactSchemaWithProperties(`{"title":{"type":["string","null"]},"groups":{"type":"array","maxItems":2,"items":{"type":"object","additionalProperties":false,"properties":{"items":{"type":"array","maxItems":2,"items":{"type":"string","maxLength":8}}}}}}`)
			},
			want: "unbounded",
		},
		{
			name: "unsupported manifest schema version",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = replaceManifestField(t, bundle.Manifest, "schemaVersion", 3)
			},
			want: "schemaVersion",
		},
		{
			name: "template id exceeds storage",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = replaceManifestField(t, bundle.Manifest, "id", "a."+strings.Repeat("b", 127))
			},
			want: "id",
		},
		{
			name: "version exceeds storage",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = replaceManifestField(t, bundle.Manifest, "version", "1.0.0+"+strings.Repeat("a", 59))
			},
			want: "version",
		},
		{
			name: "contract version exceeds storage",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = replaceManifestField(t, bundle.Manifest, "contractVersion", "1.0.0+"+strings.Repeat("a", 59))
			},
			want: "contractVersion",
		},
		{
			name: "invalid semver",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = replaceManifestField(t, bundle.Manifest, "version", "latest")
			},
			want: "semver",
		},
		{
			name: "semver leading zero",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = replaceManifestField(t, bundle.Manifest, "version", "01.0.0")
			},
			want: "semver",
		},
		{
			name: "missing owner",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = replaceManifestField(t, bundle.Manifest, "owner", "")
			},
			want: "owner",
		},
		{
			name: "external data schema path",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = replaceManifestField(t, bundle.Manifest, "dataSchema", "https://example.com/schema.json")
			},
			want: "dataSchema",
		},
		{
			name: "missing template document",
			mutate: func(bundle *Bundle) {
				delete(bundle.Templates, "main")
			},
			want: "missing",
		},
		{
			name: "unreferenced sample",
			mutate: func(bundle *Bundle) {
				bundle.Samples["extra"] = json.RawMessage(`{"title":"extra"}`)
			},
			want: "unreferenced",
		},
		{
			name: "sample without state assignment",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = mutateManifest(t, bundle.Manifest, func(manifest map[string]any) {
					views := manifest["views"].(map[string]any)
					main := views["main"].(map[string]any)
					main["samples"] = append(main["samples"].([]any), "samples/alternate.json")
				})
				bundle.Samples["alternate"] = json.RawMessage(`{"title":"alternate","groups":[]}`)
			},
			want: "samples",
		},
		{
			name: "interactive template missing action type",
			mutate: func(bundle *Bundle) {
				bundle.Manifest = mutateManifest(t, bundle.Manifest, func(manifest map[string]any) {
					views := manifest["views"].(map[string]any)
					views["main"].(map[string]any)["wireProfile"] = profileV2
					delete(manifest, "actionType")
				})
				bundle.Templates["main"] = json.RawMessage(`{
					"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"${title}"}],
					"actions":[{"type":"Action.Submit","id":"submit","data":{"owner":"ai","action_type":"test.submit"}}]
				}`)
				bundle.Reports = map[string]json.RawMessage{"main": json.RawMessage(`{
					"actions":[{"id":"submit","type":"Action.Submit","associatedInputs":"auto","dataKeys":["owner","action_type"],"inputIds":[]}],
					"inputs":[]
				}`)}
				bundle.Goldens = nil
			},
			want: "actionType",
		},
		{
			name: "golden mismatch",
			mutate: func(bundle *Bundle) {
				bundle.Goldens["shown"] = json.RawMessage(`{"type":"AdaptiveCard","version":"1.5","body":[]}`)
			},
			want: "golden",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bundle := validJSONArtifactBundle()
			tt.mutate(&bundle)
			_, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
			var validationErr *ArtifactValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error = %v, want ArtifactValidationError", err)
			}
			if !strings.Contains(err.Error(), tt.want) && !strings.Contains(validationErr.Category, tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCompileJSONArtifactRejectsUnpairedUnicodeSurrogate(t *testing.T) {
	bundle := validJSONArtifactBundle()
	bundle.Samples["shown"] = json.RawMessage(`{"title":"\ud800","groups":[]}`)
	bundle.Goldens = nil
	_, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
	var validationErr *ArtifactValidationError
	if !errors.As(err, &validationErr) || validationErr.Document != "samples.shown" {
		t.Fatalf("error = %v, want samples.shown validation error", err)
	}

	bundle.Samples["shown"] = json.RawMessage(`{"title":"\ud83d\ude80","groups":[]}`)
	if _, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits()); err != nil {
		t.Fatalf("valid astral Unicode surrogate pair rejected: %v", err)
	}
}

func TestCompileJSONArtifactHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CompileJSONArtifact(ctx, validJSONArtifactBundle(), DefaultCompileLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestLoadJSONBundleReadsEmbeddedHandoff(t *testing.T) {
	bundle, err := LoadJSONBundle(jsonCardTestData, "testdata/test.jsoncard@0.1.0", CatalogDescriptor{
		Engine: JSONTemplateEngineV1, Visibility: CatalogVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("LoadJSONBundle: %v", err)
	}
	if len(bundle.Schema) == 0 || len(bundle.Templates) != 1 || len(bundle.Samples) != 1 {
		t.Fatalf("bundle documents: schema=%d templates=%d samples=%d", len(bundle.Schema), len(bundle.Templates), len(bundle.Samples))
	}
}

func TestArtifactValidationErrorFormattingAndUnwrap(t *testing.T) {
	cause := errors.New("cause")
	err := &ArtifactValidationError{Category: "json", Document: "schema", cause: cause}
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("error = %v", err)
	}
	var nilErr *ArtifactValidationError
	if nilErr.Error() == "" {
		t.Fatal("nil validation error must have a safe message")
	}
}

func TestCanonicalJSONNumberRejectsUnstableRanges(t *testing.T) {
	tests := []struct {
		input   json.Number
		want    string
		wantErr bool
	}{
		{input: "0", want: "0"},
		{input: "-0", want: "0"},
		{input: "1e+09", want: "1e+9"},
		{input: "9007199254740992", wantErr: true},
		{input: "1e10000", wantErr: true},
	}
	for _, tt := range tests {
		got, err := canonicalJSONNumber(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("canonicalJSONNumber(%q) = %q, want error", tt.input, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("canonicalJSONNumber(%q) = %q, %v; want %q", tt.input, got, err, tt.want)
		}
	}
}

func TestValidateBoundedInputSchemaRejectsUnboundedShapes(t *testing.T) {
	tests := []map[string]any{
		{"type": "array", "additionalProperties": false},
		{"type": "object", "additionalProperties": true},
		{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"items": map[string]any{"type": "array"},
		}},
		{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"items": map[string]any{"type": "array", "maxItems": json.Number("2")},
		}},
		{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"nested": map[string]any{"type": "object", "additionalProperties": true},
		}},
	}
	for index, schema := range tests {
		if err := validateBoundedInputSchema(schema); err == nil {
			t.Errorf("schema %d unexpectedly accepted", index)
		}
	}
}

func TestValidateBoundedInputSchemaAcceptsFinitePropertyShapes(t *testing.T) {
	tests := []struct {
		name     string
		property any
		decorate func(map[string]any)
	}{
		{
			name:     "bounded nullable string",
			property: map[string]any{"type": []any{"string", "null"}, "maxLength": json.Number("8")},
		},
		{name: "enum", property: map[string]any{"enum": []any{"ready", "done"}}},
		{name: "const", property: map[string]any{"const": "ready"}},
		{
			name:     "local ref",
			property: map[string]any{"$ref": "#/$defs/title"},
			decorate: func(schema map[string]any) {
				schema["$defs"] = map[string]any{
					"title": map[string]any{"type": "string", "maxLength": json.Number("8")},
				}
			},
		},
		{
			name: "any of bounded alternatives",
			property: map[string]any{"anyOf": []any{
				map[string]any{"type": "string", "maxLength": json.Number("8")},
				map[string]any{"type": "integer"},
			}},
		},
		{
			name: "one of bounded alternatives",
			property: map[string]any{"oneOf": []any{
				map[string]any{"type": "string", "maxLength": json.Number("8")},
				map[string]any{"type": "boolean"},
			}},
		},
		{
			name: "all of includes bounded shape",
			property: map[string]any{"allOf": []any{
				map[string]any{"type": "string", "maxLength": json.Number("8")},
				map[string]any{"minLength": json.Number("1")},
			}},
		},
		{name: "false schema", property: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := boundedRootWithProperty(tt.property)
			if tt.decorate != nil {
				tt.decorate(schema)
			}
			if err := validateBoundedInputSchema(schema); err != nil {
				t.Fatalf("validateBoundedInputSchema: %v", err)
			}
		})
	}
}

func TestValidateBoundedInputSchemaRejectsAmbiguousPropertyShapes(t *testing.T) {
	tests := []struct {
		name     string
		property any
	}{
		{name: "true schema", property: true},
		{name: "non-schema value", property: "string"},
		{name: "empty type list", property: map[string]any{"type": []any{}}},
		{name: "non-string type", property: map[string]any{"type": json.Number("7")}},
		{name: "non-string type list item", property: map[string]any{"type": []any{"string", json.Number("7")}}},
		{name: "duplicate type", property: map[string]any{"type": []any{"string", "string"}, "maxLength": json.Number("8")}},
		{name: "unbounded any of", property: map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{}}}},
		{name: "unbounded one of", property: map[string]any{"oneOf": []any{map[string]any{"type": "boolean"}, map[string]any{}}}},
		{name: "unbounded all of", property: map[string]any{"allOf": []any{map[string]any{"minLength": json.Number("1")}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateBoundedInputSchema(boundedRootWithProperty(tt.property)); err == nil {
				t.Fatal("ambiguous property schema unexpectedly accepted")
			}
		})
	}
}

func TestValidateBoundedInputSchemaRejectsUnboundedLocalRefs(t *testing.T) {
	tests := []struct {
		name   string
		target any
	}{
		{name: "true schema", target: true},
		{name: "empty schema", target: map[string]any{}},
		{name: "unbounded string", target: map[string]any{"type": "string"}},
		{name: "reference cycle", target: map[string]any{"$ref": "#/$defs/value"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := boundedRootWithProperty(map[string]any{"$ref": "#/$defs/value"})
			schema["$defs"] = map[string]any{"value": tt.target}
			if err := validateBoundedInputSchema(schema); err == nil {
				t.Fatal("unbounded local reference unexpectedly accepted")
			}
		})
	}
}

func TestValidateBoundedInputSchemaRejectsUnboundedArrayItemRef(t *testing.T) {
	schema := boundedRootWithProperty(map[string]any{
		"type":     "array",
		"maxItems": json.Number("1"),
		"items":    map[string]any{"$ref": "#/$defs/value"},
	})
	schema["$defs"] = map[string]any{"value": true}
	if err := validateBoundedInputSchema(schema); err == nil {
		t.Fatal("array item reference to an unbounded schema unexpectedly accepted")
	}
}

func TestValidateBoundedInputSchemaRejectsUnboundedArrayItems(t *testing.T) {
	schema := boundedRootWithProperty(map[string]any{
		"type":     "array",
		"maxItems": json.Number("1"),
		"items":    map[string]any{},
	})
	if err := validateBoundedInputSchema(schema); err == nil {
		t.Fatal("unbounded array item schema unexpectedly accepted")
	}
}

func boundedRootWithProperty(property any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"value": property,
		},
	}
}

func replaceManifestField(t *testing.T, raw json.RawMessage, field string, value any) json.RawMessage {
	t.Helper()
	return mutateManifest(t, raw, func(manifest map[string]any) {
		manifest[field] = value
	})
}

func mutateManifest(t *testing.T, raw json.RawMessage, mutate func(map[string]any)) json.RawMessage {
	t.Helper()
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	mutate(manifest)
	out, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func artifactSchemaWithProperties(properties string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":false,"required":["title"],"properties":%s,"x-octo-constraints":%s}`,
		properties,
		validAggregateConstraint,
	))
}
