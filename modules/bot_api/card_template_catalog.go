package bot_api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
)

const (
	botTemplateWireV1        = "template-ref/v1"
	botTemplateLookupTimeout = 5 * time.Second
)

var (
	errBotTemplateRequestInvalid     = errors.New("bot template request invalid")
	errBotTemplateCatalogUnavailable = errors.New("bot template catalog unavailable")
)

type botTemplateRef struct {
	ID      cardtmpl.ID `json:"id"`
	Version string      `json:"version"`
}

type botTemplatePolicy struct {
	AdvertisedSend []botTemplateRef
	EditCompatible []botTemplateRef
}

type botTemplateViewCapability struct {
	Name          string   `json:"name"`
	States        []string `json:"states"`
	WireProfile   string   `json:"wire_profile"`
	SubmitActions []string `json:"submit_actions"`
}

type botTemplateCapability struct {
	ID      string                      `json:"id"`
	Version string                      `json:"version"`
	Views   []botTemplateViewCapability `json:"views"`
}

type botTemplatingCapability struct {
	Supported bool                    `json:"supported"`
	Wire      string                  `json:"wire"`
	Templates []botTemplateCapability `json:"templates"`
}

type botCardTemplateCatalog struct {
	catalog     cardtmpl.Catalog
	sendAllowed map[botTemplateRef]struct{}
	editAllowed map[botTemplateRef]struct{}
	capability  botTemplatingCapability
	// staticSendByID is the same policy as sendAllowed, keyed by template ID.
	// PR-C D6 requires at most one advertised send version per ID, so this is a
	// total map rather than a convenience index — the constructor rejects a
	// policy that would make it lossy.
	staticSendByID map[cardtmpl.ID]botTemplateRef
	// staticSendIDs preserves a deterministic order for the manifest.
	staticSendIDs []cardtmpl.ID
	// authorization overrides the process-wide resolver in tests. Production
	// leaves it nil and reads cardtmpl.DefaultAuthorizationSource() per request,
	// so a catalog built before the composition root still picks the resolver up.
	authorization cardtmpl.RuntimeAuthorizationSource
}

func defaultBotTemplateRefs() []botTemplateRef {
	return []botTemplateRef{{
		ID:      aireasoningprocess.TemplateID,
		Version: aireasoningprocess.TemplateVersionV3,
	}}
}

func defaultBotTemplatePolicy() botTemplatePolicy {
	return botTemplatePolicy{
		AdvertisedSend: defaultBotTemplateRefs(),
		EditCompatible: []botTemplateRef{
			{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV1},
			{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV2},
			{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV3},
		},
	}
}

func newBotCardTemplateCatalog(source any, refs []botTemplateRef) (*botCardTemplateCatalog, error) {
	return newBotCardTemplateCatalogWithPolicy(source, botTemplatePolicy{
		AdvertisedSend: refs,
		EditCompatible: refs,
	})
}

