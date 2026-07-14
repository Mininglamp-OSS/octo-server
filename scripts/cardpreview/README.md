# cardpreview — deterministic sample AC JSON generator

`go run ./scripts/cardpreview` prints the three canonical notification card
documents to stdout as `{"summary_completed": {...}, "summary_failed": {...},
"docs_shared": {...}}`. Same code paths as production
(`pkg/cardtmpl.BuildSummaryResourceCard` / `BuildDocsResourceCard`) so the
output tracks whatever the templates emit today — no hand-authored samples to
drift.

Use it to:
- Feed the actual octo-web / mobile AC renderer for a truthful preview
  screenshot (drop the JSON into a fixture, mount the same `renderOctoCard` /
  host config the chat cell uses).
- Regenerate the JSON snippets in `docs/summary-notify-card.md` §2 /
  `docs/docs-notify-card.md` §2 whenever the template shape changes.
- Spot-check the reserved `metadata.octo.variant` / `metadata.octo.source`
  namespace after adding a new Kind or Source label.

```bash
go run ./scripts/cardpreview > /tmp/cards.json
jq . /tmp/cards.json
```

The tool has no runtime dependencies beyond `pkg/cardtmpl` (which itself only
depends on `pkg/cardmsg` for the version/copy-text constants and `pkg/i18n` for
outbound language). To exercise it against octo-web's real renderer, mount
`AdaptiveCards.AdaptiveCard` with the host config from
`packages/dmworkbase/src/Messages/InteractiveCard/sdk/octoHostConfig.ts`, wrap
the mount node in `.wk-interactive-card > .wk-interactive-card-sdk` so
`packages/dmworkbase/src/Messages/InteractiveCard/index.css` styles apply, and
install a markdown processor on `AdaptiveCard.onProcessMarkdown` (the app uses
`cardMarkdownToSafeHtml`; a stock CommonMark parser produces byte-equivalent
output for the whitelist subset we ship).
