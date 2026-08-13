# Agent Mail identity gateway

This module exposes the authenticated OCTO route:

```text
/v1/mail-gateway/webapi/v0/*
```

It derives the subject from the OCTO login session and the Space from
`SpaceMiddleware`, then replaces caller credentials with a one-minute HMAC
assertion bound to the exact upstream method, URI, and body.

## Configuration

Both variables are required to enable the gateway:

```text
OCTO_MAIL_GATEWAY_URL=http://octo-mail:8090
OCTO_MAIL_GATEWAY_SECRET=<at least 32 bytes>
```

The optional `OCTO_MAIL_GATEWAY_TIMEOUT` is a Go duration and defaults to
`30s`. The value must not exceed `2m`. It bounds each no-progress phase rather
than the whole browser request:

- reading the browser request body;
- connecting to octo-mail and completing the TLS handshake;
- uploading the buffered request body and waiting for response headers;
- reading the upstream response body; and
- writing each response chunk to the browser.

Each successful body read resets the relevant progress timer, so a large
transfer that continues making progress is not cut off by a whole-request
deadline. The browser request context remains the outer cancellation boundary.

The same secret must be configured as `OCTO_MAIL_GATEWAY_SECRET` in
octo-mail. The secret is an internal service credential and must not be sent to
the browser or used as an account API key.

Only the octo-mail `/webapi/v0` surface is reachable. Browser `Authorization`,
`token`, cookies, caller identity headers, and hop-by-hop headers are not
forwarded. `X-Octo-Mailbox-ID` is the only mailbox selection header and is
validated again by octo-mail against the mapped owner.

Every proxied response is marked `Cache-Control: private, no-store` and varies
on the session token, validated Space, and selected mailbox headers. This
prevents a shared cache from reusing one caller's mail response for another
caller even though their gateway URLs are otherwise identical.

## Streaming and memory bounds

- Known-length responses retain the upstream `Content-Length`; a short transfer
  is therefore visible to the client.
- The gateway requests `Accept-Encoding: identity` from octo-mail instead of
  forwarding browser compression preferences. This prevents Go's transport
  from transparently turning a known-length gzip response into an
  unknown-length response that HTTP/2 cannot safely truncate. If an upstream
  still returns a compressed representation, its encoding and known length are
  preserved for the browser.
- HTTP/1.1 chunked responses stream immediately. A read failure, progress
  timeout, or the 64 MiB response limit aborts the HTTP/1.1 client connection so
  it cannot end as a clean chunked success.
- HTTP/1.x close-delimited responses are rejected before their status is
  committed because EOF cannot distinguish completion from truncation.
  Unknown-length responses are also rejected for HTTP/1.0 and HTTP/2 downstream
  clients, where this handler cannot reliably surface a mid-stream failure.
- Request and response bodies each have a 64 MiB per-request limit. Four slots
  bound concurrent large-body buffering. Bodies over 1 MiB additionally use a
  256 MiB weighted budget until the upstream transport closes its request body.
  Closing that body drops the transport-visible reference to the buffered bytes
  before releasing the reservation, so a slow browser response cannot retain
  unrelated upload capacity. Smaller bodies bypass that large-body budget.
  Requests without a declared length reserve the full 64 MiB allowance and are
  still bounded by the four slots and progress timeout. An over-limit
  unknown-length request is rejected with `413` and its connection is made
  non-reusable before the unread body is closed, preventing `net/http`'s
  connection-reuse drain from retaining those reservations.
