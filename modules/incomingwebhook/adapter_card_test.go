package incomingwebhook

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The card builders take the already-parsed *ev (parse unmarshals once); these
// helpers unmarshal a JSON fixture and build, keeping the tests fixture-driven.
func ghPushCardFrom(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var ev ghPushEvent
	require.NoError(t, json.Unmarshal([]byte(body), &ev))
	return buildGitHubPushCard(&ev, "en-US")
}

func ghIssueCommentCardFrom(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var ev ghIssueCommentEvent
	require.NoError(t, json.Unmarshal([]byte(body), &ev))
	return buildGitHubIssueCommentCard(&ev, "en-US")
}

func glPushCardFrom(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var ev glPushEvent
	require.NoError(t, json.Unmarshal([]byte(body), &ev))
	return buildGitLabPushCard(&ev, "en-US")
}

func glPipelineCardFrom(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var ev glPipelineEvent
	require.NoError(t, json.Unmarshal([]byte(body), &ev))
	return buildGitLabPipelineCard(&ev, "en-US")
}

// Card-path unit tests for the github/gitlab adapters (card-message
// webhook-cardmsg-adapter). No DB/Redis/IM: the builders are pure translation and
// cardmsg.Validate/BuildPlain are the authoritative gates exercised directly.

// cardBodyText concatenates every rendered TextBlock leaf (recursing into
// Container.items) so a test can assert what a leaf carries. It is the render-facing
// counterpart to cardmsg.BuildPlain (which strips markdown); here we keep the raw
// leaf text to prove escaping happened.
func cardBodyText(card map[string]interface{}) string {
	var b strings.Builder
	var walk func(items []interface{})
	walk = func(items []interface{}) {
		for _, it := range items {
			el, _ := it.(map[string]interface{})
			if el == nil {
				continue
			}
			if el["type"] == "TextBlock" {
				if s, _ := el["text"].(string); s != "" {
					b.WriteString(s)
					b.WriteByte('\n')
				}
			}
			if sub, ok := el["items"].([]interface{}); ok {
				walk(sub)
			}
		}
	}
	if body, ok := card["body"].([]interface{}); ok {
		walk(body)
	}
	return b.String()
}

// cardOpenURL returns the first Action.OpenUrl (url, title) on the card, or "","".
func cardOpenURL(card map[string]interface{}) (url, title string) {
	acts, _ := card["actions"].([]interface{})
	for _, a := range acts {
		act, _ := a.(map[string]interface{})
		if act != nil && act["type"] == "Action.OpenUrl" {
			u, _ := act["url"].(string)
			t, _ := act["title"].(string)
			return u, t
		}
	}
	return "", ""
}

func cardActionURL(card map[string]interface{}) string { u, _ := cardOpenURL(card); return u }

func TestBuildGitHubPushCard(t *testing.T) {
	card := ghPushCardFrom(t, `{
		"ref": "refs/heads/main",
		"compare": "https://github.com/octo/repo/compare/aaaa...bbbb",
		"commits": [
			{"id": "aaaabbbbccccdddd", "message": "feat: first\n\nbody", "url": "https://github.com/o/r/commit/aaaabbbb"},
			{"id": "1111222233334444", "message": "fix: second", "url": "https://github.com/o/r/commit/11112222"}
		],
		"repository": {"full_name": "octo/repo", "html_url": "https://github.com/octo/repo"},
		"sender": {"login": "alice"}
	}`)
	require.NotNil(t, card)
	// Authoritative gate: the produced card is a valid octo/v1 card.
	require.NoError(t, validateVCSCard(card), "server-built card must pass cardmsg.Validate")

	assert.Equal(t, "AdaptiveCard", card["type"])
	assert.Equal(t, cardmsg.CardVersion, card["version"])

	plain := cardmsg.BuildPlain(card)
	assert.Contains(t, plain, "alice pushed 2 commit(s) to main")
	assert.Contains(t, plain, "octo/repo")
	assert.Contains(t, plain, "aaaabbb feat: first", "first commit: short sha + first message line")
	assert.Contains(t, plain, "1111222 fix: second")
	assert.NotContains(t, plain, "body", "only the first line of a commit message is rendered")

	// Prefer the compare URL for the View action, with the localized title.
	url, title := cardOpenURL(card)
	assert.Equal(t, "https://github.com/octo/repo/compare/aaaa...bbbb", url)
	assert.Equal(t, "View on GitHub", title)
}

