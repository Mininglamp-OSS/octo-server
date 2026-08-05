package bot_api

// PR-C D6 evidence: the shadow matrix, the strict Space resolver, and the fact
// that the capability manifest and the send path reach the same verdict.
//
// The matrix table below is the specification transcribed row for row. If a
// future change makes an ungranted or blocked dynamic pointer quietly fall back
// to the static card of the same ID, exactly one line here goes red.

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
)

type stubAuthorizationSource struct {
	newSend bool
	auth    map[cardtmpl.ID]cardtmpl.RuntimeAuthorization
	err     error
	granted []cardtmpl.RuntimeAdvertisedTemplate
	listErr error

	loads int
}

func (s *stubAuthorizationSource) LoadAuthorization(
	_ context.Context,
	query cardtmpl.RuntimeAuthorizationQuery,
) (cardtmpl.RuntimeAuthorization, error) {
	s.loads++
	if s.err != nil {
		return cardtmpl.RuntimeAuthorization{}, s.err
	}
	return s.auth[query.ID], nil
}

func (s *stubAuthorizationSource) ListAuthorizedTemplates(
	context.Context, cardtmpl.CatalogPrincipal, int,
) ([]cardtmpl.RuntimeAdvertisedTemplate, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.granted, nil
}

func (s *stubAuthorizationSource) NewSendEnabled() bool { return s.newSend }

func dynamicAuthorization(version string, blocked bool, grant cardtmpl.RuntimeGrant) cardtmpl.RuntimeAuthorization {
	return cardtmpl.RuntimeAuthorization{
		Activation: cardtmpl.RuntimeActivation{
			Exists: true, Status: cardtmpl.RuntimeActivationActive, Version: version, Revision: 3,
		},
		Version: version,
		Artifact: cardtmpl.RuntimeArtifactMeta{
			ID: aireasoningprocess.TemplateID, Version: version,
			Source: cardtmpl.RuntimeSourceDynamic, Owner: "ai", Blocked: blocked,
		},
		Grant: grant,
	}
}

func sendGrant() cardtmpl.RuntimeGrant {
	return cardtmpl.RuntimeGrant{
		Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Revision: 2, Discover: true, Send: true,
	}
}

func resolvedPrincipal() botCatalogPrincipal {
	return botCatalogPrincipal{BotID: "bot-42", SpaceID: "space-1", SpaceResolved: true}
}

