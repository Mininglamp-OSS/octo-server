package bot_api

// PR-C D6 evidence: the shadow matrix, the strict Space resolver, and the fact
// that the capability manifest and the send path reach the same verdict.
//
// The matrix table below is the specification transcribed row for row. If a
// future change makes an ungranted or blocked dynamic pointer quietly fall back
// to the static card of the same ID, exactly one line here goes red.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
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
	// grantSpaceID, when set, models an *exact-Space* grant row: the stored
	// RuntimeGrant reaches only a query whose principal names that Space, and
	// every other principal sees the same authorization with no grant at all.
	//
	// Until this existed the stub answered s.auth[query.ID] for every caller and
	// ignored query.Principal entirely, so no test in this package could observe
	// two layers authorizing against different Spaces. That blind spot is how
	// the ref gate and the render came to disagree about which Space to read a
	// grant in and stayed that way across three review rounds.
	grantSpaceID string

	loads   int
	queries []cardtmpl.RuntimeAuthorizationQuery
}

func (s *stubAuthorizationSource) LoadAuthorization(
	_ context.Context,
	query cardtmpl.RuntimeAuthorizationQuery,
) (cardtmpl.RuntimeAuthorization, error) {
	s.loads++
	s.queries = append(s.queries, query)
	if s.err != nil {
		return cardtmpl.RuntimeAuthorization{}, s.err
	}
	auth := s.auth[query.ID]
	if s.grantSpaceID != "" && query.Principal.SpaceID != s.grantSpaceID {
		auth.Grant = cardtmpl.RuntimeGrant{}
	}
	return auth, nil
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

// globalSendGrant is the row a Space-less target can still be authorized by:
// the store has already applied exact-over-global precedence, so what reaches
// the decision is a grant whose scope is the empty sentinel.
func globalSendGrant() cardtmpl.RuntimeGrant {
	return cardtmpl.RuntimeGrant{
		Found: true, Scope: cardtmpl.RuntimeGrantScopeGlobal, Revision: 2, Discover: true, Send: true,
	}
}

func resolvedPrincipal() botCatalogPrincipal {
	return botCatalogPrincipal{BotID: "bot-42", SpaceID: "space-1", Space: botSpaceScoped}
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
			// The row this replaces asserted the opposite, and two reviewers
			// cited it as codifying the bug: an unavailable Space short-circuited
			// to static policy *before* the pointer was ever read, so a group-row
			// read that timed out sent the static card of an ID an operator had
			// already replaced. The pointer is a fact about the template, not
			// about the caller, so it is read either way; only the grant needs a
			// scope to be read in.
			name: "an unavailable Space does not revert a shadowed ID to static",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: dynamicAuthorization("9.0.0", false, sendGrant()),
			}},
			principal: botCatalogPrincipal{BotID: "bot-42"},
			want:      botTemplateRef{},
		},
		{
			// The other half of the same rule: withholding static is warranted
			// only because something shadowed it. With no activation row the
			// static policy is still the whole truth, unavailable Space or not,
			// and refusing here would take down cards nothing had replaced.
			name: "an unavailable Space still answers static when nothing shadows the ID",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: {},
			}},
			principal: botCatalogPrincipal{BotID: "bot-42"},
			want:      botTemplateRef{ID: id, Version: staticVersion},
		},
		{
			// A group that belongs to no Space is a determination, not a
			// failure. Only global grants can apply to it, and one does.
			name: "a Space-less target is authorized by a global grant",
			source: &stubAuthorizationSource{newSend: true, auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				id: dynamicAuthorization("9.0.0", false, globalSendGrant()),
			}},
			principal: botCatalogPrincipal{BotID: "bot-42", Space: botSpaceGlobalOnly},
			want:      botTemplateRef{ID: id, Version: "9.0.0"},
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
		name      string
		header    string
		querier   *fakeSpaceQuerier
		want      string
		wantState botCatalogSpaceState
	}{
		{
			name:    "single membership resolves",
			querier: &fakeSpaceQuerier{multiRows: map[string][]string{"bot-42": {"space-1"}}},
			want:    "space-1", wantState: botSpaceScoped,
		},
		{
			name:      "multi-Space with no hint refuses instead of picking the first",
			querier:   &fakeSpaceQuerier{multiRows: map[string][]string{"bot-42": {"space-1", "space-2"}}},
			wantState: botSpaceUnavailable,
		},
		{
			// The previous round read this sentinel as "this Bot belongs to no
			// Space" and answered GlobalOnly. It cannot mean that: platform App
			// Bots are authorized for every active Space and never get a
			// space_member row, so for them "no membership" and "no Space" are
			// different facts, and answering the second let an X-Space-ID header
			// choose between a scope that sees an exact revoke tombstone and one
			// that does not.
			name:      "no membership is something this resolver could not establish",
			querier:   &fakeSpaceQuerier{defaultErr: dbr.ErrNotFound},
			wantState: botSpaceUnavailable,
		},
		{
			name:      "a DB error refuses rather than falling back",
			querier:   &fakeSpaceQuerier{defaultErr: errors.New("db unavailable")},
			wantState: botSpaceUnavailable,
		},
		{
			name:   "an authorized header is honoured",
			header: "space-9",
			querier: &fakeSpaceQuerier{
				authDefault: true,
				multiRows:   map[string][]string{"bot-42": {"space-1", "space-2"}},
			},
			want: "space-9", wantState: botSpaceScoped,
		},
		{
			name:   "an unauthorized header refuses and never falls through",
			header: "space-victim",
			querier: &fakeSpaceQuerier{
				authDefault: false,
				multiRows:   map[string][]string{"bot-42": {"space-1"}},
			},
			wantState: botSpaceUnavailable,
		},
		{
			name:   "an unverifiable header refuses",
			header: "space-9",
			querier: &fakeSpaceQuerier{
				authErr:   errors.New("db unavailable"),
				multiRows: map[string][]string{"bot-42": {"space-1"}},
			},
			wantState: botSpaceUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ba := newTestBotAPI(test.querier)
			got, state := ba.resolveBotGrantSpaceID(newGrantSpaceContext(t, test.header), "bot-42")
			if state != test.wantState || got != test.want {
				t.Fatalf("resolveBotGrantSpaceID = (%q, %v), want (%q, %v)", got, state, test.want, test.wantState)
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
	ba := newTestBotAPIWithCatalog(t, querier, &stubAuthorizationSource{newSend: true})
	ctx := newGrantSpaceContext(t, "")

	principal := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
	if principal.Space != botSpaceScoped || principal.SpaceID != "space-group" {
		t.Fatalf("group principal = %+v, want the group's Space", principal)
	}

	thread := ba.botSendCatalogPrincipal(ctx, "bot-42",
		"group-1"+threadChannelIDSeparator+"t-9", common.ChannelTypeCommunityTopic.Uint8())
	if thread.Space != botSpaceScoped || thread.SpaceID != "space-group" {
		t.Fatalf("thread principal = %+v, want the parent group's Space", thread)
	}

	// A group with no row, a group outside any Space and a malformed thread
	// channel all refuse rather than borrowing the Bot's own Space.
	for _, unresolved := range []botCatalogPrincipal{
		ba.botSendCatalogPrincipal(ctx, "bot-42", "group-missing", common.ChannelTypeGroup.Uint8()),
		ba.botSendCatalogPrincipal(ctx, "bot-42", "malformed", common.ChannelTypeCommunityTopic.Uint8()),
	} {
		if unresolved.Space != botSpaceUnavailable || unresolved.SpaceID != "" {
			t.Fatalf("principal = %+v, want unresolved", unresolved)
		}
	}
}

// Review follow-up (#6): with the dynamic catalog dark there is nothing to
// authorize against, so the request must not resolve a Space at all. Before
// this fix a gates-off deployment paid a space_member/app_bot query on every
// feature-detection poll — a database dependency the endpoint never had.
func TestBotCatalogPrincipalSkipsSpaceLookupWhenTheCatalogIsDark(t *testing.T) {
	querier := &fakeSpaceQuerier{multiRows: map[string][]string{"bot-42": {"space-1"}}}
	ctx := newGrantSpaceContext(t, "")

	for _, source := range []cardtmpl.RuntimeAuthorizationSource{
		nil,
		&stubAuthorizationSource{newSend: false},
	} {
		querier.calls = nil
		ba := newTestBotAPIWithCatalog(t, querier, source)

		principal := ba.botCatalogPrincipalFor(ctx, "bot-42")
		if principal.Space != botSpaceUnavailable || principal.SpaceID != "" {
			t.Fatalf("dark catalog resolved a Space: %+v", principal)
		}
		send := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
		if send.Space != botSpaceUnavailable || send.SpaceID != "" {
			t.Fatalf("dark catalog resolved a send Space: %+v", send)
		}
		if len(querier.calls) != 0 {
			t.Fatalf("dark catalog issued %v", querier.calls)
		}
	}

	enabled := newTestBotAPIWithCatalog(t, querier, &stubAuthorizationSource{newSend: true})
	if principal := enabled.botCatalogPrincipalFor(ctx, "bot-42"); principal.Space != botSpaceScoped {
		t.Fatalf("enabled catalog did not resolve a Space: %+v", principal)
	}
}

// newTestBotAPIWithCatalog wires a Bot API whose template catalog carries the
// given resolver. The gate and the decision both read the catalog's accessor,
// so a test that set only the process global would be exercising a path
// production never takes.
// grantEnforcingCatalog models the one thing cardtmpl.RuntimeCatalog.Render
// does that the static test registry does not: it re-resolves the caller's
// grant from request.Access, the way resolveDynamic does before it will render a
// dynamic version. The static registry renders whatever it is given, so without
// this model the render half of an authorization is invisible to this package —
// and an invisible half is precisely what let the gate and the render disagree.
type grantEnforcingCatalog struct {
	cardtmpl.Catalog
	source     *stubAuthorizationSource
	lastRender cardtmpl.CatalogRenderRequest
}

func (c *grantEnforcingCatalog) Render(
	ctx context.Context,
	request cardtmpl.CatalogRenderRequest,
) (map[string]any, error) {
	c.lastRender = request
	auth, err := c.source.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: request.ID, Version: request.Version, Principal: request.Access.Principal,
	})
	if err != nil {
		return nil, err
	}
	if !auth.Grant.Allows(request.Access.Purpose) {
		return nil, cardtmpl.ErrRuntimeCatalogNotAuthorized
	}
	return map[string]any{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"card":         map[string]any{"type": "AdaptiveCard", "body": []any{}},
	}, nil
}

