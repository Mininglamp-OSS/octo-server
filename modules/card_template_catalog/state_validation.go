package card_template_catalog

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

type catalogArtifactReader interface {
	LoadArtifactMeta(context.Context, cardtmpl.ID, string) (cardtmpl.RuntimeArtifactMeta, error)
	LoadArtifactBundle(context.Context, cardtmpl.ID, string, string) (cardtmpl.CanonicalBundle, error)
}

func (a *API) validateTarget(ctx context.Context, id cardtmpl.ID, version string) (stateTargetReceipt, error) {
	registry := a.registry
	if registry == nil {
		registry = cardtmpl.DefaultRegistry()
	}
	if registry != nil {
		if _, err := registry.Lookup(id, version); err == nil {
			return stateTargetReceipt{meta: cardtmpl.RuntimeArtifactMeta{
				ID: id, Version: version, Source: cardtmpl.RuntimeSourceStatic,
			}}, nil
		} else if !errors.Is(err, cardtmpl.ErrTemplateUnknown) {
			return stateTargetReceipt{}, err
		}
	}
	reader, ok := a.store.(catalogArtifactReader)
	if !ok || reader == nil {
		return stateTargetReceipt{}, errors.New("card template catalog: artifact reader unavailable")
	}
	meta, err := reader.LoadArtifactMeta(ctx, id, version)
	if err != nil {
		if errors.Is(err, cardtmpl.ErrTemplateUnknown) {
			return stateTargetReceipt{}, fmt.Errorf("%w: %s@%s", ErrActivationTargetInvalid, id, version)
		}
		return stateTargetReceipt{}, err
	}
	if meta.Source != cardtmpl.RuntimeSourceDynamic {
		return stateTargetReceipt{}, fmt.Errorf("%w: non-built-in static target %s@%s", ErrCatalogIntegrity, id, version)
	}
	if meta.Blocked {
		return stateTargetReceipt{}, fmt.Errorf("%w: %s@%s", ErrArtifactBlocked, id, version)
	}
	canonical, err := reader.LoadArtifactBundle(ctx, id, version, meta.Hash)
	if err != nil {
		return stateTargetReceipt{}, err
	}
	bundle, err := cardtmpl.DecodeBundleJSON(canonical)
	if err != nil {
		return stateTargetReceipt{}, fmt.Errorf("%w: decode persisted target", cardtmpl.ErrRuntimeCatalogIntegrity)
	}
	compiler := a.compile
	if compiler == nil {
		compiler = cardtmpl.CompileJSONArtifact
	}
	artifact, err := compiler(ctx, bundle, cardtmpl.DefaultCompileLimits())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, cardtmpl.ErrArtifactCompileBusy) {
			return stateTargetReceipt{}, err
		}
		return stateTargetReceipt{}, fmt.Errorf("%w: compile persisted target", cardtmpl.ErrRuntimeCatalogIntegrity)
	}
	if artifact.Meta.ID != meta.ID || artifact.Meta.Version != meta.Version ||
		artifact.Engine != meta.Engine || artifact.Hash != meta.Hash || artifact.Owner != meta.Owner ||
		artifact.Visibility != meta.Visibility || artifact.Meta.Protocol != meta.Protocol ||
		artifact.ContractVersion != meta.ContractVersion {
		return stateTargetReceipt{}, fmt.Errorf("%w: compiled target metadata mismatch", cardtmpl.ErrRuntimeCatalogIntegrity)
	}
	if !isApprovedRuntimeOwner(artifact.Owner) {
		return stateTargetReceipt{}, fmt.Errorf("%w: target owner is not approved", ErrActivationTargetInvalid)
	}
	if err := validateInteractiveTarget(artifact.Meta); err != nil {
		return stateTargetReceipt{}, err
	}
	return stateTargetReceipt{meta: meta}, nil
}

func validateInteractiveTarget(meta cardtmpl.TemplateMeta) error {
	contract := meta.ActionContract
	if contract == nil {
		return nil
	}
	specs, err := cardactiondispatch.LoadRouteSpecs(os.Getenv("OCTO_CARD_ACTION_ROUTES"))
	if err != nil {
		return fmt.Errorf("%w: invalid card action route configuration", ErrCatalogIntegrity)
	}
	for _, spec := range specs {
		if spec.Owner == contract.Owner && spec.ActionType == contract.ActionType {
			return nil
		}
	}
	return fmt.Errorf("%w: no callback route for interactive target", ErrActivationTargetInvalid)
}
