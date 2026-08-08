package cardtmpl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
	var compactManifest bytes.Buffer
	if err := json.Compact(&compactManifest, secondBundle.Manifest); err != nil {
		t.Fatalf("compact manifest fixture: %v", err)
	}
	secondBundle.Manifest = append(json.RawMessage(nil), compactManifest.Bytes()...)
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
	if !bytes.Equal(first.Meta.Manifest, second.Meta.Manifest) {
		t.Fatal("canonical artifact produced formatting-dependent manifest metadata")
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

func TestCompileJSONArtifactRejectsSubmitInV1WithoutActionContract(t *testing.T) {
	bundle := validJSONArtifactBundle()
	card := json.RawMessage(`{
		"type":"AdaptiveCard","version":"1.5",
		"body":[{"type":"TextBlock","text":"${title}","wrap":true}],
		"actions":[{"type":"Action.Submit","id":"forged-submit","title":"Submit"}]
	}`)
	bundle.Templates["main"] = card
	bundle.Goldens["shown"] = json.RawMessage(`{
		"type":"AdaptiveCard","version":"1.5",
		"body":[{"type":"TextBlock","text":"hello","wrap":true}],
		"actions":[{"type":"Action.Submit","id":"forged-submit","title":"Submit"}]
	}`)

	_, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
	if err == nil {
		t.Fatal("octo/v1 artifact without ActionContract accepted Action.Submit")
	}
	if got := err.Error(); !strings.Contains(got, "cardmsg.Validate") ||
		!strings.Contains(got, "Action.Submit") || !strings.Contains(got, "octo/v1") {
		t.Fatalf("error = %q, want octo/v1 cardmsg profile rejection", got)
	}
}

func TestStaticRegistrationAndRuntimeCompilerHaveCanonicalParity(t *testing.T) {
	const root = "testdata/test.runtime-parity@1.0.0"
	bundle, err := LoadJSONBundle(jsonCardTestData, root, CatalogDescriptor{
		Engine: JSONTemplateEngineV1, Visibility: CatalogVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("LoadJSONBundle: %v", err)
	}
	runtimeArtifact, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
	if err != nil {
		t.Fatalf("runtime CompileJSONArtifact: %v", err)
	}
	staticArtifact, err := CompileJSONArtifact(context.Background(), bundle, staticCompileLimits())
	if err != nil {
		t.Fatalf("static CompileJSONArtifact: %v", err)
	}
	if runtimeArtifact.Hash != staticArtifact.Hash || !bytes.Equal(runtimeArtifact.Bundle, staticArtifact.Bundle) {
		t.Fatalf("static/runtime identity drift: runtime=%s static=%s", runtimeArtifact.Hash, staticArtifact.Hash)
	}

	registry := NewRegistry()
	registry.RegisterJSON(jsonCardTestData, root)
	registry.Freeze()
	staticTemplate, err := registry.Lookup("test.runtime-parity", "1.0.0")
	if err != nil {
		t.Fatalf("Lookup static template: %v", err)
	}
	staticMeta := staticTemplate.Meta()
	runtimeMeta := runtimeArtifact.Meta.Clone()
	staticMeta.InputSchema = nil
	runtimeMeta.InputSchema = nil
	if !reflect.DeepEqual(staticMeta, runtimeMeta) {
		t.Fatalf("static/runtime metadata drift:\nstatic=%+v\nruntime=%+v", staticMeta, runtimeMeta)
	}

	fields := bundle.Samples["shown"]
	env := BuildEnv{Lang: "en-US", SpaceID: "parity", WebLoginURL: "https://selfcheck.internal"}
	staticPayload, err := renderCore(context.Background(), staticTemplate, staticTemplate.Meta(), "shown", fields, env)
	if err != nil {
		t.Fatalf("render static template: %v", err)
	}
	runtimePayload, err := renderCore(context.Background(), runtimeArtifact.Template, runtimeArtifact.Meta, "shown", fields, env)
	if err != nil {
		t.Fatalf("render runtime template: %v", err)
	}
	staticCanonical, err := marshalCanonicalJSON(staticPayload)
	if err != nil {
		t.Fatal(err)
	}
	runtimeCanonical, err := marshalCanonicalJSON(runtimePayload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staticCanonical, runtimeCanonical) {
		t.Fatalf("static/runtime render drift:\nstatic=%s\nruntime=%s", staticCanonical, runtimeCanonical)
	}
}

func TestCompileBundleSamplesScopesStateLookupToEachView(t *testing.T) {
	schema, err := compileJSONSchema(map[string]any{
		"type": "object", "additionalProperties": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := manifestFile{Views: map[ViewKey]manifestView{
		"one": {States: []State{"alpha"}, Samples: []string{"samples/beta.json"}},
		"two": {States: []State{"gamma"}, Samples: []string{"samples/alpha.json"}},
	}}
	documents := map[string]json.RawMessage{
		"alpha": json.RawMessage(`{}`),
		"beta":  json.RawMessage(`{}`),
	}
	for iteration := 0; iteration < 1000; iteration++ {
		_, assignments, err := compileBundleSamples(
			manifest,
			schema,
			documents,
			jsonBudget{maxDepth: maxArtifactJSONDepth, maxNodes: maxArtifactJSONNodes},
			DefaultCompileLimits(),
		)
		if err != nil {
			t.Fatalf("compileBundleSamples iteration %d: %v", iteration, err)
		}
		got := make(map[string]string, len(assignments))
		for _, assignment := range assignments {
			got[string(assignment.view)+"/"+string(assignment.state)] = assignment.key
		}
		if got["one/alpha"] != "beta" || got["two/gamma"] != "alpha" {
			t.Fatalf("iteration %d assignments = %v, want view-local samples", iteration, got)
		}
	}
}

func TestCompileBundleSamplesReservesExactMatchesBeforePositionalFallback(t *testing.T) {
	schema, err := compileJSONSchema(map[string]any{
		"type": "object", "additionalProperties": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := manifestFile{Views: map[ViewKey]manifestView{
		"main": {
			States:  []State{"foo", "bar"},
			Samples: []string{"samples/bar.json", "samples/baz.json"},
		},
	}}
	documents := map[string]json.RawMessage{
		"bar": json.RawMessage(`{}`),
		"baz": json.RawMessage(`{}`),
	}

	_, assignments, err := compileBundleSamples(
		manifest,
		schema,
		documents,
		jsonBudget{maxDepth: maxArtifactJSONDepth, maxNodes: maxArtifactJSONNodes},
		DefaultCompileLimits(),
	)
	if err != nil {
		t.Fatalf("compileBundleSamples: %v", err)
	}
	got := make(map[State]string, len(assignments))
	for _, assignment := range assignments {
		got[assignment.state] = assignment.key
	}
	if got["foo"] != "baz" || got["bar"] != "bar" {
		t.Fatalf("assignments = %v, want foo=baz and bar=bar", got)
	}
}

func TestCompileJSONArtifactUsesProductionNumberSemanticsForGoldens(t *testing.T) {
	bundle := validJSONArtifactBundle()
	bundle.Schema = json.RawMessage(`{
		"$schema":"http://json-schema.org/draft-07/schema#",
		"type":"object",
		"additionalProperties":false,
		"required":["count"],
		"properties":{"count":{"type":"integer"}}
	}`)
	bundle.Templates["main"] = json.RawMessage(`{
		"type":"AdaptiveCard",
		"version":"1.5",
		"body":[{"type":"TextBlock","text":"${if(count == 1000000, 'equal', 'different')} ${count}"}]
	}`)
	bundle.Samples["shown"] = json.RawMessage(`{"count":1000000}`)
	bundle.Goldens["shown"] = json.RawMessage(`{
		"type":"AdaptiveCard",
		"version":"1.5",
		"body":[{"type":"TextBlock","text":"equal 1e+06"}]
	}`)

	artifact, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
	if err != nil {
		t.Fatalf("CompileJSONArtifact: %v", err)
	}
	built, err := artifact.Template.Build(context.Background(), "shown", bundle.Samples["shown"], BuildEnv{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	text, _ := built.Body[0].(map[string]any)["text"].(string)
	if text != "equal 1e+06" {
		t.Fatalf("production text = %q, want golden text", text)
	}
}

func TestCompileJSONArtifactCanonicalBundleRecompilesIntegerLimits(t *testing.T) {
	for _, literal := range []string{"1000000", "1e6"} {
		t.Run(literal, func(t *testing.T) {
			bundle := validJSONArtifactBundle()
			bundle.Schema = artifactSchemaWithIntegerLimits(literal)

			first, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
			if err != nil {
				t.Fatalf("first CompileJSONArtifact: %v", err)
			}
			stored, err := DecodeBundleJSON(first.Bundle)
			if err != nil {
				t.Fatalf("DecodeBundleJSON(canonical): %v", err)
			}
			second, err := CompileJSONArtifact(context.Background(), stored, DefaultCompileLimits())
			if err != nil {
				t.Fatalf("recompile canonical bundle: %v", err)
			}
			if first.Hash != second.Hash || !bytes.Equal(first.Bundle, second.Bundle) {
				t.Fatalf("canonical round trip drift: first=%s second=%s", first.Hash, second.Hash)
			}
		})
	}
}

func TestCompileJSONArtifactRejectsUnsafeIntegerSpellings(t *testing.T) {
	for _, literal := range []string{
		"9007199254740992.0",
		"9007199254740993e0",
		"9.007199254740992e+15",
		"-9007199254740992.0",
	} {
		t.Run(literal, func(t *testing.T) {
			bundle := validJSONArtifactBundle()
			bundle.Schema = artifactSchemaWithProperties(`{
				"title":{"type":"string","maxLength":32},
				"groups":{"type":"array","maxItems":2,"items":{"type":"object","additionalProperties":false,"properties":{"items":{"type":"array","maxItems":2,"items":{"type":"string","maxLength":8}}}}},
				"value":{"type":"number"}
			}`)
			bundle.Samples["shown"] = json.RawMessage(fmt.Sprintf(
				`{"title":"hello","groups":[{"items":["a"]}],"value":%s}`,
				literal,
			))

			_, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
			if err == nil || !strings.Contains(err.Error(), "exceeds exact JSON range") {
				t.Fatalf("CompileJSONArtifact(%s) error = %v, want exact-range rejection", literal, err)
			}
		})
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

// truncateStrings lets an artifact declare a *display* ceiling below the schema's
// *accept* ceiling, so an over-long value renders truncated instead of failing the
// card. It is opt-in per field, so the compiler has to refuse declarations that
// would silently soften a bound or point at nothing — otherwise "truncate" becomes
// a way to smuggle past fail-close validation.
func TestCompileJSONArtifactRejectsInvalidTruncateStrings(t *testing.T) {
	constraintWith := func(entry string) string {
		return `{"aggregateArrayLimits":[{"parentArray":"groups","childArray":"items","maxTotalItems":2}],` +
			`"truncateStrings":[` + entry + `]}`
	}
	for _, tc := range []struct {
		name  string
		entry string
		want  string
	}{{
		name:  "display ceiling above the accept ceiling",
		entry: `{"field":"title","maxRunes":64,"ellipsis":"…"}`,
		want:  "exceeds the schema maxLength",
	}, {
		name:  "field is not declared in the schema",
		entry: `{"field":"nosuch","maxRunes":8,"ellipsis":"…"}`,
		want:  "not a declared property",
	}, {
		name:  "field is not a string",
		entry: `{"field":"groups","maxRunes":8,"ellipsis":"…"}`,
		want:  `want string`,
	}, {
		name:  "target field has no accept ceiling to sit under",
		entry: `{"arrayField":"groups","field":"items","maxRunes":4,"ellipsis":"…"}`,
		want:  "want string",
	}, {
		name:  "ellipsis alone would fill the budget",
		entry: `{"field":"title","maxRunes":1,"ellipsis":"…"}`,
		want:  "must be shorter than maxRunes",
	}, {
		name:  "non-positive maxRunes",
		entry: `{"field":"title","maxRunes":0,"ellipsis":"…"}`,
		want:  "maxRunes must be positive",
	}, {
		name:  "unknown key",
		entry: `{"field":"title","maxRunes":8,"suffix":"…"}`,
		want:  "unknown key",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := validJSONArtifactBundle()
			bundle.Schema = json.RawMessage(fmt.Sprintf(
				`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",`+
					`"additionalProperties":false,"required":["title"],"properties":%s,"x-octo-constraints":%s}`,
				validArtifactProperties, constraintWith(tc.entry),
			))
			_, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
			if err == nil {
				t.Fatalf("compiled an artifact with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}

// The happy path: a declared display ceiling clamps at render time, and the value
// is accepted rather than rejected — the behaviour ai.reasoning-process@0.4.0 relies
// on for `phases[].thought`.
func TestCompileJSONArtifactTruncatesDeclaredStringAtRender(t *testing.T) {
	bundle := validJSONArtifactBundle()
	bundle.Schema = json.RawMessage(fmt.Sprintf(
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",`+
			`"additionalProperties":false,"required":["title"],"properties":%s,"x-octo-constraints":%s}`,
		validArtifactProperties,
		`{"aggregateArrayLimits":[{"parentArray":"groups","childArray":"items","maxTotalItems":2}],`+
			`"truncateStrings":[{"field":"title","maxRunes":8,"ellipsis":"…"}]}`,
	))
	artifact, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
	if err != nil {
		t.Fatalf("CompileJSONArtifact: %v", err)
	}

	// 32 is the accept ceiling; 8 the display one. A 20-rune title must render as
	// 7 runes + the ellipsis, not be refused.
	fields := json.RawMessage(`{"title":"aaaaaaaaaaaaaaaaaaaa","groups":[{"items":["a"]}]}`)
	card, err := artifact.Template.Build(context.Background(), "shown", fields, BuildEnv{Lang: "en-US"})
	if err != nil {
		t.Fatalf("Build with an over-display-ceiling title: %v", err)
	}
	encoded, err := json.Marshal(card.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encoded); !strings.Contains(got, `"aaaaaaa…"`) {
		t.Fatalf("body = %s, want the title clamped to 7 runes + ellipsis", got)
	}

	// At or under the display ceiling nothing changes.
	exact := json.RawMessage(`{"title":"aaaaaaaa","groups":[{"items":["a"]}]}`)
	card, err = artifact.Template.Build(context.Background(), "shown", exact, BuildEnv{Lang: "en-US"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(card.Body)
	if got := string(encoded); !strings.Contains(got, `"aaaaaaaa"`) || strings.Contains(got, "…") {
		t.Fatalf("body = %s, want the exact-length title untouched", got)
	}
}

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

func artifactSchemaWithIntegerLimits(literal string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"$schema":"http://json-schema.org/draft-07/schema#",
		"type":"object",
		"additionalProperties":false,
		"required":["title"],
		"properties":{
			"title":{"type":"string","maxLength":%[1]s},
			"groups":{"type":"array","maxItems":%[1]s,"items":{
				"type":"object","additionalProperties":false,
				"properties":{"items":{"type":"array","maxItems":%[1]s,"items":{"type":"string","maxLength":8}}}
			}}
		},
		"x-octo-constraints":{"aggregateArrayLimits":[{
			"parentArray":"groups","childArray":"items","maxTotalItems":%[1]s
		}]}
	}`, literal))
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

// The two x-octo-constraints declarations are independently optional. This used to
// be false: parseAggregateArrayLimits required aggregateArrayLimits whenever the
// block existed, so an artifact that only wanted a display ceiling could not compile
// (PR#712 review). An empty block stays an error — it declares nothing, so it is
// always a mistake rather than a deliberate no-op.
func TestCompileJSONArtifactConstraintsAreIndependentlyOptional(t *testing.T) {
	compile := func(t *testing.T, constraints string) (*CompiledArtifact, error) {
		t.Helper()
		bundle := validJSONArtifactBundle()
		schema := `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",` +
			`"additionalProperties":false,"required":["title"],"properties":` + validArtifactProperties
		if constraints != "" {
			schema += `,"x-octo-constraints":` + constraints
		}
		bundle.Schema = json.RawMessage(schema + `}`)
		return CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
	}

	const aggregate = `"aggregateArrayLimits":[{"parentArray":"groups","childArray":"items","maxTotalItems":2}]`
	const truncate = `"truncateStrings":[{"field":"title","maxRunes":8,"ellipsis":"…"}]`

	for _, tc := range []struct{ name, constraints string }{
		{"no constraints block at all", ""},
		{"aggregate limits only", `{` + aggregate + `}`},
		{"truncations only", `{` + truncate + `}`},
		{"both", `{` + aggregate + `,` + truncate + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := compile(t, tc.constraints); err != nil {
				t.Fatalf("CompileJSONArtifact(%s): %v", tc.name, err)
			}
		})
	}

	t.Run("empty block is refused", func(t *testing.T) {
		_, err := compile(t, `{}`)
		if err == nil {
			t.Fatal("compiled an artifact whose x-octo-constraints declares nothing")
		}
		if !strings.Contains(err.Error(), "at least one constraint") {
			t.Fatalf("error = %q, want it to name the empty declaration", err.Error())
		}
	})

	// Truncation must actually take effect when declared alone — otherwise the key
	// would parse but the engine would ignore it, which is worse than rejecting it.
	t.Run("truncation alone still clamps at render", func(t *testing.T) {
		artifact, err := compile(t, `{`+truncate+`}`)
		if err != nil {
			t.Fatalf("CompileJSONArtifact: %v", err)
		}
		fields := json.RawMessage(`{"title":"aaaaaaaaaaaaaaaaaaaa","groups":[{"items":["a"]}]}`)
		card, err := artifact.Template.Build(context.Background(), "shown", fields, BuildEnv{Lang: "en-US"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		encoded, err := json.Marshal(card.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(encoded); !strings.Contains(got, `"aaaaaaa…"`) {
			t.Fatalf("body = %s, want the title clamped to 7 runes + ellipsis", got)
		}
	})
}

// 声明了展示上限之后，goldens 必须记录**渲染器真正吐出的**字节。这两条路以前是分开的：
// checkArtifactGoldens 直接展开原始样本，而 jsonTemplate.Build 先截断再展开，所以一个
// 超过自身展示上限的样本会让 goldens 记下生产永不产出的字节（PR#712 review P2-1）。
// goldens 是产物唯一的字节级记录，而「匹配的 golden 洗不白伪造的控件」这条论证依赖
// selfCheckCompiledJSON 与 golden 检查看的是同一帧 —— 两边口径不同这条论证就塌了。
func TestCompileJSONArtifactGoldensSeeTheTruncatedRender(t *testing.T) {
	// 样本 title 是 20 个 a，展示上限 8 ⇒ 渲染结果是 7 个 a + 省略号。
	const sample = `{"title":"aaaaaaaaaaaaaaaaaaaa","groups":[{"items":["a"]}]}`
	const truncated = `aaaaaaa…`
	const untruncated = `aaaaaaaaaaaaaaaaaaaa`

	build := func(t *testing.T, goldenText string) (*CompiledArtifact, error) {
		t.Helper()
		bundle := validJSONArtifactBundle()
		bundle.Schema = json.RawMessage(fmt.Sprintf(
			`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",`+
				`"additionalProperties":false,"required":["title"],"properties":%s,`+
				`"x-octo-constraints":{"truncateStrings":[{"field":"title","maxRunes":8,"ellipsis":"…"}]}}`,
			validArtifactProperties,
		))
		bundle.Samples["shown"] = json.RawMessage(sample)
		bundle.Goldens["shown"] = json.RawMessage(fmt.Sprintf(
			`{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":%q,"wrap":true}]}`,
			goldenText))
		return CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
	}

	// 记录截断后字节的 golden 必须通过 —— 那才是生产字节。
	if _, err := build(t, truncated); err != nil {
		t.Fatalf("golden recording the truncated render should compile: %v", err)
	}

	// 记录未截断字节的 golden 必须被拒。这正是修复前会被接受的情形。
	_, err := build(t, untruncated)
	if err == nil {
		t.Fatal("golden recording the UNtruncated expansion compiled — the golden gate is not " +
			"looking at what the renderer emits, so goldens can record bytes production never sends")
	}
	if !strings.Contains(err.Error(), "does not match golden") {
		t.Fatalf("error = %q, want a golden mismatch", err.Error())
	}
}

// x-octo-constraints 里的字符串字段必须**显式**按类型读。可选字段（arrayField /
// ellipsis）尤其如此：用 `v, _ := entry[k].(string)` 会把「类型不对」映射到与「键不存在」
// 相同的 ""，于是声明的语义被静默改写，编译期无错、渲染期无信号。
//
// 具体后果（#712 follow-up）：非字符串 arrayField 变 "" 后，截断从 phases[].thought
// 重定向到一个不存在的顶层 thought —— **声明的展示上限彻底停止生效**，而这正是
// truncateStrings 存在的唯一目的。非字符串 ellipsis 变 "" 则静默丢掉省略号，截断过的值
// 与本来就短的值再也分不出来。
func TestCompileJSONArtifactRejectsWrongTypedConstraintFields(t *testing.T) {
	compile := func(t *testing.T, constraints string) error {
		t.Helper()
		bundle := validJSONArtifactBundle()
		bundle.Schema = json.RawMessage(fmt.Sprintf(
			`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",`+
				`"additionalProperties":false,"required":["title"],"properties":%s,"x-octo-constraints":%s}`,
			validArtifactProperties, constraints,
		))
		_, err := CompileJSONArtifact(context.Background(), bundle, DefaultCompileLimits())
		return err
	}

	const aggregate = `"aggregateArrayLimits":[{"parentArray":"groups","childArray":"items","maxTotalItems":2}]`

	for _, tc := range []struct{ name, constraints, want string }{
		// 两个可选字段：这两条是真正的 fail-open，修复前**编译通过**。
		{
			name:        "arrayField 是数字",
			constraints: `{"truncateStrings":[{"arrayField":7,"field":"title","maxRunes":8,"ellipsis":"…"}]}`,
			want:        "arrayField must be a string",
		},
		{
			name:        "arrayField 是布尔",
			constraints: `{"truncateStrings":[{"arrayField":false,"field":"title","maxRunes":8,"ellipsis":"…"}]}`,
			want:        "arrayField must be a string",
		},
		{
			name:        "ellipsis 是数字",
			constraints: `{"truncateStrings":[{"field":"title","maxRunes":8,"ellipsis":3}]}`,
			want:        "ellipsis must be a string",
		},
		// 必填字段：修复前靠下游 `== ""` 偶然挡住，但把类型错误报成「is required」。
		{
			name:        "field 是数字",
			constraints: `{"truncateStrings":[{"field":9,"maxRunes":8,"ellipsis":"…"}]}`,
			want:        "field must be a string",
		},
		{
			name:        "field 缺失",
			constraints: `{"truncateStrings":[{"maxRunes":8,"ellipsis":"…"}]}`,
			want:        "field is required",
		},
		// 空串与未 trim 是两条不同的规则，消息不能互相指控（review of #716）。
		{
			name:        "field 是空串",
			constraints: `{"truncateStrings":[{"field":"","maxRunes":8,"ellipsis":"…"}]}`,
			want:        "field must not be empty",
		},
		{
			name:        "field 前后有空白",
			constraints: `{"truncateStrings":[{"field":" title","maxRunes":8,"ellipsis":"…"}]}`,
			want:        "field must be trimmed",
		},
		// arrayField 是 schema 路径，未 trim 永远匹配不上属性名，所以照旧要求 trim。
		{
			name:        "arrayField 前后有空白",
			constraints: `{"truncateStrings":[{"arrayField":"groups ","field":"title","maxRunes":8,"ellipsis":"…"}]}`,
			want:        "arrayField must be trimmed",
		},
		{
			name:        "parentArray 是数字",
			constraints: `{"aggregateArrayLimits":[{"parentArray":1,"childArray":"items","maxTotalItems":2}]}`,
			want:        "parentArray must be a string",
		},
		{
			name:        "childArray 是对象",
			constraints: `{"aggregateArrayLimits":[{"parentArray":"groups","childArray":{},"maxTotalItems":2}]}`,
			want:        "childArray must be a string",
		},
		{
			name:        "childArray 缺失",
			constraints: `{"aggregateArrayLimits":[{"parentArray":"groups","maxTotalItems":2}]}`,
			want:        "childArray is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compile(t, tc.constraints)
			if err == nil {
				t.Fatalf("类型错误的 %s 编译通过了 —— 声明会被静默改写语义", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}

	// 反向：合法的省略 arrayField / ellipsis 必须继续通过，否则这次收紧就把可选性也一起收掉了。
	//
	// **ellipsis 不做 trim 要求**：它是展示文本而非 schema 路径，#716 之前的解析器逐字使用它，
	// 而 " …"（前导空格的省略号）是合法的排版选择。给它加 trim 会把一个**类型正确**的值从
	// 接受变成拒绝 —— 超出「只有 present-but-wrong-typed 才报错」这个本次改动自己声明的
	// 契约（review of #716 抓到这处过度收紧）。所以这里正面钉住它仍被接受。
	for _, tc := range []struct{ name, constraints string }{
		{"省略 arrayField（顶层字段）", `{` + aggregate + `,"truncateStrings":[{"field":"title","maxRunes":8,"ellipsis":"…"}]}`},
		{"省略 ellipsis（无省略号截断）", `{` + aggregate + `,"truncateStrings":[{"field":"title","maxRunes":8}]}`},
		{"ellipsis 前导空格", `{` + aggregate + `,"truncateStrings":[{"field":"title","maxRunes":8,"ellipsis":" …"}]}`},
		{"ellipsis 尾随空格", `{` + aggregate + `,"truncateStrings":[{"field":"title","maxRunes":8,"ellipsis":"… "}]}`},
		{"ellipsis 含换行", `{` + aggregate + `,"truncateStrings":[{"field":"title","maxRunes":8,"ellipsis":"...\n"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := compile(t, tc.constraints); err != nil {
				t.Fatalf("合法的省略应通过：%v", err)
			}
		})
	}
}