// PR-C review round 4 — Jerry-Xin "Critical 2" and yujiawei P1-1, found
// independently by both.
//
// requireSendableRef resolves the grant against the *target's* Space, which for
// a group comes from the authoritative group row. renderPayload then hands the
// ref to cardtmpl.Render, which re-resolves the grant from request.Access — and
// used to compose that Access from env.SpaceID, which send.go populates for DMs
// only. So a Bot holding an exact grant for the target's Space passed the gate
// and was then refused by the render, and per-Space grants — the primary Bot
// grant shape, and the entire reason resolveBotGrantSpaceID exists — were inert
// for every group and thread target.
//
// Reverting renderPayload to botCatalogAccess(purpose, botID, env.SpaceID)
// fails this test with ErrRuntimeCatalogNotAuthorized.
func TestGroupSendAuthorizesTheRenderWithTheSameSpaceAsTheGate(t *testing.T) {
	const groupSpace = "space-group"
	const dynamicVersion = "9.0.0"
	id := aireasoningprocess.TemplateID

	source := &stubAuthorizationSource{
		newSend: true,
		auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
			id: dynamicAuthorization(dynamicVersion, false, sendGrant()),
		},
		// An exact row for the group's Space and no global row: the shape
		// TestGrantsDoNotLeakBetweenBotsRealMySQL exists to protect, and the
		// only one that can express a per-tenant grant.
		grantSpaceID: groupSpace,
	}
	querier := &fakeSpaceQuerier{
		// Deliberately different from the group's Space, so a render that fell
		// back to the *sender's* membership would also be caught.
		multiRows:   map[string][]string{"bot-42": {"space-bot"}},
		groupSpaces: map[string]string{"group-1": groupSpace},
	}
	ba := newTestBotAPIWithCatalog(t, querier, source)
	rendering := &grantEnforcingCatalog{source: source}
	ba.cardTemplates.catalog = rendering

	principal := ba.botSendCatalogPrincipal(newGrantSpaceContext(t, ""), "bot-42",
		"group-1", common.ChannelTypeGroup.Uint8())
	if principal.Space != botSpaceScoped || principal.SpaceID != groupSpace {
		t.Fatalf("gate principal = %+v, want the group's Space %q", principal, groupSpace)
	}

	// BuildEnv carries no Space, exactly as send.go leaves it for a group
	// target — that emptiness is what the render used to authorize with.
	rendered, err := ba.cardTemplates.RenderPayloadForPrincipal(context.Background(), principal,
		map[string]any{
			"type":         cardmsg.InteractiveCard.Int(),
			"template_ref": map[string]any{"id": string(id), "version": dynamicVersion},
			"state":        "thinking",
			"data":         map[string]any{"title": "t"},
		}, cardtmpl.BuildEnv{})
	if err != nil {
		t.Fatalf("group dynamic send refused: %v", err)
	}

	// The ground truth: the Space the render authorized against is the one the
	// gate decided on, not env.SpaceID and not the sender's own membership.
	if got := rendering.lastRender.Access.Principal.SpaceID; got != groupSpace {
		t.Fatalf("render authorized against Space %q, gate used %q", got, groupSpace)
	}
	if got := rendering.lastRender.Access.Principal.ID; got != "bot-42" {
		t.Fatalf("render principal ID = %q, want the authenticated bot", got)
	}
	if got := rendering.lastRender.Access.Purpose; got != cardtmpl.CatalogPurposeNewSend {
		t.Fatalf("render purpose = %v, want new_send", got)
	}
	// The marker records that same authorized Space, so the D3 guards
	// downstream compare against what the send was actually authorized for.
	provenance, _ := rendered[cardmsg.CatalogProvenanceKey].(map[string]any)
	if got, _ := provenance["space_id"].(string); got != groupSpace {
		t.Fatalf("provenance space = %q, want %q", got, groupSpace)
	}
}

