package cardtmpl

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestStaticCatalogRenderMatchesFrozenRegistry(t *testing.T) {
	registry := runtimeStaticRegistry(t)
	catalog, err := NewStaticCatalog(registry)
	if err != nil {
		t.Fatalf("NewStaticCatalog: %v", err)
	}
	request := CatalogRenderRequest{
		Access: CatalogAccess{Purpose: CatalogPurposeNewSend, Principal: CatalogPrincipal{
			Kind: CatalogPrincipalSystem, ID: "test-producer", SpaceID: "space-1",
		}},
		ID: "test.runtime-parity", Version: "1.0.0", State: "shown",
		Fields: json.RawMessage(`{"title":"parity"}`),
		Env:    BuildEnv{Lang: "en-US", SpaceID: "space-1"},
	}
	got, err := catalog.Render(context.Background(), request)
	if err != nil {
		t.Fatalf("Catalog.Render: %v", err)
	}
	want, err := registry.Render(context.Background(), request.ID, request.Version, request.State, request.Fields, request.Env)
	if err != nil {
		t.Fatalf("Registry.Render: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("static catalog payload drifted\ngot=%#v\nwant=%#v", got, want)
	}
}

func TestStaticCatalogDefaultMetaFallbackAndCardMatchFrozenRegistry(t *testing.T) {
	registry := runtimeStaticRegistry(t)
	catalog, err := NewStaticCatalog(registry)
	if err != nil {
		t.Fatalf("NewStaticCatalog: %v", err)
	}
	access := staticDefaultRequest().Access
	meta, err := catalog.MetaDefault(context.Background(), CatalogDefaultRequest{
		Access: access, ID: "test.runtime-parity",
	})
	if err != nil {
		t.Fatalf("MetaDefault: %v", err)
	}
	if meta.ID != "test.runtime-parity" || meta.Version != "1.0.0" {
		t.Fatalf("default meta = %s@%s", meta.ID, meta.Version)
	}
	fields := json.RawMessage(`{"title":"parity"}`)
	gotFallback, err := catalog.FallbackText(context.Background(), CatalogFallbackRequest{
		Access: access, ID: "test.runtime-parity", State: "shown", Fields: fields, Lang: "en-US",
	})
	if err != nil {
		t.Fatalf("FallbackText: %v", err)
	}
	template, err := registry.Lookup("test.runtime-parity", "")
	if err != nil {
		t.Fatal(err)
	}
	wantFallback, err := template.FallbackText("shown", fields, "en-US")
	if err != nil {
		t.Fatal(err)
	}
	if gotFallback != wantFallback {
		t.Fatalf("fallback = %q, want %q", gotFallback, wantFallback)
	}
	request := staticDefaultRequest()
	gotCard, gotProfile, gotRenderProfile, err := RenderCatalogCardWithProfiles(context.Background(), catalog, request)
	if err != nil {
		t.Fatalf("RenderCatalogCardWithProfiles: %v", err)
	}
	wantCard, wantProfile, wantRenderProfile, err := registry.RenderCardWithProfiles(
		context.Background(), request.ID, request.Version, request.State, request.Fields, request.Env,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotCard, wantCard) || gotProfile != wantProfile || gotRenderProfile != wantRenderProfile {
		t.Fatalf("catalog card/profile drifted: card=%s profile=%q render=%q", gotCard, gotProfile, gotRenderProfile)
	}
}

func TestRuntimeCatalogDefaultWithoutActivationUsesStaticDefault(t *testing.T) {
	registry := runtimeStaticRegistry(t)
	store := &runtimeStoreStub{activation: RuntimeActivation{Exists: false}}
	catalog := newRuntimeCatalogForTest(t, registry, store, nil)

	payload, err := catalog.Render(context.Background(), staticDefaultRequest())
	if err != nil {
		t.Fatalf("Render static default: %v", err)
	}
	if payload["profile"] != profileV1 {
		t.Fatalf("profile = %v, want %s", payload["profile"], profileV1)
	}
	if store.activationCalls != 1 || store.metaCalls != 0 || store.bundleCalls != 0 {
		t.Fatalf("store calls activation/meta/bundle = %d/%d/%d, want 1/0/0",
			store.activationCalls, store.metaCalls, store.bundleCalls)
	}
}

func TestNewRuntimeCatalogRejectsMutableRegistry(t *testing.T) {
	registry := NewRegistry()
	store := &runtimeStoreStub{}
	if _, err := NewRuntimeCatalog(registry, store, RuntimeCatalogConfig{}); err == nil {
		t.Fatal("NewRuntimeCatalog accepted a mutable built-in Registry")
	}
}

func TestNewRuntimeCatalogRejectsCompileDeadlineAboveHardLimit(t *testing.T) {
	registry := NewRegistry()
	registry.Freeze()
	if _, err := NewRuntimeCatalog(registry, &runtimeStoreStub{}, RuntimeCatalogConfig{
		CompileTimeout: defaultRuntimeCompileTime + time.Nanosecond,
	}); err == nil {
		t.Fatal("NewRuntimeCatalog accepted compile timeout above hard limit")
	}
}

func TestRuntimeCatalogDefaultDBFailureDoesNotFallbackStatic(t *testing.T) {
	registry := runtimeStaticRegistry(t)
	store := &runtimeStoreStub{activationErr: errors.New("db unavailable")}
	catalog := newRuntimeCatalogForTest(t, registry, store, nil)

	_, err := catalog.Render(context.Background(), staticDefaultRequest())
	if !errors.Is(err, ErrRuntimeCatalogUnavailable) {
		t.Fatalf("Render error = %v, want ErrRuntimeCatalogUnavailable", err)
	}
}

func TestRuntimeCatalogExplicitDisabledDoesNotFallbackStatic(t *testing.T) {
	registry := runtimeStaticRegistry(t)
	store := &runtimeStoreStub{activation: RuntimeActivation{
		Exists: true, Status: RuntimeActivationDisabled, Revision: 3,
	}}
	catalog := newRuntimeCatalogForTest(t, registry, store, nil)

	_, err := catalog.Render(context.Background(), staticDefaultRequest())
	if !errors.Is(err, ErrRuntimeCatalogDisabled) {
		t.Fatalf("Render error = %v, want ErrRuntimeCatalogDisabled", err)
	}
}

func TestRuntimeCatalogReadinessGatesDBBackedPathsButNotStaticExact(t *testing.T) {
	registry := runtimeStaticRegistry(t)
	store := &runtimeStoreStub{activation: RuntimeActivation{Exists: false}}
	readyErr := ErrRuntimeCatalogUnavailable
	readyCalls := 0
	catalog, err := NewRuntimeCatalog(registry, store, RuntimeCatalogConfig{
		MaxCacheEntries: 4,
		MaxCacheBytes:   4 << 20,
		CompileTimeout:  time.Second,
		CheckReady: func() error {
			readyCalls++
			return readyErr
		},
	})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog: %v", err)
	}

	exact := staticDefaultRequest()
	exact.Version = "1.0.0"
	if _, err := catalog.Render(context.Background(), exact); err != nil {
		t.Fatalf("explicit static render while runtime is unready: %v", err)
	}
	if readyCalls != 1 || store.activationCalls != 0 {
		t.Fatalf("explicit static readiness/activation calls = %d/%d, want 1/0", readyCalls, store.activationCalls)
	}

	if _, err := catalog.Render(context.Background(), staticDefaultRequest()); !errors.Is(err, ErrRuntimeCatalogUnavailable) {
		t.Fatalf("default render error = %v, want ErrRuntimeCatalogUnavailable", err)
	}
	if readyCalls != 2 || store.activationCalls != 0 {
		t.Fatalf("unready default readiness/activation calls = %d/%d, want 2/0", readyCalls, store.activationCalls)
	}

	readyErr = nil
	if _, err := catalog.Render(context.Background(), staticDefaultRequest()); err != nil {
		t.Fatalf("ready default render: %v", err)
	}
	if readyCalls != 3 || store.activationCalls != 1 {
		t.Fatalf("ready default readiness/activation calls = %d/%d, want 3/1", readyCalls, store.activationCalls)
	}

	readyErr = ErrRuntimeCatalogIntegrity
	if _, err := catalog.Render(context.Background(), exact); !errors.Is(err, ErrRuntimeCatalogIntegrity) {
		t.Fatalf("explicit static render under catalog integrity failure = %v, want ErrRuntimeCatalogIntegrity", err)
	}
	if readyCalls != 4 || store.activationCalls != 1 {
		t.Fatalf("integrity static readiness/activation calls = %d/%d, want 4/1", readyCalls, store.activationCalls)
	}
}

