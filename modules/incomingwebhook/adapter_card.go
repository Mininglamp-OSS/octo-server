package incomingwebhook

// InteractiveCard(=17) rendering for the VCS event adapters (github / gitlab) —
// spec: .octospec/tasks/webhook-cardmsg-adapter/brief.md.
//
// When OCTO_CARD_MESSAGE_ENABLED is on, github/gitlab events render as an octo/v1
// card (structured header + body + one Action.OpenUrl); when it is off — or when a
// built card fails self-validation — they degrade to the existing markdown text
// path (bytes unchanged). We are the PRODUCER here (unlike the native
// msg_type:"card" path which takes caller AC JSON), so every external leaf is
// escaped at the card TextBlock and every URL goes through cardmsg's positive
// http(s) allowlist. The two adapters share ONE escaper + ONE card anatomy so a
// defense added for github holds identically for gitlab (trust-boundary: escape at
// the leaf + adapter parity).

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"go.uber.org/zap"
)

const (
	// cardSourceGitHub / cardSourceGitLab feed metadata.octo.source.label and the
	// localized "View on {source}" button title.
	cardSourceGitHub = "GitHub"
	cardSourceGitLab = "GitLab"

	// cardActorMax bounds an actor display name before it enters the headline —
	// same rune clamp intent as the text path's actor handling.
	cardActorMax = 64
	// cardRefMax / cardTitleMax / cardCommitMsgMax bound the external ref / title /
	// commit-message leaves (mirror the text path's clamps).
	cardRefMax       = 200
	cardTitleMax     = 200
	cardCommitMsgMax = 120
	cardShaMax       = 16
	cardQuoteMax     = 300
)

// cardMarkdownEscaper mirrors pkg/cardtmpl.escapeMarkdown: it neutralizes every AC
// markdown metacharacter so external VCS text (actor, repo, PR/issue titles, commit
// messages, comment snippets) renders LITERALLY in a TextBlock leaf — it can never
// re-open emphasis, forge a link, or activate an autolink/HTML span. This is the
// card-side twin of the text path's mdInertText; both adapters use only this one
// escaper for card leaves (trust-boundary: escape at the leaf + parity).
var cardMarkdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	`*`, `\*`,
	`_`, `\_`,
	`[`, `\[`,
	`]`, `\]`,
	`(`, `\(`,
	`)`, `\)`,
	`<`, `\<`,
	`>`, `\>`,
	"`", "\\`",
	`#`, `\#`,
	`~`, `\~`,
	`|`, `\|`,
	// `!` guards image syntax `![alt](url)`: cardmsg.Validate already allowlists any
	// image destination (defence in depth), but escaping `!` stops the image node
	// from ever forming (PR #596 review, mochashanyao nit).
	`!`, `\!`,
)

// escapeCardText clamps external text to max runes (single line), escapes inline
// markdown, then neutralizes a leading block marker — safe for placement at a card
// TextBlock / Fact line start.
func escapeCardText(s string, max int) string {
	return neutralizeLeadingBlockMarker(cardMarkdownEscaper.Replace(clipRunes(oneLine(s), max)))
}