func newBotCardTemplateCatalogWithPolicy(
	source any,
	policy botTemplatePolicy,
) (*botCardTemplateCatalog, error) {
	runtimeCatalog, err := botRuntimeCatalog(source)
	if err != nil {
		return nil, err
	}
	if len(policy.AdvertisedSend) == 0 {
		return nil, fmt.Errorf("bot template catalog: empty advertised/send allowlist")
	}
	if len(policy.EditCompatible) == 0 {
		return nil, fmt.Errorf("bot template catalog: empty edit-compatible allowlist")
	}
	catalog := &botCardTemplateCatalog{
		catalog:        runtimeCatalog,
		sendAllowed:    make(map[botTemplateRef]struct{}, len(policy.AdvertisedSend)),
		editAllowed:    make(map[botTemplateRef]struct{}, len(policy.EditCompatible)),
		staticSendByID: make(map[cardtmpl.ID]botTemplateRef, len(policy.AdvertisedSend)),
		capability: botTemplatingCapability{
			Supported: true,
			Wire:      botTemplateWireV1,
			Templates: make([]botTemplateCapability, 0, len(policy.AdvertisedSend)),
		},
	}
	for _, ref := range policy.AdvertisedSend {
		if strings.TrimSpace(string(ref.ID)) == "" || strings.TrimSpace(ref.Version) == "" {
			return nil, fmt.Errorf("bot template catalog: id and explicit version are required")
		}
		// PR-C D6: the guard is per template ID, not per exact ref. Two
		// versions of one ID are not a harmless duplicate — the dynamic
		// activation pointer resolves to a single version per ID, so a policy
		// advertising two of them has no well-defined answer to "which version
		// does the pointer shadow", and the manifest and the send path would be
		// free to pick differently.
		if existing, duplicate := catalog.staticSendByID[ref.ID]; duplicate {
			return nil, fmt.Errorf("bot template catalog: %s is advertised at both %s and %s; "+
				"exactly one send version per template ID", ref.ID, existing.Version, ref.Version)
		}
		meta, err := lookupBotTemplateMeta(runtimeCatalog, cardtmpl.CatalogExactRequest{
			Access: botCatalogAccess(cardtmpl.CatalogPurposeNewSend, "bot-api-manifest", ""),
			ID:     ref.ID, Version: ref.Version,
		})
		if err != nil {
			return nil, fmt.Errorf("bot template catalog: lookup %s@%s: %w", ref.ID, ref.Version, err)
		}
		if meta.ID != ref.ID || meta.Version != ref.Version || len(meta.Views) == 0 {
			return nil, fmt.Errorf("bot template catalog: incomplete metadata for %s@%s", ref.ID, ref.Version)
		}

		capability, err := templateCapabilityFromMeta(meta)
		if err != nil {
			return nil, err
		}
		catalog.sendAllowed[ref] = struct{}{}
		catalog.staticSendByID[ref.ID] = ref
		catalog.staticSendIDs = append(catalog.staticSendIDs, ref.ID)
		catalog.capability.Templates = append(catalog.capability.Templates, capability)
	}
	for _, ref := range policy.EditCompatible {
		if strings.TrimSpace(string(ref.ID)) == "" || strings.TrimSpace(ref.Version) == "" {
			return nil, fmt.Errorf("bot template catalog: edit-compatible id and explicit version are required")
		}
		if _, duplicate := catalog.editAllowed[ref]; duplicate {
			return nil, fmt.Errorf("bot template catalog: duplicate edit-compatible %s@%s", ref.ID, ref.Version)
		}
		meta, err := lookupBotTemplateMeta(runtimeCatalog, cardtmpl.CatalogExactRequest{
			Access: botCatalogAccess(cardtmpl.CatalogPurposeHistoricalEdit, "bot-api-manifest", ""),
			ID:     ref.ID, Version: ref.Version,
		})
		if err != nil {
			return nil, fmt.Errorf("bot template catalog: edit-compatible lookup %s@%s: %w", ref.ID, ref.Version, err)
		}
		if meta.ID != ref.ID || meta.Version != ref.Version || len(meta.Views) == 0 {
			return nil, fmt.Errorf("bot template catalog: incomplete edit-compatible metadata for %s@%s", ref.ID, ref.Version)
		}
		catalog.editAllowed[ref] = struct{}{}
	}
	for ref := range catalog.sendAllowed {
		if _, editable := catalog.editAllowed[ref]; !editable {
			return nil, fmt.Errorf("bot template catalog: advertised/send %s@%s is not edit-compatible", ref.ID, ref.Version)
		}
	}
	sort.Slice(catalog.capability.Templates, func(i, j int) bool {
		left, right := catalog.capability.Templates[i], catalog.capability.Templates[j]
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		return left.Version < right.Version
	})
	sort.Slice(catalog.staticSendIDs, func(i, j int) bool {
		return catalog.staticSendIDs[i] < catalog.staticSendIDs[j]
	})
	return catalog, nil
}