func TestBotTemplateSendRefFollowsTheShadowMatrix(t *testing.T) {
	staticVersion := aireasoningprocess.TemplateVersionV3
	id := aireasoningprocess.TemplateID

	for _, test := range []struct {
		name      string
		source    *stubAuthorizationSource
		principal botCatalogPrincipal
		want      botTemplateRef
		wantErr   bool
	}{
		{
			name:      "gate closed keeps the static policy and never reads the runtime DB",
			source:    &stubAuthorizationSource{newSend: false},
			principal: resolvedPrincipal(),
			want:      botTemplateRef{ID: id, Version: staticVersion},
		},
		{
			name: "no activation row keeps the static policy",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: {},
			}},
			principal: resolvedPrincipal(),
			want:      botTemplateRef{ID: id, Version: staticVersion},
		},
		{
			name: "pointer at a policy-listed static exact is advertised without a grant",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: {
					Activation: cardtmpl.RuntimeActivation{
						Exists: true, Status: cardtmpl.RuntimeActivationActive, Version: staticVersion,
					},
					Version:  staticVersion,
					Artifact: cardtmpl.RuntimeArtifactMeta{Source: cardtmpl.RuntimeSourceStatic},
				},
			}},
			principal: resolvedPrincipal(),
			want:      botTemplateRef{ID: id, Version: staticVersion},
		},
		{
			name: "pointer at a static exact outside the policy advertises nothing",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: {
					Activation: cardtmpl.RuntimeActivation{
						Exists: true, Status: cardtmpl.RuntimeActivationActive, Version: "0.0.1",
					},
					Version:  "0.0.1",
					Artifact: cardtmpl.RuntimeArtifactMeta{Source: cardtmpl.RuntimeSourceStatic},
				},
			}},
			principal: resolvedPrincipal(),
			want:      botTemplateRef{},
		},
		{
			name: "granted dynamic active shadows the static version",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: dynamicAuthorization("9.0.0", false, sendGrant()),
			}},
			principal: resolvedPrincipal(),
			want:      botTemplateRef{ID: id, Version: "9.0.0"},
		},
		{
			name: "ungranted dynamic active does not fall back to the static version",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: dynamicAuthorization("9.0.0", false, cardtmpl.RuntimeGrant{}),
			}},
			principal: resolvedPrincipal(),
			want:      botTemplateRef{},
		},
		{
			name: "an exact tombstone denies just like an absent grant",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: dynamicAuthorization("9.0.0", false, cardtmpl.RuntimeGrant{
					Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Revision: 7,
				}),
			}},
			principal: resolvedPrincipal(),
			want:      botTemplateRef{},
		},
		{
			name: "blocked dynamic active fails the whole ID closed",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: dynamicAuthorization("9.0.0", true, sendGrant()),
			}},
			principal: resolvedPrincipal(),
			want:      botTemplateRef{},
		},
		{
			name:      "a runtime DB failure is an error, not a static fallback",
			source:    &stubAuthorizationSource{newSend: true, err: errors.New("db unavailable")},
			principal: resolvedPrincipal(),
			wantErr:   true,
		},
		{
			name: "a disabled pointer withholds the ID without reverting to static",
			source: &stubAuthorizationSource{
				newSend: true, err: cardtmpl.ErrRuntimeCatalogDisabled,
			},
			principal: resolvedPrincipal(),
			want:      botTemplateRef{},
		},
		{
			name: "an unresolved Space withholds the dynamic overlay only",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: dynamicAuthorization("9.0.0", false, sendGrant()),
			}},
			principal: botCatalogPrincipal{BotID: "bot-42"},
			want:      botTemplateRef{ID: id, Version: staticVersion},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := newMustTestCatalog(t)
			catalog.authorization = test.source

			got, err := catalog.resolveSendRef(context.Background(), test.principal, id)
			if test.wantErr {
				if !errors.Is(err, errBotTemplateRuntimeUnavailable) {
					t.Fatalf("error = %v, want errBotTemplateRuntimeUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSendRef: %v", err)
			}
			if got != test.want {
				t.Fatalf("ref = %+v, want %+v", got, test.want)
			}
			if !test.source.newSend && test.source.loads != 0 {
				t.Fatalf("gate-off resolution issued %d runtime reads, want 0", test.source.loads)
			}
		})
	}
}

// The manifest and the send path must not be able to disagree. Rather than
// assert each separately against a hand-written expectation, this drives both
// off the same state and compares them to each other.
func TestBotTemplateManifestAndSendAgree(t *testing.T) {
	id := aireasoningprocess.TemplateID
	for _, test := range []struct {
		name   string
		source *stubAuthorizationSource
	}{
		{"gate closed", &stubAuthorizationSource{}},
		{"granted dynamic", &stubAuthorizationSource{
			newSend: true,
			auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: dynamicAuthorization("9.0.0", false, sendGrant()),
			},
		}},
		{"ungranted dynamic", &stubAuthorizationSource{
			newSend: true,
			auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: dynamicAuthorization("9.0.0", false, cardtmpl.RuntimeGrant{}),
			},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := newMustTestCatalog(t)
			catalog.authorization = test.source
			principal := resolvedPrincipal()

			advertised, err := catalog.advertisedSendRefs(context.Background(), principal)
			if err != nil {
				t.Fatalf("advertisedSendRefs: %v", err)
			}
			for _, ref := range advertised {
				accepted, err := catalog.requireSendableRef(context.Background(), principal,
					map[string]any{"id": string(ref.ID), "version": ref.Version})
				if err != nil {
					t.Fatalf("send refused the advertised ref %+v: %v", ref, err)
				}
				if accepted != ref {
					t.Fatalf("send accepted %+v for advertised %+v", accepted, ref)
				}
			}
			if len(advertised) == 0 {
				// Nothing advertised means the static version must be refused
				// too — an advertised-empty manifest with a sendable ref would
				// be the exact drift this test exists to catch.
				if _, err := catalog.requireSendableRef(context.Background(), principal, map[string]any{
					"id": string(id), "version": aireasoningprocess.TemplateVersionV3,
				}); !errors.Is(err, errBotTemplateRequestInvalid) {
					t.Fatalf("empty manifest still accepted the static ref: %v", err)
				}
			}
		})
	}
}