// The edit half of the same seam: resolveEditRef pins the stored version and
// authorizes it against the principal's Space, so the render that follows must
// use that Space too or a per-Space `edit` grant is just as inert as the send
// one was.
func TestGroupEditAuthorizesTheRenderWithTheSameSpaceAsTheGate(t *testing.T) {
	const groupSpace = "space-group"
	const dynamicVersion = "9.0.0"
	id := aireasoningprocess.TemplateID

	source := &stubAuthorizationSource{
		newSend: true,
		auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
			id: dynamicAuthorization(dynamicVersion, false, cardtmpl.RuntimeGrant{
				Found: true, Scope: cardtmpl.RuntimeGrantScopeExact,
				Revision: 4, Discover: true, Send: true, Edit: true,
			}),
		},
		grantSpaceID: groupSpace,
	}
	catalog := newMustTestCatalog(t)
	catalog.authorization = source
	rendering := &grantEnforcingCatalog{source: source}
	catalog.catalog = rendering

	principal := botCatalogPrincipal{BotID: "bot-42", SpaceID: groupSpace, Space: botSpaceScoped}
	if _, err := catalog.RenderEditPayloadForPrincipal(context.Background(), principal,
		map[string]any{
			"type":         cardmsg.InteractiveCard.Int(),
			"template_ref": map[string]any{"id": string(id), "version": dynamicVersion},
			"state":        "thinking",
			"data":         map[string]any{"title": "t"},
		}, cardtmpl.BuildEnv{}, groupSpace); err != nil {
		t.Fatalf("group dynamic edit refused: %v", err)
	}
	if got := rendering.lastRender.Access.Principal.SpaceID; got != groupSpace {
		t.Fatalf("edit render authorized against Space %q, want %q", got, groupSpace)
	}
	if got := rendering.lastRender.Access.Purpose; got != cardtmpl.CatalogPurposeHistoricalEdit {
		t.Fatalf("edit render purpose = %v, want historical_edit", got)
	}
}

func newTestBotAPIWithCatalog(
	t *testing.T,
	querier botSpaceQuerier,
	source cardtmpl.RuntimeAuthorizationSource,
) *BotAPI {
	t.Helper()
	ba := newTestBotAPI(querier)
	catalog := newMustTestCatalog(t)
	catalog.authorization = source
	ba.cardTemplates = catalog
	return ba
}

