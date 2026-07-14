---
type: Task
title: "Task: smart-summary-resource-share-provider"
description: Onboard smart-summary as a reviewed provider for user-initiated resource sharing to DM, group, or thread.
tags: ["summary", "resource-share", "provider", "intent", "access", "space", "card", "e2e"]
timestamp: 2026-07-14T14:20:00+08:00
# --- octospec extension fields ---
slug: smart-summary-resource-share-provider
upstream: octo-server#571
source: self
---

# Task: smart-summary-resource-share-provider

## Goal

Allow a user viewing a completed smart-summary resource to choose “转发到聊天”,
select DM/group/thread targets through the shared Web/App component, and send a
server-minted card as themselves.

This is a provider onboarding over the generic resource-share platform. It is
not the automatic `summary-notify` flow and is not the notification Bot's
origin-channel delivery.

Shared contract:
[provider-onboarding.md](../user-resource-share-card/provider-onboarding.md).

## User flow

```text
Summary detail
  → 转发到聊天
  → shared DM/group/thread target picker
  → smart-summary creates signed intent
  → Web/App calls octo-server /v1/resource-shares
  → card appears from the sharing user
```

The smart-summary page provides only the selected resource identity and an
intent-acquisition hook. It does not implement target selection, result
mapping, retry UX, card JSON, proof handling, or WuKongIM transport.

## Proposed provider identity

| Field | Proposed value | Confirmation |
| --- | --- | --- |
| Provider ID | `smart-summary` | Locked by current generic tests/examples |
| Resource type | `smart-summary` | Locked by current generic tests/examples |
| Intent version | `1` | Platform contract |
| Intent algorithm | ES256 | Platform contract |
| Template | `summary-share` version `1` | Review with product/client |
| Audience | `octo-server:resource-share` | Confirm deployment-wide constant |
| Issuer | Managed HTTPS smart-summary issuer | TODO: owner/config value |
| Resource ID | `summary_task.task_no` | TODO: confirm detail route still uses task_no |
| Revision | Opaque authoritative summary-result revision | TODO: choose source field |

The revision must change when visible summary content or share policy changes.
It must not be a client timestamp or mutable title. Prefer an existing
monotonic result version; otherwise add a persisted opaque revision at summary
completion.

## smart-summary user API

Proposed route; the service owner may map it to existing gateway conventions:

```http
POST /v1/summaries/{task_no}/share-intents
Authorization: <existing user session>
X-Space-ID: <active space>
Content-Type: application/json

{
  "targets": [
    {"kind": "dm", "peer_uid": "u_123"},
    {"kind": "group", "group_no": "g_123"},
    {"kind": "thread", "group_no": "g_123", "short_id": "t_123"}
  ],
  "idempotency_key": "opaque-client-key"
}
```

Before signing, smart-summary must:

- derive actor UID and Space from authenticated request context;
- load the task/result by `task_no` without exposing numeric IDs;
- require a terminal completed result; generating, failed, deleted, or missing
  results are not user-shareable;
- verify the actor still has permission to view/share the summary;
- resolve the current authoritative revision and share mode;
- validate/canonicalize the exact target shapes and provider limits;
- emit a short-lived intent, normally 60–120 seconds;
- store or otherwise enforce idempotent intent issuance for the same actor,
  resource, targets, and idempotency key.

Suggested response:

```json
{
  "intent": "<compact ES256 JWS>",
  "expires_at": 1784010000
}
```

The endpoint must use a generic non-enumerating denial for missing and
unauthorized task IDs. It does not return card JSON or a result URL.

## Intent claims

Claims are bounded presentation facts, not the summary document:

```json
{
  "title": "产品周会纪要",
  "time_range": "2026-07-06 10:00 ~ 2026-07-13 10:00",
  "participant_count": 5,
  "message_count": 128,
  "generated_at": "2026-07-13 15:04"
}
```

Rules:

- `title` is escaped and bounded; empty title falls back server-side.
- Counts are non-negative bounded integers.
- Time fields use one reviewed representation and explicit timezone policy.
- No summary body, excerpt, source messages, member names, prompts, model
  metadata, credentials, or arbitrary URL is included.
- Revalidation may reduce/redact fields but cannot widen beyond the reviewed
  schema.

For `requestable` summaries, the initial policy should omit any generated
summary excerpt. Title/time/count visibility must be approved as safe for all
current and future members of a target group/thread; otherwise the card uses a
generic title such as “智能总结”.

## Access/share modes