func TestRuntimeCatalogDynamicColdThenHotStillReadsAuthoritativeMetadata(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	store := &runtimeStoreStub{
		activation: RuntimeActivation{
			Exists: true, Status: RuntimeActivationActive, Version: artifact.Meta.Version, Revision: 1,
		},
		meta:   runtimeArtifactMeta(artifact),
		bundle: artifact.Bundle,
	}
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)
	request := dynamicDefaultRequest()

	for i := 0; i < 2; i++ {
		payload, err := catalog.Render(context.Background(), request)
		if err != nil {
			t.Fatalf("Render dynamic attempt %d: %v", i+1, err)
		}
		card, _ := payload["card"].(map[string]any)
		if card == nil {
			t.Fatalf("attempt %d returned no card", i+1)
		}
	}
	if store.activationCalls != 2 || store.metaCalls != 2 || store.bundleCalls != 1 {
		t.Fatalf("store calls activation/meta/bundle = %d/%d/%d, want 2/2/1",
			store.activationCalls, store.metaCalls, store.bundleCalls)
	}
}

func TestRuntimeCatalogHotCacheCannotBypassBlock(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	store := &runtimeStoreStub{
		activation: RuntimeActivation{
			Exists: true, Status: RuntimeActivationActive, Version: artifact.Meta.Version, Revision: 1,
		},
		meta:   runtimeArtifactMeta(artifact),
		bundle: artifact.Bundle,
	}
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)
	request := dynamicDefaultRequest()
	if _, err := catalog.Render(context.Background(), request); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	store.mu.Lock()
	store.meta.Blocked = true
	store.mu.Unlock()

	_, err := catalog.Render(context.Background(), request)
	if !errors.Is(err, ErrRuntimeCatalogBlocked) {
		t.Fatalf("blocked hot-cache render error = %v, want ErrRuntimeCatalogBlocked", err)
	}
	if store.bundleCalls != 1 {
		t.Fatalf("bundle calls = %d, want warm load only", store.bundleCalls)
	}
}

