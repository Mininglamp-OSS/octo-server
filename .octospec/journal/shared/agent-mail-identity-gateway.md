---
type: Journal
title: "Learning: Agent Mail identity gateway"
description: "Human mailbox ownership and Agent mailbox credentials need separate HTTP trust boundaries even when they share one public base URL."
tags: ["octospec-learning", "auth", "space", "agent-mail", "isolation"]
timestamp: 2026-07-29T13:50:00Z
source: self
---

# Learning: Agent Mail identity gateway

## What changed

Browser mail requests now derive identity from the authenticated OCTO user and
validated Space, then use a short-lived request-bound assertion to resolve an
explicitly provisioned octo-mail owner. Shared Basic/API-key owner injection was
removed.

## What we learned

The human owner path and the Agent `omb_` path cannot share one proxy behavior:
the human path must replace caller credentials, while the Agent path must
preserve only its mailbox-scoped credential. They can still use one public OCTO
base URL by using separate paths. Mixing them either breaks CLI authorization or
risks reintroducing a shared/over-privileged credential.

An Agent mailbox credential must also never receive a general send capability.
Manual Draft delivery does not need a second Bot bridge in octo-server: the
Mail Plugin can consume the host's trusted-owner signal and ask octo-mail to
apply a narrowly scoped, idempotent owner-confirmed Draft operation using the
existing mailbox credential. Keeping that flow out of this gateway avoids a
second cross-service identity translation and leaves octo-server responsible
only for browser identity proxying.

Buffered gateways must bound time as well as bytes. A concurrency slot or byte
reservation is not a real resource bound if a peer can stop making progress and
hold it forever. The browser body, upstream upload/header phase, upstream
response body, and downstream browser write therefore use progress timeouts,
while small bodies bypass the large-body weighted budget.

The upload byte reservation must follow the lifetime of the actual buffered
bytes, not the whole downstream handler and not merely the return of
`http.Client.Do`. A Go `RoundTripper` may close a request body asynchronously
after returning the response, so a close-aware request body drops its buffered
reader and only then releases the reservation. This keeps the accounting
truthful without allowing a slow browser response to consume all upload budget.

An identity-replacing proxy also needs an explicit cache boundary. Authenticated
users, Spaces, and mailboxes share the same URL shape, so upstream cache headers
cannot be trusted to express the gateway's identity dimensions. Gateway
responses are therefore always private and non-storable and name all three
selector headers in `Vary`.

Rejecting an unknown-length body also requires expiring the server read
deadline before closing it. `net/http` may otherwise drain an unread chunked
body to reuse the connection; a client that stops immediately after the first
over-limit byte can block that drain forever after the progress watchdog has
already stopped, retaining both the handler and its resource reservations.

Space must be part of the persisted owner mapping, not only request metadata.
The same OCTO UID in two Spaces can otherwise see one shared mailbox and violate
Space isolation even though middleware membership checks pass.

## Rule impact

No new general repository rule. The browser assertion contract, progress
bounds, and the decision not to expose a Bot Draft-send route are recorded in
the linked task brief, module README, and focused tests.
