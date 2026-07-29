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
}

func defaultBotTemplateRefs() []botTemplateRef {
	return []botTemplateRef{{
		ID:      aireasoningprocess.TemplateID,
		Version: aireasoningprocess.TemplateVersionV2,
	}}
}

func defaultBotTemplatePolicy() botTemplatePolicy {
	return botTemplatePolicy{
		AdvertisedSend: defaultBotTemplateRefs(),
		EditCompatible: []botTemplateRef{
			{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV1},
			{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV2},
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
		catalog:     runtimeCatalog,
		sendAllowed: make(map[botTemplateRef]struct{}, len(policy.AdvertisedSend)),
		editAllowed: make(map[botTemplateRef]struct{}, len(policy.EditCompatible)),
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
		if _, duplicate := catalog.sendAllowed[ref]; duplicate {
			return nil, fmt.Errorf("bot template catalog: duplicate advertised/send %s@%s", ref.ID, ref.Version)
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

		capability := botTemplateCapability{
			ID: string(meta.ID), Version: meta.Version,
			Views: make([]botTemplateViewCapability, 0, len(meta.Views)),
		}
		for view, spec := range meta.Views {
			if strings.TrimSpace(string(view)) == "" || len(spec.States) == 0 {
				return nil, fmt.Errorf("bot template catalog: %s@%s has empty view/state metadata", ref.ID, ref.Version)
			}
			viewCapability := botTemplateViewCapability{
				Name:          string(view),
				WireProfile:   spec.WireProfile,
				States:        make([]string, 0, len(spec.States)),
				SubmitActions: make([]string, 0),
			}
			for _, state := range spec.States {
				if strings.TrimSpace(string(state)) == "" {
					return nil, fmt.Errorf("bot template catalog: %s@%s view %s has empty state", ref.ID, ref.Version, view)
				}
				viewCapability.States = append(viewCapability.States, string(state))
			}
			sort.Strings(viewCapability.States)

			if spec.WireProfile == cardmsg.ProfileV2 {
				report, ok := meta.Interaction(view)
				if !ok {
					return nil, fmt.Errorf("bot template catalog: %s@%s view %s missing interaction report", ref.ID, ref.Version, view)
				}
				seenActions := make(map[string]struct{})
				for _, action := range report.Actions {
					if action.Type != "Action.Submit" {
						continue
					}
					if strings.TrimSpace(action.ID) == "" {
						return nil, fmt.Errorf("bot template catalog: %s@%s view %s has empty Submit id", ref.ID, ref.Version, view)
					}
					if _, duplicate := seenActions[action.ID]; duplicate {
						return nil, fmt.Errorf("bot template catalog: %s@%s view %s has duplicate Submit id %q", ref.ID, ref.Version, view, action.ID)
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
		catalog.sendAllowed[ref] = struct{}{}
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
	return catalog, nil
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
	return c.renderPayload(ctx, "bot-api-compat", cardtmpl.CatalogPurposeNewSend,
		inbound, env, c.sendAllowed, "not Bot-callable for new send")
}

func (c *botCardTemplateCatalog) RenderPayloadForPrincipal(
	ctx context.Context,
	botID string,
	inbound map[string]any,
	env cardtmpl.BuildEnv,
) (map[string]any, error) {
	if c == nil || strings.TrimSpace(botID) == "" {
		return nil, errBotTemplateCatalogUnavailable
	}
	return c.renderPayload(ctx, botID, cardtmpl.CatalogPurposeNewSend,
		inbound, env, c.sendAllowed, "not Bot-callable for new send")
}

func (c *botCardTemplateCatalog) RenderEditPayload(
	ctx context.Context,
	inbound map[string]any,
	env cardtmpl.BuildEnv,
) (map[string]any, error) {
	if c == nil {
		return nil, errBotTemplateCatalogUnavailable
	}
	return c.renderPayload(ctx, "bot-api-compat", cardtmpl.CatalogPurposeHistoricalEdit,
		inbound, env, c.editAllowed, "not edit-compatible")
}

func (c *botCardTemplateCatalog) RenderEditPayloadForPrincipal(
	ctx context.Context,
	botID string,
	inbound map[string]any,
	env cardtmpl.BuildEnv,
) (map[string]any, error) {
	if c == nil || strings.TrimSpace(botID) == "" {
		return nil, errBotTemplateCatalogUnavailable
	}
	return c.renderPayload(ctx, botID, cardtmpl.CatalogPurposeHistoricalEdit,
		inbound, env, c.editAllowed, "not edit-compatible")
}

func (c *botCardTemplateCatalog) renderPayload(
	ctx context.Context,
	botID string,
	purpose cardtmpl.CatalogPurpose,
	inbound map[string]any,
	env cardtmpl.BuildEnv,
	allowed map[botTemplateRef]struct{},
	denialReason string,
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
	ref, err := c.requireRef(inbound["template_ref"], allowed, denialReason)
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
	// Retain only the canonical ref as server-authored provenance. Raw Model B
	// ingress rejects card+template_ref, so edit can distinguish a Registry frame
	// from a caller-authored card that merely forged metadata.octo.template.
	rendered["template_ref"] = map[string]any{
		"id": string(ref.ID), "version": ref.Version,
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

func requireEffectiveCardTemplate(envelope []byte, want botTemplateRef) error {
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
	return nil
}