func TestRuntimeCatalogHotCacheRejectsMetadataIdentityMismatch(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	store := &runtimeStoreStub{
		meta:   runtimeArtifactMeta(artifact),
		bundle: artifact.Bundle,
	}
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)
	if _, err := catalog.MetaExact(context.Background(), CatalogExactRequest{
		Access: dynamicDefaultRequest().Access, ID: artifact.Meta.ID, Version: artifact.Meta.Version,
	}); err != nil {
		t.Fatalf("warm cache: %v", err)
	}

	const aliasID ID = "test.runtime-card-alias"
	store.mu.Lock()
	store.meta.ID = aliasID
	store.meta.Version = "9.9.9"
	store.mu.Unlock()

	_, err := catalog.MetaExact(context.Background(), CatalogExactRequest{
		Access: dynamicDefaultRequest().Access, ID: aliasID, Version: "9.9.9",
	})
	if !errors.Is(err, ErrRuntimeCatalogIntegrity) {
		t.Fatalf("aliased hot-cache error = %v, want ErrRuntimeCatalogIntegrity", err)
	}
	if store.bundleCalls != 1 {
		t.Fatalf("bundle calls = %d, want no second load for same hash", store.bundleCalls)
	}
}

func TestRuntimeCatalogColdLoadRejectsStoredMetadataDrift(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	tests := []struct {
		name   string
		mutate func(*RuntimeArtifactMeta)
	}{
		{name: "owner", mutate: func(meta *RuntimeArtifactMeta) { meta.Owner += ".other" }},
		{name: "visibility", mutate: func(meta *RuntimeArtifactMeta) { meta.Visibility = CatalogVisibilityPublic }},
		{name: "protocol", mutate: func(meta *RuntimeArtifactMeta) { meta.Protocol = "octo-card@9.9" }},
		{name: "contract version", mutate: func(meta *RuntimeArtifactMeta) { meta.ContractVersion = "9.9.9" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			meta := runtimeArtifactMeta(artifact)
			test.mutate(&meta)
			store := &runtimeStoreStub{meta: meta, bundle: artifact.Bundle}
			catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)

			_, err := catalog.MetaExact(context.Background(), CatalogExactRequest{
				Access: dynamicDefaultRequest().Access, ID: artifact.Meta.ID, Version: artifact.Meta.Version,
			})
			if !errors.Is(err, ErrRuntimeCatalogIntegrity) {
				t.Fatalf("metadata drift error = %v, want ErrRuntimeCatalogIntegrity", err)
			}
		})
	}
}