func TestBuildGitLabPipelineCard_StatusColor(t *testing.T) {
	card := glPipelineCardFrom(t, `{
		"object_attributes": {"id": 4567, "ref": "test", "status": "success", "duration": 446},
		"user": {"username": "bob"},
		"project": {"path_with_namespace": "grp/app", "web_url": "https://gitlab.com/grp/app"},
		"builds": [
			{"name": "build", "status": "success"},
			{"name": "unit", "status": "success"},
			{"name": "lint", "status": "success"},
			{"name": "e2e", "status": "success"},
			{"name": "deploy", "status": "success"}
		]
	}`)
	require.NotNil(t, card)
	require.NoError(t, validateVCSCard(card))

	// success → headline TextBlock carries color=Good; status word lives in the FactSet.
	body0 := card["body"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "Good", body0["color"])
	plain := cardmsg.BuildPlain(card)
	assert.Contains(t, plain, "Pipeline #4567")
	assert.Contains(t, plain, "Branch: test")
	assert.Contains(t, plain, "Status: success")
	assert.Contains(t, plain, "Duration: 7m 26s", "446s → 7m 26s")
	assert.Contains(t, plain, "Jobs (5): build / unit / lint / e2e / deploy")
	assert.Equal(t, "https://gitlab.com/grp/app/-/pipelines/4567", cardActionURL(card))

	// Non-terminal status → no card (degrades to text/skip upstream).
	assert.Nil(t, glPipelineCardFrom(t, `{"object_attributes":{"status":"running"}}`))
}

func TestFormatPipelineDuration(t *testing.T) {
	assert.Equal(t, "", formatPipelineDuration(0))
	assert.Equal(t, "42s", formatPipelineDuration(42))
	assert.Equal(t, "7m 26s", formatPipelineDuration(446))
	assert.Equal(t, "1h 2m", formatPipelineDuration(3720))
	// A hostile/absurd external duration is clamped, not rendered as "277777777h …".
	assert.Equal(t, "100h 0m", formatPipelineDuration(1_000_000_000))
}

// TestAllVCSCardBuilders_Smoke exercises every event-specific builder (the ones only
// indirectly covered elsewhere: GitHub PR/issues/release, GitLab tag/note) so a
// regression in any one is caught: card is non-nil, passes the authoritative
// validator, derives non-empty plain, and carries the expected headline.
func TestAllVCSCardBuilders_Smoke(t *testing.T) {
	cases := []struct {
		name     string
		build    func(t *testing.T) map[string]interface{}
		headline string
	}{
		{"gh pull_request opened", func(t *testing.T) map[string]interface{} {
			var ev ghPullRequestEvent
			require.NoError(t, json.Unmarshal([]byte(`{"action":"opened","pull_request":{"number":9,"title":"Add cards","html_url":"https://github.com/o/r/pull/9"},"repository":{"full_name":"o/r"},"sender":{"login":"alice"}}`), &ev))
			return buildGitHubPullRequestCard(&ev, "en-US")
		}, "alice opened a pull request"},
		{"gh issues closed", func(t *testing.T) map[string]interface{} {
			var ev ghIssuesEvent
			require.NoError(t, json.Unmarshal([]byte(`{"action":"closed","issue":{"number":3,"title":"Bug","html_url":"https://github.com/o/r/issues/3"},"repository":{"full_name":"o/r"},"sender":{"login":"bob"}}`), &ev))
			return buildGitHubIssuesCard(&ev, "en-US")
		}, "bob closed an issue"},
		{"gh release published", func(t *testing.T) map[string]interface{} {
			var ev ghReleaseEvent
			require.NoError(t, json.Unmarshal([]byte(`{"action":"published","release":{"tag_name":"v1.2.0","name":"v1.2.0","html_url":"https://github.com/o/r/releases/v1.2.0"},"repository":{"full_name":"o/r"},"sender":{"login":"carol"}}`), &ev))
			return buildGitHubReleaseCard(&ev, "en-US")
		}, "carol published a release"},
		{"gl tag push", func(t *testing.T) map[string]interface{} {
			var ev glPushEvent
			require.NoError(t, json.Unmarshal([]byte(`{"ref":"refs/tags/v2.0.0","after":"abc123","user_username":"dave","project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"}}`), &ev))
			return buildGitLabTagPushCard(&ev, "en-US")
		}, "dave pushed tag"},
		{"gl note on MR", func(t *testing.T) map[string]interface{} {
			var ev glNoteEvent
			require.NoError(t, json.Unmarshal([]byte(`{"user":{"username":"erin"},"object_attributes":{"note":"lgtm","noteable_type":"MergeRequest","url":"https://gitlab.com/o/r/-/merge_requests/5#note_1"},"merge_request":{"iid":5,"title":"Refactor"},"project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"}}`), &ev))
			return buildGitLabNoteCard(&ev, "en-US")
		}, "erin commented"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			card := c.build(t)
			require.NotNil(t, card)
			require.NoError(t, validateVCSCard(card), "server-built card must pass cardmsg.Validate")
			assert.NotEmpty(t, cardmsg.BuildPlain(card))
			assert.Contains(t, cardBodyText(card), c.headline)
		})
	}
}