// templateCapabilityFromMeta projects one template's metadata into the wire
// capability shape. The constructor and the request-scoped manifest share it so
// that a dynamic template is described exactly as a static one would be — a
// producer must not be able to tell the two apart from the manifest, since the
// whole point of the catalog is that the source is an operator concern.
func templateCapabilityFromMeta(meta cardtmpl.TemplateMeta) (botTemplateCapability, error) {
	capability := botTemplateCapability{
		ID: string(meta.ID), Version: meta.Version,
		Views: make([]botTemplateViewCapability, 0, len(meta.Views)),
	}
	for view, spec := range meta.Views {
		if strings.TrimSpace(string(view)) == "" || len(spec.States) == 0 {
			return botTemplateCapability{}, fmt.Errorf(
				"bot template catalog: %s@%s has empty view/state metadata", meta.ID, meta.Version)
		}
		viewCapability := botTemplateViewCapability{
			Name:          string(view),
			WireProfile:   spec.WireProfile,
			States:        make([]string, 0, len(spec.States)),
			SubmitActions: make([]string, 0),
		}
		for _, state := range spec.States {
			if strings.TrimSpace(string(state)) == "" {
				return botTemplateCapability{}, fmt.Errorf(
					"bot template catalog: %s@%s view %s has empty state", meta.ID, meta.Version, view)
			}
			viewCapability.States = append(viewCapability.States, string(state))
		}
		sort.Strings(viewCapability.States)

		if spec.WireProfile == cardmsg.ProfileV2 {
			report, ok := meta.Interaction(view)
			if !ok {
				return botTemplateCapability{}, fmt.Errorf(
					"bot template catalog: %s@%s view %s missing interaction report", meta.ID, meta.Version, view)
			}
			seenActions := make(map[string]struct{})
			for _, action := range report.Actions {
				if action.Type != "Action.Submit" {
					continue
				}
				if strings.TrimSpace(action.ID) == "" {
					return botTemplateCapability{}, fmt.Errorf(
						"bot template catalog: %s@%s view %s has empty Submit id", meta.ID, meta.Version, view)
				}
				if _, duplicate := seenActions[action.ID]; duplicate {
					return botTemplateCapability{}, fmt.Errorf(
						"bot template catalog: %s@%s view %s has duplicate Submit id %q",
						meta.ID, meta.Version, view, action.ID)
				}
				seenActions[action.ID] = struct{}{}
				viewCapability.SubmitActions = append(viewCapability.SubmitActions, action.ID)
			}
			sort.Strings(viewCapability.SubmitActions)
		}
		capability.Views = append(capability.Views, viewCapability)
	}
	sort.Slice(capability.Views, func(i, j int) bool {
		return capability.Views[i].Name < capability.Views[j].Name
	})
	return capability, nil
}

func (c *botCardTemplateCatalog) Capability() botTemplatingCapability {
	if c == nil {
		return botTemplatingCapability{Supported: false, Wire: botTemplateWireV1, Templates: []botTemplateCapability{}}
	}
	out := botTemplatingCapability{
		Supported: c.capability.Supported,
		Wire:      c.capability.Wire,
		Templates: make([]botTemplateCapability, len(c.capability.Templates)),
	}
	for i, template := range c.capability.Templates {
		out.Templates[i] = botTemplateCapability{ID: template.ID, Version: template.Version, Views: make([]botTemplateViewCapability, len(template.Views))}
		for j, view := range template.Views {
			out.Templates[i].Views[j] = botTemplateViewCapability{
				Name: view.Name, WireProfile: view.WireProfile,
				States:        append(make([]string, 0, len(view.States)), view.States...),
				SubmitActions: append(make([]string, 0, len(view.SubmitActions)), view.SubmitActions...),
			}
		}
	}
	return out
}

func (c *botCardTemplateCatalog) RenderPayload(
	ctx context.Context,
	inbound map[string]any,
	env cardtmpl.BuildEnv,
) (map[string]any, error) {
	if c == nil {
		return nil, errBotTemplateCatalogUnavailable
	}
	return c.renderPayload(ctx, "bot-api-compat", cardtmpl.CatalogPurposeNewSend, inbound, env,
		c.staticRefResolver(c.sendAllowed, "not Bot-callable for new send"), false, "")
}