// Review follow-up (#2): a group send authorizes against the target's Space, so
// the stored marker must carry that Space too. Stamping env.SpaceID (which
// send.go only fills for DMs) left every group card with space_id:"" — and all
// three downstream D3 guards are written as `if SpaceID != ""`, so they became
// no-ops for exactly the frames that cross Space boundaries.
func TestProvenanceSpacePrefersTheAuthorizedSpaceOverTheRenderEnv(t *testing.T) {
	group := botCatalogPrincipal{BotID: "bot-42", SpaceID: "space-group", Space: botSpaceScoped}
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

// Review follow-up (#1/#3): a group card must stay editable after PR-C started
// recording the target's Space in the marker.
//
// The send stamps the group's Space; a group envelope has no top-level
// space_id, so an edit guard that compared the marker against the envelope
// would refuse — and the round trip below is exactly the coverage whose absence
// let that regression through. The edit must also re-stamp the same Space
// rather than blanking it, since every downstream guard is written as
// `if SpaceID != ""`.
func TestGroupCardStaysEditableAndKeepsItsAuthorizedSpace(t *testing.T) {
	ba := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows:   map[string][]string{"bot-42": {"space-bot"}},
		groupSpaces: map[string]string{"group-1": "space-group"},
	}, &stubAuthorizationSource{newSend: true})
	ctx := newGrantSpaceContext(t, "")
	catalog := ba.cardTemplates

	// Send into a group: the marker records the group's Space, not the Bot's.
	sendPrincipal := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
	if sendPrincipal.SpaceID != "space-group" {
		t.Fatalf("send principal = %+v, want the group's Space", sendPrincipal)
	}
	env := cardtmpl.BuildEnv{Lang: "zh-CN"} // group sends leave env.SpaceID empty
	sent, err := catalog.RenderPayloadForPrincipal(context.Background(), sendPrincipal,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any), env)
	if err != nil {
		t.Fatalf("group send: %v", err)
	}
	envelope, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	markers, err := cardmsg.CatalogFrameMarkers(envelope)
	if err != nil {
		t.Fatalf("stored markers: %v", err)
	}
	if markers.Provenance.SpaceID != "space-group" {
		t.Fatalf("stored provenance Space = %q, want the authorized group Space",
			markers.Provenance.SpaceID)
	}

	// Editing the same card resolves the same Space from the same target, so
	// the guard passes even though the envelope carries no space_id.
	editPrincipal := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
	ref := botTemplateRef{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV3}
	if err := requireEffectiveCardTemplate(envelope, ref, "bot-42", editProvenanceSpaceCheck(true, editPrincipal)); err != nil {
		t.Fatalf("group card was refused by its own edit guard: %v", err)
	}

	// And the re-stamped marker keeps the Space rather than blanking it.
	edited, err := catalog.RenderEditPayloadForPrincipal(context.Background(), editPrincipal,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any), env,
		storedProvenanceSpaceID(envelope))
	if err != nil {
		t.Fatalf("group edit: %v", err)
	}
	editedEnvelope, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	editedMarkers, err := cardmsg.CatalogFrameMarkers(editedEnvelope)
	if err != nil {
		t.Fatalf("edited markers: %v", err)
	}
	if editedMarkers.Provenance.SpaceID != "space-group" {
		t.Fatalf("edit erased the authorized Space: %q", editedMarkers.Provenance.SpaceID)
	}

	// A frame replayed against a target in another Space is still refused.
	if err := requireEffectiveCardTemplate(envelope, ref, "bot-42", verifiableEditSpace("space-elsewhere")); err == nil {
		t.Fatal("a frame moved to another Space was accepted")
	}
}

// PR-C review round 5 — yujiawei P1-2.
//
// requireEffectiveCardTemplate accepts the envelope's own top-level space_id as
// a witness for the stored marker. That value is server-authored and pinned by
// Snapshot, so it is a trustworthy record of what the send wrote — but it is the
// frame speaking about itself. Matching it proves the frame is internally
// consistent; it proves nothing about which Space the grant behind *this* edit
// was read in, and resolveEditRef reads that grant from principal.SpaceID, which
// for a DM comes from the current request's X-Space-ID header.
//
// So while the envelope could substitute for the real comparison, a Bot in
// Spaces A and B could send a card under A's grant, lose A's edit permission,
// then rewrite that same card by presenting X-Space-ID: space-B — case 1 failed
// correctly, and the frame's own "space-A" waved it through. The witness now
// applies only where no Space was established at all, which is the multi-Space
// DM lockout it was added for.
func TestEditRefusesAFrameFromAnotherSpaceEvenWhenTheEnvelopeCorroboratesIt(t *testing.T) {
	catalog := newMustTestCatalog(t)
	catalog.authorization = &stubAuthorizationSource{newSend: false}

	principal := botCatalogPrincipal{BotID: "bot-42", SpaceID: "space-a", Space: botSpaceScoped}
	sent, err := catalog.RenderPayloadForPrincipal(context.Background(), principal,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any),
		cardtmpl.BuildEnv{SpaceID: "space-a"})
	if err != nil {
		t.Fatalf("DM send: %v", err)
	}
	// What the DM send path stamps on the envelope after rendering.
	sent["space_id"] = "space-a"
	envelope, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	ref := botTemplateRef{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV3}

	// The grant behind this edit was read in space-b. The frame's own claim of
	// space-a must not stand in for the comparison that just failed.
	if err := requireEffectiveCardTemplate(envelope, ref, "bot-42",
		verifiableEditSpace("space-b")); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("cross-Space edit error = %v, want errBotTemplateRequestInvalid", err)
	}
	// Authorized in the frame's own Space: accepted, as before.
	if err := requireEffectiveCardTemplate(envelope, ref, "bot-42",
		verifiableEditSpace("space-a")); err != nil {
		t.Fatalf("same-Space edit refused: %v", err)
	}
	// And the case the witness exists for is preserved: a multi-Space Bot with
	// no header establishes no Space, so the envelope is all there is.
	if err := requireEffectiveCardTemplate(envelope, ref, "bot-42",
		editSpaceCheck{unavailable: true}); err != nil {
		t.Fatalf("multi-Space DM edit lost its envelope witness: %v", err)
	}
}

// PR-C review round 5 — yujiawei P2-5, and the group half of Octo-Q's C4.
//
// space.status = 1 is load-bearing in every other Space resolver in
// modules/bot_api/db.go — querySpaceIDsByRobotID, both branches of
// isBotSpaceAuthorized, and isUserSpaceMember all require it. queryGroupSpaceID
// did not, so a grant scoped to a Space an operator had disabled still
// authorized sends into that Space's groups, while the same bot's membership,
// an X-Space-ID naming it, and a DM peer inside it were all refused.
//
// A disabled Space is not "no Space" either: answering botSpaceGlobalOnly would
// let a global grant deliver where the scoped one had just been switched off.
func TestAGroupInADisabledSpaceHasNoReadableGrantScope(t *testing.T) {
	querier := &fakeSpaceQuerier{
		multiRows: map[string][]string{"bot-42": {"space-bot"}},
		groupSpaces: map[string]string{
			"group-live":      "space-live",
			"group-disabled":  "space-disabled",
			"group-spaceless": "",
		},
		inactiveSpaces: map[string]bool{"space-disabled": true},
	}
	ba := newTestBotAPIWithCatalog(t, querier, &stubAuthorizationSource{newSend: true})
	ctx := newGrantSpaceContext(t, "")
	group := common.ChannelTypeGroup.Uint8()

	if got := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-live", group); got.Space != botSpaceScoped ||
		got.SpaceID != "space-live" {
		t.Fatalf("active group principal = %+v, want scoped to space-live", got)
	}
	if got := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-disabled", group); got.Space != botSpaceUnavailable ||
		got.SpaceID != "" {
		t.Fatalf("disabled-Space group principal = %+v, want unavailable with no Space", got)
	}
	// The determination this must stay distinct from.
	if got := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-spaceless", group); got.Space != botSpaceGlobalOnly {
		t.Fatalf("Space-less group principal = %+v, want global-only", got)
	}
}

