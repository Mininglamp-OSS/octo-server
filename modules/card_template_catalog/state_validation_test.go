package card_template_catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	docscommented "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_commented"
)

func TestValidateTargetRecompilesDynamicArtifactAndChecksStoredMetadata(t *testing.T) {
	root, err := cardtmpl.DecodeStrictJSONObject(validControlRequestJSON(t, false))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := cardtmpl.DecodeBundleValue(root["bundle"])
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := cardtmpl.CompileJSONArtifact(context.Background(), bundle, cardtmpl.DefaultCompileLimits())
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeTargetValidationStore{
		meta: cardtmpl.RuntimeArtifactMeta{
			ID: artifact.Meta.ID, Version: artifact.Meta.Version, Source: cardtmpl.RuntimeSourceDynamic,
			Engine: artifact.Engine, Hash: artifact.Hash, Owner: artifact.Owner,
			Visibility: artifact.Visibility, Protocol: artifact.Meta.Protocol,
			ContractVersion: artifact.ContractVersion,
		},
		bundle: artifact.Bundle,
	}
	previous := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(nil)
	t.Cleanup(func() { cardtmpl.SetDefaultRegistry(previous) })
	compileCalls := 0
	api := &API{store: store, compile: func(ctx context.Context, input cardtmpl.Bundle, limits cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
		compileCalls++
		return cardtmpl.CompileJSONArtifact(ctx, input, limits)
	}}
	receipt, err := api.validateTarget(context.Background(), artifact.Meta.ID, artifact.Meta.Version)
	if err != nil {
		t.Fatalf("validateTarget: %v", err)
	}
	if receipt.meta.Hash != artifact.Hash || receipt.meta.Source != cardtmpl.RuntimeSourceDynamic {
		t.Fatalf("validation receipt = %+v", receipt.meta)
	}
	if compileCalls != 1 || store.bundleCalls != 1 {
		t.Fatalf("compile/bundle calls = %d/%d, want 1/1", compileCalls, store.bundleCalls)
	}

	store.meta.Owner = "other"
	if _, err := api.validateTarget(context.Background(), artifact.Meta.ID, artifact.Meta.Version); !errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
		t.Fatalf("metadata mismatch error = %v, want ErrRuntimeCatalogIntegrity", err)
	}
}

func TestValidateTargetAllowsStaticInteractiveTemplateWithoutRouteSpec(t *testing.T) {
	t.Setenv("OCTO_CARD_ACTION_ROUTES", "")
	registry := cardtmpl.NewRegistry()
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV2)
	registry.Freeze()

	api := &API{registry: registry, store: &fakeCatalogStore{}}
	receipt, err := api.validateTarget(context.Background(), aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV2)
	if err != nil {
		t.Fatalf("validate static interactive target: %v", err)
	}
	if receipt.meta.Source != cardtmpl.RuntimeSourceStatic {
		t.Fatalf("static interactive source = %q, want static", receipt.meta.Source)
	}
}

func TestValidateInteractiveTargetAllowsHiddenControlsSuccessorWithoutRouteSpec(t *testing.T) {
	t.Setenv("OCTO_CARD_ACTION_ROUTES", "")
	registry := cardtmpl.NewRegistry()
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV3)
	registry.Freeze()

	template, err := registry.Lookup(aireasoningprocess.TemplateID, aireasoningprocess.TemplateVersionV3)
	if err != nil {
		t.Fatal(err)
	}
	if contract := template.Meta().ActionContract; contract != nil {
		t.Fatalf("0.3.0 ActionContract = %+v, want nil", contract)
	}
	if err := validateInteractiveTarget(template.Meta()); err != nil {
		t.Fatalf("validate hidden-controls successor interaction contract: %v", err)
	}
}

func TestValidateTargetRequiresConfiguredRouteForDynamicInteractiveTemplate(t *testing.T) {
	t.Setenv("OCTO_CARD_ACTION_ROUTES", "")
	artifact := integrationArtifactForVersion(t, "1.0.0")
	artifact.Meta.ActionContract = &cardtmpl.TemplateActionContract{Owner: "ai", ActionType: "reasoning.control"}
	meta := receiptForIntegrationArtifact(artifact).meta
	api := &API{
		registry: cardtmpl.NewRegistry(),
		store:    &fakeTargetValidationStore{meta: meta, bundle: artifact.Bundle},
		compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			return artifact, nil
		},
	}
	_, err := api.validateTarget(context.Background(), artifact.Meta.ID, artifact.Meta.Version)
	if !errors.Is(err, ErrActivationTargetInvalid) {
		t.Fatalf("validateTarget error = %v, want ErrActivationTargetInvalid", err)
	}
}

