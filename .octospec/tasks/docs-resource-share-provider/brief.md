---
type: Task
title: "Task: docs-resource-share-provider"
description: Onboard the document service as a reviewed provider for user-initiated resource sharing to DM, group, or thread.
tags: ["docs", "document", "resource-share", "provider", "intent", "access", "space", "card", "e2e"]
timestamp: 2026-07-14T14:20:00+08:00
# --- octospec extension fields ---
slug: docs-resource-share-provider
upstream: octo-server#571
source: self
---

# Task: docs-resource-share-provider

## Goal

Allow a user viewing a document to choose “转发到聊天”, select DM/group/thread
targets through the shared Web/App component, and send a server-minted document
card as themselves.

The card may represent a publicly readable document or a restricted document
whose landing page offers permission request. Sending the card does not grant
document access by default.

Shared contract:
[provider-onboarding.md](../user-resource-share-card/provider-onboarding.md).

## Known vs TODO

This checkout does not contain the document service implementation. The
following contract is actionable but the docs owner must confirm:

- repository/service name and externally reachable API/gateway;
- existing user authentication and Space binding;
- authoritative document ID and revision/version field;
- ACL/share-policy API and permission-request workflow;
- production detail/request route;
- managed key/config owner and operational SLO.

These values must be verified against the docs source before implementation;
they are not inferred from octo-server.

## User flow

```text
Document detail/list
  → 转发到聊天
  → shared DM/group/thread target picker
  → docs creates signed intent
  → Web/App calls octo-server /v1/resource-shares
  → card appears from the sharing user
```

The docs UI provides a selected resource identity and
`getShareIntent(document, targets, idempotencyKey)`. It does not own target
selection, result mapping, retry behavior, card JSON, proof verification, or IM
transport.

## Proposed provider identity

| Field | Proposed value | Confirmation |
| --- | --- | --- |
| Provider ID | `docs` | Matches the generic provider vocabulary |
| Resource type | `document` | TODO: confirm canonical type |
| Intent version | `1` | Platform contract |
| Intent algorithm | ES256 | Platform contract |
| Template | `document-share` version `1` | Review with product/client |
| Audience | `octo-server:resource-share` | Confirm deployment-wide constant |
| Issuer | Managed HTTPS docs issuer | TODO: owner/config value |
| Resource ID | Opaque document ID | TODO: confirm non-enumerable identifier |
| Revision | Authoritative document version/ETag | TODO: confirm source field |

The revision must change whenever visible card facts, document content, or
share/access policy changes. A client timestamp, title, or editable path is not
an authoritative revision.

## Docs user API

Proposed route; adapt it to the actual docs API conventions:

