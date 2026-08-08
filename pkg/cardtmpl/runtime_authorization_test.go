package cardtmpl

// PR-C D4 evidence: one authorization decision comes from one snapshot.
//
// The stub below counts snapshot calls and records the query it was asked, so
// the tests can assert the property that actually matters — not "the answer was
// right" but "the answer came from a single read". A future refactor that
// reintroduces a read-pointer-then-check-grant split fails here even if every
// individual answer stays correct.

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type authorizationStoreStub struct {
	runtimeStoreStub

	authMu    sync.Mutex
	authCalls int
	queries   []RuntimeAuthorizationQuery

	auth    RuntimeAuthorization
	authErr error
	// mutate lets a test bend one reply into an incoherent shape.
	mutate func(RuntimeAuthorizationQuery, RuntimeAuthorization) RuntimeAuthorization
}

func (s *authorizationStoreStub) LoadAuthorization(
	_ context.Context,
	query RuntimeAuthorizationQuery,
) (RuntimeAuthorization, error) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.authCalls++
	s.queries = append(s.queries, query)
	if s.authErr != nil {
		return RuntimeAuthorization{}, s.authErr
	}
	reply := s.auth
	if query.Version != "" {
		// A pinned query must be answered without the pointer.
		reply.Activation = RuntimeActivation{}
		reply.Version = query.Version
	}
	if s.mutate != nil {
		reply = s.mutate(query, reply)
	}
	return reply, nil
}

func (s *authorizationStoreStub) snapshotCalls() int {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.authCalls
}

func (s *authorizationStoreStub) lastQuery(t *testing.T) RuntimeAuthorizationQuery {
	t.Helper()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if len(s.queries) == 0 {
		t.Fatal("no authorization query was issued")
	}
	return s.queries[len(s.queries)-1]
}

func newAuthorizationStoreStub(artifact *CompiledArtifact, grant RuntimeGrant) *authorizationStoreStub {
	meta := runtimeArtifactMeta(artifact)
	stub := &authorizationStoreStub{
		auth: RuntimeAuthorization{
			Activation: RuntimeActivation{
				Exists: true, Status: RuntimeActivationActive, Version: meta.Version, Revision: 7,
			},
			Version: meta.Version, Artifact: meta, Grant: grant,
		},
	}
	stub.bundle = artifact.Bundle
	return stub
}

func grantedSend() RuntimeGrant {
	return RuntimeGrant{Found: true, Scope: RuntimeGrantScopeExact, Revision: 3, Discover: true, Send: true}
}

func TestRuntimeCatalogDynamicNewSendUsesExactlyOneSnapshot(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	store := newAuthorizationStoreStub(artifact, grantedSend())
	var seen RuntimeGrant
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store,
		func(_ context.Context, _ CatalogAccess, _ RuntimeArtifactMeta, grant RuntimeGrant) error {
			seen = grant
			return nil
		})

	if _, err := catalog.Render(context.Background(), dynamicDefaultRequest()); err != nil {
		t.Fatalf("dynamic default render: %v", err)
	}
	if store.snapshotCalls() != 1 {
		t.Fatalf("snapshot calls = %d, want 1", store.snapshotCalls())
	}
	// The legacy split readers must not have been consulted at all: if either
	// still runs, the decision is again spread across several points in time.
	if store.activationCalls != 0 || store.metaCalls != 0 {
		t.Fatalf("split reads activation/meta = %d/%d, want 0/0", store.activationCalls, store.metaCalls)
	}
	if seen != grantedSend() {
		t.Fatalf("authorizer grant = %+v, want %+v", seen, grantedSend())
	}
	if query := store.lastQuery(t); query.Version != "" || query.ID != "test.runtime-card" {
		t.Fatalf("new-send query = %+v, want the activation pointer resolved for the template", query)
	}
}

func TestRuntimeCatalogPinnedReadsNeverFollowActivationPointer(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	artifact.Meta.interactions = map[ViewKey]InteractionReport{
		"main": {Actions: []DeclaredAction{{Type: "Action.Submit", ID: "submit"}}},
	}
	store := newAuthorizationStoreStub(artifact, RuntimeGrant{
		Found: true, Scope: RuntimeGrantScopeGlobal, Discover: true, Edit: true,
	})
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)
	catalog.compile = func(context.Context, Bundle, CompileLimits) (*CompiledArtifact, error) {
		return artifact, nil
	}

	access := CatalogAccess{Purpose: CatalogPurposeActionContext, Principal: CatalogPrincipal{
		Kind: CatalogPrincipalInternalProducer, ID: "docs-notify", SpaceID: "space-1",
	}}
	if _, err := catalog.ActionContext(context.Background(), CatalogActionRequest{
		CatalogExactRequest: CatalogExactRequest{
			Access: access, ID: artifact.Meta.ID, Version: artifact.Meta.Version,
		},
		ActionID: "submit",
	}); err != nil {
		t.Fatalf("dynamic action context: %v", err)
	}
	if query := store.lastQuery(t); query.Version != artifact.Meta.Version {
		t.Fatalf("action query = %+v, want the stored exact version pinned", query)
	}
	if store.snapshotCalls() != 1 {
		t.Fatalf("snapshot calls = %d, want 1", store.snapshotCalls())
	}
}