func TestValidateTargetRejectsUnapprovedDynamicOwner(t *testing.T) {
	root, err := cardtmpl.DecodeStrictJSONObject(validControlRequestJSON(t, false))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := cardtmpl.DecodeBundleValue(root["bundle"])
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := cardtmpl.CompileJSONArtifact(context.Background(), bundle, cardtmpl.DefaultCompileLimits())
	if err != nil {
		t.Fatal(err)
	}
	unapproved := *artifact
	unapproved.Owner = "ext.vendor"
	meta := cardtmpl.RuntimeArtifactMeta{
		ID: unapproved.Meta.ID, Version: unapproved.Meta.Version, Source: cardtmpl.RuntimeSourceDynamic,
		Engine: unapproved.Engine, Hash: unapproved.Hash, Owner: unapproved.Owner,
		Visibility: unapproved.Visibility, Protocol: unapproved.Meta.Protocol,
		ContractVersion: unapproved.ContractVersion,
	}
	api := &API{
		store: &fakeTargetValidationStore{meta: meta, bundle: unapproved.Bundle},
		compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			return &unapproved, nil
		},
	}
	_, err = api.validateTarget(context.Background(), unapproved.Meta.ID, unapproved.Meta.Version)
	if !errors.Is(err, ErrActivationTargetInvalid) {
		t.Fatalf("unapproved owner error = %v, want ErrActivationTargetInvalid", err)
	}
}

func TestValidateTargetStaticDisplayAndDynamicFailurePaths(t *testing.T) {
	displayRegistry := cardtmpl.NewRegistry()
	displayRegistry.Register(docscommented.New(), docscommented.Assets, docscommented.HandoffRoot)
	displayRegistry.Freeze()
	api := &API{registry: displayRegistry, store: &fakeCatalogStore{}}
	receipt, err := api.validateTarget(context.Background(), docscommented.TemplateID, docscommented.TemplateVersion)
	if err != nil || receipt.meta.Source != cardtmpl.RuntimeSourceStatic {
		t.Fatalf("static display receipt=%+v err=%v", receipt.meta, err)
	}

	artifact := integrationArtifactForVersion(t, "1.0.0")
	meta := receiptForIntegrationArtifact(artifact).meta
	meta.Blocked = true
	blockedStore := &fakeTargetValidationStore{meta: meta, bundle: artifact.Bundle}
	api = &API{store: blockedStore}
	if _, err := api.validateTarget(context.Background(), artifact.Meta.ID, artifact.Meta.Version); !errors.Is(err, ErrArtifactBlocked) {
		t.Fatalf("blocked target error=%v, want ErrArtifactBlocked", err)
	}
	if blockedStore.bundleCalls != 0 {
		t.Fatalf("blocked target bundle calls=%d, want 0", blockedStore.bundleCalls)
	}

	api = &API{store: &fakeCatalogStore{}}
	if _, err := api.validateTarget(context.Background(), artifact.Meta.ID, artifact.Meta.Version); err == nil {
		t.Fatal("validateTarget accepted store without artifact reader")
	}

	meta.Blocked = false
	api = &API{
		store: &fakeTargetValidationStore{meta: meta, bundle: artifact.Bundle},
		compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			return nil, cardtmpl.ErrArtifactCompileBusy
		},
	}
	if _, err := api.validateTarget(context.Background(), artifact.Meta.ID, artifact.Meta.Version); !errors.Is(err, cardtmpl.ErrArtifactCompileBusy) {
		t.Fatalf("compile busy error=%v, want ErrArtifactCompileBusy", err)
	}
}

func TestValidateInteractiveTargetRejectsInvalidRouteConfiguration(t *testing.T) {
	t.Setenv("OCTO_CARD_ACTION_ROUTES", "not-json")
	meta := cardtmpl.TemplateMeta{ActionContract: &cardtmpl.TemplateActionContract{Owner: "docs", ActionType: "docs.access.decision"}}
	if err := validateInteractiveTarget(meta); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("invalid route config error=%v, want ErrCatalogIntegrity", err)
	}
}

type fakeTargetValidationStore struct {
	fakeCatalogStore
	meta        cardtmpl.RuntimeArtifactMeta
	metaErr     error
	bundle      cardtmpl.CanonicalBundle
	bundleErr   error
	bundleCalls int
}

func (s *fakeTargetValidationStore) LoadArtifactMeta(context.Context, cardtmpl.ID, string) (cardtmpl.RuntimeArtifactMeta, error) {
	return s.meta, s.metaErr
}

func (s *fakeTargetValidationStore) LoadArtifactBundle(context.Context, cardtmpl.ID, string, string) (cardtmpl.CanonicalBundle, error) {
	s.bundleCalls++
	return append(cardtmpl.CanonicalBundle(nil), s.bundle...), s.bundleErr
}
