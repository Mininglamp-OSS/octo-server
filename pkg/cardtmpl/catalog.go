package cardtmpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type CatalogPurpose string

const (
	CatalogPurposeNewSend        CatalogPurpose = "new_send"
	CatalogPurposeHistoricalEdit CatalogPurpose = "historical_edit"
	CatalogPurposeActionContext  CatalogPurpose = "action_context"
)

type CatalogPrincipalKind string

const (
	CatalogPrincipalBot              CatalogPrincipalKind = "bot"
	CatalogPrincipalInternalProducer CatalogPrincipalKind = "internal_producer"
	CatalogPrincipalSystem           CatalogPrincipalKind = "system"
)

type CatalogPrincipal struct {
	Kind    CatalogPrincipalKind
	ID      string
	SpaceID string
}

type CatalogAccess struct {
	Purpose   CatalogPurpose
	Principal CatalogPrincipal
}

type CatalogRenderRequest struct {
	Access  CatalogAccess
	ID      ID
	Version string
	State   State
	Fields  json.RawMessage
	Env     BuildEnv
}

type CatalogExactRequest struct {
	Access  CatalogAccess
	ID      ID
	Version string
}

type CatalogDefaultRequest struct {
	Access CatalogAccess
	ID     ID
}

type CatalogFallbackRequest struct {
	Access  CatalogAccess
	ID      ID
	Version string
	State   State
	Fields  json.RawMessage
	Lang    string
}

type CatalogActionRequest struct {
	CatalogExactRequest
	ActionID string
}

type CatalogActionContext struct {
	View ViewKey
	Meta TemplateMeta
}

// Catalog is the runtime-only template surface. Registration and default
// mutation intentionally remain methods on the boot-time Registry only.
type Catalog interface {
	Render(context.Context, CatalogRenderRequest) (map[string]any, error)
	MetaExact(context.Context, CatalogExactRequest) (TemplateMeta, error)
	MetaDefault(context.Context, CatalogDefaultRequest) (TemplateMeta, error)
	FallbackText(context.Context, CatalogFallbackRequest) (string, error)
	ActionContext(context.Context, CatalogActionRequest) (CatalogActionContext, error)
}

type StaticCatalog struct {
	registry *Registry
}

func NewStaticCatalog(registry *Registry) (*StaticCatalog, error) {
	if registry == nil {
		return nil, errors.New("cardtmpl: static catalog registry is required")
	}
	return &StaticCatalog{registry: registry}, nil
}

func (c *StaticCatalog) Render(ctx context.Context, request CatalogRenderRequest) (map[string]any, error) {
	if c == nil || c.registry == nil {
		return nil, errors.New("cardtmpl: static catalog unavailable")
	}
	return c.registry.Render(ctx, request.ID, request.Version, request.State, request.Fields, request.Env)
}

func (c *StaticCatalog) MetaExact(_ context.Context, request CatalogExactRequest) (TemplateMeta, error) {
	if c == nil || c.registry == nil {
		return TemplateMeta{}, errors.New("cardtmpl: static catalog unavailable")
	}
	template, err := c.registry.Lookup(request.ID, request.Version)
	if err != nil {
		return TemplateMeta{}, err
	}
	return template.Meta(), nil
}

func (c *StaticCatalog) MetaDefault(_ context.Context, request CatalogDefaultRequest) (TemplateMeta, error) {
	if c == nil || c.registry == nil {
		return TemplateMeta{}, errors.New("cardtmpl: static catalog unavailable")
	}
	template, err := c.registry.Lookup(request.ID, "")
	if err != nil {
		return TemplateMeta{}, err
	}
	return template.Meta(), nil
}

func (c *StaticCatalog) FallbackText(ctx context.Context, request CatalogFallbackRequest) (string, error) {
	if c == nil || c.registry == nil {
		return "", errors.New("cardtmpl: static catalog unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	template, err := c.registry.Lookup(request.ID, request.Version)
	if err != nil {
		return "", err
	}
	return template.FallbackText(request.State, request.Fields, request.Lang)
}

func (c *StaticCatalog) ActionContext(_ context.Context, request CatalogActionRequest) (CatalogActionContext, error) {
	if c == nil || c.registry == nil {
		return CatalogActionContext{}, errors.New("cardtmpl: static catalog unavailable")
	}
	view, err := c.registry.ActionView(request.ID, request.Version, request.ActionID)
	if err != nil {
		return CatalogActionContext{}, err
	}
	template, err := c.registry.Lookup(request.ID, request.Version)
	if err != nil {
		return CatalogActionContext{}, err
	}
	return CatalogActionContext{View: view, Meta: template.Meta()}, nil
}

func actionViewFromMeta(meta TemplateMeta, actionID string) (ViewKey, error) {
	if strings.TrimSpace(actionID) == "" {
		return "", ErrActionUnknown
	}
	var matched ViewKey
	for view, report := range meta.interactions {
		for _, action := range report.Actions {
			if action.Type != "Action.Submit" || action.ID != actionID {
				continue
			}
			if matched != "" && matched != view {
				return "", fmt.Errorf("%w: %s@%s action %q is ambiguous",
					ErrActionUnknown, meta.ID, meta.Version, actionID)
			}
			matched = view
		}
	}
	if matched == "" {
		return "", fmt.Errorf("%w: %s@%s action %q", ErrActionUnknown, meta.ID, meta.Version, actionID)
	}
	return matched, nil
}

func RenderCatalogCardWithProfiles(
	ctx context.Context,
	catalog Catalog,
	request CatalogRenderRequest,
) (cardDoc json.RawMessage, profile, renderProfile string, err error) {
	if catalog == nil {
		return nil, "", "", errors.New("cardtmpl: catalog unavailable")
	}
	payload, err := catalog.Render(ctx, request)
	if err != nil {
		return nil, "", "", err
	}
	raw, err := json.Marshal(payload["card"])
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: marshal card: %v", ErrRenderFailed, err)
	}
	profile, _ = payload["profile"].(string)
	renderProfile, _ = payload["render_profile"].(string)
	return raw, profile, renderProfile, nil
}

var (
	defaultCatalogMu sync.RWMutex
	defaultCatalog   Catalog
)

// SetDefaultCatalog installs the process runtime reader. Production installs
// it once in the composition root before module factories are constructed.
func SetDefaultCatalog(c Catalog) {
	defaultCatalogMu.Lock()
	defer defaultCatalogMu.Unlock()
	defaultCatalog = c
}

// DefaultCatalog returns the installed runtime reader. Tests and pre-overlay
// binaries safely fall back to an adapter over the current frozen Registry.
func DefaultCatalog() Catalog {
	defaultCatalogMu.RLock()
	catalog := defaultCatalog
	defaultCatalogMu.RUnlock()
	if catalog != nil {
		return catalog
	}
	registry := DefaultRegistry()
	if registry == nil {
		return nil
	}
	static, err := NewStaticCatalog(registry)
	if err != nil {
		return nil
	}
	return static
}
