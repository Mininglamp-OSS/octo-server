---
type: Task
title: "Task: agent-mail-identity-gateway"
description: "Bind Agent Mail requests to the authenticated OCTO user and validated Space instead of a shared mailbox credential."
tags: ["agent-mail", "auth", "space", "isolation", "proxy", "wire-contract", "testing"]
timestamp: 2026-07-29T10:15:00Z
slug: agent-mail-identity-gateway
source: self
upstream: "https://github.com/Mininglamp-OSS/octo-server/issues/735"
---

# Task: agent-mail-identity-gateway

## Goal

Route browser Agent Mail requests through an authenticated OCTO gateway. The
gateway derives the actor from the OCTO session and the Space from validated
middleware state, then presents a short-lived signed identity assertion to
octo-mail. Different users and Spaces must never inherit one shared mailbox
credential.

## Background

The current local `/mail-api` proxy injects one server-side Basic/API-key
credential. It is useful for single-mailbox UI development but makes every OCTO
user appear as the same mail owner. V1 requires user, mailbox, and Space
isolation before additional product expansion.

## Load-bearing list

- OCTO authentication: the actor is always `GetLoginUID()` and never comes from
  a request field or caller-controlled header.
- Space isolation: the route requires a non-empty Space validated by
  `SpaceMiddleware`; missing or non-member Spaces fail closed.
- Gateway assertion: method, upstream request URI, body digest, actor, Space,
  selected mailbox, issue time, expiry, and nonce are HMAC-bound with a managed
  shared secret; octo-mail consumes each nonce once.
- Proxy boundary: browser Authorization headers are never forwarded as
  octo-mail owner credentials; hop-by-hop headers and secrets are not logged;
  private mail responses cannot be reused across authenticated identities by a
  shared cache.
- octo-mail mapping: a signed `(issuer, subject, space)` resolves to one
  provisioned owner principal and a mailbox owned by that principal.
- Rule mutations retain the human-owner requirement; an Agent credential is not
  upgraded by the gateway.
- Streaming bounds: request buffering, upstream upload, response headers,
  upstream response reads, and downstream response writes have explicit
  size/concurrency/progress limits.

## Out of scope

- Public-domain reputation, SPF/DKIM/DMARC/PTR, or external mailbox hosting.
- A general Channel gateway or shared IM/Mail protocol.
- Automatic Agent task execution or Runtime processing state.
- Silently choosing among multiple sender identities without an explicit,
  ownership-checked mailbox selection.
- Owner confirmation and Agent Draft delivery. Those remain inside the Mail
  Plugin and octo-mail credential boundary; octo-server exposes no Bot
  Draft-send endpoint.

## Acceptance

- Requests without a valid OCTO session or validated Space never reach
  octo-mail.
- User A and user B in the same Space resolve to different provisioned mail
  owners and cannot list, read, send as, or mutate each other's mailbox.
- The same UID in two Spaces resolves only through the exact provisioned Space
  binding; no fallback to another Space exists.
- Tampered, expired, wrong-method, wrong-path, or wrong-body gateway assertions
  are rejected uniformly.
- The mailbox selector is signed and a gateway assertion is single-use.
- Browser requests no longer depend on a global `MAIL_API_AUTHORIZATION` or
  `MAIL_API_TOKEN` for owner identity.
- Stalled request bodies, upstream uploads, response-header waits, upstream
  response bodies, and downstream response writes release their gateway
  resources within the configured progress timeout; small requests do not wait
  behind the large-body budget.
- Oversized, stalled, malformed, and truncated transfers fail explicitly and
  release their concurrency slots and byte-budget reservations.
- Proxied mail responses are non-storable and vary on every request header that
  selects the authenticated user, Space, or mailbox.
- Focused gateway/auth/isolation tests, formatting, vet, and relevant builds
  pass before Docker integration is declared complete.
