---
type: Journal
title: "Journal: card-action-internal-http-actions"
description: Follow-up to #588 — allow plain HTTP callbacks and 1-5 bounded custom actions on approval_card without weakening the server-owned card boundary.
tags: ["card", "internal", "trust-boundary", "wire-contract", "testing"]
timestamp: 2026-07-16T02:00:00+08:00
# --- octospec extension fields ---
task: card-action-internal-http-actions
source: self
---

# Journal: card-action-internal-http-actions

## What was done

Two small follow-ups to #588 plus one bundled configuration collapse:

- `internal/cardactiondispatch` — `validateCallbackURL` now accepts both
  `http://` and `https://`; the static `OCTO_CARD_ACTION_ROUTES[].url` list is
  itself the exact allowlist. Reject-cases were tightened at the same time:
  `u.Hostname() != ""` (blocks `http://:8080/x`), `u.ForceQuery` (blocks
  trailing `?`), raw-`#` prefilter (blocks trailing `#` and embedded `#` that
  `url.Parse` would swallow), unsupported schemes, credentials, opaque forms.
  `NewRegistry` is now two-argument; `LoadAllowedURLs` was deleted.
- `main.go` — reads `OCTO_CARD_ACTION_ALLOWED_URLS` only to emit one structured
  deprecation WARN (`deprecated_env=OCTO_CARD_ACTION_ALLOWED_URLS`), then
  ignores it. Rolling upgrades that still carry the variable do not fail
  startup.
- `pkg/cardtmpl` — added `ApprovalRequestAction` and an optional
  `ApprovalRequestCard.Actions` slice. When `Actions == nil` the byte-for-byte
  legacy `approve/deny` output stays intact (golden-bytes test). When non-nil,
  1..5 buttons are rendered with server-derived action IDs
  (`approval-<decision>`), owner/action_type/decision injected into every
  submit payload, and `data` reserved-key checks unchanged. A non-nil empty
  slice is treated as a caller bug, not a silent fallback. Titles are checked
  for control characters on the raw string before `TrimSpace` runs.
- `modules/notify` — added `ApprovalCardFields.Actions` (`json:"actions,omitempty"`)
  and `ApprovalCardAction`. Delivery routes nil to the localized approve/deny
  path; any non-nil slice enters the strict validator.
- Contract-critical tests are consumer-neutral: 7 HTTP-scheme acceptance
  shapes, 13 URL rejection shapes, plain-HTTP HMAC-and-transport E2E, plain-HTTP
  redirect rejection with typed `redirect_rejected` category, and a
  custom-decision orchestration E2E (`approval-execute` → HMAC callback →
  consumer maps to `approved` → standard finalizer renders standard terminal
  wording → requester notification).

Locked wire-contract decisions (drawn from the discussion draft, not from any
one owner):

- Actions optional; when present 1-5 items, `decision` matches
  `^[a-z][a-z0-9_.-]{0,47}$`, `title` is 1-80 runes, no control characters,
  unique decisions per card, no mixing with approve/deny defaults.
- Terminal states unchanged: `approved`/`denied`/`cancelled`/`pending`.
  Consumers map their custom `decision` to a state — the server does not
  translate.
- Only `approved`/`denied` notify the requester, matching #588. Custom
  `decision` values never trigger direct notification.
- Caller-provided button i18n is single-copy per request; per-recipient
  language rendering is explicitly out of scope for this change.

## Load-bearing intent

- **URL is operator-authorized, not shape-recognized.** The presence of
  `http://` in a static route is the authorization. octo-server does not
  inspect hostnames (K8s DNS, docker service names, `host.docker.internal`,
  IP literals). Reachability is a NetworkPolicy/service-mesh concern; scheme
  choice is a confidentiality decision the operator makes explicitly.
- **Config is single-source.** `OCTO_CARD_ACTION_ROUTES` is the exact URL
  registry. `OCTO_CARD_ACTION_ALLOWED_URLS` shared the same ConfigMap boundary,
  produced dual-write drift, and gave no independent review — hence removed.
- **Card content is server-built.** Callers pass `decision` + display `title`;
  the server owns action IDs, injects reserved metadata, and remains the only
  code path that can add a URL/style/input. `data` reserved-key checks
  (`owner`/`action_type`/`decision`) still apply.
- **Nil ≠ empty.** `Actions == nil` means "omitted, use defaults." An explicit
  `"actions": []` is a caller bug and is rejected at 400, not silently
  downgraded. This closes a subtle bypass where a caller intending zero
  buttons would get the localized approve/deny anyway.