// D6 requires *both* DM principals to be valid members of the Space a dynamic
// send is authorized against. Checking only the Bot would let its own Space's
// grant deliver a card to somebody outside that Space — a cross-tenant delivery
// wearing a DM's clothes.
func TestDMSendRequiresThePeerToBeInTheAuthorizedSpace(t *testing.T) {
	dm := common.ChannelTypePerson.Uint8()
	for _, test := range []struct {
		name      string
		querier   *fakeSpaceQuerier
		peer      string
		wantSpace string
		wantState botCatalogSpaceState
	}{
		{
			name: "peer in the Bot's Space authorizes the send",
			querier: &fakeSpaceQuerier{
				multiRows:    map[string][]string{"bot-42": {"space-1"}},
				memberSpaces: map[string]map[string]bool{"user-in": {"space-1": true}},
			},
			peer: "user-in", wantSpace: "space-1", wantState: botSpaceScoped,
		},
		{
			name: "peer outside the Space withholds the dynamic overlay",
			querier: &fakeSpaceQuerier{
				multiRows:    map[string][]string{"bot-42": {"space-1"}},
				memberSpaces: map[string]map[string]bool{"user-in": {"space-1": true}},
			},
			peer: "user-elsewhere", wantState: botSpaceUnavailable,
		},
		{
			name: "a peer membership lookup failure refuses rather than assumes",
			querier: &fakeSpaceQuerier{
				multiRows:      map[string][]string{"bot-42": {"space-1"}},
				memberSpaceErr: errors.New("db unavailable"),
			},
			peer: "user-in", wantState: botSpaceUnavailable,
		},
		{
			name: "a self-addressed channel is not a peer",
			querier: &fakeSpaceQuerier{
				multiRows:    map[string][]string{"bot-42": {"space-1"}},
				memberSpaces: map[string]map[string]bool{"bot-42": {"space-1": true}},
			},
			peer: "bot-42", wantState: botSpaceUnavailable,
		},
		{
			// The cell no case in this table could previously construct: every
			// other one seeds a membership, so the resolver never returned
			// dbr.ErrNotFound and the DM × "no membership" combination went
			// unexercised.
			//
			// It is the platform App Bot population — visible in every active
			// Space, never given a space_member row (db.go isBotSpaceAuthorized
			// branch 2). Reading that absence as "this Bot has no Space" made the
			// principal global-scoped, which skips requireDMPeerInSpace entirely
			// and reads the grant with scope_space_id IN ('',''), where an exact
			// revoke tombstone for the peer's Space is not even visible. Sending
			// X-Space-ID would have been refused by that tombstone, so omitting
			// one header selected the more permissive of two answers.
			name: "a Bot with no membership never becomes global-scoped on a DM",
			querier: &fakeSpaceQuerier{
				memberSpaces: map[string]map[string]bool{"user-in": {"space-1": true}},
			},
			peer: "user-in", wantState: botSpaceUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ba := newTestBotAPIWithCatalog(t, test.querier, &stubAuthorizationSource{newSend: true})
			got := ba.botSendCatalogPrincipal(newGrantSpaceContext(t, ""), "bot-42", test.peer, dm)
			if got.Space != test.wantState || got.SpaceID != test.wantSpace {
				t.Fatalf("DM principal = %+v, want space=%q state=%v",
					got, test.wantSpace, test.wantState)
			}
		})
	}
}

// With the catalog dark the peer is never consulted either — the whole point of
// the gate is that a gates-off deployment issues no new queries.
func TestDMPeerCheckIsSkippedWhenTheCatalogIsDark(t *testing.T) {
	querier := &fakeSpaceQuerier{multiRows: map[string][]string{"bot-42": {"space-1"}}}
	ba := newTestBotAPIWithCatalog(t, querier, &stubAuthorizationSource{newSend: false})
	if got := ba.botSendCatalogPrincipal(newGrantSpaceContext(t, ""), "bot-42",
		"user-in", common.ChannelTypePerson.Uint8()); got.Space == botSpaceScoped {
		t.Fatalf("dark catalog resolved a DM Space: %+v", got)
	}
	if len(querier.calls) != 0 {
		t.Fatalf("dark catalog queried %v", querier.calls)
	}
}

// The Space guard has to tell "we know of no Space" apart from "we could not
// find out". Conflating them is what made every group card permanently
// uneditable the first time; this covers the general form rather than the one
// path that was reported.
func TestEditSpaceCheckSeparatesUnknownFromEmpty(t *testing.T) {
	resolved := botCatalogPrincipal{BotID: "bot-1", SpaceID: "space-a", Space: botSpaceScoped}
	unresolved := botCatalogPrincipal{BotID: "bot-1"}

	if got := editProvenanceSpaceCheck(true, resolved); !got.verifiable || got.unavailable ||
		got.spaceID != "space-a" {
		t.Fatalf("a resolved Space produced %+v", got)
	}
	if got := editProvenanceSpaceCheck(true, unresolved); got.verifiable || !got.unavailable {
		t.Fatalf("an unresolvable Space under a live gate produced %+v", got)
	}
	// The gate being closed is a configuration state, not an outage: nothing
	// ever asked for a Space, so there is nothing to be unavailable.
	if got := editProvenanceSpaceCheck(false, unresolved); got.verifiable || got.unavailable {
		t.Fatalf("a dark catalog produced %+v", got)
	}
	if got := editProvenanceSpaceCheck(false, resolved); got.verifiable || got.unavailable {
		t.Fatalf("a dark catalog with a stale principal produced %+v", got)
	}
}