// RenderPayloadForPrincipal is the authenticated new-send path. Unlike the
// compat variant above it re-resolves the ref against the runtime catalog for
// this exact principal, so an activation, block or revoke takes effect on the
// very next send rather than at the next process restart.
func (c *botCardTemplateCatalog) RenderPayloadForPrincipal(
	ctx context.Context,
	principal botCatalogPrincipal,
	inbound map[string]any,
	env cardtmpl.BuildEnv,
) (map[string]any, error) {
	if c == nil || strings.TrimSpace(principal.BotID) == "" {
		return nil, errBotTemplateCatalogUnavailable
	}
	return c.renderPayload(ctx, principal.BotID, cardtmpl.CatalogPurposeNewSend, inbound, env,
		func(ctx context.Context, value any) (botTemplateRef, error) {
			return c.requireSendableRef(ctx, principal, value)
		}, true, provenanceSpaceID(principal, env))
}

func (c *botCardTemplateCatalog) RenderEditPayload(
	ctx context.Context,
	inbound map[string]any,
	env cardtmpl.BuildEnv,
) (map[string]any, error) {
	if c == nil {
		return nil, errBotTemplateCatalogUnavailable
	}
	return c.renderPayload(ctx, "bot-api-compat", cardtmpl.CatalogPurposeHistoricalEdit, inbound, env,
		c.staticRefResolver(c.editAllowed, "not edit-compatible"), false, "")
}

func (c *botCardTemplateCatalog) RenderEditPayloadForPrincipal(
	ctx context.Context,
	principal botCatalogPrincipal,
	inbound map[string]any,
	env cardtmpl.BuildEnv,
	storedSpaceID string,
) (map[string]any, error) {
	if c == nil || strings.TrimSpace(principal.BotID) == "" {
		return nil, errBotTemplateCatalogUnavailable
	}
	// An edit *preserves* the marker's Space; it does not re-derive it. The
	// stored value is what the send was authorized against, and it is already
	// on the frame — recomputing it here means the answer depends on whether
	// the resolver happens to work at edit time, so a gate rollback or a group
	// row that will not load would quietly rewrite the card with an empty
	// Space and disable every downstream `if Provenance.SpaceID != ""` guard.
	// This is the same rule pkg/cardtmpl's updater already follows when it
	// copies the stored markers through verbatim.
	//
	// Only a frame with no stored Space at all falls back to composing one,
	// which is what a pre-PR-C frame being edited for the first time needs.
	space := strings.TrimSpace(storedSpaceID)
	if space == "" {
		space = provenanceSpaceID(principal, env)
	}
	return c.renderPayload(ctx, principal.BotID, cardtmpl.CatalogPurposeHistoricalEdit, inbound, env,
		c.editRefResolver(principal), true, space)
}

// storedProvenanceSpaceID reads the Space a frame's own marker records, or ""
// when the frame carries no marker (a pre-PR-C card) or cannot be parsed. The
// guard that runs before it has already rejected a malformed marker, so an
// empty answer here means "nothing was stored", never "we failed to look".
func storedProvenanceSpaceID(envelope []byte) string {
	markers, err := cardmsg.CatalogFrameMarkers(envelope)
	if err != nil || !markers.HasProvenance {
		return ""
	}
	return markers.Provenance.SpaceID
}

