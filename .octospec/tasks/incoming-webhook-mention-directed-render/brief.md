---
type: Task
title: "Task: incoming-webhook-mention-directed-render"
description: Opt-in server-side resolution of directed mention.uids into visible @name pills (resolve display name → compose into content → generate UTF-16 entities) on the native incoming-webhook push endpoint.
tags: ["incomingwebhook", "mention", "wire-contract", "trust-boundary", "render-layer"]
timestamp: 2026-06-24T08:00:00Z
# --- octospec extension fields ---
slug: incoming-webhook-mention-directed-render
upstream: Mininglamp-OSS/octo-server#448
source: self
---

# Task: incoming-webhook-mention-directed-render

> #448 item ① option (b). Ships in the SAME PR as the broadcast half
> (`incoming-webhook-mention-broadcast`), reusing its compose/offset machinery;
> together they close #448.

## Goal

Let a native incoming-webhook caller render a directed `@member` pill by sending
**only the uid** — the server resolves the member's display name, composes
`@<name>` into the text content, and **generates** the matching UTF-16
`mention.entities` element. Today (#449) the caller must supply the `@name` text
and the offsets itself; this does it server-side.

Opt-in via `mention.render` (default off) so existing #445 callers that send
`mention.uids` purely for red-dot/bot routing keep their content untouched.

Target shape — caller sends:
```json
{ "content": "执行吧", "mention": { "uids": ["27o…_bot"], "render": true } }
```
server emits (existing wire fields only — zero client change):
```json
{ "content": "@我的天 执行吧",
  "mention": { "uids": ["27o…_bot"],
    "entities": [{ "uid": "27o…_bot", "offset": 0, "length": 4 }] } }
```

## Background

- **#448 item ①** asks for directed-@ label/uid consistency, with option (b):
  "backend resolves uid→display name and composes/validates the `@name` text,
  optionally adding `entities`." This task is option (b).
- **Client send-side grounding** (octo-web `mentionSendParse.ts`): when a user @s
  a member, the client emits `content` containing `@<displayName>` inline +
  `entities.push({uid, offset, length})` (offset = UTF-16 position) + `uids`.
  The server must **generate the same shape** for a bare uid.
- **Display name source**: the group module resolves a member's shown name via
  `LEFT JOIN user … IFNULL(user.name,'')` (see `modules/group/db.go`). v1 uses
  that `user.name`. `remark` is viewer-specific (a webhook has no viewer; the
  composed literal is shown to all recipients as-is) and the verified-`real_name`
  override is a space-member-list refinement (#344) — both out of scope for v1.
- **Shared machinery**: reuses the broadcast half's prefix-compose + UTF-16
  offset handling. The two prepends compose in one pass: broadcast literals
  first, then directed `@name`s, then the original content; generated directed
  entities carry offsets into that combined prefix.

## Load-bearing list

- **wire-contract** — output uses only existing fields (`content`, `mention.uids`,
  `mention.entities`); `mention.render` is a new **request-only** boolean (does
  not reach the wire / clients). With `render` off, output is byte-identical to
  today.
- **trust-boundary / external-content / webhook** — the composed `@name` text is
  **derived from server-side group data** (the member's `user.name`), not from
  caller-controlled strings, and only for uids that pass the existing member
  gate; the only caller input is the boolean flag + the uid list (already gated).
  No markdown/regex metacharacters are introduced by the server (the `@` + name +
  space are inserted literally; the name is group data, same as any rendered
  message author name).
- **space / isolation** — names are resolved ONLY for current internal-normal
  members of the webhook's `group_no` (same gate as #445/#449); a token cannot
  resolve or render a cross-group / cross-Space identity.
- **render-layer offset consistency** — generated entity offsets are **UTF-16
  code units** into the final content; when broadcast literals also prepend, the
  directed offsets account for the broadcast prefix length.
- **content length cap** — directed render can prepend up to `maxMentionUIDs`
  (50) `@name` tokens; the compose stops adding names once the composed content
  would exceed `maxContentRunes`, so the cap stays authoritative (remaining uids
  still route via `mention.uids`, just without a pill).
- existing push pipeline + the `richtext` path must be unaffected (render is
  text-path only).

## Out of scope

- **Inline placement / placeholder grammar** — v1 prepends `@name`s at the
  content head (matches the example); a caller-controlled inline position is a
  later option.
- **`remark` / verified-`real_name` precedence** — v1 uses the group member
  `user.name`; richer display-name precedence (incl. the #344 chain) is deferred.
- **Richtext** directed render (text path only, mirrors entities/broadcast).
- **Caller-supplied `entities` coexisting with `render`** — if the caller sends
  `entities`, they are authoritative (#449) and `render` is ignored (no
  auto-generation); the two are mutually exclusive per push.
- Routing semantics (red-dot / notify / `ExpandAisToBotUIDs`) — unchanged; the
  directed uids still populate `mention.uids` for routing.

## Acceptance

- `render:true` + `uids:[member]` + member name resolves → delivered
  `content == "@<name> " + original`; `mention.entities` has one element
  `{uid, offset:0, length: utf16Len("@<name>")}`; `mention.uids` still carries
  the uid (routing unchanged).
- multiple member uids → prepended in caller order; each entity offset points at
  its own `@` in the final content (UTF-16).
- `render:true` but a uid is a non-member / has an empty name → that uid is
  skipped silently (no `@ ` pill), still routed via `mention.uids`.
- `render` absent/false → content byte-identical to today; no entities generated
  (full backward compatibility with #445/#449 callers).
- `render:true` AND caller-supplied `entities` → caller entities win; no
  auto-generation (they may still shift if a broadcast literal prepends).
- `render:true` + broadcast permitted → `content == "@所有人 …" + "@<name> " +
  original`; broadcast literals first, directed `@name`s after, offsets correct.
- richtext push with `render:true` → no content compose, no generated entities.
- compose stops adding `@name`s before the composed content exceeds
  `maxContentRunes`.
- **forged-broadcast guard** (adversarial-review hardening): a member whose display
  name is a broadcast label (`所有人`/`所有AI`/`All People`/`All AIs`/`all`,
  case-insensitive), **or starts with one at a non-word boundary** (e.g. `所有人 X`
  / `所有人:` / `all-hands` — iOS `@\S+` would emit a standalone `@所有人`/`@all`
  broadcast token), or contains `@`, is **skipped** for render — else `@<name>`
  would render as a broadcast pill and bypass the `allow_mention_*` capability gate
  (or break client `@`-tokenization); the uid still routes via `mention.uids`.
  Names that merely continue a label into a longer word (`所有人事部`, `allen`) still
  render (boundary check, not prefix match).
- `go test ./modules/incomingwebhook/...` passes incl. new render tests +
  `TestIncomingWebhookNoLegacyResponseError`; `golangci-lint` clean.