// A dynamic ref the producer read from an older manifest must be re-checked,
// not trusted: the send path re-resolves on every call.
func TestBotTemplateSendRejectsStaleDynamicRef(t *testing.T) {
	id := aireasoningprocess.TemplateID
	source := &stubAuthorizationSource{
		newSend: true,
		auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
			id: dynamicAuthorization("9.0.0", false, sendGrant()),
		},
	}
	catalog := newMustTestCatalog(t)
	catalog.authorization = source
	principal := resolvedPrincipal()
	stale := map[string]any{"id": string(id), "version": "8.0.0"}

	if _, err := catalog.requireSendableRef(context.Background(), principal, stale); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("stale version error = %v, want errBotTemplateRequestInvalid", err)
	}
	// The revoke lands between the manifest read and the send.
	source.auth[id] = dynamicAuthorization("9.0.0", false, cardtmpl.RuntimeGrant{})
	if _, err := catalog.requireSendableRef(context.Background(), principal, map[string]any{
		"id": string(id), "version": "9.0.0",
	}); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("revoked ref error = %v, want errBotTemplateRequestInvalid", err)
	}
}

// The advertised set is the union of the static policy and the Bot's other
// granted templates, with the static IDs resolved through the same matrix.
func TestBotTemplateManifestIncludesGrantedDynamicTemplates(t *testing.T) {
	extra := cardtmpl.ID("pilot.docs-card")
	source := &stubAuthorizationSource{
		newSend: true,
		auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
			aireasoningprocess.TemplateID: {},
		},
		granted: []cardtmpl.RuntimeAdvertisedTemplate{
			{ID: extra, Authorization: cardtmpl.RuntimeAuthorization{
				Version: "0.4.0",
				Artifact: cardtmpl.RuntimeArtifactMeta{
					ID: extra, Version: "0.4.0", Source: cardtmpl.RuntimeSourceDynamic, Owner: "docs",
				},
				Grant: sendGrant(),
			}},
			{ID: "pilot.blocked", Authorization: cardtmpl.RuntimeAuthorization{
				Version: "0.1.0",
				Artifact: cardtmpl.RuntimeArtifactMeta{
					Source: cardtmpl.RuntimeSourceDynamic, Blocked: true,
				},
				Grant: sendGrant(),
			}},
		},
	}
	catalog := newMustTestCatalog(t)
	catalog.authorization = source

	refs, err := catalog.advertisedSendRefs(context.Background(), resolvedPrincipal())
	if err != nil {
		t.Fatalf("advertisedSendRefs: %v", err)
	}
	found := map[cardtmpl.ID]string{}
	for _, ref := range refs {
		found[ref.ID] = ref.Version
	}
	if found[aireasoningprocess.TemplateID] != aireasoningprocess.TemplateVersionV3 {
		t.Fatalf("static policy ref missing from %+v", refs)
	}
	if found[extra] != "0.4.0" {
		t.Fatalf("granted dynamic ref missing from %+v", refs)
	}
	if _, blocked := found["pilot.blocked"]; blocked {
		t.Fatalf("blocked template was advertised: %+v", refs)
	}
}

// A list failure fails the manifest closed rather than quietly shrinking it to
// the static policy, which would look identical to "you have no grants".
func TestBotTemplateManifestFailsClosedOnListError(t *testing.T) {
	catalog := newMustTestCatalog(t)
	catalog.authorization = &stubAuthorizationSource{
		newSend: true,
		auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
			aireasoningprocess.TemplateID: {},
		},
		listErr: errors.New("db unavailable"),
	}
	if _, err := catalog.advertisedSendRefs(context.Background(), resolvedPrincipal()); !errors.Is(
		err, errBotTemplateRuntimeUnavailable) {
		t.Fatalf("list error = %v, want errBotTemplateRuntimeUnavailable", err)
	}
}