```http
POST /v1/documents/{document_id}/share-intents
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

Before signing, docs must:

- derive actor UID and Space from authenticated context;
- load the document and authoritative revision;
- verify the actor can view and is allowed to share a reference;
- evaluate document lifecycle/classification and share mode;
- canonicalize/bound exact targets;
- mint a short-lived ES256 intent, normally 60–120 seconds;
- apply actor/resource rate limits and idempotent issuance;
- use one non-enumerating error for missing vs unauthorized resources.

The request does not accept actor UID, revision, template, share mode, URL,
card fields, sender, proof, or transport metadata as authority.

Suggested response:

```json
{
  "intent": "<compact ES256 JWS>",
  "expires_at": 1784010000
}
```

## Intent claims

Default claims are minimal metadata:

```json
{
  "title": "项目方案",
  "document_type": "document",
  "updated_at": "2026-07-14T06:00:00Z"
}
```

Rules:

- No document body, excerpt, comments, collaborators, ACL list, internal path,
  storage key, download URL, access token, or presigned URL.
- `title` is included only when safe for every viewer of the target
  conversation; otherwise use the generic “文档” title.
- `document_type` comes from a bounded provider enum.
- `updated_at` uses one normalized representation and is not itself the
  authoritative revision.
- Claims are revalidated and may be reduced/redacted before card creation.

An excerpt is out of scope for initial onboarding. Adding one requires an
explicit disclosure review for requestable cards and future group members.

## Access/share modes

Docs maps its authoritative policy to:

- `public`: the document is readable according to the provider's public/tenant
  policy. CTA is “查看详情”.
- `requestable`: sharing a safe reference is allowed; the current viewer either
  opens the document or reaches the provider's permission-request page. CTA is
  “查看或申请权限”.
- `forbidden`: deleted, quarantined, confidential/no-share, wrong-Space, or
  otherwise non-shareable document. No card is sent.

The exact meaning of “public” must be confirmed:

- public within the same Space/tenant;
- authenticated public across approved tenants;
- anonymous internet public.

octo-server does not choose among these. Phase one still rejects cross-Space IM
targets. If docs supports anonymous public links, the URL is provider/server
built under an allowlisted origin and must not contain a client-supplied or
permanent bearer credential.

For `requestable` mode, docs owns request creation, approval, notifications,
expiry, revocation, and audit. The phase-one card only opens that workflow; it
does not submit or approve access inline.

## S2S revalidation

octo-server's docs adapter calls an authenticated internal API or an existing
authoritative docs ACL/detail API with equivalent guarantees.

It must recheck:

- exact document ID, Space, and signed revision;
- current lifecycle/classification;
- actor's current view/share permission;
- current public/requestable/forbidden mode;
- safe bounded card facts;
- exact target list echo.

Suggested response:

```json
{
  "resource": {
    "type": "document",
    "id": "doc_123",
    "revision": "version_9"
  },
  "share_mode": "requestable",
  "card": {
    "title": "项目方案",
    "description": "文档",
    "fields": [
      {"label": "更新时间", "value": "2026-07-14 14:00"}
    ]
  },
  "targets": [
    {"target": {"kind": "group", "group_no": "g_123"}, "allowed": true}
  ]
}
```

A stale revision, permission/policy change, deleted document, malformed
response, timeout, or provider outage fails closed before transport. The
provider does not have to enumerate every group/thread member for
`requestable` mode because the persisted card contains only safe metadata and
the landing route reauthorizes each viewer.

## Card and deep link

octo-server owns the fixed document template. Docs returns typed values only;
it never returns Adaptive Card JSON or an arbitrary link.

The reviewed deep link must:

- use one configured HTTPS origin and fixed route shape;
- carry only opaque document identity and, if required, Space;
- contain no access token, presigned storage URL, ACL, or user identity;
- authenticate and authorize the current viewer on every open;
- open the document when allowed, otherwise show the request page only for
  `requestable`;
- use one non-enumerating denial for forbidden/missing documents.

TODO: docs owner must provide the exact detail/request route and whether Space
is a path/query component. If Space is required, complete the generic typed
link-input extension before provider enablement.

## Key and configuration ownership

- docs owns the ES256 private intent key and active `kid`.
- octo-server receives only the reviewed public verification ring.
- key rotation overlaps longer than maximum intent lifetime.
- octo-server registers immutable provider ID/type, issuer/audience, public
  keys, claims schema, template/deep-link builder, S2S adapter, enable flag, and
  target budget.
- S2S credentials are managed separately from intent/proof keys and never
  appear in intent, deep link, request logs, or metrics.

## Reliability and observability

- The user intent endpoint and S2S revalidation have bounded timeouts and
  explicit unavailable responses.
- S2S failure never falls back to an unverified generic card or plain-text URL.
- Provider metrics cover intent minted/denied, mode, stale revision,
  revalidation latency/failure, and key/config readiness using bounded labels.
- Audit may record actor, document reference, revision, Space, request ID,
  mode, and outcome under retention policy, but never title/body/claims,
  compact intent, signatures, tokens, or credentials.

## Out of scope

- Granting document access merely because a card was sent.
- Inline permission request/approval through `Action.Submit`.
- Document edits, comments, downloads, or previews inside the card.
- Provider-specific target picker or separate docs share transport.
- Copying/forwarding an existing document card payload.
- Cross-Space IM delivery.

## Acceptance

- Intent API derives actor/Space/revision and rejects caller-supplied authority,
  URL, card, sender, proof, and transport fields.
- ACL matrix covers owner/editor/viewer, share-disabled, deleted/quarantined,
  wrong-Space, public, requestable, forbidden, and DB/dependency errors.
- Claims tests prove document body, excerpt, ACL, collaborators, storage keys,
  download/presigned URLs, and credentials never enter the intent/card.
- S2S tests cover revision/policy changes, actor access removal, timeout,
  malformed response, and provider outage with zero transport.
- Landing-route tests cover authorized view, request page, forbidden/missing
  non-enumeration, revocation, and no credential in URL.
- octo-server provider tests cover public/requestable template variants,
  deep-link allowlist, key rotation, finalization/proof, limits, and flags.
- Cross-repository E2E covers DM, group, thread, proof verification, identical
  intent retry, partial denial, and current-viewer view/request routing.
- Rollback disables only docs sharing and does not affect smart-summary,
  normal messages, or historic proof verification.
