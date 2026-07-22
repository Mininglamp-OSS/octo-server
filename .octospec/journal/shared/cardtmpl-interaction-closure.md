---
type: Journal
title: "Journal: cardtmpl-interaction-closure"
description: Closed the post-#633 interactive-card loop. One commit ships CardUpdater (authoritative ReplaceView + progress-frame Append over the existing CardMutator CAS/revision/CMD path), a route-versioned callback envelope (legacy default, opt-in octo-card-v1), docs.access-request@0.3.0 with an approved/rejected result view registered beside the frozen 0.2.0, and two bounded Prometheus counters. In-flight 0.2.0 pending cards are authoritatively upgraded to 0.3.0/result in place; missing decorative fields are omitted, not fabricated.
tags: ["cardtmpl", "platform", "docs-access-request", "wire-contract", "trust-boundary", "callback", "card-update", "auth", "space", "i18n", "metrics", "testing"]
timestamp: 2026-07-22T16:40:00+08:00
# --- octospec extension fields ---
task: cardtmpl-interaction-closure
upstream: octo-server#633
source: self
---

# Journal: cardtmpl-interaction-closure

## What was done

One commit (`4336a01c`) on `cardtmpl-interaction-closure` closing roadmap
group A — the "read-only notification → interactive terminal" gap left open by
#633. Four tightly-coupled pieces:

1. **A1 `CardUpdater`** (`pkg/cardtmpl/updater.go`)
   - `ReplaceView`: authoritative pending→result re-render via
     `Registry.RenderCard`, persisted through the existing
     `carddispatch.CardMutator`. Message ID / channel binding / sender / Space
     / `card_seq` come from the stored message + action event, never callback
     display fields. Returns typed render/mutation errors with no fallback
     write.
   - `Append`: reads the *effective* card (`message_extra.content_edit` when
     present, else original payload), appends exactly one JSON object to
     `card.body`, re-runs `cardmsg.Validate/Finalize`, and mutates through the
     same path. Enforces a strictly consecutive `card_seq`
     (`target.CardSeq == snapshot.CardSeq + 1`). Non-object elements, non-card
     sources, stale sequences, and revoked/deleted targets fail closed.
   - Both compose `CardMutator` (CAS + idempotent replay + best-effort
     revision append + CMD sync); no new write transport was created.

2. **A2 standardized callback envelope** (`internal/cardactiondispatch`)
   - Routes default to the legacy flat `DecisionRequest` body (byte-compatible;
     queue rows created before this change still decode). A validated route
     option `octo-card-v1` selects the nested envelope: `protocol`,
     `type=card.action`, `action.{id,data,inputs}`,
     `card.{template_id,template_version,view,message_id,channel_id,channel_type}`,
     `actor.uid`, opaque `trigger_id` derived from the durable event ID. HMAC
     continues to cover the exact request body.
   - `response_url` is deliberately NOT emitted — §7 defines no authenticated
     response request body, so no unusable URL is published.
   - Ingress (`modules/message/api_card_action.go`
     `resolveRegistryCardContext`) enriches the internal event only from the
     effective server-authored frame + Registry: template identity from
     `metadata.octo.template`, view from `Registry.ActionView`. Corrupt/partial
     metadata or an action not declared for the resolved view fails closed
     *before* enqueue; legacy cards without template metadata retain the
     existing zero-context path.

3. **A3 `docs.access-request@0.3.0`** (`pkg/cardtmpl/docs_access_request/v3.go`)
   - New `result` view (`approved`/`rejected`, `octo/v1`) registered *beside*
     the frozen `0.2.0`. `pending` view stays `octo/v2` with the same action
     structure; the interaction report only *adds* optional `Action.data`
     context keys (avatar/reason/time/permission/source) — a purely additive,
     backward-compatible evolution.
   - The docs finalizer (`modules/notify/action_finalizer.go`) now calls
     `CardUpdater.ReplaceView` with target version `0.3.0` for
     approved/denied; `cancelled`/`unavailable` stay on the legacy fallback
     (`0.3.0` does not declare those states). Denial reason is rune-bounded
     before rendering.
   - The `0.2.0` handoff tree has **zero diff** vs merge commit `2917e6a6`.