// The advertised policy must have exactly one send version per template ID:
// the activation pointer resolves one version per ID, so two would leave
// "which version does the pointer shadow" undefined.
func TestBotTemplateCatalogRejectsTwoSendVersionsOfOneID(t *testing.T) {
	_, err := newBotCardTemplateCatalogWithPolicy(testBotTemplateRegistry(t), botTemplatePolicy{
		AdvertisedSend: []botTemplateRef{
			{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV2},
			{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV3},
		},
		EditCompatible: []botTemplateRef{
			{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV2},
			{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV3},
		},
	})
	if err == nil {
		t.Fatal("policy advertising two versions of one template ID was accepted")
	}
}

func newGrantSpaceContext(t *testing.T, header string) *wkhttp.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("GET", "/v1/bot/card/profile", nil)
	if header != "" {
		ginCtx.Request.Header.Set("X-Space-ID", header)
	}
	return &wkhttp.Context{Context: ginCtx}
}

// The grant Space resolver refuses every ambiguity the dispatch-tagging
// resolver is allowed to paper over.
func TestResolveBotGrantSpaceIDRefusesEveryAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name    string
		header  string
		querier *fakeSpaceQuerier
		want    string
		wantOK  bool
	}{
		{
			name:    "single membership resolves",
			querier: &fakeSpaceQuerier{multiRows: map[string][]string{"bot-42": {"space-1"}}},
			want:    "space-1", wantOK: true,
		},
		{
			name:    "multi-Space with no hint refuses instead of picking the first",
			querier: &fakeSpaceQuerier{multiRows: map[string][]string{"bot-42": {"space-1", "space-2"}}},
			wantOK:  false,
		},
		{
			name:    "no membership refuses",
			querier: &fakeSpaceQuerier{defaultErr: dbr.ErrNotFound},
			wantOK:  false,
		},
		{
			name:    "a DB error refuses rather than falling back",
			querier: &fakeSpaceQuerier{defaultErr: errors.New("db unavailable")},
			wantOK:  false,
		},
		{
			name:   "an authorized header is honoured",
			header: "space-9",
			querier: &fakeSpaceQuerier{
				authDefault: true,
				multiRows:   map[string][]string{"bot-42": {"space-1", "space-2"}},
			},
			want: "space-9", wantOK: true,
		},
		{
			name:   "an unauthorized header refuses and never falls through",
			header: "space-victim",
			querier: &fakeSpaceQuerier{
				authDefault: false,
				multiRows:   map[string][]string{"bot-42": {"space-1"}},
			},
			wantOK: false,
		},
		{
			name:   "an unverifiable header refuses",
			header: "space-9",
			querier: &fakeSpaceQuerier{
				authErr:   errors.New("db unavailable"),
				multiRows: map[string][]string{"bot-42": {"space-1"}},
			},
			wantOK: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ba := newTestBotAPI(test.querier)
			got, ok := ba.resolveBotGrantSpaceID(newGrantSpaceContext(t, test.header), "bot-42")
			if ok != test.wantOK || got != test.want {
				t.Fatalf("resolveBotGrantSpaceID = (%q, %v), want (%q, %v)", got, ok, test.want, test.wantOK)
			}
		})
	}
}

// A card sent into a group is authorized against the group's Space, not the
// sending Bot's own — otherwise a grant in one tenant would deliver into
// another.
func TestBotSendCatalogPrincipalUsesTheTargetGroupSpace(t *testing.T) {
	// The target-authoritative resolution only runs when there is a dynamic
	// decision to make; with the gate closed it is deliberately skipped (see
	// TestBotCatalogPrincipalSkipsSpaceLookupWhenTheCatalogIsDark).
	previous := cardtmpl.DefaultAuthorizationSource()
	cardtmpl.SetDefaultAuthorizationSource(&stubAuthorizationSource{newSend: true})
	t.Cleanup(func() { cardtmpl.SetDefaultAuthorizationSource(previous) })

	querier := &fakeSpaceQuerier{
		multiRows:   map[string][]string{"bot-42": {"space-bot"}},
		groupSpaces: map[string]string{"group-1": "space-group"},
	}
	ba := newTestBotAPI(querier)
	ctx := newGrantSpaceContext(t, "")

	principal := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
	if !principal.SpaceResolved || principal.SpaceID != "space-group" {
		t.Fatalf("group principal = %+v, want the group's Space", principal)
	}

	thread := ba.botSendCatalogPrincipal(ctx, "bot-42",
		"group-1"+threadChannelIDSeparator+"t-9", common.ChannelTypeCommunityTopic.Uint8())
	if !thread.SpaceResolved || thread.SpaceID != "space-group" {
		t.Fatalf("thread principal = %+v, want the parent group's Space", thread)
	}

	// A group with no row, a group outside any Space and a malformed thread
	// channel all refuse rather than borrowing the Bot's own Space.
	for _, unresolved := range []botCatalogPrincipal{
		ba.botSendCatalogPrincipal(ctx, "bot-42", "group-missing", common.ChannelTypeGroup.Uint8()),
		ba.botSendCatalogPrincipal(ctx, "bot-42", "malformed", common.ChannelTypeCommunityTopic.Uint8()),
	} {
		if unresolved.SpaceResolved || unresolved.SpaceID != "" {
			t.Fatalf("principal = %+v, want unresolved", unresolved)
		}
	}
}

