package incomingwebhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const expectedVCSGitBranchIcon = "data:image/svg+xml,%3Csvg%20xmlns%3D%22http%3A%2F%2Fwww.w3.org%2F2000%2Fsvg%22%20width%3D%2216%22%20height%3D%2216%22%20viewBox%3D%220%200%2024%2024%22%20fill%3D%22none%22%20stroke%3D%22%236b7075%22%20stroke-width%3D%222%22%20stroke-linecap%3D%22round%22%20stroke-linejoin%3D%22round%22%3E%3Cpath%20d%3D%22M15%206a9%209%200%200%200-9%209V3%22%2F%3E%3Ccircle%20cx%3D%2218%22%20cy%3D%226%22%20r%3D%223%22%2F%3E%3Ccircle%20cx%3D%226%22%20cy%3D%2218%22%20r%3D%223%22%2F%3E%3C%2Fsvg%3E"

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

func ghPullRequestCardFrom(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var ev ghPullRequestEvent
	require.NoError(t, json.Unmarshal([]byte(body), &ev))
	return buildGitHubPullRequestCard(&ev, "en-US")
}

func ghIssuesCardFrom(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var ev ghIssuesEvent
	require.NoError(t, json.Unmarshal([]byte(body), &ev))
	return buildGitHubIssuesCard(&ev, "en-US")
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

func glMergeRequestCardFrom(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var ev glMergeRequestEvent
	require.NoError(t, json.Unmarshal([]byte(body), &ev))
	return buildGitLabMergeRequestCard(&ev, "en-US")
}

func glIssueCardFrom(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var ev glIssueEvent
	require.NoError(t, json.Unmarshal([]byte(body), &ev))
	return buildGitLabIssueCard(&ev, "en-US")
}

// Card-path unit tests for the github/gitlab adapters (card-message
// webhook-cardmsg-adapter). No DB/Redis/IM: the builders are pure translation and
// cardmsg.Validate/BuildPlain are the authoritative gates exercised directly.

// cardBodyText concatenates every rendered TextBlock leaf (recursing into
// Container.items) plus every FactSet title/value so a test can assert what a leaf
// carries. It is the render-facing counterpart to cardmsg.BuildPlain (which strips
// markdown); here we keep the raw leaf text to prove escaping happened.
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
			if el["type"] == "FactSet" {
				if facts, ok := el["facts"].([]interface{}); ok {
					for _, f := range facts {
						fact, _ := f.(map[string]interface{})
						if fact == nil {
							continue
						}
						if s, _ := fact["title"].(string); s != "" {
							b.WriteString(s)
							b.WriteByte('\n')
						}
						if s, _ := fact["value"].(string); s != "" {
							b.WriteString(s)
							b.WriteByte('\n')
						}
					}
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

func walkCardValue(value interface{}, visit func(map[string]interface{})) {
	switch v := value.(type) {
	case map[string]interface{}:
		visit(v)
		for _, child := range v {
			walkCardValue(child, visit)
		}
	case []interface{}:
		for _, child := range v {
			walkCardValue(child, visit)
		}
	}
}

func cardNodeByID(card map[string]interface{}, id string) map[string]interface{} {
	var found map[string]interface{}
	walkCardValue(card, func(node map[string]interface{}) {
		if found == nil && node["id"] == id {
			found = node
		}
	})
	return found
}

func cardFactValue(card map[string]interface{}, title string) string {
	var value string
	walkCardValue(card, func(node map[string]interface{}) {
		if value != "" || node["type"] != "FactSet" {
			return
		}
		facts, _ := node["facts"].([]interface{})
		for _, raw := range facts {
			fact, _ := raw.(map[string]interface{})
			if fact != nil && fact["title"] == title {
				value, _ = fact["value"].(string)
				return
			}
		}
	})
	return value
}

// cardOpenURL returns the first Action.OpenUrl (url, title) on the card, or "","".
func cardOpenURL(card map[string]interface{}) (url, title string) {
	walkCardValue(card, func(node map[string]interface{}) {
		if url == "" && node["type"] == "Action.OpenUrl" {
			url, _ = node["url"].(string)
			title, _ = node["title"].(string)
		}
	})
	return url, title
}

func cardActionURL(card map[string]interface{}) string { u, _ := cardOpenURL(card); return u }

func TestVCSCardForgeAnatomy(t *testing.T) {
	tests := []struct {
		name      string
		card      map[string]interface{}
		source    string
		context   string
		badge     string
		actionURL string
	}{
		{
			name: "github push",
			card: ghPushCardFrom(t, `{
				"ref":"refs/heads/main",
				"compare":"https://github.com/o/r/compare/a...b",
				"commits":[{"id":"abc1234","message":"ship"}],
				"repository":{"full_name":"o/r","html_url":"https://github.com/o/r"},
				"sender":{"login":"alice"}}`),
			source: "GitHub", context: "o/r", badge: "Push",
			actionURL: "https://github.com/o/r/compare/a...b",
		},
		{
			name: "gitlab pipeline",
			card: glPipelineCardFrom(t, `{
				"object_attributes":{"id":42,"status":"success","ref":"main"},
				"project":{"path_with_namespace":"g/r","web_url":"https://gitlab.com/g/r"}}`),
			source: "GitLab", context: "g/r", badge: "success",
			actionURL: "https://gitlab.com/g/r/-/pipelines/42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotNil(t, tt.card)
			require.NoError(t, validateVCSCard(tt.card))

			header := cardNodeByID(tt.card, "vcs-header")
			require.NotNil(t, header)
			assert.Equal(t, "ColumnSet", header["type"])

			icon := cardNodeByID(tt.card, "vcs-source-icon")
			require.NotNil(t, icon)
			assert.Equal(t, expectedVCSGitBranchIcon, icon["url"])
			assert.Equal(t, "16px", icon["width"])
			assert.Equal(t, "16px", icon["height"])

			source := cardNodeByID(tt.card, "vcs-source-label")
			require.NotNil(t, source)
			assert.Equal(t, tt.source, source["text"])
			context := cardNodeByID(tt.card, "vcs-source-context")
			require.NotNil(t, context)
			assert.Equal(t, tt.context, context["text"])

			badge := cardNodeByID(tt.card, "vcs-event-badge")
			require.NotNil(t, badge)
			assert.Equal(t, tt.badge, badge["text"])

			content := cardNodeByID(tt.card, "vcs-content")
			require.NotNil(t, content)
			assert.Equal(t, "Container", content["type"])
			assert.Equal(t, "accent", content["style"])
			assert.Equal(t, true, content["bleed"])

			headline := cardNodeByID(tt.card, "vcs-headline")
			require.NotNil(t, headline)
			assert.NotContains(t, headline, "color", "headline stays neutral; semantic color belongs to the badge")

			footer := cardNodeByID(tt.card, "vcs-footer")
			require.NotNil(t, footer)
			assert.Equal(t, "emphasis", footer["style"])
			assert.Equal(t, true, footer["bleed"])
			assert.NotContains(t, tt.card, "actions", "navigation is rendered once inside the footer")
			assert.Equal(t, tt.actionURL, cardActionURL(tt.card))
		})
	}
}

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

func TestBuildGitHubPullRequestCard_Facts(t *testing.T) {
	card := ghPullRequestCardFrom(t, `{
		"action": "opened",
		"pull_request": {"number": 42, "title": "Add cards", "html_url": "https://github.com/o/r/pull/42",
			"base": {"ref": "main"}, "head": {"ref": "feature/cards"},
			"labels": [{"name": "backend"}, {"name": "needs-review"}]},
		"repository": {"full_name": "o/r"},
		"sender": {"login": "carol"}
	}`)
	require.NotNil(t, card)
	require.NoError(t, validateVCSCard(card))

	plain := cardmsg.BuildPlain(card)
	assert.Contains(t, plain, "carol opened a pull request")
	assert.Contains(t, plain, "#42 · Add cards")
	assert.Contains(t, plain, "Source: feature/cards")
	assert.Contains(t, plain, "Target: main")
	assert.Contains(t, plain, "Labels (2): backend / needs-review")

	t.Run("no base/head/labels omits the facts entirely", func(t *testing.T) {
		card := ghPullRequestCardFrom(t, `{
			"action": "opened",
			"pull_request": {"number": 42, "title": "Add cards", "html_url": "https://github.com/o/r/pull/42"},
			"repository": {"full_name": "o/r"},
			"sender": {"login": "carol"}
		}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		body, _ := card["body"].([]interface{})
		for _, it := range body {
			el, _ := it.(map[string]interface{})
			assert.NotEqual(t, "FactSet", el["type"], "no FactSet block when there is nothing to show")
		}
	})

	t.Run("synchronize action is rendered too (no longer filtered)", func(t *testing.T) {
		card := ghPullRequestCardFrom(t, `{"action":"synchronize","pull_request":{"number":1,"title":"x"},"sender":{"login":"carol"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		assert.Contains(t, cardmsg.BuildPlain(card), "carol pushed new commits to a pull request")
	})

	t.Run("ready_for_review headline places the object mid-phrase", func(t *testing.T) {
		card := ghPullRequestCardFrom(t, `{"action":"ready_for_review","pull_request":{"number":1,"title":"x"},"sender":{"login":"carol"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		assert.Contains(t, cardmsg.BuildPlain(card), "carol marked a pull request ready for review")
	})

	t.Run("converted_to_draft headline places the object mid-phrase", func(t *testing.T) {
		card := ghPullRequestCardFrom(t, `{"action":"converted_to_draft","pull_request":{"number":1,"title":"x"},"sender":{"login":"carol"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		assert.Contains(t, cardmsg.BuildPlain(card), "carol converted a pull request to draft")
	})

	t.Run("missing action has no card (nothing to render)", func(t *testing.T) {
		assert.Nil(t, ghPullRequestCardFrom(t, `{"pull_request":{"number":1}}`))
	})

	t.Run("hostile unknown action is escaped in the card headline (trust-boundary)", func(t *testing.T) {
		card := ghPullRequestCardFrom(t, `{"action":"**pwn** [x](http://evil.example)",
			"pull_request":{"number":1,"title":"x"},"sender":{"login":"carol"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card), "server-built card must pass cardmsg.Validate")
		leaves := cardBodyText(card)
		assert.Contains(t, leaves, `\*\*pwn\*\* \[x\]\(http://evil.example\)`,
			"markdown metacharacters in an unmapped action must be escaped, never form a live link/emphasis")
	})
}

func TestBuildGitHubIssuesCard_Facts(t *testing.T) {
	card := ghIssuesCardFrom(t, `{
		"action": "opened",
		"issue": {"number": 7, "title": "Login broken", "html_url": "https://github.com/o/r/issues/7",
			"labels": [{"name": "bug"}, {"name": "P1"}]},
		"repository": {"full_name": "o/r"},
		"sender": {"login": "dave"}
	}`)
	require.NotNil(t, card)
	require.NoError(t, validateVCSCard(card))
	plain := cardmsg.BuildPlain(card)
	assert.Contains(t, plain, "Labels (2): bug / P1")

	t.Run("no labels omits the fact", func(t *testing.T) {
		card := ghIssuesCardFrom(t, `{
			"action": "opened",
			"issue": {"number": 7, "title": "Login broken"},
			"repository": {"full_name": "o/r"},
			"sender": {"login": "dave"}
		}`)
		require.NotNil(t, card)
		body, _ := card["body"].([]interface{})
		for _, it := range body {
			el, _ := it.(map[string]interface{})
			assert.NotEqual(t, "FactSet", el["type"])
		}
	})

	t.Run("labeled action is rendered too (no longer filtered)", func(t *testing.T) {
		card := ghIssuesCardFrom(t, `{"action":"labeled","issue":{"number":1,"title":"x"},"sender":{"login":"dave"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		assert.Contains(t, cardmsg.BuildPlain(card), "dave labeled an issue")
	})

	t.Run("hostile unknown action is escaped in the card headline (trust-boundary)", func(t *testing.T) {
		card := ghIssuesCardFrom(t, `{"action":"**pwn** [x](http://evil.example)",
			"issue":{"number":1,"title":"x"},"sender":{"login":"dave"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card), "server-built card must pass cardmsg.Validate")
		assert.Contains(t, cardBodyText(card), `\*\*pwn\*\* \[x\]\(http://evil.example\)`)
	})
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

	// success → semantic color lives only on the compact status badge.
	headline := cardNodeByID(card, "vcs-headline")
	require.NotNil(t, headline)
	assert.NotContains(t, headline, "color")
	badge := cardNodeByID(card, "vcs-event-badge")
	require.NotNil(t, badge)
	assert.Equal(t, "success", badge["text"])
	assert.Equal(t, "Good", badge["color"])
	plain := cardmsg.BuildPlain(card)
	assert.Contains(t, plain, "Pipeline #4567")
	assert.Contains(t, plain, "Branch: test")
	assert.Contains(t, plain, "Status: success")
	assert.Contains(t, plain, "Duration: 7m 26s", "446s → 7m 26s")
	assert.Equal(t, "build  \nunit  \nlint  \ne2e  \ndeploy", cardFactValue(card, "Jobs (5)"),
		"two spaces before each newline produce CommonMark hard breaks in octo-web")
	assert.Equal(t, "https://gitlab.com/grp/app/-/pipelines/4567", cardActionURL(card))

	// Non-terminal statuses are rendered too (no longer filtered to success/failed/canceled).
	// In-progress-ish ones get a distinct "Accent" color so they don't look identical to
	// an unrecognized status (PR #610 review, mochashanyao P2).
	for _, status := range []string{"running", "pending", "created", "waiting_for_resource", "preparing", "scheduled"} {
		t.Run("color for "+status, func(t *testing.T) {
			card := glPipelineCardFrom(t, fmt.Sprintf(`{"object_attributes":{"id":1,"status":%q}}`, status))
			require.NotNil(t, card)
			require.NoError(t, validateVCSCard(card))
			badge := cardNodeByID(card, "vcs-event-badge")
			require.NotNil(t, badge)
			assert.Equal(t, "Accent", badge["color"])
			// `_` is escaped by escapeCardText (`_` → `\_`); BuildPlain's stripMarkdown
			// (a real markdown parser) does not fold that back to a bare `_`, so the
			// plain rendering keeps the backslash — confirmed empirically here.
			assert.Contains(t, cardmsg.BuildPlain(card), "Status: "+strings.ReplaceAll(status, "_", `\_`))
		})
	}

	// A genuinely unrecognized status (not one of the 9 mapped values) keeps the
	// default headline color — proves the fallback still exists, not that "running"
	// specifically is unmapped (it no longer is).
	unknown := glPipelineCardFrom(t, `{"object_attributes":{"id":1,"status":"some_future_gitlab_status"}}`)
	require.NotNil(t, unknown)
	require.NoError(t, validateVCSCard(unknown))
	unknownBadge := cardNodeByID(unknown, "vcs-event-badge")
	require.NotNil(t, unknownBadge)
	assert.NotContains(t, unknownBadge, "color", "unmapped status keeps the default badge color")

	// Missing status (malformed payload, nothing to render) → no card.
	assert.Nil(t, glPipelineCardFrom(t, `{"object_attributes":{"id":1}}`))
}

func TestBuildGitLabPipelineCard_UsesEnglishLabelsAndJobLinesForAllLanguages(t *testing.T) {
	var ev glPipelineEvent
	require.NoError(t, json.Unmarshal([]byte(`{
		"object_attributes":{"id":7,"status":"success","ref":"main","duration":42},
		"project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"},
		"builds":[{"name":"build"},{"name":"unit"},{"name":"deploy"}]
	}`), &ev))

	for _, lang := range []string{"en-US", "zh-CN"} {
		t.Run(lang, func(t *testing.T) {
			card := buildGitLabPipelineCard(&ev, lang)
			require.NotNil(t, card)
			require.NoError(t, validateVCSCard(card))
			assert.Equal(t, "main", cardFactValue(card, "Branch"))
			assert.Equal(t, "success", cardFactValue(card, "Status"))
			assert.Equal(t, "42s", cardFactValue(card, "Duration"))
			assert.Equal(t, "build  \nunit  \ndeploy", cardFactValue(card, "Jobs (3)"),
				"raw newlines are CommonMark soft breaks and collapse in browser layout")
			assert.Empty(t, cardFactValue(card, "分支"))
			assert.Empty(t, cardFactValue(card, "任务 (3)"))
		})
	}

	t.Run("labels keep slash separator", func(t *testing.T) {
		card := glIssueCardFrom(t, `{
			"user":{"username":"alice"},
			"object_attributes":{"iid":1,"title":"bug","url":"https://gitlab.com/o/r/-/issues/1","action":"open"},
			"labels":[{"title":"backend"},{"title":"needs-review"}],
			"project":{"path_with_namespace":"o/r"}
		}`)
		require.NotNil(t, card)
		assert.Equal(t, "backend / needs-review", cardFactValue(card, "Labels (2)"))
	})
}

func TestFormatPipelineDuration(t *testing.T) {
	assert.Equal(t, "", formatPipelineDuration(0))
	assert.Equal(t, "42s", formatPipelineDuration(42))
	assert.Equal(t, "7m 26s", formatPipelineDuration(446))
	assert.Equal(t, "1h 2m", formatPipelineDuration(3720))
	// A hostile/absurd external duration is clamped, not rendered as "277777777h …",
	// and prefixed ">" so it reads distinctly from a genuine ~100h pipeline.
	assert.Equal(t, ">100h 0m", formatPipelineDuration(1_000_000_000))
	// A duration that lands exactly on the cap is real, not clamped — no prefix.
	assert.Equal(t, "100h 0m", formatPipelineDuration(maxPipelineDurationSec))
}

func TestBuildGitLabMergeRequestCard_Facts(t *testing.T) {
	card := glMergeRequestCardFrom(t, `{
		"user": {"username": "carol"},
		"object_attributes": {"iid": 42, "title": "Add cards", "url": "https://gitlab.com/o/r/-/merge_requests/42",
			"action": "open", "source_branch": "feature/cards", "target_branch": "main"},
		"labels": [{"title": "backend"}, {"title": "needs-review"}],
		"project": {"path_with_namespace": "o/r"}
	}`)
	require.NotNil(t, card)
	require.NoError(t, validateVCSCard(card))

	plain := cardmsg.BuildPlain(card)
	assert.Contains(t, plain, "carol opened a merge request")
	assert.Contains(t, plain, "!42 · Add cards")
	assert.Contains(t, plain, "Source: feature/cards")
	assert.Contains(t, plain, "Target: main")
	assert.Contains(t, plain, "Labels (2): backend / needs-review")

	t.Run("no source/target/labels omits the facts entirely", func(t *testing.T) {
		card := glMergeRequestCardFrom(t, `{
			"user": {"username": "carol"},
			"object_attributes": {"iid": 42, "title": "Add cards", "url": "https://gitlab.com/o/r/-/merge_requests/42", "action": "open"},
			"project": {"path_with_namespace": "o/r"}
		}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		body, _ := card["body"].([]interface{})
		for _, it := range body {
			el, _ := it.(map[string]interface{})
			assert.NotEqual(t, "FactSet", el["type"], "no FactSet block when there is nothing to show")
		}
	})

	t.Run("update action is rendered too (no longer filtered)", func(t *testing.T) {
		card := glMergeRequestCardFrom(t, `{"user":{"username":"carol"},"object_attributes":{"iid":1,"title":"x","action":"update"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		assert.Contains(t, cardmsg.BuildPlain(card), "carol updated a merge request")
	})

	t.Run("missing action has no card (nothing to render)", func(t *testing.T) {
		assert.Nil(t, glMergeRequestCardFrom(t, `{"object_attributes":{}}`))
	})

	t.Run("hostile unknown action is escaped in the card headline (trust-boundary)", func(t *testing.T) {
		// action is an unvalidated external field (any URL-token holder sets it) — the
		// glActionVerb raw-passthrough fallback must be escaped before it reaches the
		// card headline TextBlock, same as every other external leaf on this card.
		card := glMergeRequestCardFrom(t, `{"user":{"username":"carol"},
			"object_attributes":{"iid":1,"title":"x","action":"**pwn** [x](http://evil.example)"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card), "server-built card must pass cardmsg.Validate")
		leaves := cardBodyText(card)
		assert.Contains(t, leaves, `\*\*pwn\*\* \[x\]\(http://evil.example\)`,
			"markdown metacharacters in an unmapped action must be escaped, never form a live link/emphasis")
	})
}

func TestBuildGitLabIssueCard_Facts(t *testing.T) {
	card := glIssueCardFrom(t, `{
		"user": {"username": "dave"},
		"object_attributes": {"iid": 7, "title": "Login broken", "url": "https://gitlab.com/o/r/-/issues/7", "action": "open"},
		"labels": [{"title": "bug"}, {"title": "P1"}],
		"project": {"path_with_namespace": "o/r"}
	}`)
	require.NotNil(t, card)
	require.NoError(t, validateVCSCard(card))
	plain := cardmsg.BuildPlain(card)
	assert.Contains(t, plain, "Labels (2): bug / P1")

	t.Run("no labels omits the fact", func(t *testing.T) {
		card := glIssueCardFrom(t, `{
			"user": {"username": "dave"},
			"object_attributes": {"iid": 7, "title": "Login broken", "url": "https://gitlab.com/o/r/-/issues/7", "action": "open"},
			"project": {"path_with_namespace": "o/r"}
		}`)
		require.NotNil(t, card)
		body, _ := card["body"].([]interface{})
		for _, it := range body {
			el, _ := it.(map[string]interface{})
			assert.NotEqual(t, "FactSet", el["type"])
		}
	})

	t.Run("hostile unknown action is escaped in the card headline (trust-boundary)", func(t *testing.T) {
		card := glIssueCardFrom(t, `{"user":{"username":"dave"},
			"object_attributes":{"iid":7,"title":"Login broken","action":"**pwn** [x](http://evil.example)"}}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card), "server-built card must pass cardmsg.Validate")
		assert.Contains(t, cardBodyText(card), `\*\*pwn\*\* \[x\]\(http://evil.example\)`)
	})
}

// GitLab MR labels are project-defined but attacker-influencable (the URL token
// holder can POST arbitrary JSON); glLabelsFact must escape each title like every
// other card leaf, and cap the rendered list like the pipeline card's Jobs fact —
// the (N) count must still reflect the true total, not the truncated list.
func TestGlLabelsFact_OverflowAndEscaping(t *testing.T) {
	names := make([]string, 0, maxRenderedLabels+3)
	for i := 0; i < maxRenderedLabels+3; i++ {
		names = append(names, fmt.Sprintf(`{"title":"label-%d"}`, i))
	}
	card := glMergeRequestCardFrom(t, fmt.Sprintf(`{
		"user": {"username": "carol"},
		"object_attributes": {"iid": 1, "title": "x", "url": "https://gitlab.com/o/r/-/merge_requests/1", "action": "open"},
		"labels": [%s],
		"project": {"path_with_namespace": "o/r"}
	}`, strings.Join(names, ",")))
	require.NotNil(t, card)
	require.NoError(t, validateVCSCard(card))
	plain := cardmsg.BuildPlain(card)
	assert.Contains(t, plain, fmt.Sprintf("Labels (%d):", maxRenderedLabels+3))
	assert.NotContains(t, plain, fmt.Sprintf("label-%d", maxRenderedLabels+2), "labels beyond the cap are not rendered")

	t.Run("label title is escaped as a card leaf", func(t *testing.T) {
		card := glMergeRequestCardFrom(t, `{
			"user": {"username": "carol"},
			"object_attributes": {"iid": 1, "title": "x", "url": "https://gitlab.com/o/r/-/merge_requests/1", "action": "open"},
			"labels": [{"title": "**evil** [x](http://attacker)"}],
			"project": {"path_with_namespace": "o/r"}
		}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card), "server-built card must pass cardmsg.Validate")
		leaves := cardBodyText(card)
		assert.Contains(t, leaves, `\*\*evil\*\* \[x\]\(http://attacker\)`,
			"markdown metacharacters in a label title must be escaped, never form a live link/emphasis")
	})

	t.Run("whitespace-only label title is dropped, not counted as a blank slot", func(t *testing.T) {
		// escapeCardText's oneLine() trims a whitespace-only title to "" (unlike a
		// lone backtick, which cardMarkdownEscaper *escapes* to `\`` rather than
		// stripping — it does not go blank). cappedFactValue must drop titles that
		// come out blank, from both the joined value and the (N) count (PR #610
		// review, mochashanyao P2: verifies this documented behavior explicitly).
		card := glMergeRequestCardFrom(t, `{
			"user": {"username": "carol"},
			"object_attributes": {"iid": 1, "title": "x", "url": "https://gitlab.com/o/r/-/merge_requests/1", "action": "open"},
			"labels": [{"title": "backend"}, {"title": "   "}],
			"project": {"path_with_namespace": "o/r"}
		}`)
		require.NotNil(t, card)
		require.NoError(t, validateVCSCard(card))
		plain := cardmsg.BuildPlain(card)
		assert.Contains(t, plain, "Labels (1): backend", "blank title is dropped from both the list and the (N) count")
	})
}

// TestAllVCSCardBuilders_Smoke exercises every event-specific builder so a regression
// in any one is caught: card is non-nil, passes the authoritative validator, derives
// non-empty plain, carries the expected headline, and uses the shared Forge anatomy.
func TestAllVCSCardBuilders_Smoke(t *testing.T) {
	cases := []struct {
		name     string
		build    func(t *testing.T) map[string]interface{}
		headline string
		source   string
		badge    string
	}{
		{"gh push", func(t *testing.T) map[string]interface{} {
			var ev ghPushEvent
			require.NoError(t, json.Unmarshal([]byte(`{"ref":"refs/heads/main","commits":[{"id":"abc1234","message":"ship"}],"repository":{"full_name":"o/r","html_url":"https://github.com/o/r"},"sender":{"login":"amy"}}`), &ev))
			return buildGitHubPushCard(&ev, "en-US")
		}, "amy pushed 1 commit(s)", "GitHub", "Push"},
		{"gh pull_request opened", func(t *testing.T) map[string]interface{} {
			var ev ghPullRequestEvent
			require.NoError(t, json.Unmarshal([]byte(`{"action":"opened","pull_request":{"number":9,"title":"Add cards","html_url":"https://github.com/o/r/pull/9"},"repository":{"full_name":"o/r"},"sender":{"login":"alice"}}`), &ev))
			return buildGitHubPullRequestCard(&ev, "en-US")
		}, "alice opened a pull request", "GitHub", "Pull Request"},
		{"gh issues closed", func(t *testing.T) map[string]interface{} {
			var ev ghIssuesEvent
			require.NoError(t, json.Unmarshal([]byte(`{"action":"closed","issue":{"number":3,"title":"Bug","html_url":"https://github.com/o/r/issues/3"},"repository":{"full_name":"o/r"},"sender":{"login":"bob"}}`), &ev))
			return buildGitHubIssuesCard(&ev, "en-US")
		}, "bob closed an issue", "GitHub", "Issue"},
		{"gh issue comment", func(t *testing.T) map[string]interface{} {
			var ev ghIssueCommentEvent
			require.NoError(t, json.Unmarshal([]byte(`{"action":"created","issue":{"number":4,"title":"Bug"},"comment":{"body":"lgtm","html_url":"https://github.com/o/r/issues/4#issuecomment-1"},"repository":{"full_name":"o/r"},"sender":{"login":"bea"}}`), &ev))
			return buildGitHubIssueCommentCard(&ev, "en-US")
		}, "bea commented on an issue", "GitHub", "Comment"},
		{"gh release published", func(t *testing.T) map[string]interface{} {
			var ev ghReleaseEvent
			require.NoError(t, json.Unmarshal([]byte(`{"action":"published","release":{"tag_name":"v1.2.0","name":"v1.2.0","html_url":"https://github.com/o/r/releases/v1.2.0"},"repository":{"full_name":"o/r"},"sender":{"login":"carol"}}`), &ev))
			return buildGitHubReleaseCard(&ev, "en-US")
		}, "carol published a release", "GitHub", "Release"},
		{"gl push", func(t *testing.T) map[string]interface{} {
			var ev glPushEvent
			require.NoError(t, json.Unmarshal([]byte(`{"ref":"refs/heads/main","commits":[{"id":"abc123","message":"ship"}],"user_username":"dana","project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"}}`), &ev))
			return buildGitLabPushCard(&ev, "en-US")
		}, "dana pushed 1 commit(s)", "GitLab", "Push"},
		{"gl tag push", func(t *testing.T) map[string]interface{} {
			var ev glPushEvent
			require.NoError(t, json.Unmarshal([]byte(`{"ref":"refs/tags/v2.0.0","after":"abc123","user_username":"dave","project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"}}`), &ev))
			return buildGitLabTagPushCard(&ev, "en-US")
		}, "dave pushed tag", "GitLab", "Tag"},
		{"gl merge request", func(t *testing.T) map[string]interface{} {
			var ev glMergeRequestEvent
			require.NoError(t, json.Unmarshal([]byte(`{"user":{"username":"erin"},"object_attributes":{"iid":5,"title":"Refactor","action":"open","url":"https://gitlab.com/o/r/-/merge_requests/5"},"project":{"path_with_namespace":"o/r"}}`), &ev))
			return buildGitLabMergeRequestCard(&ev, "en-US")
		}, "erin opened a merge request", "GitLab", "Merge Request"},
		{"gl issue", func(t *testing.T) map[string]interface{} {
			var ev glIssueEvent
			require.NoError(t, json.Unmarshal([]byte(`{"user":{"username":"finn"},"object_attributes":{"iid":6,"title":"Bug","action":"open","url":"https://gitlab.com/o/r/-/issues/6"},"project":{"path_with_namespace":"o/r"}}`), &ev))
			return buildGitLabIssueCard(&ev, "en-US")
		}, "finn opened an issue", "GitLab", "Issue"},
		{"gl note on MR", func(t *testing.T) map[string]interface{} {
			var ev glNoteEvent
			require.NoError(t, json.Unmarshal([]byte(`{"user":{"username":"erin"},"object_attributes":{"note":"lgtm","noteable_type":"MergeRequest","url":"https://gitlab.com/o/r/-/merge_requests/5#note_1"},"merge_request":{"iid":5,"title":"Refactor"},"project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"}}`), &ev))
			return buildGitLabNoteCard(&ev, "en-US")
		}, "erin commented", "GitLab", "Comment"},
		{"gl pipeline", func(t *testing.T) map[string]interface{} {
			var ev glPipelineEvent
			require.NoError(t, json.Unmarshal([]byte(`{"object_attributes":{"id":7,"status":"success","ref":"main"},"project":{"path_with_namespace":"o/r","web_url":"https://gitlab.com/o/r"}}`), &ev))
			return buildGitLabPipelineCard(&ev, "en-US")
		}, "Pipeline #7", "GitLab", "success"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			card := c.build(t)
			require.NotNil(t, card)
			require.NoError(t, validateVCSCard(card), "server-built card must pass cardmsg.Validate")
			assert.NotEmpty(t, cardmsg.BuildPlain(card))
			assert.Contains(t, cardBodyText(card), c.headline)
			assert.Equal(t, c.source, cardNodeByID(card, "vcs-source-label")["text"])
			assert.Equal(t, c.badge, cardNodeByID(card, "vcs-event-badge")["text"])
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
		headlineNode := cardNodeByID(card, "vcs-headline")
		require.NotNil(t, headlineNode)
		headline, _ := headlineNode["text"].(string)
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

	t.Run("missing semantic content projection → text", func(t *testing.T) {
		card := map[string]interface{}{
			"type": "AdaptiveCard", "version": cardmsg.CardVersion,
			"body": []interface{}{map[string]interface{}{"type": "TextBlock", "text": "headline"}},
		}
		require.NoError(t, validateVCSCard(card), "precondition: the generic card shape is valid")
		req := vcsPushReq("fallback text", card)
		assert.Empty(t, req.MsgType, "a malformed server-authored VCS anatomy must not reach the card path")
		assert.Equal(t, "fallback text", req.Content)
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
		// missing action field → still skip (the one remaining filter, now that
		// action-based filtering is removed)
		_, skip, _ := parseGitHubPush(ghHeader("pull_request"), []byte(`{}`))
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
	assert.NotContains(t, payload, "render_profile", "generic native card requests retain legacy rendering unless their own contract opts in")
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

func TestVCSCardEnvelopeSelectsForgeWithoutExposingCallerOverride(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "1")
	body := []byte(`{
		"ref":"refs/heads/main",
		"commits":[{"id":"abc1234","message":"ship"}],
		"repository":{"full_name":"o/r","html_url":"https://github.com/o/r"},
		"sender":{"login":"alice"}
	}`)
	req, skip, invalid := parseGitHubPush(ghHeader("push"), body)
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	require.Equal(t, msgTypeCard, req.MsgType)

	m := &incomingWebhookModel{WebhookID: "iwh_vcs", SpaceID: "sp_1", Name: "VCS"}
	payload, err := buildCardPayload(m, req, false)
	require.NoError(t, err)
	assert.Equal(t, cardmsg.ProfileV1, payload["profile"])
	assert.Equal(t, cardmsg.RenderProfileOctoChatV1, payload["render_profile"])
	require.NoError(t, cardmsg.Validate(payload))
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"render_profile":"octo-chat/v1"`)

	t.Run("native JSON cannot author render profile", func(t *testing.T) {
		cardJSON, err := json.Marshal(req.Card)
		require.NoError(t, err)
		var native pushPayloadReq
		require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(
			`{"msg_type":"card","render_profile":"octo-chat/v1","plain_projection":{"body":[{"type":"TextBlock","text":"forged"}]},"card":%s}`,
			cardJSON,
		)), &native))
		assert.Nil(t, native.plainProjection, "native JSON cannot select a server-owned plain projection")
		nativePayload, err := buildCardPayload(m, &native, false)
		require.NoError(t, err)
		assert.NotContains(t, nativePayload, "render_profile")
		assert.NotEqual(t, "forged", nativePayload["plain"])
	})
}

func TestVCSCardEnvelopePlainStartsWithEventHeadlineAndKeepsContext(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "1")
	body := []byte(`{
		"object_attributes":{"id":21511,"status":"success","ref":"develop"},
		"project":{"path_with_namespace":"dmwork/octo-server","web_url":"https://gitlab.com/dmwork/octo-server"}
	}`)
	req, skip, invalid := parseGitLabPush(glHeader("Pipeline Hook"), body)
	require.NotNil(t, req, "skip=%q invalid=%q", skip, invalid)
	require.Equal(t, msgTypeCard, req.MsgType)

	payload, err := buildCardPayload(
		&incomingWebhookModel{WebhookID: "iwh_vcs", SpaceID: "sp_1", Name: "VCS"},
		req,
		false,
	)
	require.NoError(t, err)
	assert.Equal(t, "Pipeline #21511\ndmwork/octo-server\nBranch: develop\nStatus: success", payload["plain"],
		"plain must start with the event headline, retain repository context, and exclude decorative header content")
}

func TestVCSViewLabel(t *testing.T) {
	assert.Equal(t, "View on GitHub", vcsViewLabel(cardSourceGitHub, "en-US"))
	assert.Equal(t, "在 GitHub 查看", vcsViewLabel(cardSourceGitHub, "zh-CN"))
	assert.Equal(t, "在 GitLab 查看", vcsViewLabel(cardSourceGitLab, "zh-Hans"))
}