// TestVCSCard_TrustBoundary is the parity gate: a hostile actor / title / commit
// message / URL from either platform must (a) render literally in the card leaf (no
// emphasis/link breakout) and (b) never produce a non-http(s) actionable URL. Proven
// for BOTH github and gitlab with the same assertions.
func TestVCSCard_TrustBoundary(t *testing.T) {
	// A commit message that tries to inject a link + bold, and a hostile repo URL.
	ghCard := ghPushCardFrom(t, `{
		"ref": "refs/heads/main",
		"commits": [{"id":"0badc0de","message":"[click](javascript:alert(1)) **not bold** ]break[","url":"u"}],
		"repository": {"full_name": "a]b*c/repo", "html_url": "javascript:alert(1)"},
		"sender": {"login": "mallory"}
	}`)
	glCard := glPushCardFrom(t, `{
		"ref": "refs/heads/main",
		"commits": [{"id":"0badc0de","message":"[click](javascript:alert(1)) **not bold** ]break["}],
		"user": {"name": "m]a*l"},
		"project": {"path_with_namespace": "a]b*c/repo", "web_url": "javascript:alert(1)"}
	}`)

	for _, tc := range []struct {
		name string
		card map[string]interface{}
	}{
		{"github", ghCard},
		{"gitlab", glCard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, tc.card)
			// The authoritative gate passing IS the proof no unescaped javascript: link
			// or forbidden element leaked — Validate walks every markdown link target
			// through the positive http(s) allowlist.
			require.NoError(t, validateVCSCard(tc.card))

			leaves := cardBodyText(tc.card)
			// The hostile markup is escaped: brackets/parens/asterisks are backslash-escaped,
			// so no live link or emphasis is formed.
			assert.Contains(t, leaves, `\[click\]\(javascript:alert\(1\)\)`,
				"link syntax must be escaped to render literally")
			assert.Contains(t, leaves, `\*\*not bold\*\*`, "bold syntax must be escaped")
			assert.NotContains(t, leaves, "](javascript:", "no live markdown link may survive")

			// The hostile repo URL is not http(s) → button omitted, never an actionable
			// javascript: URL.
			assert.NotContains(t, cardActionURL(tc.card), "javascript:")
			assert.Empty(t, cardActionURL(tc.card), "non-http(s) repo url → no View button")

			// Derived plain shows the literal text (defense visible to search/quote too).
			assert.Contains(t, cardmsg.BuildPlain(tc.card), "not bold")
		})
	}
}

func TestNeutralizeLeadingBlockMarker(t *testing.T) {
	// Leading bullet / ordered / thematic-break markers are neutralized.
	assert.Equal(t, `\- rm -rf`, neutralizeLeadingBlockMarker("- rm -rf"))
	assert.Equal(t, `\+ item`, neutralizeLeadingBlockMarker("+ item"))
	assert.Equal(t, `\---`, neutralizeLeadingBlockMarker("---"))
	assert.Equal(t, `1\. Deploy`, neutralizeLeadingBlockMarker("1. Deploy"))
	assert.Equal(t, `12\. x`, neutralizeLeadingBlockMarker("12. x"))
	// Non-markers and already-escaped leads are untouched.
	assert.Equal(t, "alice pushed", neutralizeLeadingBlockMarker("alice pushed"))
	assert.Equal(t, `\[x`, neutralizeLeadingBlockMarker(`\[x`))
	assert.Equal(t, "1.5 rating", neutralizeLeadingBlockMarker("1.5 rating"), "ordered marker needs whitespace/end after the dot")
	assert.Equal(t, "", neutralizeLeadingBlockMarker(""))
}

