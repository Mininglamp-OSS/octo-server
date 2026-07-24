package bot_api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
)

const botTemplateWireV1 = "template-ref/v1"

var (
	errBotTemplateRequestInvalid     = errors.New("bot template request invalid")
	errBotTemplateCatalogUnavailable = errors.New("bot template catalog unavailable")
)

type botTemplateRef struct {
	ID      cardtmpl.ID `json:"id"`
	Version string      `json:"version"`
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
	registry   *cardtmpl.Registry
	allowed    map[botTemplateRef]struct{}
	capability botTemplatingCapability
}

func defaultBotTemplateRefs() []botTemplateRef {
	return []botTemplateRef{{
		ID:      aireasoningprocess.TemplateID,
		Version: aireasoningprocess.TemplateVersion,
	}}
}

func newBotCardTemplateCatalog(registry *cardtmpl.Registry, refs []botTemplateRef) (*botCardTemplateCatalog, error) {
	if registry == nil {
		return nil, errBotTemplateCatalogUnavailable
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("bot template catalog: empty allowlist")
	}
	catalog := &botCardTemplateCatalog{
		registry: registry,
		allowed:  make(map[botTemplateRef]struct{}, len(refs)),
		capability: botTemplatingCapability{
			Supported: true,
			Wire:      botTemplateWireV1,
			Templates: make([]botTemplateCapability, 0, len(refs)),
		},
	}
	for _, ref := range refs {
		if strings.TrimSpace(string(ref.ID)) == "" || strings.TrimSpace(ref.Version) == "" {
			return nil, fmt.Errorf("bot template catalog: id and explicit version are required")
		}
		if _, duplicate := catalog.allowed[ref]; duplicate {
			return nil, fmt.Errorf("bot template catalog: duplicate %s@%s", ref.ID, ref.Version)
		}
		tmpl, err := registry.Lookup(ref.ID, ref.Version)
		if err != nil {
			return nil, fmt.Errorf("bot template catalog: lookup %s@%s: %w", ref.ID, ref.Version, err)
		}
		meta := tmpl.Meta()
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
		catalog.allowed[ref] = struct{}{}
		catalog.capability.Templates = append(catalog.capability.Templates, capability)
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
	if c == nil || c.registry == nil {
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
	ref, err := parseBotTemplateRef(inbound["template_ref"])
	if err != nil {
		return nil, err
	}
	if _, ok := c.allowed[ref]; !ok {
		return nil, fmt.Errorf("%w: template is not Bot-callable", errBotTemplateRequestInvalid)
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
	rendered, err := c.registry.Render(ctx, ref.ID, ref.Version, cardtmpl.State(state), rawData, env)
	if err != nil {
		if errors.Is(err, cardtmpl.ErrFieldsInvalid) || errors.Is(err, cardtmpl.ErrStateUnknown) || errors.Is(err, cardtmpl.ErrTemplateUnknown) {
			return nil, fmt.Errorf("%w: %v", errBotTemplateRequestInvalid, err)
		}
		return nil, fmt.Errorf("bot template render %s@%s: %w", ref.ID, ref.Version, err)
	}
	for _, key := range []string{"mention", "reply"} {
		if value, ok := inbound[key]; ok {
			rendered[key] = value
		}
	}
	return rendered, nil
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
