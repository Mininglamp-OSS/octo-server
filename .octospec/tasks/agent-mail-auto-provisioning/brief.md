---
type: Task
title: "Task: agent-mail-auto-provisioning"
description: "Provision the authenticated OCTO user and Space in octo-mail before proxying browser mail requests."
tags: ["agent-mail", "auth", "space", "gateway", "provisioning"]
timestamp: 2026-08-14T00:00:00Z
slug: agent-mail-auto-provisioning
source: self
upstream: "https://github.com/Mininglamp-OSS/octo-server/issues/768"
---

# Task: agent-mail-auto-provisioning

## Goal

Make the existing authenticated Agent Mail gateway ensure that a normal OCTO
user has an octo-mail owner identity for the current Space before the original
WebAPI request is proxied. The browser must not call admin APIs or provide mail
database identifiers.

## Background

The gateway already derives the subject from the OCTO session, validates the
Space, signs an exact upstream request, and strips caller credentials. octo-mail
currently requires a pre-created `issuer + subject + space_id` binding, so a
fresh user receives an authentication failure until an operator manually
creates mail directory rows.

## Load-bearing list

- auth: the OCTO session remains the only source of the subject.
- space: provisioning is exact to the Space accepted by SpaceMiddleware.
- trust boundary: only the existing request-bound gateway HMAC authenticates
  the internal provisioning call.
- wire contract: the original `/v1/mail-gateway/webapi/v0/*` request and response
  remain unchanged.

## Out of scope

- No octo-web, CLI, or plugin changes.
- No browser-supplied subject, Space, tenant, domain, account, or address IDs.
- No change to mail message, draft, authorization, or mailbox business APIs.
- No removal of octo-mail admin APIs.

## Acceptance

- A valid authenticated UID and validated Space are provisioned before proxying.
- The preferred mailbox localpart comes from server-owned user data, never the
  browser request.
- Concurrent first requests for one UID and Space collapse to one provisioning
  operation; successful results are cached per process.
- Provisioning failure prevents the original request from reaching octo-mail.
- Existing proxy request/response, header stripping, size limits, and signing
  behavior remain covered by tests.

## Reuse decisions

- Reuse the current `gatewayConfig`, hardened HTTP client, upstream URL base,
  request-bound `signAssertion`, and localized gateway error envelope.
- Reuse the authenticated UID and `SpaceMiddleware` result; no second auth or
  Space lookup path is introduced.
- Reuse the repository's established `singleflight` + in-process cache pattern
  to collapse concurrent first-use work.
- Read the preferred localpart from the existing server-owned `user` record;
  do not create a parallel profile store or import frontend data.

## Related work

- `Mininglamp-OSS/octo-mail#55` provides the trusted internal provisioning
  endpoint called by this gateway path.