- **Terminal contract unchanged.** No new callback result states, no
  per-action data/URL/input/style, no arbitrary card JSON, and no
  custom-decision path into requester notification. The wire contract from
  #588 is deliberately not widened.
- **HMAC does not encrypt.** Both docs (`callback-dispatch.md`,
  `callback-consumer.md`) now carry an explicit residual-risk callout for
  plain-HTTP transport and for the absence of independently-signed callback
  responses.

## Deferred (out of scope, tracked)

- Independently-signed callback responses. HTTP transport is still
  fail-closed on the request signature; response tampering remains a residual
  risk.
- Custom `outcome_label` / business terminal wording. Standard approval
  wording (`approval.approved` / `denied` / `cancelled`) is what renders
  regardless of the custom decision.
- Non-terminal "still clickable" actions. Every finalizer still removes the
  buttons on any typed response; `pending` shows the standard "unavailable"
  terminal wording.
- Per-recipient language rendering of caller-supplied button titles.
- `OCTO_DOCS_APPROVAL_CARD_ENABLED` behavior. Kept as-is; separate rollout.

## Injected rule compliance

- **error-handling**: no new response paths; `/v1/internal/notify` already
  goes through the localized envelope. Card-build failures still surface as
  the existing `errNotifyCardInvalid`. Custom-decision validation errors
  reject at 400 through the same route.
- **rate-limit**: no route, middleware, or Redis frequency-counter change.
- **wire-contract**: an unset optional field on the JSON boundary preserves
  legacy output byte-for-byte (`TestBuildApprovalRequestCardOmittedActionsIsByteCompatible`
  pins golden bytes).
- **testing**: focused unit + race + integration + a new orchestration E2E
  exercise every failure category the reviewer named — nil vs non-nil-empty
  actions, raw `#`/`?`/empty-host URLs, leading-whitespace control chars,
  plain-HTTP HMAC over cleartext, plain-HTTP redirect rejection, and
  proxy-env-variable ignoring.

## Verification

- `go build ./... && go vet ./... && gofmt -l ...` — PASS.
- `go test -race ./internal/cardactiondispatch/... ./pkg/cardtmpl/... ./modules/notify/... .` — PASS.
- Coverage — cardactiondispatch 81.5%, cardtmpl 89.9%, notify 71.2%.
- `go test -tags=integration -race ./internal/cardactiondispatch/...` on a
  freshly-recreated local `test` schema — PASS (all 3 orchestration E2Es
  including new custom-decision path).
- `go test -tags=integration -race -run TestCardActionSenderBoundCallbackRouting ./modules/message/...`
  on a freshly-recreated schema — PASS. This is the compile-blocker the
  reviewer flagged; its `NewRegistry` call site was migrated to the
  two-argument signature.
- `make i18n-lint` (direct-error-response + unregistered-code) — PASS.

## Gotchas worth remembering

- Go's `url.Parse` silently drops trailing empty `#` and produces
  `Fragment == ""`. Checking the parsed field alone lets `https://host/path#`
  through. Prefilter the raw string for `#` before parsing. The same applies
  in principle to `?`, though `u.ForceQuery` covers that case.
- `strings.TrimSpace` also strips control-class whitespace (`\t \n \r \v`).
  Any "reject control characters" rule must run on the raw string, not the
  trimmed one, or leading/trailing tabs and newlines slip through.
- `omitempty` only affects encode. For decode, `nil` (field absent) and
  `[]` (explicit empty array) are distinct on the wire; treat them as
  different states rather than collapsing with `len() == 0`.
- Local integration tests need the shared `test` schema dropped/recreated
  between packages, matching CI. `Unable to create migration plan / unknown
  migration` is the canonical symptom — do not try to reconcile migrations
  in-place.

## Rollout

- No DB migration. `kubectl rollout` is sufficient.
- Deploy → verify no `OCTO_CARD_ACTION_ALLOWED_URLS deprecated` WARN (or
  clean it up when convenient).
- Add each new consumer as one JSON route entry plus its
  `<OWNER>_CARD_ACTION_SECRET` + `<OWNER>_NOTIFY_TOKEN` secrets. `http://`
  in the route URL is a conscious operator authorization.
- Rollback: delete the route from `OCTO_CARD_ACTION_ROUTES` and restart, or
  change its `url` back to HTTPS. In-flight events retry until the new URL
  is reachable or the attempt cap is exhausted (then DLQ, replayable via
  `tools/card-action-dlq`).