// A store that answers a pinned query with pointer state, or that resolves a
// different version than the activation row implies, has produced a snapshot
// that cannot be reasoned about. The catalog rejects it instead of picking
// whichever half looks more plausible.
func TestRuntimeCatalogRejectsIncoherentAuthorizationSnapshot(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	for _, test := range []struct {
		name    string
		pinned  bool
		mutate  func(RuntimeAuthorizationQuery, RuntimeAuthorization) RuntimeAuthorization
		wantErr error
	}{
		{
			name:   "pinned reply leaks the activation pointer",
			pinned: true,
			mutate: func(_ RuntimeAuthorizationQuery, auth RuntimeAuthorization) RuntimeAuthorization {
				auth.Activation = RuntimeActivation{Exists: true, Status: RuntimeActivationActive, Version: "9.9.9"}
				return auth
			},
			wantErr: ErrRuntimeCatalogIntegrity,
		},
		{
			name: "resolved version disagrees with the activation row",
			mutate: func(_ RuntimeAuthorizationQuery, auth RuntimeAuthorization) RuntimeAuthorization {
				auth.Version = "9.9.9"
				return auth
			},
			wantErr: ErrRuntimeCatalogIntegrity,
		},
		{
			name: "activation row carries an unknown status",
			mutate: func(_ RuntimeAuthorizationQuery, auth RuntimeAuthorization) RuntimeAuthorization {
				auth.Activation.Status = "half-active"
				return auth
			},
			wantErr: ErrRuntimeCatalogIntegrity,
		},
		{
			name: "disabled pointer never degrades to a static same-ID version",
			mutate: func(_ RuntimeAuthorizationQuery, auth RuntimeAuthorization) RuntimeAuthorization {
				auth.Activation = RuntimeActivation{Exists: true, Status: RuntimeActivationDisabled, Revision: 5}
				auth.Version = ""
				return auth
			},
			wantErr: ErrRuntimeCatalogDisabled,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newAuthorizationStoreStub(artifact, grantedSend())
			store.mutate = test.mutate
			catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, allowDynamicForTest)

			var err error
			if test.pinned {
				_, err = catalog.MetaExact(context.Background(), CatalogExactRequest{
					Access: dynamicDefaultRequest().Access, ID: artifact.Meta.ID, Version: artifact.Meta.Version,
				})
			} else {
				_, err = catalog.Render(context.Background(), dynamicDefaultRequest())
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

// The single-snapshot rework must not put the runtime DB in the path of a
// frozen static card: an explicit static exact stays readable with the store
// entirely unreachable, which is what keeps a gates-off deployment unaffected.
func TestRuntimeCatalogStaticExactNeedsNoSnapshot(t *testing.T) {
	store := &authorizationStoreStub{authErr: errors.New("db unavailable")}
	store.activationErr = errors.New("db unavailable")
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, nil)

	request := staticDefaultRequest()
	request.Version = "1.0.0"
	if _, err := catalog.Render(context.Background(), request); err != nil {
		t.Fatalf("explicit static render with an unreachable store: %v", err)
	}
	fallback := CatalogFallbackRequest{
		Access: request.Access, ID: request.ID, Version: "1.0.0",
		State: request.State, Fields: request.Fields, Lang: "en-US",
	}
	if _, err := catalog.FallbackText(context.Background(), fallback); err != nil {
		t.Fatalf("explicit static fallback with an unreachable store: %v", err)
	}
	if store.snapshotCalls() != 0 {
		t.Fatalf("snapshot calls = %d, want 0", store.snapshotCalls())
	}
}

// A grant-unaware store can still serve static resolution, but it can never
// produce an allow for a dynamic business purpose — so the split read it
// performs cannot be used to smuggle an authorization decision.
func TestRuntimeCatalogWithoutResolverCannotAuthorizeDynamic(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	store := &runtimeStoreStub{
		activation: RuntimeActivation{
			Exists: true, Status: RuntimeActivationActive, Version: artifact.Meta.Version, Revision: 2,
		},
		meta:   runtimeArtifactMeta(artifact),
		bundle: artifact.Bundle,
	}
	granted := false
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store,
		func(_ context.Context, _ CatalogAccess, _ RuntimeArtifactMeta, grant RuntimeGrant) error {
			if grant.Allows(CatalogPurposeNewSend) {
				granted = true
				return nil
			}
			return ErrRuntimeCatalogNotAuthorized
		})

	_, err := catalog.Render(context.Background(), dynamicDefaultRequest())
	if !errors.Is(err, ErrRuntimeCatalogNotAuthorized) {
		t.Fatalf("render error = %v, want ErrRuntimeCatalogNotAuthorized", err)
	}
	if granted {
		t.Fatal("a store without the resolver produced an authorized dynamic access")
	}
}

func TestRuntimeGrantAllowsMapsPurposeToPermission(t *testing.T) {
	full := RuntimeGrant{Found: true, Discover: true, Send: true, Edit: true}
	for _, test := range []struct {
		purpose CatalogPurpose
		grant   RuntimeGrant
		want    bool
	}{
		{CatalogPurposeNewSend, full, true},
		{CatalogPurposeHistoricalEdit, full, true},
		{CatalogPurposeActionContext, full, true},
		{CatalogPurposeNewSend, RuntimeGrant{Found: true, Discover: true}, false},
		{CatalogPurposeHistoricalEdit, RuntimeGrant{Found: true, Discover: true, Send: true}, false},
		{CatalogPurposeActionContext, RuntimeGrant{Found: true, Discover: true, Send: true}, false},
		{CatalogPurpose("unheard-of"), full, false},
		{CatalogPurposeNewSend, RuntimeGrant{}, false},
	} {
		if got := test.grant.Allows(test.purpose); got != test.want {
			t.Fatalf("%s grant %+v allows = %v, want %v", test.purpose, test.grant, got, test.want)
		}
	}
}

func TestActivationVersionIsTheSingleActivationRule(t *testing.T) {
	for _, test := range []struct {
		name       string
		activation RuntimeActivation
		want       string
		wantErr    error
	}{
		{"absent row has no version", RuntimeActivation{}, "", nil},
		{"active row resolves", RuntimeActivation{
			Exists: true, Status: RuntimeActivationActive, Version: "1.2.3"}, "1.2.3", nil},
		{"disabled row is a typed denial", RuntimeActivation{
			Exists: true, Status: RuntimeActivationDisabled, Revision: 9}, "", ErrRuntimeCatalogDisabled},
		{"active row without a version is corrupt", RuntimeActivation{
			Exists: true, Status: RuntimeActivationActive, Version: "  "}, "", ErrRuntimeCatalogIntegrity},
		{"unknown status is corrupt", RuntimeActivation{
			Exists: true, Status: "paused"}, "", ErrRuntimeCatalogIntegrity},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ActivationVersion("test.runtime-card", test.activation)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("version = %q, want %q", got, test.want)
			}
		})
	}
}