// neutralizeLeadingBlockMarker backslash-escapes a leading markdown BLOCK marker so
// external text placed at a TextBlock/Fact line start cannot open a bullet/ordered
// list or a thematic break. octo-web's CardMarkdown renders ul/ol/li (and <hr>), and
// these markers are NOT covered by cardMarkdownEscaper — it only neutralizes INLINE
// markers (`* _ [ ] ( ) < > \` # ~ |`); the ordered `)` marker and `*`/`_` are
// already escaped there, leaving `-`, `+`, and ordered `N.` as the injection surface
// (e.g. a hostile issue-comment body `1. Deploy approved` forging a numbered list in
// a trusted-webhook card). Operates on the already-inline-escaped string; a leading
// backslash/other char is a no-op. stripMarkdown unescapes for the authoritative
// plain, so the literal marker still shows in search/quote text.
func neutralizeLeadingBlockMarker(s string) string {
	if s == "" {
		return s
	}
	// Bullet list markers (`- x`, `+ x`) and dash thematic breaks (`---`): a leading
	// backslash makes the char literal and the line neither a list nor an <hr>.
	if s[0] == '-' || s[0] == '+' {
		return `\` + s
	}
	// Ordered list marker `N.` followed by whitespace/end — escape the dot. (`N)` is
	// already neutralized because `)` is inline-escaped above.)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s) && s[i] == '.' {
		if rest := s[i+1:]; rest == "" || rest[0] == ' ' || rest[0] == '\t' {
			return s[:i] + `\.` + rest
		}
	}
	return s
}

// cardCodeSpan renders an external short token (branch / tag / short SHA) as a
// markdown code span. Backticks are stripped from the content (a single-backtick
// span cannot safely escape a backtick), and inside a code span AC does not
// interpret markdown, so the token shows verbatim. Empty content yields "" (no
// empty span).
func cardCodeSpan(s string, max int) string {
	inner := mdCodeSpanText(s, max)
	if inner == "" {
		return ""
	}
	return "`" + inner + "`"
}

// httpURLForCard returns raw when it is an absolute http(s) URL with a host, else
// "". Mirrors cardmsg.checkURL's positive allowlist so the adapter can DEGRADE a
// bad/missing link by omitting the button (the card stays valid) rather than failing
// validation — the primary navigation is always a structured Action.OpenUrl, never a
// markdown destination the caller could break out of.
func httpURLForCard(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if s := strings.ToLower(u.Scheme); (s == "http" || s == "https") && u.Host != "" {
		return raw
	}
	return ""
}

// vcsCardData is the neutral card model BOTH adapters fill — one anatomy keeps the
// github and gitlab cards structurally identical (parity). All string fields are
// already-safe markdown (composed with escapeCardText / cardCodeSpan); the assembler
// does not re-escape.
type vcsCardData struct {
	source     string    // "GitHub" / "GitLab": metadata.octo.source.label + button label
	variant    string    // metadata.octo.variant, e.g. "vcs.github.push"
	badge      string    // fixed event label, or an escaped pipeline status
	badgeColor string    // optional semantic color for the pipeline status badge
	headline   string    // bold title line in the neutral content surface
	subtitle   string    // subtle repository/project context in the header; "" omits it
	lines      []string  // body lines (commit list / "#N · title"); each its own TextBlock
	facts      []vcsFact // labeled metadata rows (pipeline Branch/Status/Duration/Jobs); "" omits the FactSet
	quote      string    // comment snippet; "" omits it; rendered in an emphasis Container
	url        string    // Action.OpenUrl target (already http(s)-validated); "" omits the footer
}

// vcsFact is one FactSet row (title/value already-safe markdown; the assembler does
// not re-escape).
type vcsFact struct {
	title string
	value string
}

// card assembles the octo/v1 AdaptiveCard object (the value that becomes req.Card;
// handlePush's msgTypeCard branch wraps it in the type-17 envelope and runs
// cardmsg.Validate/Finalize). Uses only whitelisted elements: TextBlock, Container,
// ActionSet(Action.OpenUrl).
func (d vcsCardData) card(lang string) map[string]interface{} {
	headline := map[string]interface{}{
		"type": "TextBlock", "id": "vcs-headline", "text": d.headline,
		"weight": "Bolder", "wrap": true,
	}
	contentItems := []interface{}{headline}
	for _, ln := range d.lines {
		if ln == "" {
			continue
		}
		contentItems = append(contentItems, map[string]interface{}{
			"type": "TextBlock", "text": ln, "wrap": true, "spacing": "Small",
		})
	}
	if len(d.facts) > 0 {
		facts := make([]interface{}, 0, len(d.facts))
		for _, f := range d.facts {
			facts = append(facts, map[string]interface{}{"title": f.title, "value": f.value})
		}
		contentItems = append(contentItems, map[string]interface{}{"type": "FactSet", "facts": facts})
	}
	if d.quote != "" {
		contentItems = append(contentItems, map[string]interface{}{
			"type": "Container", "style": "emphasis",
			"items": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": d.quote, "wrap": true, "isSubtle": true},
			},
		})
	}

	headerColumns := []interface{}{
		map[string]interface{}{
			"type": "Column", "width": "auto",
			"items": []interface{}{map[string]interface{}{
				"type": "Image", "id": "vcs-source-icon", "url": cardmsg.VCSGitBranchIcon,
				"width": "16px", "height": "16px", "spacing": "None",
			}},
		},
		map[string]interface{}{
			"type": "Column", "width": "stretch", "spacing": "Small",
			"items": vcsHeaderTextItems(d.source, d.subtitle),
		},
	}
	if d.badge != "" {
		badge := map[string]interface{}{
			"type": "TextBlock", "id": "vcs-event-badge", "text": d.badge,
			"size": "Small", "weight": "Bolder", "spacing": "None", "wrap": false,
		}
		if d.badgeColor != "" {
			badge["color"] = d.badgeColor
		}
		headerColumns = append(headerColumns, map[string]interface{}{
			"type": "Column", "width": "auto", "spacing": "Small",
			"items": []interface{}{badge},
		})
	}
	body := []interface{}{
		map[string]interface{}{
			"type": "ColumnSet", "id": "vcs-header", "spacing": "None",
			"verticalContentAlignment": "Center", "columns": headerColumns,
		},
		map[string]interface{}{
			"type": "Container", "id": "vcs-content", "style": "accent", "bleed": true,
			"separator": true, "spacing": "Small", "items": contentItems,
		},
	}
	if d.url != "" {
		// Navigation remains one structured Action.OpenUrl. The destination was
		// preflighted by httpURLForCard and is checked again by cardmsg.Validate;
		// it is not duplicated into unvalidated metadata or card.actions.
		body = append(body, map[string]interface{}{
			"type": "Container", "id": "vcs-footer", "style": "emphasis", "bleed": true,
			"separator": true, "spacing": "None",
			"items": []interface{}{map[string]interface{}{
				"type": "ActionSet", "spacing": "None",
				"actions": []interface{}{map[string]interface{}{
					"type": "Action.OpenUrl", "title": vcsViewLabel(d.source, lang), "url": d.url,
				}},
			}},
		})
	}
	metaOcto := map[string]interface{}{"source": map[string]interface{}{"label": d.source}}
	if d.variant != "" {
		metaOcto["variant"] = d.variant
	}
	card := map[string]interface{}{
		"type":     "AdaptiveCard",
		"version":  cardmsg.CardVersion,
		"metadata": map[string]interface{}{"octo": metaOcto},
		"body":     body,
	}
	return card
}

func vcsHeaderTextItems(source, context string) []interface{} {
	items := []interface{}{map[string]interface{}{
		"type": "TextBlock", "id": "vcs-source-label", "text": source,
		"size": "Small", "weight": "Bolder", "spacing": "None", "wrap": false,
	}}
	if context != "" {
		items = append(items, map[string]interface{}{
			"type": "TextBlock", "id": "vcs-source-context", "text": context,
			"size": "Small", "isSubtle": true, "spacing": "None", "wrap": true,
		})
	}
	return items
}

// vcsViewLabel localizes the "View on {GitHub|GitLab}" action title via the
// deployment outbound language (a content label, not an errcode — same discipline as
// modules/notify / pkg/cardtmpl label sets).
func vcsViewLabel(source, lang string) string {
	if isZhLang(lang) {
		return "在 " + source + " 查看"
	}
	return "View on " + source
}

func isZhLang(lang string) bool {
	return strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh")
}

// validateVCSCard runs the authoritative cardmsg.Validate over a minimal type-17
// envelope wrapping card, so a server-built card that is (via a bug) malformed is
// caught HERE and degraded to text — never surfaced as a 400. Envelope-only fields
// (from / space_id) are not needed by Validate (it checks structure / URL / size).
func validateVCSCard(card map[string]interface{}) error {
	return cardmsg.Validate(map[string]interface{}{
		"type":           cardmsg.InteractiveCard.Int(),
		"card":           card,
		"card_version":   cardmsg.CardVersion,
		"profile":        cardmsg.ProfileV1,
		"render_profile": cardmsg.RenderProfileOctoChatV1,
	})
}

// vcsPlainProjection returns the semantic content area as a standalone card-shaped
// projection for authoritative plain derivation. The Forge icon, source label, and
// badge are visual chrome and stay excluded. Repository/project context is retained
// immediately after the event headline so notifications and search results remain
// attributable without letting header chrome displace the headline.
//
// The projection is package-internal and is extracted from the already validated
// server-authored card. Native msg_type:"card" callers cannot select or forge it.
func vcsPlainProjection(card map[string]interface{}) map[string]interface{} {
	body, ok := card["body"].([]interface{})
	if !ok {
		return nil
	}
	for _, raw := range body {
		element, ok := raw.(map[string]interface{})
		if !ok || element["id"] != "vcs-content" {
			continue
		}
		items, ok := element["items"].([]interface{})
		if !ok {
			return nil
		}
		context := vcsHeaderContextForPlain(body)
		if context == nil || len(items) == 0 {
			return map[string]interface{}{"body": items}
		}
		projectionItems := make([]interface{}, 0, len(items)+1)
		projectionItems = append(projectionItems, items[0], context)
		projectionItems = append(projectionItems, items[1:]...)
		return map[string]interface{}{"body": projectionItems}
	}
	return nil
}

func vcsHeaderContextForPlain(body []interface{}) map[string]interface{} {
	for _, raw := range body {
		header, ok := raw.(map[string]interface{})
		if !ok || header["id"] != "vcs-header" {
			continue
		}
		columns, ok := header["columns"].([]interface{})
		if !ok {
			return nil
		}
		for _, rawColumn := range columns {
			column, ok := rawColumn.(map[string]interface{})
			if !ok {
				continue
			}
			items, ok := column["items"].([]interface{})
			if !ok {
				continue
			}
			for _, rawItem := range items {
				item, ok := rawItem.(map[string]interface{})
				if ok && item["id"] == "vcs-source-context" {
					return item
				}
			}
		}
		return nil
	}
	return nil
}

// vcsPushReq picks the pushPayloadReq for a rendered VCS event: a card request when a
// card was built (flag on) AND it self-validates, else the text request (the degrade
// path — flag off OR card build/validate failure). Card plain is derived from the
// server-owned semantic content projection, so no caller-controlled text seed is used.
func vcsPushReq(text string, card map[string]interface{}) *pushPayloadReq {
	if card != nil {
		if err := validateVCSCard(card); err != nil {
			// A server-built card failing self-validation is a card-builder bug (an
			// attacker-controlled field can't reach here un-escaped). We still degrade to
			// text so delivery is preserved, but log it so the regression is visible in
			// production instead of silent (PR #596 review, Jerry-Xin). Response contract
			// unchanged. zap.L() is the package-scope logger (cf. modules/cardtrust).
			zap.L().Warn("incomingwebhook: built VCS card failed self-validation; degrading to text",
				zap.Error(err))
		} else {
			plainProjection := vcsPlainProjection(card)
			if plainProjection == nil {
				zap.L().Warn("incomingwebhook: built VCS card is missing its semantic content projection; degrading to text")
				return &pushPayloadReq{Content: clipRunes(text, maxContentRunes())}
			}
			return &pushPayloadReq{
				MsgType:         msgTypeCard,
				Card:            card,
				renderProfile:   cardmsg.RenderProfileOctoChatV1,
				plainProjection: plainProjection,
			}
		}
	}
	return &pushPayloadReq{Content: clipRunes(text, maxContentRunes())}
}

// maxRenderedJobs bounds how many pipeline job names the "Jobs (N)" fact lists
// before eliding — the (N) count still reflects the true total.
const maxRenderedJobs = 8

// maxRenderedLabels bounds how many GitLab MR/Issue label titles the "Labels (N)"
// fact lists before eliding — same elide convention as maxRenderedJobs.
const maxRenderedLabels = 10

// cardFactItemMax bounds a single Jobs/Labels fact item (job name / label title)
// before it enters the "/"-joined FactSet value. Deliberately its own constant
// rather than reusing cardActorMax: job names and label titles are a different
// domain (project-defined, can legitimately run longer) than an actor display
// name, even though both happened to want 64 today (PR #610 review, yujiawei P2).
const cardFactItemMax = 64

// cappedFactValue builds a capped, "/"-joined FactSet value from raw external name
// strings for GitHub/GitLab PR/MR/Issue Labels. Each name is escaped via
// escapeCardText; blank names (e.g. an empty label title) are dropped before capping
// and counting, so the fact never shows an empty slot or an inflated (N). Returns
// ("", 0) when there is nothing left to show — the caller omits the FactSet row
// entirely in that case.
func cappedFactValue(rawNames []string, max int) (value string, count int) {
	return cappedFactValueWithSeparator(rawNames, max, " / ", " …")
}

// cappedPipelineJobsValue preserves the same filtering, escaping, ordering, cap,
// count, and overflow semantics as Labels, but gives every rendered job its own
// line. The two spaces before each newline are CommonMark's hard-break syntax;
// bare newlines are soft breaks and octo-web's markdown renderer collapses them
// to spaces. It is intentionally pipeline-only so Labels retain their slash
// separator.
func cappedPipelineJobsValue(rawNames []string, max int) (value string, count int) {
	return cappedFactValueWithSeparator(rawNames, max, "  \n", "  \n…")
}

func cappedFactValueWithSeparator(rawNames []string, max int, separator, overflowSuffix string) (value string, count int) {
	names := make([]string, 0, len(rawNames))
	for _, n := range rawNames {
		if v := escapeCardText(n, cardFactItemMax); v != "" {
			names = append(names, v)
		}
	}
	if len(names) == 0 {
		return "", 0
	}
	shown := names
	overflow := len(names) > max
	if overflow {
		shown = names[:max]
	}
	value = strings.Join(shown, separator)
	if overflow {
		value += overflowSuffix
	}
	return value, len(names)
}

// vcsCardLabels are the localized FactSet titles for the GitLab merge_request card's
// Source/Target branch rows and the Labels row shared with the issue card (issues
// have no source/target branch).
type vcsCardLabels struct {
	source, target, labels string
}

func vcsCardLabelsFor(lang string) vcsCardLabels {
	if isZhLang(lang) {
		return vcsCardLabels{source: "源分支", target: "目标分支", labels: "标签"}
	}
	return vcsCardLabels{source: "Source", target: "Target", labels: "Labels"}
}

// pipelineLabels are deliberately language-neutral: product terminology for the
// pipeline card remains English even when the surrounding action title is localized.
type pipelineLabels struct {
	branch, status, duration, jobs string
}

func pipelineLabelsFor(_ string) pipelineLabels {
	return pipelineLabels{branch: "Branch", status: "Status", duration: "Duration", jobs: "Jobs"}
}

// maxPipelineDurationSec sanity-caps the rendered duration. GitLab's
// object_attributes.duration is an unbounded external float64; without a cap a
// hostile/buggy self-hosted instance sending e.g. 1e12 would render an absurd
// "277777777h 46m" string (PR #596 review, yujiawei P2). 100h is far above any real
// pipeline; anything larger renders as the cap.
const maxPipelineDurationSec = 100 * 3600

// formatPipelineDuration renders a pipeline elapsed time (seconds) as a compact,
// language-neutral "1h 2m" / "3m 42s" / "42s". <= 0 (missing / null) → ""; values
// above the sanity cap are clamped and prefixed ">" so a clamped value reads
// distinctly from a genuine ~100h pipeline (PR #610 review, mochashanyao P2).
func formatPipelineDuration(sec int) string {
	if sec <= 0 {
		return ""
	}
	prefix := ""
	if sec > maxPipelineDurationSec {
		sec = maxPipelineDurationSec
		prefix = ">"
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	switch {
	case h > 0:
		return prefix + fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return prefix + fmt.Sprintf("%dm %ds", m, s)
	default:
		return prefix + fmt.Sprintf("%ds", s)
	}
}

// pipelineStatusColor maps a GitLab pipeline status to the compact badge color.
// The headline stays neutral. Unknown → "" (default badge color).
func pipelineStatusColor(status string) string {
	switch status {
	case "success":
		return "Good"
	case "failed":
		return "Attention"
	case "canceled":
		return "Warning"
	case "running", "pending", "created", "waiting_for_resource", "preparing", "scheduled":
		// In-progress-ish statuses only became reachable once the terminal-status
		// filter was removed; without a color they were visually indistinguishable
		// from an unrecognized status (PR #610 review, mochashanyao P2).
		return "Accent"
	}
	return ""
}

// joinShaMsg composes a "`sha` message" body line, trimming to avoid a leading space
// when the sha code span is empty.
func joinShaMsg(sha, msg string) string {
	return strings.TrimSpace(sha + " " + msg)
}

// numberedTitle composes a "<mark>N · <title>" body line (e.g. "#1234 · Add cards"),
// with the external title escaped. mark is a safe literal ("#" for issues/PRs,
// "!" for GitLab MRs).
func numberedTitle(mark string, number int, title string) string {
	t := escapeCardText(title, cardTitleMax)
	if t == "" {
		return fmt.Sprintf("%s%d", mark, number)
	}
	return fmt.Sprintf("%s%d · %s", mark, number, t)
}
