---
type: Task
title: "Task: card-action-internal-http-actions"
description: Allow plain HTTP callbacks declared in the static route registry and bounded custom actions on the generic approval_card ingress, without weakening the server-owned card boundary or the config-driven URL allowlist.
tags: ["card", "internal", "trust-boundary", "wire-contract", "testing"]
timestamp: 2026-07-16T10:00:00+08:00
slug: card-action-internal-http-actions
upstream: "TBD — follow-up to octo-server#588"
source: self
---

# Task: card-action-internal-http-actions

## Goal

Two small follow-ups to the callback dispatch layer merged in #588:

1. Allow `OCTO_CARD_ACTION_ROUTES[].url` to use plain `http://` in addition to
   `https://`. The static route registry is itself the allowlist; hostname shape
   (Kubernetes DNS, docker service name, `host.docker.internal`, IP literal) is
   not inspected. The presence of `http://` in a route is the operator's
   explicit authorization.
2. Let the generic `approval_card` ingress render a bounded list of server-built
   `Action.Submit` buttons instead of always forcing localized `approve` /
   `deny`.

The callback remains config-driven and the card remains template-driven. Neither
change permits request-supplied callback URLs or arbitrary Adaptive Card JSON.
The contract is consumer-neutral: no owner is privileged, and examples must not
become requirements that future services have to copy.

Bundled configuration collapse (out of the discussion draft):

3. Delete `OCTO_CARD_ACTION_ALLOWED_URLS`. `OCTO_CARD_ACTION_ROUTES[].url` is
   already the exact URL and shares the same ConfigMap/operator boundary; the
   two lists cannot form an independent security review and only produce
   dual-write drift. If the deprecated env is set at startup, log a single
   structured WARN and ignore it (do not fail startup — rolling upgrades may
   still carry the old variable).

## Implementation gate

Update and review the standalone operations and consumer integration documents
first. Do not change production code or tests for HTTP/actions/config collapse
until the consumer-neutral contract, examples, security boundaries, and rollout
steps are confirmed.

## Locked decisions (from the discussion draft)

| # | Question | Decision |
|---|---|---|
| 1 | actions count | Optional; when provided, 1-5 items. Absent = current localized approve/deny, byte-compatible with #588. |
| 2 | terminal result states | Reuse `approved` / `denied` / `cancelled` / `pending`. Consumer maps its custom `decision` to a state. |
| 3 | non-terminal "still clickable" actions | Not in this change. Current finalizers always remove actions and update the card. |
| 4 | button i18n | Caller-provided single copy per request. Per-recipient language rendering is a future v2. |
| 5 | requester notification for custom decisions | Only `approved` / `denied` notify the requester (unchanged from #588). Custom `decision` never triggers direct requester notification; the mapped `state` does. |

## Contract

`approval_card.actions` is optional. When omitted, the existing localized
approve/deny actions are emitted unchanged. When present it contains 1-5 items:

```json
{
  "decision": "publish",
  "title": "发布"
}
```

- `decision` is the stable callback value and must be unique within the card,
  lowercase, and match `[a-z][a-z0-9_.-]{0,47}`.
- `title` is display text, trimmed, non-empty, and at most 80 Unicode code
  points. Control characters are rejected.
- octo-server derives the action ID as `approval-<decision>`, injects the
  authoritative `owner` and `action_type`, and copies only the existing bounded
  shared `data` map. Per-action data, styles, URLs, inputs, and caller-authored
  card JSON remain unsupported.
- The callback receives the selected `decision` through the existing signed
  `DecisionRequest`. The existing typed result states (`pending`, `approved`,
  `denied`, `cancelled`) do not change.

Example:

```json
{
  "service": "tasks",
  "approval_card": {
    "action_type": "task.execute.decision",
    "title": "执行任务",
    "description": "请选择本次任务的处理方式",
    "data": {"task_id": "task-1"},
    "actions": [
      {"decision": "execute", "title": "执行"},
      {"decision": "reject", "title": "拒绝"},
      {"decision": "cancel", "title": "取消"}
    ]
  }
}
```

## Security boundaries

- The static `OCTO_CARD_ACTION_ROUTES` is the exact URL registry. Card content,
  action requests, and consumer responses cannot supply or override the callback
  URL. HMAC signing, timeout, retry/DLQ, disabled redirects, and disabled
  environment proxies are unchanged.
- Both `http://` and `https://` are accepted; other schemes and non-absolute
  URLs, URLs with credentials, query strings, fragments, or opaque forms remain
  rejected at startup.
- Hostname form is not inspected. Operators are responsible for making the
  chosen HTTP destination reachable only from the octo-server pods that need
  it (NetworkPolicy, service mesh, container network, or localhost binding).
- HMAC only authenticates the request; it does not encrypt payload or response.
  Callback bodies may contain operator UID, Space ID, and business identifiers.
  When a route uses `http://`, the deployment must treat the transport as a
  trusted network boundary. Cross-cluster, cross-network, or public callbacks
  must use HTTPS.
- Callback secrets and notify tokens remain per-capability. A notify token
  cannot equal any callback secret (existing per-route + cross-route check);
  legacy `NOTIFY_INTERNAL_TOKEN` and `OCTO_DOCS_NOTIFY_TOKEN` also cannot
  collide with action notify tokens (existing exclusion check).
- Custom actions are structural fields only. Callers cannot choose owner,
  callback URL, action JSON, or callback headers.

## Out of scope

- A global "allow insecure HTTP" switch or Kubernetes/Docker environment
  detection.
- Per-recipient language rendering of caller-supplied button titles.
- New callback result states, custom terminal-card layouts, or non-terminal
  "still clickable" actions.
- Per-action arbitrary data, styles, URLs, inputs, or card JSON.
- TLS issuance, service-mesh setup, or NetworkPolicy management.
- Independent signing of callback responses (known residual risk; tracked
  separately).

## Acceptance

- Existing HTTPS routes and approve/deny cards are byte-compatible with #588.
- `OCTO_CARD_ACTION_ROUTES[].url` accepts `http://` and delivers with the same
  HMAC, retry, DLQ, and redirect/proxy discipline as `https://`.
- `OCTO_CARD_ACTION_ALLOWED_URLS`, when set at startup, produces a single
  structured deprecation WARN and does not affect routing decisions.
- Absolute URL requirements (scheme in {http,https}, host present, no
  credentials/query/fragment/opaque) continue to reject malformed values at
  startup.
- Custom `approval_card.actions` renders the requested labels and signed
  callback decisions; empty, duplicate, malformed, or oversized actions are
  rejected before card production.
- Unit tests cover URL and template boundaries; notify integration tests cover
  backward compatibility (omitted `actions` byte-equal to #588) and 1-5 custom
  action rendering.
- The `card-action-callback-consumer.md` and `card-action-callback-dispatch.md`
  documents are updated by reference rather than duplicating #588 design, and
  they carry the residual-risk callout for plain HTTP.
- A new first-party consumer, regardless of implementation language or owner,
  can complete route registration, card production, signature verification,
  idempotent decision handling, typed response, rollout, and DLQ operations
  from the two standalone integration documents. Consumer-specific examples
  are illustrative only and must not introduce owner-specific requirements;
  the signing section retains the deterministic language-neutral test vector
  in addition to the TypeScript reference implementation.