smart-summary returns one authoritative mode:

- `public`: the landing route permits the intended audience to read the result.
- `requestable`: the card is shareable, but the landing route checks the
  current viewer and shows a permission-request flow when needed.
- `forbidden`: no card is sent.

“Public” is a resource policy, not “anyone with the URL”. The landing route
still authenticates the current viewer and enforces any Space boundary.

TODO for smart-summary owner:

- define which existing task/result policies map to each mode;
- define who may request and who may approve access;
- decide whether request approval is supported initially or whether
  `requestable` is disabled until that workflow exists;
- define whether sharing can ever create a grant (default: no).

## S2S revalidation

octo-server's smart-summary adapter calls an authenticated internal provider
API, or an existing authoritative task/result API with equivalent guarantees.

It must recheck:

- exact `task_no`, Space, and signed revision;
- completed/non-deleted lifecycle;
- actor's current share permission;
- share mode and safe preview fields;
- exact target list echo.

Suggested response:

```json
{
  "resource": {
    "type": "smart-summary",
    "id": "TN_20260713_abcd",
    "revision": "result-rev-9"
  },
  "share_mode": "requestable",
  "card": {
    "title": "产品周会纪要",
    "description": "智能总结",
    "fields": [
      {"label": "时间范围", "value": "2026-07-06 ~ 2026-07-13"},
      {"label": "消息数量", "value": "128"}
    ]
  },
  "targets": [
    {"target": {"kind": "group", "group_no": "g_123"}, "allowed": true}
  ]
}
```

A stale revision, permission change, deleted result, malformed response,
timeout, or unavailable provider fails closed before WuKongIM transport.

## Card and deep link

The card is generated by octo-server from typed fields. smart-summary never
returns Adaptive Card elements or caller-controlled URLs.

The existing summary route is expected to remain:

```text
/s/{task_no}?sp={space_id}
```

TODO: confirm the production Web route, whether `sp` is required, and the
authoritative public origin. The link contains no access token. On open:

1. authenticate current viewer;
2. load task by task_no and exact Space;
3. open the result when allowed;
4. otherwise show the permission-request page for `requestable`;
5. return a non-enumerating denial for forbidden/missing resources.

If `sp` remains required, octo-server must first complete the generic typed
link-input TODO because its current `DeepLinkBuilder` receives only the
resource reference.

## Key and configuration ownership

- smart-summary owns the ES256 private intent key and active `kid`.
- octo-server receives only the reviewed public-key ring.
- smart-summary supports overlapping key rotation for longer than maximum
  intent lifetime.
- octo-server adds an immutable provider spec, enable flag, public keys,
  issuer/audience, claims validator, template/deep-link builder, S2S adapter,
  and provider target budget.
- all provider/global flags remain off until client proof verification and the
  shared share component are deployed.

## Observability

smart-summary records bounded outcomes for intent minted/denied, stale
revision, share mode, and S2S revalidation latency/failure. Metric labels never
include UID, Space, task number, title, target ID, or claim content.

Structured audit may include actor, task reference, revision, Space, request ID,
mode, and bounded outcome under existing retention policy. Never log compact
intent, claims, signature, key, summary content, prompts, or source messages.

## Out of scope

- Automatic completion notification from the `notification` Bot.
- Automatic origin group/thread delivery.
- Provider-specific target picker or card renderer in Web/App.
- Inline `Action.Submit` access requests or approvals.
- Copying an existing notification card/message payload.
- Cross-Space target delivery.

## Acceptance

- User-authenticated intent API derives actor/Space and rejects caller-supplied
  actor, revision, template, issuer, URL, card, sender, or transport fields.
- Completed/current/shareable summary mints an ES256 intent bound to exact
  canonical targets; non-terminal, deleted, stale, wrong-Space, or unauthorized
  summaries fail without transport.
- Claims schema and size tests cover title/time/count bounds and prove no
  summary body/source messages enter the intent or card.
- S2S tests cover actor permission removal, result revision change, mode change,
  timeout, malformed response, and provider outage.
- Public/requestable/forbidden policy tests pin safe preview and landing-route
  behavior.
- octo-server provider tests cover key rotation, deep-link allowlist, template
  snapshot/finalization, rate budget, and feature flags.
- Cross-repository E2E covers DM, group, thread, proof verification, identical
  intent retry, partial denial, and current-viewer view/request routing.
- Rollback disables only smart-summary sharing and does not affect notification
  Bot delivery, docs sharing, normal messages, or historic proof verification.