func (c *botCardTemplateCatalog) renderPayload(
	ctx context.Context,
	botID string,
	purpose cardtmpl.CatalogPurpose,
	inbound map[string]any,
	env cardtmpl.BuildEnv,
	resolveRef func(context.Context, any) (botTemplateRef, error),
	authorProvenance bool,
	// provenanceSpaceID is the Space the marker is stamped with. It is a
	// separate value from env.SpaceID on purpose: env.SpaceID drives rendered
	// content (deep links) and send.go only populates it for DMs, while the
	// marker must carry the Space the send was actually *authorized* against —
	// for a group or thread that is the target's Space, which env.SpaceID never
	// sees. Stamping "" there would silently disable every downstream D3 Space
	// guard, since all of them are written as `if Provenance.SpaceID != ""`.
	provenanceSpaceID string,
) (map[string]any, error) {
	if c == nil || c.catalog == nil {
		return nil, errBotTemplateCatalogUnavailable
	}
	if !cardmsg.IsCardPayload(inbound) {
		return nil, fmt.Errorf("%w: type must be 17", errBotTemplateRequestInvalid)
	}
	allowedKeys := map[string]struct{}{
		"type": {}, "template_ref": {}, "state": {}, "data": {},
		"mention": {}, "reply": {},
	}
	for key := range inbound {
		if _, ok := allowedKeys[key]; !ok {
			return nil, fmt.Errorf("%w: field %q is not accepted in Registry mode", errBotTemplateRequestInvalid, key)
		}
	}
	if _, hasCard := inbound["card"]; hasCard {
		return nil, fmt.Errorf("%w: raw card and template_ref are mutually exclusive", errBotTemplateRequestInvalid)
	}
	ref, err := resolveRef(ctx, inbound["template_ref"])
	if err != nil {
		return nil, err
	}
	state, ok := inbound["state"].(string)
	if !ok || state == "" || state != strings.TrimSpace(state) {
		return nil, fmt.Errorf("%w: state is required", errBotTemplateRequestInvalid)
	}
	data, ok := inbound["data"].(map[string]any)
	if !ok || data == nil {
		return nil, fmt.Errorf("%w: data must be an object", errBotTemplateRequestInvalid)
	}
	if mirrored, exists := data["state"]; exists {
		mirroredState, ok := mirrored.(string)
		if !ok || mirroredState != state {
			return nil, fmt.Errorf("%w: data.state must equal state", errBotTemplateRequestInvalid)
		}
	}
	rawData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal data: %v", errBotTemplateRequestInvalid, err)
	}
	rendered, err := c.catalog.Render(ctx, cardtmpl.CatalogRenderRequest{
		Access: botCatalogAccess(purpose, botID, env.SpaceID),
		ID:     ref.ID, Version: ref.Version, State: cardtmpl.State(state), Fields: rawData, Env: env,
	})
	if err != nil {
		if errors.Is(err, cardtmpl.ErrFieldsInvalid) || errors.Is(err, cardtmpl.ErrStateUnknown) || errors.Is(err, cardtmpl.ErrTemplateUnknown) {
			return nil, fmt.Errorf("%w: %v", errBotTemplateRequestInvalid, err)
		}
		return nil, fmt.Errorf("bot template render %s@%s: %w", ref.ID, ref.Version, err)
	}
	// Retain the canonical ref plus the authenticated-principal provenance as
	// server-authored markers (PR-C D3). Raw Model B ingress rejects both keys,
	// so edit/action ingress can trust a marked frame came from this boundary.
	// env.SpaceID is server-resolved (send: resolveBotActiveSpaceID; edit: the
	// Snapshot-pinned envelope Space) — callers never supply it.
	rendered["template_ref"] = map[string]any{
		"id": string(ref.ID), "version": ref.Version,
	}
	if authorProvenance {
		rendered[cardmsg.CatalogProvenanceKey] = cardmsg.CatalogProvenance{
			Version:       cardmsg.CatalogProvenanceVersion,
			PrincipalType: cardmsg.CatalogPrincipalWireBot,
			PrincipalID:   botID,
			SpaceID:       provenanceSpaceID,
		}.MarshalMap()
	}
	for _, key := range []string{"mention", "reply"} {
		if value, ok := inbound[key]; ok {
			rendered[key] = value
		}
	}
	return rendered, nil
}

// requireEditableRef is the edit catalog authorization boundary for a
// caller-supplied template_ref. Call it before any target lookup so an unlisted
// ref cannot use edit responses as a message existence or ownership oracle.
func (c *botCardTemplateCatalog) requireEditableRef(value any) (botTemplateRef, error) {
	if c == nil {
		return botTemplateRef{}, errBotTemplateCatalogUnavailable
	}
	return c.requireRef(value, c.editAllowed, "template is not edit-compatible")
}

