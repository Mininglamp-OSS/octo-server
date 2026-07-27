package cardtmpl

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/jsontmpl"
)

// jsonTemplate is the generic Template backing JSON-mode cards (roadmap E1). Its
// Build compiles the view's `.template.json` against caller data through the
// jsontmpl evaluator instead of hand-writing the body in Go, so one instance
// serves any card that ships a handoff with `views[].template` files. The
// per-view template trees are parsed once at register time (viewAST) and only
// re-bound per render — no per-frame JSON parse (progress cards re-render often).
type jsonTemplate struct {
	meta                 TemplateMeta
	viewAST              map[ViewKey]any
	aggregateArrayLimits []jsonTemplateAggregateArrayLimit
}

type jsonTemplateFieldValidator interface {
	validateFields(any) error
}

type jsonTemplateAggregateArrayLimit struct {
	ParentArray   string `json:"parentArray"`
	ChildArray    string `json:"childArray"`
	MaxTotalItems int    `json:"maxTotalItems"`
}

type jsonTemplateConstraintSet struct {
	AggregateArrayLimits []jsonTemplateAggregateArrayLimit `json:"aggregateArrayLimits"`
}

type jsonTemplateSchemaEnvelope struct {
	Properties  map[string]json.RawMessage `json:"properties"`
	Constraints json.RawMessage            `json:"x-octo-constraints"`
}

// SetMeta satisfies metaSetter; Registry injects the assembled meta at Register.
func (t *jsonTemplate) SetMeta(m TemplateMeta) { t.meta = m }

// Meta returns a defensive deep copy, matching the Template contract.
func (t *jsonTemplate) Meta() TemplateMeta { return t.meta.Clone() }

func (t *jsonTemplate) validateFields(value any) error {
	if len(t.aggregateArrayLimits) == 0 {
		return nil
	}
	root, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("JSON template fields are %T, want object", value)
	}
	for _, limit := range t.aggregateArrayLimits {
		parentValue, exists := root[limit.ParentArray]
		if !exists {
			continue
		}
		parents, ok := parentValue.([]any)
		if !ok {
			return fmt.Errorf("aggregate parent %q is %T, want array", limit.ParentArray, parentValue)
		}
		total := 0
		for index, parentValue := range parents {
			parent, ok := parentValue.(map[string]any)
			if !ok {
				return fmt.Errorf("aggregate parent %q item %d is %T, want object", limit.ParentArray, index, parentValue)
			}
			childValue, exists := parent[limit.ChildArray]
			if !exists {
				continue
			}
			children, ok := childValue.([]any)
			if !ok {
				return fmt.Errorf("aggregate child %q[].%q is %T, want array", limit.ParentArray, limit.ChildArray, childValue)
			}
			total += len(children)
			if total > limit.MaxTotalItems {
				return fmt.Errorf("aggregate %s[].%s has %d items, max %d",
					limit.ParentArray, limit.ChildArray, total, limit.MaxTotalItems)
			}
		}
	}
	return nil
}

func (t *jsonTemplate) validateRawFields(fields json.RawMessage) error {
	if len(fields) == 0 {
		return fmt.Errorf("%w: fields must not be empty", ErrFieldsInvalid)
	}
	var parsed any
	if err := json.Unmarshal(fields, &parsed); err != nil {
		return fmt.Errorf("%w: %v", ErrFieldsInvalid, err)
	}
	if err := t.meta.InputSchema.Validate(parsed); err != nil {
		return fmt.Errorf("%w: %v", ErrFieldsInvalid, err)
	}
	if err := t.validateFields(parsed); err != nil {
		return fmt.Errorf("%w: %v", ErrFieldsInvalid, err)
	}
	return nil
}

// jsonTemplateEscaper is the data-escaping policy for JSON templates (decision
// D6): identity. Goldens bind caller data literally; the trust boundary is
// enforced by cardmsg.Validate (URL allowlist over markdown links + element
// whitelist + node/size caps) which renderCore applies to the compiled card
// after Build — not by markdown-escaping inside the engine. The escaper stays a
// seam so a future card can opt into escaping without touching the evaluator.
func jsonTemplateEscaper(s string) string { return s }

