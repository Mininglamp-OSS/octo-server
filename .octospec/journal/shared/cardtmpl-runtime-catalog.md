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
- Golden expansion now gives strict-decoder `json.Number` values the same
  comparison and interpolation semantics as production `float64` rendering.
  Positive integer schema/aggregate limits accept equivalent JSON exponent
  notation, so canonical bundles containing `1e+6` round-trip through storage
  and recompilation without being mislabeled as unbounded.
- Bounded-schema analysis now resolves bundle-local JSON Pointers at every
  schema-bearing position, including array `items`; boolean/empty/unbounded
  targets and reference cycles fail closed. The validator performs one
  deterministic traversal, checks cancellation throughout, enforces a total
  visit budget, and caches successfully validated local refs; context failures
  remain server failures rather than being relabeled as invalid artifacts.
  Sample assignment is view-local, and a parity fixture compares static and
  runtime compilation for identity, metadata, interactions, samples, and
  rendered output.
- Kept Go-authored templates fail-closed: a Go template that declares the
  JSON-only `x-octo-constraints` extension now panics during registration instead
  of silently ignoring the constraint. Legacy static JSON fixtures retain their
  pre-E3 governance compatibility while still sharing the parser/compiler.
- Added case-sensitive, immutable `card_template_version_claim`,
  `card_template_artifact`, and append-only `card_template_audit` storage.
  Publish atomically inserts claim + artifact + success audit; same-hash retry is
  idempotent, while different-hash or static-source reuse commits an audit-only
  conflict row without changing claim/artifact state. Any success or rejected
  audit failure rolls its transaction back; an audit commit failure is surfaced
  as unavailable instead of claiming an unaudited conflict response.
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
  metrics. The envelope is strictly parsed once and its validated bundle value
  is decoded directly, avoiding the earlier full-envelope canonicalize plus
  nested-bundle reparse. Every response reports `active=false`; no production
  render path reads the dynamic artifact table in PR-A.
- Persistent catalog-integrity violations are logged and reported as the shared
  internal error with a dedicated `integrity_error` metric result; transient or
  unknown store failures retain the retryable unavailable response. Publish DB
  work has its own 10-second deadline, retries the complete transaction up to
  three times for MySQL 1205/1213, and exposes separate bounded timeout,
  cancellation, busy, integrity, conflict, and generic-error outcomes.
- Kept startup reconciliation fail-closed without an ignore-conflict switch.
  The [startup recovery runbook](../../../docs/card-template-runtime-catalog-runbook.md)
  defines rollback/corrective-image recovery for a future built-in exact-key
  collision and reserves direct row repair for a separately audited integrity
  incident.

## Load-bearing decisions

- Publish, activate, and authorize remain separate. PR-A only validates and
  persists inactive immutable artifacts; activation/runtime overlay is PR-B,
  while grants and B1/B2 discovery/export are PR-C.
- MySQL is authoritative for version claims and artifacts. Static reconciliation
  happens transactionally before the module serves routes; correctness never
  relies on process-local registration order or eventual cache invalidation.
- Permanent exact-key claims are a safety boundary, not data to rewrite during
  rollout recovery. A conflicting built-in must move to a new version; binary
  rollback is the PR-A break-glass action because no dynamic runtime reader is
  enabled yet.
- Canonical identity is server-computed from parsed content. Equivalent JSON
  formatting produces the same bundle hash and the same canonical manifest
  metadata; caller-supplied hashes are not accepted.
- Runtime governance is stricter than frozen legacy registration: manifest
  identity lengths match the persistence columns, SemVer is canonical, schemas
  must be structurally bounded, and unsupported/ambiguous forms fail closed.

## Verification

- Final focused suites passed for `pkg/cardtmpl/...` and
  `modules/card_template_catalog`; coverage is 80.6% and 83.0% respectively.
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
- Schema walkers must not combine a structural pass with an unrestricted
  catch-all pass over the same keywords. That turns ordinary nesting into
  exponential CPU work, and a deadline checked only after the walker cannot
  bound it. Single traversal, in-walker context checks, and a total visit budget
  are one invariant and must be tested together.
- A duplicate-key fallback can still deadlock under concurrent idempotent
  publishes. Retry the whole transaction, never a statement inside the failed
  transaction, and restrict retries to known transient MySQL codes.
- Canonical hash equality is insufficient if metadata retains raw formatting.
  Hash input and exposed compiled metadata must derive from the same parsed,
  canonical representation so an idempotent same-hash publish cannot expose
  replica-dependent metadata.
- Validation and production rendering must normalize numeric values at the
  evaluator boundary: strict compilation intentionally preserves `json.Number`
  for canonical identity, while production field decoding yields `float64`.
  Conditions and interpolation must therefore share one value-semantic format.
- Sample assignment with both exact-name and positional fallback is a two-phase
  allocation problem. Reserve every exact match first, then assign only the
  remaining samples in manifest order; a one-pass fallback can steal a later
  state's exact sample.

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
- Before activation or wider control-plane delegation, decide whether the
  remaining single strict envelope parse needs separate admission ahead of the
  compiler semaphore. The redundant nested-bundle parse, canonical sort
  allocations, publish deadline, overload/context classification, and transient
  1205/1213 transaction retry are complete in PR-A.
- Decide in PR-B governance work whether database triggers or restricted grants
  must enforce artifact/audit immutability against direct SQL. Atomic success
  audits and audit-only immutable/source-conflict records are complete in PR-A.
- Split the compiler's owner-required policy from action-contract validation,
  and make the JSON node budget bundle-wide if document limits are ever raised.
  The existing byte/document caps keep the current per-document budget bounded.
- The `blocked` publish response remains an omitted `false` in PR-A and mirrors
  the forward-compatible persistence column; PR-B owns the first state
  transition and must add behavior tests before clients may rely on it.
- Activate/rollback/block, runtime overlay/cache, grants, B1/B2, Bot capability
  merge, multi-replica runtime recovery, and production go-live remain PR-B/PR-C
  and separate rollout work.
