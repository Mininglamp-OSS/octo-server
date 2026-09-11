---
type: Task
title: "Task: PPT comment mention routing"
description: Route PPT comment tasks through a default-off kind gate and the existing Bot event channel.
tags: ["bot-api", "wire-contract", "test"]
timestamp: 2026-09-10T00:00:00Z
slug: ppt-comment-mention
upstream: Mininglamp-OSS/octo-server#867
source: self
---

# PPT comment mention routing

Goal: accept the explicit `ppt` document kind and carry it through the authoritative comment event, preserving root-thread routing and deduplication namespaces.

Load-bearing path: mention request normalization, PPT opt-in plus shared document mention gate, event payload/fingerprint and ingress observability. Authentication, bot eligibility and atomic queue delivery retain their existing implementation.

Out of scope: PPT body edits, frontend, credentials, new endpoints and remote deployment.

Acceptance: missing document kind keeps legacy behavior; `PPT` normalizes to `ppt`; unknown kinds fail validation; root and reply events retain the correct thread; PPT fingerprints differ from ordinary document fingerprints. PPT requires `OCTO_DOCS_BOT_MENTION_PPT_ENABLED=true` in addition to the existing `OCTO_DOCS_BOT_MENTION_ENABLED` and document/Space allowlist. The PPT switch defaults off; missing, blank or invalid boolean values fail closed even with a shared wildcard. Legacy/HTML tasks remain independent of the PPT switch. Disabled requests leave no terminal claim; previously accepted events replay even after either gate is disabled, and switching off does not cancel accepted tasks. Gate tests must cover both switch transitions, new requests after disable, allowlist misses and legacy/HTML compatibility. Run the full `modules/bot_mention` package with isolated MySQL and Redis, then independent review.

Wire contract: `event_data.doc_kind` is absent for legacy Docs, `html` for the
HTML service, or `ppt` for the dedicated Docs backend PPT API. PPT `doc_id` is
the canonical `doc_meta.doc_id`, shared with the Docs metadata namespace; it is
not the Bento JSON's internal deck ID. `html_ppt` is not accepted on the wire.
The handler normalizes case/whitespace before enqueueing this field.

The existing document allowlist remains a global business-ID gate, not an
authorization grant. Both producer and consumer must retain document/Space
permission checks. The companion plugin separates PPT session/queue scopes
with its PPT kind prefix and uses the original root thread; the server's
existing `(bot_uid, idempotency_key)` claim plus full-payload fingerprint remain
unchanged. Rollout sequencing is a prerequisite: release and upgrade every consuming PPT-aware plugin and its CLI first, then deploy the PPT-aware Server, and only then deploy/open the Docs producer of `doc_kind=ppt` and frontend entry. Keep the PPT switch disabled until every relevant consumer is verified, then explicitly set `OCTO_DOCS_BOT_MENTION_PPT_ENABLED=true` and restart Server. Shared enabled gates and wildcard allowlists alone do not permit new PPT tasks. The frontend flag is not a server-side gate. Both mention switches are read at startup. This gate only controls comment task admission, not PPT creation, direct CLI/API editing, collaboration or export. Operators may stop new PPT tasks independently by disabling this switch and restarting Server.
Ingress outcome logs include normalized `doc_kind`; the new
`dmwork_doc_bot_mention_ingress_by_kind_total{result,doc_kind}` counter uses only
legacy/html/ppt/unknown, preserving existing result-only metric series.