// TestVCSCard_ListMarkerNeutralized proves the trust-boundary defense for BLOCK
// markdown: attacker-controlled text at a TextBlock line start (issue-comment body →
// quote; GitLab display-name fallback → headline) cannot open a bullet/ordered list.
func TestVCSCard_ListMarkerNeutralized(t *testing.T) {
	t.Run("github issue_comment quote", func(t *testing.T) {
		card := ghIssueCommentCardFrom(t, `{
			"action":"created",
			"issue":{"number":7,"title":"t","html_url":"h"},
			"comment":{"html_url":"https://github.com/o/r/issues/7#c1","body":"1. Deploy approved now"},
			"repository":{"full_name":"o/r"},"sender":{"login":"mallory"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		leaves := cardBodyText(card)
		assert.Contains(t, leaves, `1\. Deploy approved now`, "leading ordered marker must be escaped so the quote can't become a numbered list")
		assert.NotContains(t, leaves, "\n1. ", "no live ordered-list marker at a leaf line start")
		// The comment content is preserved in the authoritative plain (search/quote).
		assert.Contains(t, cardmsg.BuildPlain(card), "Deploy approved now")
	})

	t.Run("gitlab display-name headline", func(t *testing.T) {
		// username absent → free-text user_name fallback lands at the headline line start.
		card := glPushCardFrom(t, `{
			"ref":"refs/heads/main",
			"commits":[{"id":"abc1234","message":"m","url":"u"}],
			"user_name":"- Security Team",
			"project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		headline := card["body"].([]interface{})[0].(map[string]interface{})["text"].(string)
		assert.True(t, strings.HasPrefix(headline, `\- Security Team`),
			"a display name starting with '- ' must not open a bullet list in the headline; got %q", headline)
	})
}

// TestVCSPushReq_Degrade covers the degrade contract: nil card or a card that fails
// self-validation falls back to the text payload (never a card request / 400).
func TestVCSPushReq_Degrade(t *testing.T) {
	text := "**alice** pushed 1 commit(s) to `main`"

	t.Run("nil card → text", func(t *testing.T) {
		req := vcsPushReq(text, nil)
		assert.Empty(t, req.MsgType)
		assert.Equal(t, text, req.Content)
	})

	t.Run("invalid card → text (degrade, not 400)", func(t *testing.T) {
		bad := map[string]interface{}{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []interface{}{map[string]interface{}{"type": "Bogus"}}, // not whitelisted
		}
		require.Error(t, validateVCSCard(bad), "precondition: card is invalid")
		req := vcsPushReq(text, bad)
		assert.Empty(t, req.MsgType, "an invalid card must not reach the card path")
		assert.Equal(t, text, req.Content)
	})

	t.Run("valid card → card path", func(t *testing.T) {
		good := ghPushCardFrom(t, `{"ref":"refs/heads/main","commits":[{"id":"abc1234","message":"m","url":"u"}],"repository":{"full_name":"o/r","html_url":"https://github.com/o/r"},"sender":{"login":"a"}}`)
		require.NotNil(t, good)
		req := vcsPushReq("fallback text", good)
		assert.Equal(t, msgTypeCard, req.MsgType)
		assert.NotNil(t, req.Card)
		assert.Empty(t, req.Content, "card path does not set text content")
	})
}

// TestVCSParse_FlagGate covers the top-level dispatch: with the flag off the adapter
// emits the historical text path (MsgType empty); with the flag on it emits a card.
func TestVCSParse_FlagGate(t *testing.T) {
	ghPush := []byte(`{"ref":"refs/heads/main","commits":[{"id":"abc1234","message":"m","url":"u"}],"repository":{"full_name":"o/r","html_url":"https://github.com/o/r"},"sender":{"login":"a"}}`)
	glMR := []byte(`{"object_attributes":{"iid":42,"title":"Refactor","url":"https://gitlab.com/o/r/-/merge_requests/42","action":"open"},"user":{"username":"a"},"project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"}}`)

	t.Run("flag off → text", func(t *testing.T) {
		t.Setenv(cardmsg.EnvEnabled, "")
		req, _, _ := parseGitHubPush(ghHeader("push"), ghPush)
		require.NotNil(t, req)
		assert.Empty(t, req.MsgType)
		assert.NotEmpty(t, req.Content)
	})

	t.Run("flag on → card (github + gitlab)", func(t *testing.T) {
		t.Setenv(cardmsg.EnvEnabled, "1")

		req, skip, invalid := parseGitHubPush(ghHeader("push"), ghPush)
		require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
		assert.Equal(t, msgTypeCard, req.MsgType)
		require.NoError(t, validateVCSCard(req.Card))

		req2, skip2, invalid2 := parseGitLabPush(glHeader("Merge Request Hook"), glMR)
		require.NotNil(t, req2, "skip=%q invalid=%q", skip2, invalid2)
		assert.Equal(t, msgTypeCard, req2.MsgType)
		require.NoError(t, validateVCSCard(req2.Card))
	})

	t.Run("flag on but skip/no_event unchanged", func(t *testing.T) {
		t.Setenv(cardmsg.EnvEnabled, "1")
		// missing event header → still no_event
		_, _, invalid := parseGitHubPush(http.Header{}, []byte(`{}`))
		assert.Equal(t, "no_event", invalid)
		// subset-outside action → still skip
		_, skip, _ := parseGitHubPush(ghHeader("pull_request"), []byte(`{"action":"synchronize"}`))
		assert.Equal(t, "event", skip)
	})
}

// TestBuildCardPayload_AdapterEnvelope locks the full production envelope path: an
// adapter-produced req.Card fed through buildCardPayload must yield a type-17 payload
// with server-pinned card_version/profile, a from.kind=webhook identity carrying the
// webhook_id + configured name, the server-derived space_id, and a non-placeholder
// authoritative plain derived by Finalize (PR #596 review, Jerry-Xin).
func TestBuildCardPayload_AdapterEnvelope(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "1")
	card := ghPushCardFrom(t, `{
		"ref":"refs/heads/main",
		"commits":[{"id":"abc1234","message":"fix: guard nil","url":"https://github.com/o/r/commit/abc1234"}],
		"repository":{"full_name":"o/r","html_url":"https://github.com/o/r"},
		"sender":{"login":"alice"}}`)
	require.NotNil(t, card)

	m := &incomingWebhookModel{WebhookID: "iwh_abc", SpaceID: "sp_1", Name: "CI Bot"}
	payload, err := buildCardPayload(m, &pushPayloadReq{MsgType: msgTypeCard, Card: card}, false)
	require.NoError(t, err)

	assert.Equal(t, cardmsg.InteractiveCard.Int(), payload["type"])
	assert.Equal(t, cardmsg.CardVersion, payload["card_version"])
	assert.Equal(t, cardmsg.ProfileV1, payload["profile"])
	assert.Equal(t, "sp_1", payload["space_id"], "space_id is server-derived from the webhook row")

	from, _ := payload["from"].(map[string]interface{})
	require.NotNil(t, from)
	assert.Equal(t, extraKindValue, from["kind"])
	assert.Equal(t, "iwh_abc", from["webhook_id"])
	assert.Equal(t, "CI Bot", from["name"])

	plain, _ := payload["plain"].(string)
	assert.NotEmpty(t, plain, "Finalize derives an authoritative plain")
	assert.NotEqual(t, cardmsg.PlaceholderCard, plain, "plain comes from the card body, not the [卡片] fallback")
	assert.Contains(t, plain, "pushed")
}

func TestVCSViewLabel(t *testing.T) {
	assert.Equal(t, "View on GitHub", vcsViewLabel(cardSourceGitHub, "en-US"))
	assert.Equal(t, "在 GitHub 查看", vcsViewLabel(cardSourceGitHub, "zh-CN"))
	assert.Equal(t, "在 GitLab 查看", vcsViewLabel(cardSourceGitLab, "zh-Hans"))
}