// The compiled-artifact cache must not be able to outlive a revoke. It is keyed
// on the immutable content hash and holds no grant state, so a hot entry cannot
// carry a stale permission — but that is an argument, and the acceptance matrix
// asks for evidence. The sibling test covers a hot cache against a *block*;
// this one covers it against a revoke, which is the case an operator reaches
// for when a producer has to be cut off right now.
func TestRuntimeCatalogHotCacheCannotOutliveARevoke(t *testing.T) {
	artifact := compileRuntimeFixture(t)
	store := newAuthorizationStoreStub(artifact, grantedSend())
	// The permission rule the production authorizer applies. Using the
	// allow-everything stub here would make the test pass without ever
	// consulting the grant, which is the very thing being checked.
	enforceGrant := func(_ context.Context, access CatalogAccess, _ RuntimeArtifactMeta, grant RuntimeGrant) error {
		if !grant.Allows(access.Purpose) {
			return ErrRuntimeCatalogNotAuthorized
		}
		return nil
	}
	catalog := newRuntimeCatalogForTest(t, runtimeStaticRegistry(t), store, enforceGrant)
	request := dynamicDefaultRequest()

	if _, err := catalog.Render(context.Background(), request); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}
	warmBundleCalls := store.bundleCalls
	warmSnapshots := store.snapshotCalls()
	if warmSnapshots == 0 {
		t.Fatal("the warm render took no snapshot at all")
	}

	// Revoke: the row becomes a tombstone, which is a *found* grant with no
	// permissions — not an absent one.
	store.authMu.Lock()
	store.auth.Grant = RuntimeGrant{Found: true, Scope: RuntimeGrantScopeExact, Revision: 4}
	store.authMu.Unlock()

	if _, err := catalog.Render(context.Background(), request); !errors.Is(err, ErrRuntimeCatalogNotAuthorized) {
		t.Fatalf("post-revoke hot render error = %v, want ErrRuntimeCatalogNotAuthorized", err)
	}
	// The denial came from a fresh snapshot, not from a cached decision.
	if store.snapshotCalls() <= warmSnapshots {
		t.Fatalf("the hot path answered without re-reading the grant (snapshots %d -> %d)",
			warmSnapshots, store.snapshotCalls())
	}
	// And it did not need to reload the bundle to find out, which is what makes
	// "the cache holds bytes, never permissions" observable rather than merely
	// asserted.
	if store.bundleCalls != warmBundleCalls {
		t.Fatalf("bundle calls = %d, want the warm load only (%d)", store.bundleCalls, warmBundleCalls)
	}

	// An edit grant is a separate permission: revoking send must not leave edit
	// standing, and restoring only edit must not re-enable send.
	store.authMu.Lock()
	store.auth.Grant = RuntimeGrant{Found: true, Scope: RuntimeGrantScopeExact, Revision: 5,
		Discover: true, Edit: true}
	store.authMu.Unlock()
	if _, err := catalog.Render(context.Background(), request); !errors.Is(err, ErrRuntimeCatalogNotAuthorized) {
		t.Fatalf("an edit-only grant authorized a new send: %v", err)
	}
}