// Build expands the view template for state against the (already schema-checked)
// fields and returns the AC body/actions fragment. It marshals no top level and
// writes no metadata — that is renderCore's job.
func (t *jsonTemplate) Build(ctx context.Context, state State, fields json.RawMessage, env BuildEnv) (BuildResult, error) {
	view, ok := t.meta.ViewFor(state)
	if !ok {
		return BuildResult{}, fmt.Errorf("jsonTemplate: no view for state %q", state)
	}
	ast, ok := t.viewAST[view]
	if !ok {
		return BuildResult{}, fmt.Errorf("jsonTemplate: view %q has no compiled template", view)
	}
	var data map[string]any
	if err := json.Unmarshal(fields, &data); err != nil {
		return BuildResult{}, fmt.Errorf("jsonTemplate: unmarshal fields: %w", err)
	}
	expanded, err := jsontmpl.Expand(ctx, ast, jsontmpl.Scope{Data: data}, jsonTemplateEscaper)
	if err != nil {
		return BuildResult{}, fmt.Errorf("jsonTemplate: expand view %q: %w", view, err)
	}
	card, ok := expanded.(map[string]any)
	if !ok {
		return BuildResult{}, fmt.Errorf("jsonTemplate: expanded view %q is %T, want object", view, expanded)
	}
	body, ok := card["body"].([]any)
	if !ok || len(body) == 0 {
		return BuildResult{}, fmt.Errorf("jsonTemplate: view %q produced no body", view)
	}
	var actions []any
	if a, ok := card["actions"].([]any); ok {
		actions = a
	}
	// JSON display templates carry no separate deep link (any OpenUrl lives in
	// the body); renderCore omits metadata.webUrl when DeepLink is empty.
	return BuildResult{Body: body, Actions: actions}, nil
}

// FallbackText derives plain text from the compiled card via cardmsg.BuildPlain
// — the same plain derivation the send path uses — so the fallback matches what
// clients reduce the card to. Unlike Render this path does not run the full
// cardmsg.Validate gate (the built card is a body/actions fragment, not a wire
// payload). Its resource surface is bounded on the input side by jsontmpl.Expand's
// maxExpandNodes ceiling (so an unbounded $data array can't balloon the fragment).
// It runs the same schema and JSON-template semantic constraints as Render before
// expansion. BuildPlain itself imposes NO final payload-size limit (only Finalize /
// RecheckPayloadSize do, neither runs here — PR #654 review yujiawei P2). Injection
// is not a concern — BuildPlain reduces markdown links to their visible label.
func (t *jsonTemplate) FallbackText(state State, fields json.RawMessage, lang string) (string, error) {
	if err := t.validateRawFields(fields); err != nil {
		return "", err
	}
	br, err := t.Build(context.Background(), state, fields, BuildEnv{Lang: lang})
	if err != nil {
		return "", err
	}
	card := map[string]any{
		"type":    "AdaptiveCard",
		"version": cardmsg.CardVersion,
		"body":    br.Body,
	}
	if len(br.Actions) > 0 {
		card["actions"] = br.Actions
	}
	return cardmsg.BuildPlain(card), nil
}

// RegisterJSON registers a JSON-mode card from its handoff assets. Every
// manifest view must declare a `template` path; each file is parsed into a
// reusable AST at register time. It then delegates to Register, which assembles
// the TemplateMeta (schema / views / interactions), injects it via SetMeta, and
// runs the sample self-check — so JSON cards get the identical fail-close
// registration guarantees as Go cards (missing template / schema / bad sample /
// whitelist violation all panic at boot, never at first render).
func (r *Registry) RegisterJSON(assets embed.FS, root string) {
	mf := loadManifest(assets, root)
	aggregateArrayLimits := loadJSONTemplateAggregateArrayLimits(assets, root)
	viewAST := make(map[ViewKey]any, len(mf.Views))
	for vk, vs := range mf.Views {
		if strings.TrimSpace(vs.Template) == "" {
			panic(fmt.Errorf("cardtmpl: RegisterJSON: view %q in %s/manifest.json declares no template path", vk, root))
		}
		p := path.Join(root, vs.Template)
		b, err := assets.ReadFile(p)
		if err != nil {
			panic(fmt.Errorf("cardtmpl: RegisterJSON: read template %s: %w", p, err))
		}
		var ast any
		if err := json.Unmarshal(b, &ast); err != nil {
			panic(fmt.Errorf("cardtmpl: RegisterJSON: parse template %s: %w", p, err))
		}
		// Fail-close: every Action.ToggleVisibility target must resolve to an
		// element id in the same template (a dangling target is an authoring bug
		// — a button that toggles nothing — caught here, not at first render).
		if err := jsontmpl.ValidateToggleTargets(ast); err != nil {
			panic(fmt.Errorf("cardtmpl: RegisterJSON: template %s: %w", p, err))
		}
		viewAST[vk] = ast
	}
	r.Register(&jsonTemplate{
		viewAST:              viewAST,
		aggregateArrayLimits: aggregateArrayLimits,
	}, assets, root)
}

