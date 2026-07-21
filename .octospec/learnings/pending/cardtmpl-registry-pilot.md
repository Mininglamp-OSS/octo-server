---
type: Learning
title: "Handoff schemas describe wire shape, not caller shape — do not wire them unchanged as input contracts"
description: A design-phase JSON Schema shipped alongside a card handoff is authored to describe the fully-composed card data (every field the renderer might reference). Wiring it unchanged as the *caller input* schema for Registry.Render turns legitimate legacy calls into 400s the day the pilot goes live, because caller-optional fields (anonymous actor, missing avatar, server-composed URLs) are marked required in the handoff schema. The pilot L1 schema must be a curated subset that reflects what callers actually submit.
tags: ["cardtmpl", "wire-contract", "schema", "handoff", "cross-repo", "input-validation"]
timestamp: 2026-07-22T00:00:00Z
# --- octospec extension fields ---
source: self
origin_task: cardtmpl-registry-pilot
origin_pr: self
status: pending
candidate_rule: error-handling
---

# Handoff schema ≠ caller-input schema

## Context

The `docs.access-request@0.2.0` handoff (delivered by the web/design side)
included a `contract/data.schema.json` describing the **fully-composed card
data** — every field the renderer might reference: `requester.name` required
with `minLength: 1`, `requester.avatarUrl` required with `pattern: ^https://`,
`document.url` required, `permission.{label,roleLabel}` required, etc.

The pilot Registry uses that same file as its **caller-input** contract:
`Registry.Render` fails with `cardtmpl.ErrFieldsInvalid` — which the new C1
policy converts to HTTP 400 zero-delivery — whenever the input doesn't pass.

Wired unchanged, this would 400 every anonymous-requester DocsCard call
(`ActorName=""`), every request without an avatar URL, and every request
where `document.url` is meant to be server-composed from `WebLoginURL + docId`.
Existing legacy tests (`card_test.go:297` explicitly covers `ActorName=""`)
would go red.

## The trap

Handoff schemas are typically authored by the client/design side to describe
the *output* — the shape of the compiled card data used by their renderer or
their `${}` template expansion. They naturally treat "the field renders on
the card" as a proxy for "the field is required." That is correct for the
renderer but wrong for the caller: server-filled fields (labels, deep-links,
permission strings, timestamps) are populated by the platform, not by the
caller.

If you paste-load the handoff schema straight into the Registry as the input
contract, you conflate the two audiences.

## The fix (this PR)

Split them explicitly:

- **The pilot's own `contract/data.schema.json`** (living inside
  `pkg/cardtmpl/docs_access_request/handoff/…`) is trimmed to reflect the
  *caller input*: `requester`/`permission` fields optional,
  `document.url` optional, `document.docId` added as the identifier the
  mapping layer carries through, only `requestId`/`state`/`document.title`
  required.
- **Server-filled fields** (permission labels, requestedAtDisplay,
  messageTimeDisplay, `document.url`) are documented as optional and
  filled by the mapping layer (`modules/notify/card_via_registry.go`)
  before `Registry.Render` sees the fields.
- The handoff-shape schema (the design-side wire contract) is preserved as
  reference for future full-card renderers but is NOT the Registry input
  contract.

## What to do next time

When adopting any handoff/design-side schema as a Registry input contract:

1. **Grep every caller** for the fields the schema marks required. If any
   legitimately supplies nothing (empty string, absent field), the schema is
   too strict.
2. **Identify server-filled fields.** A field is server-filled if the
   platform (i18n label, timestamp formatter, URL composer,
   permission-derived text, avatar renderer, etc.) fills it before render,
   NOT the caller. Mark those optional in the input schema and populate
   them in the mapping layer.
3. **Distinguish `docId`-like identifiers from URL-shape fields.** A schema
   that requires `document.url: pattern=^https://` forces callers to pre-compose
   URLs; if the platform composes them, require `docId` instead and derive
   the URL server-side.
4. **Keep the design-side schema around**, referenced from the L1 contract
   doc, so both audiences are served.

## Candidate rule promotion

Should this become part of `error-handling` (guidance around what input to
reject at Registry ingress) or a new `cross-repo-contract` rule dedicated to
handoff artefact adoption? Discuss in the promotion PR.