// The regression this closes: a group card sent while the dynamic catalog was
// live records the group's Space, and rolling the gate back must not make that
// card uneditable forever. A transient lookup failure must not make it
// uneditable either — but it must not silently pass the check, so it reports a
// retryable outage instead of a permanent client error.
func TestGroupCardStaysEditableAcrossAGateRollback(t *testing.T) {
	ba := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows:   map[string][]string{"bot-42": {"space-bot"}},
		groupSpaces: map[string]string{"group-1": "space-group"},
	}, &stubAuthorizationSource{newSend: true})
	ctx := newGrantSpaceContext(t, "")
	catalog := ba.cardTemplates

	principal := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
	if principal.Space != botSpaceScoped || principal.SpaceID != "space-group" {
		t.Fatalf("send principal = %+v", principal)
	}
	sent, err := catalog.RenderPayloadForPrincipal(context.Background(), principal,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any),
		cardtmpl.BuildEnv{Lang: "zh-CN"})
	if err != nil {
		t.Fatalf("group send: %v", err)
	}
	envelope, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	ref := botTemplateRef{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV3}

	// The gate rolls back. The card still names space-group, and the edit path
	// can no longer resolve any Space at all.
	dark := editProvenanceSpaceCheck(false, botCatalogPrincipal{BotID: "bot-42"})
	if err := requireEffectiveCardTemplate(envelope, ref, "bot-42", dark); err != nil {
		t.Fatalf("a gate rollback made an existing group card uneditable: %v", err)
	}

	// The gate is live but the group row will not load. That is an outage, and
	// it must be reported as one — never as "your request is malformed".
	blind := editProvenanceSpaceCheck(true, botCatalogPrincipal{BotID: "bot-42"})
	err = requireEffectiveCardTemplate(envelope, ref, "bot-42", blind)
	if !errors.Is(err, errBotTemplateRuntimeUnavailable) {
		t.Fatalf("an unresolvable Space error = %v, want errBotTemplateRuntimeUnavailable", err)
	}
	// And it is still not an invalid-request error, which is what the handler
	// branches on to decide between 500 and 400.
	if errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatal("an outage was also classified as a client error")
	}

	// A genuinely different Space is still refused, permanently.
	if err := requireEffectiveCardTemplate(envelope, ref, "bot-42",
		verifiableEditSpace("space-elsewhere")); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("cross-Space edit error = %v, want errBotTemplateRequestInvalid", err)
	}
}

// batchAuthorizationSource is stubAuthorizationSource plus the optional batch
// interface, so the manifest takes the fast path. It answers from the same map,
// which is the point: the two paths must not be able to disagree.
type batchAuthorizationSource struct {
	stubAuthorizationSource
	batches    int
	batchErr   error
	batchedIDs [][]cardtmpl.ID
}

func (s *batchAuthorizationSource) LoadAuthorizations(
	_ context.Context, ids []cardtmpl.ID, _ cardtmpl.CatalogPrincipal,
) (map[cardtmpl.ID]cardtmpl.RuntimeAuthorization, error) {
	s.batches++
	s.batchedIDs = append(s.batchedIDs, append([]cardtmpl.ID(nil), ids...))
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	out := make(map[cardtmpl.ID]cardtmpl.RuntimeAuthorization, len(ids))
	for _, id := range ids {
		out[id] = s.auth[id]
	}
	return out, nil
}

// The manifest resolves the whole static policy list in one round trip, and
// reaches the same conclusion it reached one query at a time. The second half
// is what matters: a batch that saved round trips by dropping the "activated
// but ungranted" row would silently re-advertise the static version of a
// template an operator had already moved to a dynamic one.
func TestAdvertisedSendRefsBatchesAndAgreesWithThePerIDPath(t *testing.T) {
	authorizations := map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
		// Activated dynamically, but this Bot holds no grant: the ID becomes
		// unsendable and must NOT fall back to its static version.
		aireasoningprocess.TemplateID: {
			Version: "0.4.0-dyn",
			Artifact: cardtmpl.RuntimeArtifactMeta{
				ID: aireasoningprocess.TemplateID, Version: "0.4.0-dyn",
				Source: cardtmpl.RuntimeSourceDynamic,
			},
		},
	}
	batched := &batchAuthorizationSource{
		stubAuthorizationSource: stubAuthorizationSource{newSend: true, auth: authorizations},
	}
	perID := &stubAuthorizationSource{newSend: true, auth: authorizations}

	principal := botCatalogPrincipal{BotID: "bot-1", SpaceID: "space-1", Space: botSpaceScoped}
	batchedCatalog := newMustTestCatalog(t)
	batchedCatalog.authorization = batched
	perIDCatalog := newMustTestCatalog(t)
	perIDCatalog.authorization = perID

	fromBatch, err := batchedCatalog.advertisedSendRefs(context.Background(), principal)
	if err != nil {
		t.Fatalf("batched advertisedSendRefs: %v", err)
	}
	fromLoop, err := perIDCatalog.advertisedSendRefs(context.Background(), principal)
	if err != nil {
		t.Fatalf("per-ID advertisedSendRefs: %v", err)
	}
	if len(fromBatch) != len(fromLoop) {
		t.Fatalf("batched = %+v, per-ID = %+v", fromBatch, fromLoop)
	}
	for i := range fromBatch {
		if fromBatch[i] != fromLoop[i] {
			t.Fatalf("batched = %+v, per-ID = %+v", fromBatch, fromLoop)
		}
	}
	for _, ref := range fromBatch {
		if ref.ID == aireasoningprocess.TemplateID {
			t.Fatalf("an activated-but-ungranted template was still advertised: %+v", ref)
		}
	}

	// One batch, and no per-ID reads left over for the policy list.
	if batched.batches != 1 {
		t.Fatalf("batch calls = %d, want exactly one", batched.batches)
	}
	if batched.loads != 0 {
		t.Fatalf("the batched source still issued %d per-ID reads", batched.loads)
	}
	if len(batched.batchedIDs) != 1 || len(batched.batchedIDs[0]) != len(perIDCatalog.staticSendIDs) {
		t.Fatalf("batched IDs = %+v, want the whole policy list", batched.batchedIDs)
	}
	if perID.loads != len(perIDCatalog.staticSendIDs) {
		t.Fatalf("per-ID reads = %d, want one per policy ID", perID.loads)
	}
}

