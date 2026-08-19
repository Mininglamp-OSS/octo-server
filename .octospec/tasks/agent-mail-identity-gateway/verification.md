# Verification

> Historical verification note: the non-HTTP/1.1 response behavior recorded
> below describes the original task head. It is superseded for handler-facing
> HTTP/2 connections by `.octospec/tasks/agent-mail-http2-response/brief.md`,
> which validates and bounded-buffers unknown-length responses before commit.

- Rules checked: `space-isolation`, `trust-boundary`, `error-handling`,
  `rate-limit`, `testing`, `commit-style`.
- Commands run:
  - `go test ./modules/agentmailgateway/... -count=1` -> pass.
  - `go test -race ./modules/agentmailgateway/... -count=1` -> pass.
  - `go vet ./modules/agentmailgateway/... ./pkg/i18n ./pkg/metrics` -> pass.
  - `go build ./...` -> pass.
  - `make i18n-extract-check && make i18n-lint` -> pass.
  - `gofmt -l` on the touched Go files -> clean.
  - `git diff --check` -> pass.
- Acceptance:
  - Browser gateway assertions bind UID, Space, mailbox, method, URI, body,
    lifetime, and nonce.
  - octo-server exposes no Bot Draft-send bridge; owner confirmation and scoped
    delivery remain in the Mail Plugin and octo-mail boundary.
  - At the original task head, known-length and self-delimiting unknown-length
    responses streamed with client-visible truncation on HTTP/1.1; ambiguous
    close-delimited and non-HTTP/1.1 unknown-length responses failed before
    status commit. Handler-facing HTTP/2 behavior is now superseded as noted
    above.
  - Upstream compression negotiation is fixed to `identity`, so a gzip response
    retains its known wire length and succeeds over an HTTP/2 downstream. At
    the original task head, genuinely unknown-length HTTP/2 responses remained
    fail-closed; that behavior is now superseded as noted above.
  - Request buffering has per-request, concurrent-buffering, aggregate
    accounted-byte, and progress-time bounds. Real stalled and over-limit
    unknown-length network bodies produce explicit errors and release the
    large-body slot and byte reservation; the upstream request body drops its
    buffered reader before releasing the reservation; upstream upload/response
    and downstream write stalls terminate; and slow browser reads do not retain
    request-body budget after the upstream upload closes.
  - Every proxied response overrides upstream cache policy with `private,
    no-store` and varies on `token`, `X-Space-ID`, and `X-Octo-Mailbox-ID`.
  - Conditional attachment resume forwards `If-Range`, the signed assertion
    test pins the mailbox claim, and `HEAD` metadata is not rejected merely
    because the represented body is larger than the response-body limit.
- Out of scope check: no general Bot mail-send endpoint, new approval UI, or
  automatic-send behavior was added.