func TestRuntimeCatalogRejectsMalformedStoredHashBeforeBundleLoad(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	meta := runtimeArtifactMeta(artifact)
	meta.Hash = "not-a-sha256"
	store := &runtimeStoreStub{meta: meta, bundle: artifact.Bundle}
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)

	_, err := catalog.MetaExact(context.Background(), CatalogExactRequest{
		Access: dynamicDefaultRequest().Access, ID: artifact.Meta.ID, Version: artifact.Meta.Version,
	})
	if !errors.Is(err, ErrRuntimeCatalogIntegrity) {
		t.Fatalf("malformed hash error = %v, want ErrRuntimeCatalogIntegrity", err)
	}
	if store.bundleCalls != 0 {
		t.Fatalf("bundle calls = %d, want 0 for malformed metadata", store.bundleCalls)
	}
}

func TestRuntimeCatalogPreservesAuthoritativeStoreErrorClass(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	request := CatalogExactRequest{
		Access: dynamicDefaultRequest().Access, ID: artifact.Meta.ID, Version: artifact.Meta.Version,
	}
	tests := []struct {
		name      string
		metaErr   error
		bundleErr error
		want      error
		unavail   bool
	}{
		{name: "unknown", metaErr: ErrTemplateUnknown, want: ErrTemplateUnknown},
		{name: "metadata integrity", metaErr: ErrRuntimeCatalogIntegrity, want: ErrRuntimeCatalogIntegrity},
		{name: "bundle integrity", bundleErr: ErrRuntimeCatalogIntegrity, want: ErrRuntimeCatalogIntegrity},
		{name: "database unavailable", metaErr: errors.New("db unavailable"), want: ErrRuntimeCatalogUnavailable, unavail: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &runtimeStoreStub{
				meta: runtimeArtifactMeta(artifact), metaErr: test.metaErr,
				bundle: artifact.Bundle, bundleErr: test.bundleErr,
			}
			catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)
			_, err := catalog.MetaExact(context.Background(), request)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if errors.Is(err, ErrRuntimeCatalogUnavailable) != test.unavail {
				t.Fatalf("unavailable classification = %v, want %v (error=%v)",
					errors.Is(err, ErrRuntimeCatalogUnavailable), test.unavail, err)
			}
		})
	}
}