// A batch read that fails is an outage, and the manifest must fail closed
// rather than quietly degrading to the static answer — advertising a version
// that may have been shadowed is exactly the D6 row this whole path exists for.
func TestAdvertisedSendRefsFailsClosedWhenTheBatchReadFails(t *testing.T) {
	batched := &batchAuthorizationSource{
		stubAuthorizationSource: stubAuthorizationSource{newSend: true},
		batchErr:                errors.New("db unavailable"),
	}
	catalog := newMustTestCatalog(t)
	catalog.authorization = batched
	_, err := catalog.advertisedSendRefs(context.Background(),
		botCatalogPrincipal{BotID: "bot-1", SpaceID: "space-1", Space: botSpaceScoped})
	if !errors.Is(err, errBotTemplateRuntimeUnavailable) {
		t.Fatalf("err = %v, want errBotTemplateRuntimeUnavailable", err)
	}
}

// A Bot that belongs to two Spaces can never satisfy the strict grant resolver
// — the ambiguity is a permanent property of that Bot, not a blip. Its DM sends
// still stamp a marker, from the forgiving resolver that also fills the
// envelope's space_id. If the edit guard only ever consulted the strict
// resolver, every one of those cards would be uneditable forever.
func TestDMCardStaysEditableWhenTheStrictResolverCannotAgree(t *testing.T) {
	ba := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows: map[string][]string{"bot-42": {"space-one", "space-two"}},
	}, &stubAuthorizationSource{newSend: true})
	ctx := newGrantSpaceContext(t, "")

	// Ambiguous membership: the grant principal refuses to resolve.
	principal := ba.botSendCatalogPrincipal(ctx, "bot-42", "peer-1", common.ChannelTypePerson.Uint8())
	if principal.Space == botSpaceScoped {
		t.Fatalf("a two-Space Bot resolved a grant Space: %+v", principal)
	}
	// The DM send path still injects the forgiving resolver's Space, and the
	// marker is stamped from the same value.
	env := cardtmpl.BuildEnv{Lang: "zh-CN", SpaceID: "space-one"}
	sent, err := ba.cardTemplates.RenderPayloadForPrincipal(context.Background(), principal,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any), env)
	if err != nil {
		t.Fatalf("dm send: %v", err)
	}
	sent = ba.enrichBotPayloadWithResolvedSpaceID("bot-42", sent, "space-one")
	envelope, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	markers, err := cardmsg.CatalogFrameMarkers(envelope)
	if err != nil {
		t.Fatalf("stored markers: %v", err)
	}
	if markers.Provenance.SpaceID != "space-one" {
		t.Fatalf("dm marker Space = %q", markers.Provenance.SpaceID)
	}

	ref := botTemplateRef{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV3}
	check := editProvenanceSpaceCheck(true, principal)
	if !check.unavailable {
		t.Fatalf("expected the strict resolver to report unavailable, got %+v", check)
	}
	if err := requireEffectiveCardTemplate(envelope, ref, "bot-42", check); err != nil {
		t.Fatalf("a multi-Space Bot was locked out of editing its own DM card: %v", err)
	}

	// A marker naming a Space the envelope does not corroborate is still
	// refused — the envelope is a witness, not a bypass.
	forged := map[string]any{}
	if err := json.Unmarshal(envelope, &forged); err != nil {
		t.Fatal(err)
	}
	forged["space_id"] = "space-elsewhere"
	forgedEnvelope, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireEffectiveCardTemplate(forgedEnvelope, ref, "bot-42", check); err == nil {
		t.Fatal("a marker contradicting both witnesses was accepted")
	}
}

// An edit preserves the Space the send recorded. Re-deriving it means the answer
// depends on whether the resolver happens to work at edit time, so a gate
// rollback would rewrite the card with an empty Space and quietly disable every
// downstream `if Provenance.SpaceID != ""` guard for it.
func TestEditPreservesTheStoredProvenanceSpaceEvenWhenTheGateIsDark(t *testing.T) {
	ba := newTestBotAPIWithCatalog(t, &fakeSpaceQuerier{
		multiRows:   map[string][]string{"bot-42": {"space-bot"}},
		groupSpaces: map[string]string{"group-1": "space-group"},
	}, &stubAuthorizationSource{newSend: true})
	ctx := newGrantSpaceContext(t, "")
	catalog := ba.cardTemplates

	principal := ba.botSendCatalogPrincipal(ctx, "bot-42", "group-1", common.ChannelTypeGroup.Uint8())
	sent, err := catalog.RenderPayloadForPrincipal(context.Background(), principal,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any),
		cardtmpl.BuildEnv{Lang: "zh-CN"})
	if err != nil {
		t.Fatalf("group send: %v", err)
	}
	envelope, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	if got := storedProvenanceSpaceID(envelope); got != "space-group" {
		t.Fatalf("stored Space = %q", got)
	}

	// The gate is rolled back: no principal Space, and a group envelope has no
	// top-level space_id to fall back to either.
	dark := botCatalogPrincipal{BotID: "bot-42"}
	edited, err := catalog.RenderEditPayloadForPrincipal(context.Background(), dark,
		registrySendBody(t, "reasoning", testReasoningData(t, "reasoning"))["payload"].(map[string]any),
		cardtmpl.BuildEnv{Lang: "zh-CN"}, storedProvenanceSpaceID(envelope))
	if err != nil {
		t.Fatalf("dark-gate edit: %v", err)
	}
	editedEnvelope, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	editedMarkers, err := cardmsg.CatalogFrameMarkers(editedEnvelope)
	if err != nil {
		t.Fatalf("edited markers: %v", err)
	}
	if editedMarkers.Provenance.SpaceID != "space-group" {
		t.Fatalf("the edit rewrote the Space to %q, erasing the send's record",
			editedMarkers.Provenance.SpaceID)
	}
}

// The batch reports a disabled pointer as a status rather than an error, so the
// pure decision has to recognise it. If it did not, a disabled template would
// look like "no dynamic version" and fall back to its static version — the one
// outcome D6 forbids, because the operator disabled that ID deliberately.
func TestDecideSendRefRefusesADisabledPointerFromTheBatch(t *testing.T) {
	catalog := newMustTestCatalog(t)
	staticRef, hasStatic := catalog.staticSendVersion(aireasoningprocess.TemplateID)
	if !hasStatic {
		t.Fatal("fixture lost its static send policy")
	}
	disabled := cardtmpl.RuntimeAuthorization{
		Activation: cardtmpl.RuntimeActivation{Exists: true, Status: cardtmpl.RuntimeActivationDisabled},
	}
	if ref := catalog.decideSendRef(aireasoningprocess.TemplateID, staticRef, true, disabled, botSpaceScoped); ref.ID != "" {
		t.Fatalf("a disabled pointer fell back to the static version: %+v", ref)
	}
	// And it is the *disabled status* doing the work, not the empty version: a
	// template with no activation row at all still keeps its static version.
	none := cardtmpl.RuntimeAuthorization{}
	if ref := catalog.decideSendRef(aireasoningprocess.TemplateID, staticRef, true, none, botSpaceScoped); ref != staticRef {
		t.Fatalf("an unactivated template lost its static version: %+v", ref)
	}
}

