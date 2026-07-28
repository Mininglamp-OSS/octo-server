---
type: Journal
title: "Journal: cardtmpl-runtime-catalog (roadmap E3 PR-A)"
description: Add the dark, immutable publishing foundation for governed runtime JSON card templates without enabling activation, discovery, or dynamic sends.
tags: ["cardtmpl", "runtime-catalog", "json-template", "control-plane", "database", "trust-boundary", "wire-contract", "testing", "e3"]
timestamp: 2026-07-28T11:14:10+08:00
# --- octospec extension fields ---
task: cardtmpl-runtime-catalog
upstream: "roadmap E3; Issue #669"
pr: 670
source: self
---

# Journal: cardtmpl-runtime-catalog (roadmap E3 PR-A)

## What was done

- Added a shared, error-returning JSON artifact compiler used by both runtime
  control requests and static `Registry.RegisterJSON`. It strictly decodes every
  document, rejects duplicate keys/trailing tokens/invalid Unicode and unsafe
  references, applies document/depth/node/concurrency budgets, compiles schemas
  and templates, renders state samples, checks interaction reports and goldens,
  and derives a deterministic canonical SHA-256 identity.
- Bounded-schema analysis now resolves bundle-local JSON Pointers at every
  schema-bearing position, including array `items`; boolean/empty/unbounded
  targets and reference cycles fail closed. Sample assignment is view-local,
  and a parity fixture compares static and runtime compilation for identity,
  metadata, interactions, samples, and rendered output.
- Kept Go-authored templates fail-closed: a Go template that declares the
  JSON-only `x-octo-constraints` extension now panics during registration instead
  of silently ignoring the constraint. Legacy static JSON fixtures retain their
  pre-E3 governance compatibility while still sharing the parser/compiler.
- Added case-sensitive, immutable `card_template_version_claim`,
  `card_template_artifact`, and append-only `card_template_audit` storage.
  Publish atomically inserts claim + artifact + success audit; same-hash retry is
  idempotent, different-hash or static-source reuse is a conflict, and audit
  failure rolls the transaction back.
- Reconciled the frozen built-in Registry into persistent static claims during
  startup. A dynamic/static exact-key collision or incomplete catalog state
  fails startup rather than allowing replicas to resolve different content.
  Reconciliation uses a no-op upsert followed by source/artifact verification
  and bounded retry for transient transaction failures, avoiding the duplicate
  INSERT lock-upgrade pattern under concurrent replica startup.
- Added `POST /v1/manager/card-templates/validate` and `/publish` behind Auth,
  the shared UID rate limiter, and an explicit super-admin guard. Requests have
  a 2 MiB pre-decode cap, strict envelopes, bounded reason/change-ticket/actor
  fields, localized safe errors, and low-cardinality operation/compile/DB
  metrics. Every response reports `active=false`; no production render path
  reads the dynamic artifact table in PR-A.
- Persistent catalog-integrity violations are logged and reported as the shared
  internal error with a dedicated `integrity_error` metric result; transient or
  unknown store failures retain the retryable unavailable response.

## Load-bearing decisions

- Publish, activate, and authorize remain separate. PR-A only validates and
  persists inactive immutable artifacts; activation/runtime overlay is PR-B,
  while grants and B1/B2 discovery/export are PR-C.
- MySQL is authoritative for version claims and artifacts. Static reconciliation
  happens transactionally before the module serves routes; correctness never
  relies on process-local registration order or eventual cache invalidation.
- Canonical identity is server-computed from parsed content. Equivalent JSON
  formatting produces the same bundle hash and the same canonical manifest
  metadata; caller-supplied hashes are not accepted.
- Runtime governance is stricter than frozen legacy registration: manifest
  identity lengths match the persistence columns, SemVer is canonical, schemas
  must be structurally bounded, and unsupported/ambiguous forms fail closed.

## Verification

- Final focused suites passed for `pkg/cardtmpl/...` and
  `modules/card_template_catalog`; coverage is 80.4% and 81.9% respectively.
- Final race suites passed for cardtmpl/catalog, `pkg/cardmsg`,
  `internal/carddispatch`, `internal/cardactiondispatch`, and the database-free
  Bot template catalog/policy lane.
- `go build ./...`, focused `go vet`, `golangci-lint run ./...`,
  `make i18n-extract-check`, `make i18n-lint`, and `git diff --check` passed.
- The local Bot profile integration lane remains blocked before assertions by a
  stale shared test-database migration record
  (`20191106000001_event_legacy01.sql`). A clean test database remains required
  for that integration lane and for the merge gate.

## Structural learnings / gotchas

- A successful control-plane `validate` response is part of the publish
  contract. Compiler limits must match persistence widths; otherwise a bundle
  can validate successfully and then deterministically fail at the database.
- Checking only `type == "string"` or `type == "array"` is not a complete schema
  resource proof. `{}` permits every shape, and union types such as
  `["string", "null"]` bypass a scalar type assertion. The analyzer now handles
  finite unions, enum/const, local refs, and supported compositions
  conservatively; unknown shapes fail closed.
- Allowing the syntax of a local `$ref` is not the same as proving its target is
  bounded. Every reference must be resolved against the root schema with cycle
  detection, and every nested schema position (notably array `items`) must apply
  the same proof.
- Bundle-wide lookup maps are unsafe for view-local sample/state binding: key
  collisions across views combine with randomized Go map iteration to make the
  compiled artifact nondeterministic. Bind only against the current view's
  declared samples.
- Concurrent startup reconciliation must avoid a plain duplicate INSERT followed
  by verification, which can create an InnoDB S-to-X lock upgrade cycle. A no-op
  upsert plus post-write verification removes that shape; bounded retry is still
  required for transient deadlock/lock-timeout errors.
- Canonical hash equality is insufficient if metadata retains raw formatting.
  Hash input and exposed compiled metadata must derive from the same parsed,
  canonical representation so an idempotent same-hash publish cannot expose
  replica-dependent metadata.

## Review follow-ups / out of scope

- Before PR-B enables dynamic resolution, bound object key cardinality and
  explicitly reject or constrain open-keyspace schema constructs such as
  `patternProperties`; `additionalProperties=false` alone is not a complete
  object resource bound.
- Before untrusted runtime fields can reach dynamic rendering, make their decode
  path preserve the compiler's duplicate-key/trailing-token/depth/node rules;
  the existing static render path still uses standard `json.Unmarshal` before
  schema and final card validation.
- Add a clean-MySQL concurrent same-identity publish integration test and decide
  whether the 2 MiB canonical bundle and engine-contract invariants also need DB
  `CHECK`s as defense in depth.
- Freeze and document the exact canonical-number representation for
  `octo-json-template/v1`; add formal JCS conformance vectors before claiming
  cross-system RFC 8785 reproducibility or changing the identity algorithm.
- Before activation, acquire compile admission before repeated strict-decode and
  canonicalization work, and give publish DB work an explicit request-scoped
  timeout so overload, timeout, transient DB failure, and integrity failure stay
  independently observable.
- Activate/rollback/block, runtime overlay/cache, grants, B1/B2, Bot capability
  merge, multi-replica runtime recovery, and production go-live remain PR-B/PR-C
  and separate rollout work.
