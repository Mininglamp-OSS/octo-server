# Change log

Change history for this repo's `.octospec/`, following the
[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
change-log convention (§7). Newest first.

## 2026-06-24

- **Add** — Task `incomingwebhook-webhooks-alias` (#455): `/v1/webhooks/{id}/{token}`
  push-route alias for the canonical `/v1/incoming-webhooks/...` (native + 5
  adapters), reusing the identical middleware chain. Generalized `pkg/accesslog`
  token scrubbing (`ScrubPath` + panic-dump regex) to mask BOTH prefixes (#246
  parity). Brief + context under `.octospec/tasks/incomingwebhook-webhooks-alias/`,
  journal `.octospec/journal/shared/incomingwebhook-webhooks-alias.md`.
- **Add** — Task `incoming-webhook-mention-broadcast` (#448 item ②): broadcast-pill
  auto-compose on the native incoming-webhook push endpoint. When a permitted
  `mention.all`/`mention.bots` is set, the server prepends the canonical broadcast
  literal (`@所有人`/`@所有AI`) + a space to the text content so all three clients
  render a pill; directed-entity (#449) offsets shift by the prefix's UTF-16
  length. Text-path only; routing / red-dot / bot-summon unchanged. Brief +
  context + journal under `.octospec/tasks/incoming-webhook-mention-broadcast/`
  and `.octospec/journal/shared/incoming-webhook-mention-broadcast.md`.
- **Add** — Task `incoming-webhook-mention-directed-render` (#448 item ① b):
  opt-in server-side directed @mention name-resolution. `mention.render:true`
  resolves each member uid → `user.name`, prepends `@<name> ` to text content, and
  generates the UTF-16 `mention.entities`. Refactored the broadcast compose into one
  `composeMentionContent`. Adversarial review added a forged-broadcast guard (skip
  names that are broadcast labels or contain `@`), incremental budget tracking, and
  cap/iOS/byte-size docs. Ships in the same PR as the broadcast half (#450) → the
  two close #448. Brief + context + journal under
  `.octospec/tasks/incoming-webhook-mention-directed-render/` and
  `.octospec/journal/shared/incoming-webhook-mention-directed-render.md`.

## 2026-06-23

- **Add** — Task `upstream-dep-metrics` (#440 P0-a): upstream-dependency
  observability. Added `dmwork_dependency_duration_seconds` (object-storage
  `DownloadURL` latency) and connection-pool metrics (`go_sql_*` via
  DBStatsCollector + `dmwork_redis_pool_*` via a scrape-time collector). No
  background goroutine, no `octo-lib` change, no business-logic change. Brief +
  context + journal under `.octospec/tasks/upstream-dep-metrics/` and
  `.octospec/journal/shared/upstream-dep-metrics.md`.

## 2026-06-19

- **Update** — Adopted OKF v0.1 compatible frontmatter across all repo rules
  (`commit-style`, `error-handling`, `rate-limit`, `space-isolation`,
  `testing`): added `type`, `title`, `description`, `tags`, `timestamp`. The
  octospec orchestration fields are retained as OKF extension fields.
- **Update** — Bumped global inheritance pin to `octo-spec@1.1.0`.
- **Creation** — Added `.octospec/index.md` (human-readable rule catalog) and
  this `.octospec/log.md` change log.

## 2026-06-18

- **Creation** — octospec pilot scaffolding: rules `error-handling`,
  `rate-limit`, `space-isolation`, `testing`, `commit-style`; manifest, task
  templates, slash commands (PR #418).
- **Creation** — Dogfood task `member-list-name-fallback` (#344 → PR #420).

## 2026-06-19 (tooling)

- **Update** — Synced OKF-aware slash commands, workflow skill, and task brief
  template from octo-spec 1.1.0 so generated briefs/journals stay conformant.
