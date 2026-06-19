# octo-server rules index

Human-readable catalog of this repo's `.octospec/rules/`. Each rule is an
[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
`Rule` unit with octospec extension fields for on-demand injection. Repo rules
inherit and may override the global constitution
(`inherits: octo-spec@1.1.0` in `manifest.yaml`).

## Rules

- [Space isolation & access control](rules/space-isolation.md) — queries and access checks must enforce space isolation; no cross-space leakage. *(load-bearing, priority 92)*
- [Error handling & i18n](rules/error-handling.md) — user-facing errors must use the localized error envelope. *(load-bearing, priority 85)*
- [Rate limiting](rules/rate-limit.md) — per-UID/per-route rate limiting for service endpoints. *(load-bearing, priority 80)*
- [Testing conventions](rules/testing.md) — test setup must bound the DB connection pool and clean up. *(priority 70)*
- [Commit & PR style](rules/commit-style.md) — repo commit/PR conventions inheriting the global commit rule. *(priority 60)*

For machine-precise injection metadata see each rule's frontmatter, or
`rules/_index.yaml` for the consolidated index.