// resolveEditRef authorizes one historical-edit ref, static or dynamic.
//
// PR-C review: `requireEditableRef` alone consults only `editAllowed`, a
// boot-time map of code-reviewed static exacts. A version published at runtime
// can never be in that map, so a Bot that had just been granted `send` on a
// dynamic template — and had used it — was refused every edit of the card it
// had itself sent, before stored provenance or the runtime authorizer were ever
// consulted. `send` reached the dynamic authorizer and `edit` never did, which
// left the D2 `send|edit` grant half-wired.
//
// Two rules make this different from the send path:
//
//   - It is pinned. The query carries the stored exact version, so it never
//     follows the activation pointer; following it would rewrite an existing
//     card across versions, which is exactly what D6 forbids.
//   - The static allowlist answers first, without any DB read. That is what
//     keeps a gates-off deployment on precisely its pre-PR-C behaviour: an
//     unresolved principal (which is what a dark catalog produces) withholds
//     only the dynamic overlay.
func (c *botCardTemplateCatalog) resolveEditRef(
	ctx context.Context,
	principal botCatalogPrincipal,
	value any,
) (botTemplateRef, error) {
	if c == nil || c.catalog == nil {
		return botTemplateRef{}, errBotTemplateCatalogUnavailable
	}
	ref, err := parseBotTemplateRef(value)
	if err != nil {
		return botTemplateRef{}, err
	}
	if _, ok := c.editAllowed[ref]; ok {
		return ref, nil
	}
	notEditable := fmt.Errorf("%w: template is not edit-compatible", errBotTemplateRequestInvalid)
	source := c.authorizationSource()
	if source == nil || !principal.SpaceResolved {
		return botTemplateRef{}, notEditable
	}
	auth, err := source.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: ref.ID, Version: ref.Version, Principal: principal.catalogPrincipal(),
	})
	if err != nil {
		// One indistinguishable refusal for "no such version" and "not granted",
		// matching the send path: a producer must not be able to use edit
		// rejections to enumerate which versions exist.
		if errors.Is(err, cardtmpl.ErrTemplateUnknown) ||
			errors.Is(err, cardtmpl.ErrRuntimeCatalogDisabled) {
			return botTemplateRef{}, notEditable
		}
		return botTemplateRef{}, errors.Join(errBotTemplateRuntimeUnavailable, err)
	}
	if auth.Artifact.Blocked || !auth.Grant.Allows(cardtmpl.CatalogPurposeHistoricalEdit) {
		return botTemplateRef{}, notEditable
	}
	return ref, nil
}

// editRefResolver adapts resolveEditRef to the render path's resolver
// signature, so the pre-gate and the render agree by construction rather than
// by two allowlists that happen to match.
func (c *botCardTemplateCatalog) editRefResolver(
	principal botCatalogPrincipal,
) func(context.Context, any) (botTemplateRef, error) {
	return func(ctx context.Context, value any) (botTemplateRef, error) {
		return c.resolveEditRef(ctx, principal, value)
	}
}

// staticRefResolver adapts a boot-time allowlist to the resolver signature.
// Historical edit stays on this path on purpose: an edit is pinned to the
// version already stored in the frame, so following the activation pointer
// there would rewrite an existing card across versions (D6).
func (c *botCardTemplateCatalog) staticRefResolver(
	allowed map[botTemplateRef]struct{},
	denialReason string,
) func(context.Context, any) (botTemplateRef, error) {
	return func(_ context.Context, value any) (botTemplateRef, error) {
		return c.requireRef(value, allowed, denialReason)
	}
}

func (c *botCardTemplateCatalog) requireRef(
	value any,
	allowed map[botTemplateRef]struct{},
	denialReason string,
) (botTemplateRef, error) {
	if c == nil || c.catalog == nil {
		return botTemplateRef{}, errBotTemplateCatalogUnavailable
	}
	ref, err := parseBotTemplateRef(value)
	if err != nil {
		return botTemplateRef{}, err
	}
	if _, ok := allowed[ref]; !ok {
		return botTemplateRef{}, fmt.Errorf("%w: %s", errBotTemplateRequestInvalid, denialReason)
	}
	return ref, nil
}