4. **A4 bounded metrics** (`pkg/cardtmpl/metrics.go`)
   - `dmwork_cardtmpl_callback_total{template_id,version,action_id,result=ok|rejected|error}`
     and `dmwork_cardtmpl_update_total{template_id,version,result=ok|error}`.
     Labels come only from registered metadata + declared interactions — no
     UID, message ID, doc ID, URL, arbitrary input, or error text.

## Structural learning

- **Published L1 versions are frozen; evolution is a new version registered
  side-by-side.** `0.2.0` and `0.3.0` are both live; `SetDefault` moved new
  sends to `0.3.0` only after its register-time self-check passed (fail-close:
  a failing self-check panics `main.go` at boot). An in-flight `0.2.0/pending`
  message is authoritatively replaced by `0.3.0/result` — message ID and
  channel binding stay, only `metadata.octo.template.version` changes. This is
  the reusable shape for every future card version bump.
- **`CardUpdater` composes the mutator, it is not a second write path.** All
  authority (sender, message ID, channel, Space, `card_seq`) is re-derived from
  stored state inside `CardMutator`; the updater only supplies rendered content
  + the target coordinates. Callback display fields never influence the write.

## Gotchas worth remembering

- **`card_seq` for the docs `ReplaceView` is sourced from `event.EventID`**
  (`action_finalizer.go`). This leans on the durable event ID being
  monotonic to satisfy the mutator's `card_seq` CAS ordering. It holds for the
  current snowflake-style IDs, but it is an *implicit* contract: swapping the
  event-ID generator for a non-monotonic scheme would silently break update
  ordering. Promoted to a pending learning.
- **Docs uses the in-process finalizer, which does NOT go through the HTTP
  callback envelope.** A2's `legacy`/`octo-card-v1` route option only affects
  external server-to-server HTTP consumers. Do not assume the envelope format
  changes the docs approval path — that path is `DocsActionFinalizer.Finalize`
  called directly by the dispatcher.
- **Two label vocabularies still exist** (pilot `resultLabels` + notify
  `docsLabelsFor`); A3 added a third copy for the result view. Bounded by tests
  today; folding into an i18n locale source is deferred to roadmap C (G4).

## Verification (this run)

Locally green: `go vet` (focused), `pkg/cardtmpl` `-race -cover`
(80.5% / 85.8%), `internal/cardactiondispatch` envelope, `pkg/cardmsg`
template-context, `internal/carddispatch` mutation CAS, `modules/notify`
finalizer + v3 result, `modules/message` `resolveRegistryCardContext`,
`make i18n-extract-check`, `make i18n-lint`.

Not run here, with an honest note on where they run:
- `golangci-lint` — not installed on this host; the CI Vet/lint lane covers it.
- The `//go:build integration` card-action orchestration in `modules/message`
  (`TestCardActionEndToEndAndIdempotency`, `…TrustModel`, `…SenderBound…`, etc.)
  runs neither here nor in CI. Under `-tags integration` the `message` test
  binary hits a **compile-time import cycle** (message test → app_bot →
  bot_api → messages_search → message), and the CI test lane deliberately runs
  the DEFAULT build **without** `-tags integration` (`ci.yml`, ref #557 — the
  integration suite needs a separately-provisioned `conv_ext_test` DB). Per the
  in-source note, that path's D6/D9 CAS is covered by `modules/bot_api` IM
  cases; the `message` e2e files require the dmworkim `make env-test`
  environment to execute. A's own `message`-level regression coverage here is
  the non-integration `TestResolveRegistryCardContextUsesEffectiveMetadataAndReport`,
  which ran green in the default lane. The pre-existing orchestration e2e was
  therefore NOT re-verified in this finish.