// Review (Jerry-Xin blocking / yujiawei S2+P1-2): the `send|edit` grant was
// half-wired. `send` reached the dynamic authorizer; `edit` was gated on a
// boot-time map of static exacts that a runtime-published version can never be
// in, so a Bot could be granted a dynamic template, send it, and then have no
// way to edit the card it had just sent. For a state-machine card that is a
// dead end.
func TestBotEditResolvesADynamicRefThroughTheEditGrant(t *testing.T) {
	dynamicRef := botTemplateRef{ID: aireasoningprocess.TemplateID, Version: "9.9.9-dyn"}
	principal := botCatalogPrincipal{BotID: "bot-1", SpaceID: "space-1", Space: botSpaceScoped}
	authorized := func(grant cardtmpl.RuntimeGrant) *stubAuthorizationSource {
		return &stubAuthorizationSource{
			newSend: true,
			auth: map[cardtmpl.ID]cardtmpl.RuntimeAuthorization{
				dynamicRef.ID: {
					Version: dynamicRef.Version,
					Artifact: cardtmpl.RuntimeArtifactMeta{
						ID: dynamicRef.ID, Version: dynamicRef.Version,
						Source: cardtmpl.RuntimeSourceDynamic,
					},
					Grant: grant,
				},
			},
		}
	}
	refValue := map[string]any{"id": string(dynamicRef.ID), "version": dynamicRef.Version}

	// Granted edit: the dynamic exact resolves even though it is absent from
	// the static EditCompatible allowlist.
	catalog := newMustTestCatalog(t)
	catalog.authorization = authorized(cardtmpl.RuntimeGrant{
		Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Discover: true, Edit: true,
	})
	if _, ok := catalog.editAllowed[dynamicRef]; ok {
		t.Fatal("fixture precondition broken: the dynamic ref is in the static allowlist")
	}
	got, err := catalog.resolveEditRef(context.Background(), principal, refValue)
	if err != nil {
		t.Fatalf("a granted dynamic edit was refused: %v", err)
	}
	if got != dynamicRef {
		t.Fatalf("resolved %+v, want %+v", got, dynamicRef)
	}

	// The read is pinned to the stored exact version — an edit must never
	// follow the activation pointer, or it would rewrite a card across
	// versions (D6).
	if len(catalog.authorization.(*stubAuthorizationSource).queries) > 0 {
		if v := catalog.authorization.(*stubAuthorizationSource).queries[0].Version; v != dynamicRef.Version {
			t.Fatalf("edit query version = %q, want the stored exact %q", v, dynamicRef.Version)
		}
	}

	// Revoked (a tombstone: found, no permissions) → refused, and refused as a
	// client error rather than an outage.
	revoked := newMustTestCatalog(t)
	revoked.authorization = authorized(cardtmpl.RuntimeGrant{
		Found: true, Scope: cardtmpl.RuntimeGrantScopeExact,
	})
	if _, err := revoked.resolveEditRef(context.Background(), principal, refValue); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("revoked edit error = %v, want errBotTemplateRequestInvalid", err)
	}

	// A send-only grant does not confer edit.
	sendOnly := newMustTestCatalog(t)
	sendOnly.authorization = authorized(cardtmpl.RuntimeGrant{
		Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Discover: true, Send: true,
	})
	if _, err := sendOnly.resolveEditRef(context.Background(), principal, refValue); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("send-only edit error = %v, want errBotTemplateRequestInvalid", err)
	}

	// Blocked artifact refuses even with an edit grant.
	blockedSource := authorized(cardtmpl.RuntimeGrant{
		Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Discover: true, Edit: true,
	})
	blockedAuth := blockedSource.auth[dynamicRef.ID]
	blockedAuth.Artifact.Blocked = true
	blockedSource.auth[dynamicRef.ID] = blockedAuth
	blocked := newMustTestCatalog(t)
	blocked.authorization = blockedSource
	if _, err := blocked.resolveEditRef(context.Background(), principal, refValue); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("blocked edit error = %v, want errBotTemplateRequestInvalid", err)
	}
}

// The static half must not acquire a DB dependency: a code-reviewed static
// exact stays editable with no resolver at all, which is what keeps a
// gates-off deployment on exactly its pre-PR-C behaviour.
func TestBotEditKeepsStaticRefsAnsweringWithoutTheRuntime(t *testing.T) {
	catalog := newMustTestCatalog(t)
	staticRef := botTemplateRef{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV3}
	if _, ok := catalog.editAllowed[staticRef]; !ok {
		t.Fatal("fixture precondition broken: the static ref is not edit-compatible")
	}
	refValue := map[string]any{"id": string(staticRef.ID), "version": staticRef.Version}

	// No resolver, unresolved principal — the dark-catalog shape.
	got, err := catalog.resolveEditRef(context.Background(), botCatalogPrincipal{BotID: "bot-1"}, refValue)
	if err != nil {
		t.Fatalf("a static edit needed the runtime: %v", err)
	}
	if got != staticRef {
		t.Fatalf("resolved %+v, want %+v", got, staticRef)
	}

	// And an unknown dynamic ref is still refused there, rather than falling
	// open because no resolver was available to say no.
	unknown := map[string]any{"id": string(aireasoningprocess.TemplateID), "version": "9.9.9-dyn"}
	if _, err := catalog.resolveEditRef(context.Background(), botCatalogPrincipal{BotID: "bot-1"}, unknown); !errors.Is(err, errBotTemplateRequestInvalid) {
		t.Fatalf("dark-catalog dynamic edit error = %v, want errBotTemplateRequestInvalid", err)
	}
}