func TestRuntimeCatalogClassifiesCompileCapacitySeparatelyFromStoredCorruption(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	request := CatalogExactRequest{
		Access: dynamicDefaultRequest().Access, ID: artifact.Meta.ID, Version: artifact.Meta.Version,
	}
	tests := []struct {
		name          string
		compileErr    error
		want          error
		wantIntegrity bool
	}{
		{name: "deadline", compileErr: context.DeadlineExceeded, want: ErrRuntimeCatalogUnavailable},
		{name: "busy", compileErr: ErrArtifactCompileBusy, want: ErrRuntimeCatalogUnavailable},
		{name: "invalid stored bundle", compileErr: &ArtifactValidationError{Category: "schema", Document: "schema"}, want: ErrRuntimeCatalogIntegrity, wantIntegrity: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &runtimeStoreStub{meta: runtimeArtifactMeta(artifact), bundle: artifact.Bundle}
			catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)
			catalog.compile = func(context.Context, Bundle, CompileLimits) (*CompiledArtifact, error) {
				return nil, test.compileErr
			}
			_, err := catalog.MetaExact(context.Background(), request)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if errors.Is(err, ErrRuntimeCatalogIntegrity) != test.wantIntegrity {
				t.Fatalf("integrity classification = %v, want %v (error=%v)",
					errors.Is(err, ErrRuntimeCatalogIntegrity), test.wantIntegrity, err)
			}
		})
	}
}

func TestCompiledArtifactCacheEnforcesEntryLRU(t *testing.T) {
	cache := newCompiledArtifactCache(2, 32)
	a := &CompiledArtifact{Bundle: CanonicalBundle("aaa")}
	b := &CompiledArtifact{Bundle: CanonicalBundle("bbb")}
	c := &CompiledArtifact{Bundle: CanonicalBundle("ccc")}
	cache.add("a", a)
	cache.add("b", b)
	if _, ok := cache.get("a"); !ok {
		t.Fatal("expected a to be cached")
	}
	cache.add("c", c)

	if _, ok := cache.get("b"); ok {
		t.Fatal("least-recently-used b was not evicted")
	}
	if entries, bytes := cache.size(); entries != 2 || bytes != 6 {
		t.Fatalf("cache size = %d entries/%d bytes, want 2/6", entries, bytes)
	}
}

func TestCompiledArtifactCacheEnforcesByteLimit(t *testing.T) {
	cache := newCompiledArtifactCache(4, 5)
	cache.add("a", &CompiledArtifact{Bundle: CanonicalBundle("aaa")})
	cache.add("b", &CompiledArtifact{Bundle: CanonicalBundle("bbb")})

	if _, ok := cache.get("a"); ok {
		t.Fatal("old entry a was not evicted to honor byte limit")
	}
	if entries, bytes := cache.size(); entries != 1 || bytes != 3 {
		t.Fatalf("cache size = %d entries/%d bytes, want 1/3", entries, bytes)
	}
	cache.add("too-large", &CompiledArtifact{Bundle: CanonicalBundle("123456")})
	if _, ok := cache.get("too-large"); ok {
		t.Fatal("entry larger than the byte budget must not be cached")
	}
}

func TestRuntimeCatalogSingleflightSharesConcurrentCompile(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	store := &runtimeStoreStub{
		meta: runtimeArtifactMeta(artifact), bundle: artifact.Bundle,
		bundleStarted: started, bundleRelease: release,
	}
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)

	const callers = 16
	begin := make(chan struct{})
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-begin
			_, err := catalog.loadCompiled(context.Background(), runtimeArtifactMeta(artifact))
			errs <- err
		}()
	}
	close(begin)
	<-started
	close(release)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent load %d: %v", i+1, err)
		}
	}
	store.mu.Lock()
	bundleCalls := store.bundleCalls
	store.mu.Unlock()
	if bundleCalls != 1 {
		t.Fatalf("bundle calls = %d, want one shared load", bundleCalls)
	}
}