func loadJSONTemplateAggregateArrayLimits(assets embed.FS, root string) []jsonTemplateAggregateArrayLimit {
	p := path.Join(root, "contract", "data.schema.json")
	b, err := assets.ReadFile(p)
	if err != nil {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: read schema constraints %s: %w", p, err))
	}
	var schema jsonTemplateSchemaEnvelope
	if err := json.Unmarshal(b, &schema); err != nil {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: parse schema constraints %s: %w", p, err))
	}
	if len(schema.Constraints) == 0 {
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(schema.Constraints))
	decoder.DisallowUnknownFields()
	var constraints jsonTemplateConstraintSet
	if err := decoder.Decode(&constraints); err != nil {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: parse x-octo-constraints in %s: %w", p, err))
	}
	if len(constraints.AggregateArrayLimits) == 0 {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: x-octo-constraints in %s has no aggregateArrayLimits", p))
	}

	seen := make(map[string]struct{}, len(constraints.AggregateArrayLimits))
	for index, limit := range constraints.AggregateArrayLimits {
		if limit.ParentArray == "" || limit.ParentArray != strings.TrimSpace(limit.ParentArray) {
			panic(fmt.Errorf("cardtmpl: RegisterJSON: aggregateArrayLimits[%d].parentArray is required in %s", index, p))
		}
		if limit.ChildArray == "" || limit.ChildArray != strings.TrimSpace(limit.ChildArray) {
			panic(fmt.Errorf("cardtmpl: RegisterJSON: aggregateArrayLimits[%d].childArray is required in %s", index, p))
		}
		if limit.MaxTotalItems <= 0 {
			panic(fmt.Errorf("cardtmpl: RegisterJSON: aggregateArrayLimits[%d].maxTotalItems must be positive in %s", index, p))
		}
		key := limit.ParentArray + "\x00" + limit.ChildArray
		if _, duplicate := seen[key]; duplicate {
			panic(fmt.Errorf("cardtmpl: RegisterJSON: duplicate aggregateArrayLimits target %s[].%s in %s",
				limit.ParentArray, limit.ChildArray, p))
		}
		seen[key] = struct{}{}
		validateJSONTemplateAggregateTarget(schema.Properties, limit, p)
	}
	return constraints.AggregateArrayLimits
}

func validateJSONTemplateAggregateTarget(
	properties map[string]json.RawMessage,
	limit jsonTemplateAggregateArrayLimit,
	schemaPath string,
) {
	parentRaw, exists := properties[limit.ParentArray]
	if !exists {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: aggregate parentArray %q not found in %s", limit.ParentArray, schemaPath))
	}
	var parent struct {
		Type  string `json:"type"`
		Items struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"items"`
	}
	if err := json.Unmarshal(parentRaw, &parent); err != nil {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: parse aggregate parentArray %q in %s: %w",
			limit.ParentArray, schemaPath, err))
	}
	if parent.Type != "array" || parent.Items.Type != "object" {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: aggregate parentArray %q must be an array of objects in %s",
			limit.ParentArray, schemaPath))
	}
	childRaw, exists := parent.Items.Properties[limit.ChildArray]
	if !exists {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: aggregate childArray %q not found under %q in %s",
			limit.ChildArray, limit.ParentArray, schemaPath))
	}
	var child struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(childRaw, &child); err != nil {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: parse aggregate childArray %q under %q in %s: %w",
			limit.ChildArray, limit.ParentArray, schemaPath, err))
	}
	if child.Type != "array" {
		panic(fmt.Errorf("cardtmpl: RegisterJSON: aggregate childArray %q under %q must be an array in %s",
			limit.ChildArray, limit.ParentArray, schemaPath))
	}
}