func lookupBotTemplateMeta(
	catalog cardtmpl.Catalog,
	request cardtmpl.CatalogExactRequest,
) (cardtmpl.TemplateMeta, error) {
	ctx, cancel := context.WithTimeout(context.Background(), botTemplateLookupTimeout)
	defer cancel()
	return catalog.MetaExact(ctx, request)
}

func botRuntimeCatalog(source any) (cardtmpl.Catalog, error) {
	switch typed := source.(type) {
	case nil:
		return nil, errBotTemplateCatalogUnavailable
	case *cardtmpl.Registry:
		if typed == nil {
			return nil, errBotTemplateCatalogUnavailable
		}
		catalog, err := cardtmpl.NewStaticCatalog(typed)
		if err != nil {
			return nil, fmt.Errorf("bot template catalog: static adapter: %w", err)
		}
		return catalog, nil
	case cardtmpl.Catalog:
		if typed == nil {
			return nil, errBotTemplateCatalogUnavailable
		}
		return typed, nil
	default:
		return nil, errBotTemplateCatalogUnavailable
	}
}

func botCatalogAccess(purpose cardtmpl.CatalogPurpose, botID, spaceID string) cardtmpl.CatalogAccess {
	return cardtmpl.CatalogAccess{
		Purpose: purpose,
		Principal: cardtmpl.CatalogPrincipal{
			Kind: cardtmpl.CatalogPrincipalBot, ID: botID, SpaceID: spaceID,
		},
	}
}

func parseBotTemplateRef(value any) (botTemplateRef, error) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != 2 {
		return botTemplateRef{}, fmt.Errorf("%w: template_ref must contain id and version", errBotTemplateRequestInvalid)
	}
	id, idOK := object["id"].(string)
	version, versionOK := object["version"].(string)
	if !idOK || !versionOK || id == "" || version == "" || id != strings.TrimSpace(id) || version != strings.TrimSpace(version) {
		return botTemplateRef{}, fmt.Errorf("%w: template_ref id/version are required", errBotTemplateRequestInvalid)
	}
	return botTemplateRef{ID: cardtmpl.ID(id), Version: version}, nil
}

// editSpaceCheck is what this edit knows about the Space it is authorized
// against. Three states, deliberately distinct — collapsing them is what made
// the Space comparison a source of permanent failures twice over.
//
//   - verifiable, with a Space: compare the stored marker against it.
//   - not verifiable: the deployment never establishes a Space at all (the
//     dynamic catalog is dark), so there is nothing to compare with. The edit
//     is still bound to its author by the canonical bot marker, the
//     PrincipalID == editor check and Snapshot ownership.
//   - unavailable: a Space should have been resolvable and was not. The edit
//     cannot be verified now but may well succeed later, so it must not be
//     reported as a permanent client error.
type editSpaceCheck struct {
	spaceID     string
	verifiable  bool
	unavailable bool
}

// editProvenanceSpaceCheck maps the send-side principal onto those three
// states. dynamicEnabled is read at the call site rather than inferred from the
// principal because botSendCatalogPrincipal returns the same zero principal for
// "the gate is closed" and "the group row would not load", and only the second
// of those is an outage.
//
// A DM whose peer turned out to be outside the Bot's Space also lands in
// `unavailable`. That is a deny wearing an outage's clothes, and the imprecision
// is deliberate: over-reporting an outage costs a retry, while under-reporting
// one costs a permanently uneditable card.
func editProvenanceSpaceCheck(dynamicEnabled bool, principal botCatalogPrincipal) editSpaceCheck {
	if !dynamicEnabled {
		return editSpaceCheck{}
	}
	if !principal.SpaceResolved {
		return editSpaceCheck{unavailable: true}
	}
	return editSpaceCheck{spaceID: principal.SpaceID, verifiable: true}
}