// Review follow-up (#6): with the dynamic catalog dark there is nothing to
// authorize against, so the request must not resolve a Space at all. Before
// this fix a gates-off deployment paid a space_member/app_bot query on every
// feature-detection poll — a database dependency the endpoint never had.
func TestBotCatalogPrincipalSkipsSpaceLookupWhenTheCatalogIsDark(t *testing.T) {
	previous := cardtmpl.DefaultAuthorizationSource()
	t.Cleanup(func() { cardtmpl.SetDefaultAuthorizationSource(previous) })

	querier := &fakeSpaceQuerier{multiRows: map[string][]string{"bot-42": {"space-1"}}}
	ba := newTestBotAPI(querier)
	ctx := newGrantSpaceContext(t, "")

	for _, source := range []cardtmpl.RuntimeAuthorizationSource{
		nil,
		&stubAuthorizationSource{newSend: false},
	} {
		querier.calls = nil
		cardtmpl.SetDefaultAuthorizationSource(source)

		principal := ba.botCatalogPrincipalFor(ctx, "bot-42")
		if principal.SpaceResolved || principal.SpaceID != "" {
			t.Fatalf("dark catalog resolved a Space: %+v", principal)
		}
		send := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
		if send.SpaceResolved || send.SpaceID != "" {
			t.Fatalf("dark catalog resolved a send Space: %+v", send)
		}
		if len(querier.calls) != 0 {
			t.Fatalf("dark catalog issued %v", querier.calls)
		}
	}

	cardtmpl.SetDefaultAuthorizationSource(&stubAuthorizationSource{newSend: true})
	if principal := ba.botCatalogPrincipalFor(ctx, "bot-42"); !principal.SpaceResolved {
		t.Fatalf("enabled catalog did not resolve a Space: %+v", principal)
	}
}

// Review follow-up (#2): a group send authorizes against the target's Space, so
// the stored marker must carry that Space too. Stamping env.SpaceID (which
// send.go only fills for DMs) left every group card with space_id:"" — and all
// three downstream D3 guards are written as `if SpaceID != ""`, so they became
// no-ops for exactly the frames that cross Space boundaries.
func TestProvenanceSpacePrefersTheAuthorizedSpaceOverTheRenderEnv(t *testing.T) {
	group := botCatalogPrincipal{BotID: "bot-42", SpaceID: "space-group", SpaceResolved: true}
	if got := provenanceSpaceID(group, cardtmpl.BuildEnv{}); got != "space-group" {
		t.Fatalf("group provenance Space = %q, want the authorized Space", got)
	}
	// A DM keeps stamping exactly what it stamped before the target-authoritative
	// resolver existed.
	dm := botCatalogPrincipal{BotID: "bot-42"}
	if got := provenanceSpaceID(dm, cardtmpl.BuildEnv{SpaceID: "space-dm"}); got != "space-dm" {
		t.Fatalf("DM provenance Space = %q, want the render env Space", got)
	}
	if got := provenanceSpaceID(dm, cardtmpl.BuildEnv{}); got != "" {
		t.Fatalf("unresolved provenance Space = %q, want empty", got)
	}
}