func TestRuntimeCatalogSingleflightRevalidatesResultForEveryWaiter(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	store := &runtimeStoreStub{
		meta: runtimeArtifactMeta(artifact), bundle: artifact.Bundle,
		bundleStarted: started, bundleRelease: release,
	}
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)

	firstResult := make(chan error, 1)
	go func() {
		_, err := catalog.loadCompiled(context.Background(), runtimeArtifactMeta(artifact))
		firstResult <- err
	}()
	<-started

	aliasMeta := runtimeArtifactMeta(artifact)
	aliasMeta.ID = "test.runtime-card-alias"
	aliasMeta.Version = "9.9.9"
	secondResult := make(chan error, 1)
	go func() {
		_, err := catalog.loadCompiled(context.Background(), aliasMeta)
		secondResult <- err
	}()
	close(release)

	if err := <-firstResult; err != nil {
		t.Fatalf("first waiter: %v", err)
	}
	if err := <-secondResult; !errors.Is(err, ErrRuntimeCatalogIntegrity) {
		t.Fatalf("aliased shared waiter error = %v, want ErrRuntimeCatalogIntegrity", err)
	}
}

func TestRuntimeCatalogCallerCancellationDoesNotCancelSharedCompile(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	meta := runtimeArtifactMeta(artifact)
	started := make(chan struct{})
	release := make(chan struct{})
	store := &runtimeStoreStub{
		meta: meta, bundle: artifact.Bundle,
		bundleStarted: started, bundleRelease: release,
	}
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)

	firstResult := make(chan error, 1)
	go func() {
		_, err := catalog.loadCompiled(context.Background(), meta)
		firstResult <- err
	}()
	<-started

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalog.loadCompiled(canceled, meta); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("shared compile was canceled by waiter: %v", err)
	}
	store.mu.Lock()
	bundleCalls := store.bundleCalls
	store.mu.Unlock()
	if bundleCalls != 1 {
		t.Fatalf("bundle calls = %d, want shared work to continue once", bundleCalls)
	}
}