// requireEffectiveCardTemplate proves the stored frame is the one this edit is
// entitled to rewrite.
//
// space is the Space this edit resolved for the target, composed exactly as the
// send composed the marker it is being compared against. An earlier revision
// compared the marker to the envelope's top-level `space_id`, which only DM
// sends ever write — so once group sends started recording their target Space,
// every group and thread card became permanently uneditable. Comparing two
// values produced by the same rule means they agree whenever nothing moved, and
// disagree exactly when the frame did.
func requireEffectiveCardTemplate(
	envelope []byte,
	want botTemplateRef,
	editorBotID string,
	space editSpaceCheck,
) error {
	decoder := json.NewDecoder(bytes.NewReader(envelope))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || !cardmsg.IsCardPayload(payload) {
		return fmt.Errorf("%w: effective target is not a card", errBotTemplateRequestInvalid)
	}
	provenance, err := parseBotTemplateRef(payload["template_ref"])
	if err != nil || provenance != want {
		return fmt.Errorf("%w: effective Registry provenance mismatch", errBotTemplateRequestInvalid)
	}
	card, _ := payload["card"].(map[string]any)
	metadata, _ := card["metadata"].(map[string]any)
	octo, _ := metadata["octo"].(map[string]any)
	template, _ := octo["template"].(map[string]any)
	id, _ := template["id"].(string)
	version, _ := template["version"].(string)
	if id != string(want.ID) || version != want.Version {
		return fmt.Errorf("%w: effective template mismatch", errBotTemplateRequestInvalid)
	}
	// PR-C D3：stored catalog_provenance 存在时必须是本 bot 署名的 canonical
	// 标记，且声明的 Space 与信封自身一致 —— internal producer 的帧、别的 bot
	// 的帧或被篡改的标记都不能经 Bot 模板编辑路径改写。缺失即 pre-PR-C 帧，
	// 走既有兼容路径（Snapshot ownership 已保证 FromUID == 编辑 bot）。
	markers, err := cardmsg.CatalogFrameMarkers(envelope)
	if err != nil {
		return fmt.Errorf("%w: effective catalog markers: %v", errBotTemplateRequestInvalid, err)
	}
	if markers.HasProvenance {
		if markers.Provenance.PrincipalType != cardmsg.CatalogPrincipalWireBot ||
			markers.Provenance.PrincipalID != editorBotID {
			return fmt.Errorf("%w: stored provenance does not match editing bot", errBotTemplateRequestInvalid)
		}
		if markers.Provenance.SpaceID != "" {
			// The envelope's own space_id is a second, independent witness: a DM
			// send writes it, and it is written from the *forgiving* resolver,
			// which is also what stamped the marker whenever the strict grant
			// resolver refused. Consulting it first is what keeps a Bot that
			// spans two Spaces — a permanent property, not a blip — from being
			// locked out of editing its own DM cards forever.
			envelopeSpace, _ := payload["space_id"].(string)
			switch {
			case space.verifiable && markers.Provenance.SpaceID == strings.TrimSpace(space.spaceID):
			case envelopeSpace != "" && markers.Provenance.SpaceID == envelopeSpace:
			case space.unavailable && envelopeSpace == "":
				// Nothing to compare against and something should have been
				// resolvable. Refuse, but as a retryable outage: the frame may
				// be perfectly valid and the caller cannot fix our lookup.
				return fmt.Errorf("%w: edit space could not be resolved, stored provenance unverifiable",
					errBotTemplateRuntimeUnavailable)
			case !space.verifiable && envelopeSpace == "":
				// The deployment establishes no Space at all (the dynamic
				// catalog is dark), so there is nothing this check can assert.
				// The canonical bot marker, the PrincipalID comparison above and
				// Snapshot ownership still bind the edit to the card's author.
			default:
				return fmt.Errorf("%w: stored provenance space mismatch", errBotTemplateRequestInvalid)
			}
		}
	}
	return nil
}

// provenanceSpaceID picks the Space a new send's marker is stamped with.
//
// The authorization principal wins when it resolved one, because that is the
// Space the grant decision was actually made against — for a group or thread
// send it is the target's Space, which env.SpaceID (DM-only) never carries.
// env.SpaceID remains the fallback so a DM keeps stamping exactly what it
// stamped before PR-C's target-authoritative resolution existed.
func provenanceSpaceID(principal botCatalogPrincipal, env cardtmpl.BuildEnv) string {
	if principal.SpaceResolved && principal.SpaceID != "" {
		return principal.SpaceID
	}
	return env.SpaceID
}
