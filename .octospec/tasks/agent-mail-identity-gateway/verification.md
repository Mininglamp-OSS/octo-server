# Verification

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
  - Known-length and self-delimiting unknown-length responses stream with
    client-visible truncation on HTTP/1.1; ambiguous close-delimited and
    non-HTTP/1.1 unknown-length responses fail before status commit.
  - Upstream compression negotiation is fixed to `identity`, so a gzip response
    retains its known wire length and succeeds over an HTTP/2 downstream while
    genuinely unknown-length HTTP/2 responses remain fail-closed.
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