func TestRuntimeCatalogHooksUseBoundedResolveAndCacheOutcomes(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	store := &runtimeStoreStub{
		activation: RuntimeActivation{
			Exists: true, Status: RuntimeActivationActive, Version: artifact.Meta.Version, Revision: 1,
		},
		meta: runtimeArtifactMeta(artifact), bundle: artifact.Bundle,
	}
	var resolveLabels, cacheLabels []string
	var gaugeEntries, gaugeBytes int
	catalog, err := NewRuntimeCatalog(runtimeStaticRegistry(t), store, RuntimeCatalogConfig{
		MaxCacheEntries: 4, MaxCacheBytes: 4 << 20, CompileTimeout: time.Second,
		AuthorizeDynamic: allowDynamicForTest,
		Hooks: RuntimeCatalogHooks{
			OnResolve: func(source RuntimeSource, result string) {
				resolveLabels = append(resolveLabels, string(source)+":"+result)
			},
			OnCache:     func(result string) { cacheLabels = append(cacheLabels, result) },
			OnCacheSize: func(entries, bytes int) { gaugeEntries, gaugeBytes = entries, bytes },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := catalog.Render(context.Background(), dynamicDefaultRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(resolveLabels, []string{"dynamic:ok", "dynamic:ok"}) {
		t.Fatalf("resolve labels = %v", resolveLabels)
	}
	if !reflect.DeepEqual(cacheLabels, []string{"miss", "hit"}) {
		t.Fatalf("cache labels = %v", cacheLabels)
	}
	if gaugeEntries != 1 || gaugeBytes != len(artifact.Bundle) {
		t.Fatalf("cache gauges = %d/%d, want 1/%d", gaugeEntries, gaugeBytes, len(artifact.Bundle))
	}
}

func TestRuntimeCatalogDynamicMetadataFallbackAndActionContext(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	artifact.Meta.interactions = map[ViewKey]InteractionReport{
		"main": {Actions: []DeclaredAction{{Type: "Action.Submit", ID: "submit"}}},
	}
	store := &runtimeStoreStub{
		activation: RuntimeActivation{
			Exists: true, Status: RuntimeActivationActive, Version: artifact.Meta.Version, Revision: 1,
		},
		meta: runtimeArtifactMeta(artifact), bundle: artifact.Bundle,
	}
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)
	catalog.compile = func(context.Context, Bundle, CompileLimits) (*CompiledArtifact, error) {
		return artifact, nil
	}
	access := dynamicDefaultRequest().Access
	meta, err := catalog.MetaDefault(context.Background(), CatalogDefaultRequest{Access: access, ID: artifact.Meta.ID})
	if err != nil || meta.Version != artifact.Meta.Version {
		t.Fatalf("MetaDefault meta=%+v err=%v", meta, err)
	}
	fallback, err := catalog.FallbackText(context.Background(), CatalogFallbackRequest{
		Access: access, ID: artifact.Meta.ID, Version: artifact.Meta.Version,
		State: "shown", Fields: json.RawMessage(`{"title":"hello","groups":[{"items":["a"]}]}`), Lang: "en-US",
	})
	if err != nil || fallback == "" {
		t.Fatalf("FallbackText fallback=%q err=%v", fallback, err)
	}
	action, err := catalog.ActionContext(context.Background(), CatalogActionRequest{
		CatalogExactRequest: CatalogExactRequest{Access: access, ID: artifact.Meta.ID, Version: artifact.Meta.Version},
		ActionID:            "submit",
	})
	if err != nil || action.View != "main" || action.Meta.Version != artifact.Meta.Version {
		t.Fatalf("ActionContext action=%+v err=%v", action, err)
	}
}

func TestRuntimeCatalogResultLabelsAreBounded(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{ErrTemplateUnknown, "unknown"},
		{ErrRuntimeCatalogDisabled, "disabled"},
		{ErrRuntimeCatalogNewSendDisabled, "disabled"},
		{ErrRuntimeCatalogBlocked, "blocked"},
		{ErrRuntimeCatalogNotAuthorized, "unauthorized"},
		{ErrRuntimeCatalogIntegrity, "integrity"},
		{ErrRuntimeCatalogUnavailable, "unavailable"},
		{context.DeadlineExceeded, "unavailable"},
		{errors.New("other"), "error"},
	}
	for _, test := range tests {
		if got := runtimeCatalogResult(test.err); got != test.want {
			t.Errorf("runtimeCatalogResult(%v)=%q, want %q", test.err, got, test.want)
		}
	}
}

func TestDefaultCatalogFallsBackToCurrentRegistryAndHonorsExplicitInstall(t *testing.T) {
	previousRegistry := DefaultRegistry()
	SetDefaultCatalog(nil)
	registry := runtimeStaticRegistry(t)
	SetDefaultRegistry(registry)
	t.Cleanup(func() {
		SetDefaultCatalog(nil)
		SetDefaultRegistry(previousRegistry)
	})

	fallback := DefaultCatalog()
	if fallback == nil {
		t.Fatal("DefaultCatalog did not adapt the current Registry")
	}
	explicit, err := NewStaticCatalog(registry)
	if err != nil {
		t.Fatal(err)
	}
	SetDefaultCatalog(explicit)
	if DefaultCatalog() != explicit {
		t.Fatal("DefaultCatalog did not return the explicit process catalog")
	}
}

func TestActionViewFromMetaRejectsUnknownAndAmbiguousActions(t *testing.T) {
	meta := TemplateMeta{ID: "test.actions", Version: "1.0.0", interactions: map[ViewKey]InteractionReport{
		"a": {Actions: []DeclaredAction{{Type: "Action.Submit", ID: "submit"}}},
		"b": {Actions: []DeclaredAction{{Type: "Action.Submit", ID: "submit"}}},
	}}
	if _, err := actionViewFromMeta(meta, "missing"); !errors.Is(err, ErrActionUnknown) {
		t.Fatalf("missing action error=%v, want ErrActionUnknown", err)
	}
	if _, err := actionViewFromMeta(meta, "submit"); !errors.Is(err, ErrActionUnknown) {
		t.Fatalf("ambiguous action error=%v, want ErrActionUnknown", err)
	}
}

func runtimeStaticRegistry(t *testing.T) *Registry {
	t.Helper()
	registry := NewRegistry()
	registry.RegisterJSON(jsonCardTestData, "testdata/test.runtime-parity@1.0.0")
	registry.SetDefault("test.runtime-parity", "1.0.0")
	registry.Freeze()
	return registry
}

func compileRuntimeFixture(t *testing.T) *CompiledArtifact {
	t.Helper()
	artifact, err := CompileJSONArtifact(context.Background(), validJSONArtifactBundle(), DefaultCompileLimits())
	if err != nil {
		t.Fatalf("CompileJSONArtifact fixture: %v", err)
	}
	return artifact
}

func runtimeArtifactMeta(artifact *CompiledArtifact) RuntimeArtifactMeta {
	return RuntimeArtifactMeta{
		ID: artifact.Meta.ID, Version: artifact.Meta.Version, Source: RuntimeSourceDynamic,
		Engine: artifact.Engine, Hash: artifact.Hash, Owner: artifact.Owner,
		Visibility: artifact.Visibility, Protocol: artifact.Meta.Protocol,
		ContractVersion: artifact.ContractVersion,
	}
}

func newRuntimeCatalogForTest(
	t *testing.T,
	registry *Registry,
	store RuntimeArtifactStore,
	authorize RuntimeDynamicAuthorizeFunc,
) *RuntimeCatalog {
	t.Helper()
	catalog, err := NewRuntimeCatalog(registry, store, RuntimeCatalogConfig{
		MaxCacheEntries:  4,
		MaxCacheBytes:    4 << 20,
		CompileTimeout:   time.Second,
		AuthorizeDynamic: authorize,
	})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog: %v", err)
	}
	return catalog
}

func staticDefaultRequest() CatalogRenderRequest {
	return CatalogRenderRequest{
		Access: CatalogAccess{Purpose: CatalogPurposeNewSend, Principal: CatalogPrincipal{
			Kind: CatalogPrincipalSystem, ID: "test-producer", SpaceID: "space-1",
		}},
		ID: "test.runtime-parity", State: "shown",
		Fields: json.RawMessage(`{"title":"parity"}`),
		Env:    BuildEnv{Lang: "en-US", SpaceID: "space-1"},
	}
}

func dynamicDefaultRequest() CatalogRenderRequest {
	return CatalogRenderRequest{
		Access: CatalogAccess{Purpose: CatalogPurposeNewSend, Principal: CatalogPrincipal{
			Kind: CatalogPrincipalSystem, ID: "test-producer", SpaceID: "space-1",
		}},
		ID: "test.runtime-card", State: "shown",
		Fields: json.RawMessage(`{"title":"hello","groups":[{"items":["a"]}]}`),
		Env:    BuildEnv{Lang: "en-US", SpaceID: "space-1"},
	}
}

func allowDynamicForTest(context.Context, CatalogAccess, RuntimeArtifactMeta, RuntimeGrant) error {
	return nil
}

type runtimeStoreStub struct {
	mu              sync.Mutex
	bundleStartOnce sync.Once

	activation    RuntimeActivation
	activationErr error
	meta          RuntimeArtifactMeta
	metaErr       error
	bundle        CanonicalBundle
	bundleErr     error
	bundleStarted chan struct{}
	bundleRelease <-chan struct{}

	activationCalls int
	metaCalls       int
	bundleCalls     int
}

func (s *runtimeStoreStub) LoadActivation(context.Context, ID) (RuntimeActivation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activationCalls++
	return s.activation, s.activationErr
}

func (s *runtimeStoreStub) LoadArtifactMeta(context.Context, ID, string) (RuntimeArtifactMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metaCalls++
	return s.meta, s.metaErr
}

func (s *runtimeStoreStub) LoadArtifactBundle(ctx context.Context, _ ID, _, _ string) (CanonicalBundle, error) {
	s.mu.Lock()
	s.bundleCalls++
	bundle := append(CanonicalBundle(nil), s.bundle...)
	err := s.bundleErr
	started := s.bundleStarted
	release := s.bundleRelease
	s.mu.Unlock()
	if started != nil {
		s.bundleStartOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return bundle, err
}
