package cardtmpl

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"

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
	stringTruncations    []jsonTemplateStringTruncation
}

type jsonTemplateFieldValidator interface {
	validateFields(any) error
}

type jsonTemplateAggregateArrayLimit struct {
	ParentArray   string `json:"parentArray"`
	ChildArray    string `json:"childArray"`
	MaxTotalItems int    `json:"maxTotalItems"`
}

// jsonTemplateStringTruncation declares a field the engine clamps at render time
// instead of rejecting. The schema's own `maxLength` stays the *accept* ceiling
// (an unbounded string would fail RequireBoundedSchema, and a pathological
// payload must still be refused); this is the narrower *display* ceiling applied
// after validation, so an over-long value degrades to truncated text rather than
// failing the whole card.
//
// It also makes the rendered frame's size independent of how generous the accept
// ceiling is — the declared field contributes the same bytes whether the caller
// sent 400 code points or 4001.
//
// That is NOT the same as keeping the frame inside the column it is persisted in,
// and an earlier version of this comment wrongly claimed it was. A frame's size is
// the template's fixed chrome plus *every* free string plus the `plain` copy
// cardmsg.Finalize appends, so clamping one field bounds one term of a sum.
// Measured on ai.reasoning-process@0.4.0: with `thought` clamped to a single code
// point the adversarial worst case is still 107% of the column. The persistence
// budget is enforced at the write boundary instead — see
// carddispatch.NormalizeFrameForPersistence.
type jsonTemplateStringTruncation struct {
	// ArrayField, when set, scopes Field to objects inside that top-level array
	// (e.g. ArrayField="phases", Field="thought" → phases[].thought). Empty
	// means Field is a top-level property.
	ArrayField string `json:"arrayField"`
	Field      string `json:"field"`
	MaxRunes   int    `json:"maxRunes"`
	// Ellipsis is appended when a value is cut, and counts toward MaxRunes, so
	// the result is never longer than MaxRunes code points.
	Ellipsis string `json:"ellipsis"`
}

// truncate returns value clamped to MaxRunes code points, appending Ellipsis when
// it had to cut. Counting is by code point to match JSON Schema's maxLength unit;
// cutting by byte would split a multi-byte rune.
func (tr jsonTemplateStringTruncation) truncate(value string) (string, bool) {
	runes := []rune(value)
	if len(runes) <= tr.MaxRunes {
		return value, false
	}
	suffix := []rune(tr.Ellipsis)
	keep := tr.MaxRunes - len(suffix)
	if keep < 0 {
		keep = 0
	}
	return string(runes[:keep]) + tr.Ellipsis, true
}

// SetMeta satisfies metaSetter; Registry injects the assembled meta at Register.
func (t *jsonTemplate) SetMeta(m TemplateMeta) { t.meta = m }

// Meta returns a defensive deep copy, matching the Template contract.
func (t *jsonTemplate) Meta() TemplateMeta { return t.meta.Clone() }

// applyStringTruncations clamps declared fields in place, after schema validation
// and before expansion. Declared-only: anything not named in
// x-octo-constraints.truncateStrings keeps the fail-close behaviour, so this
// cannot silently soften a bound nobody opted into.
func (t *jsonTemplate) applyStringTruncations(data map[string]any) {
	applyStringTruncationsTo(t.stringTruncations, data)
}

// applyStringTruncationsTo is the free form, so the compile-time golden check can
// run the *same* clamp the renderer does. Keeping it a method only would leave the
// two paths agreeing by coincidence: checkArtifactGoldens expands the raw sample,
// so a sample above its own display ceiling would make the goldens record bytes
// production never emits — and the goldens are the artifact's only byte-level
// record of what a template emits (PR#712 review P2-1).
func applyStringTruncationsTo(truncations []jsonTemplateStringTruncation, data map[string]any) {
	if len(truncations) == 0 || data == nil {
		return
	}
	clamp := func(holder map[string]any, field string, tr jsonTemplateStringTruncation) {
		raw, exists := holder[field]
		if !exists {
			return
		}
		value, ok := raw.(string)
		if !ok {
			return
		}
		if truncated, cut := tr.truncate(value); cut {
			holder[field] = truncated
		}
	}
	for _, tr := range truncations {
		if tr.ArrayField == "" {
			clamp(data, tr.Field, tr)
			continue
		}
		items, ok := data[tr.ArrayField].([]any)
		if !ok {
			continue
		}
		for _, rawItem := range items {
			if item, ok := rawItem.(map[string]any); ok {
				clamp(item, tr.Field, tr)
			}
		}
	}
}

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
	t.applyStringTruncations(data)
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
	bundle, err := LoadJSONBundle(assets, root, CatalogDescriptor{
		Engine:     JSONTemplateEngineV1,
		Visibility: CatalogVisibilityPrivate,
	})
	if err != nil {
		panic(fmt.Errorf("cardtmpl: RegisterJSON %s: %w", root, err))
	}
	artifact, err := CompileJSONArtifact(context.Background(), bundle, staticCompileLimits())
	if err != nil {
		panic(fmt.Errorf("cardtmpl: RegisterJSON %s: %w", root, err))
	}
	r.registerCompiledJSON(artifact)
}
