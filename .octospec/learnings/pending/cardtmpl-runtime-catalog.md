---
type: Learning
title: "Runtime artifact validation must prove persistence and resource invariants"
description: A validate endpoint for untrusted authored artifacts must enforce the same storage widths and schema resource bounds as publish; unknown or union schema forms cannot be treated as bounded by omission.
tags: ["runtime-catalog", "json-schema", "database", "validation", "trust-boundary", "testing"]
timestamp: 2026-07-28T11:14:10+08:00
# --- octospec extension fields ---
source: self
origin_task: cardtmpl-runtime-catalog
origin_pr: 670
status: pending
candidate_rule: trust-boundary
---

# Runtime artifact validation must prove persistence and resource invariants

For a control plane that exposes separate `validate` and `publish` operations,
"valid" must mean that the artifact satisfies every deterministic invariant
required by the immutable store. If validation accepts a 129-byte identity but
the database column is `VARCHAR(128)`, the API has created a validate/publish
contract drift even though persistence still fails closed.

Mirror persistence invariants at the compiler boundary and lock them with
tests: identity length/collation, canonical version syntax, canonical bundle
size, engine/visibility values, and any metadata written into narrower columns.
The store should still revalidate defensive invariants, but it must not be the
first layer to discover a caller-correctable artifact error.

Schema resource analysis must also reason about every shape the schema permits,
not only a convenient decoded representation:

- `{}` allows strings, arrays, and objects and is therefore not bounded;
- `type: ["string", "null"]` still requires a string bound;
- finite `enum`/`const`, internal refs, array items, and compositions need
  explicit, conservative handling; refs must be resolved with cycle detection;
- open-keyspace object constructs such as `patternProperties` need a key-count
  bound or rejection before runtime activation.

Unknown schema forms should fail closed until the engine contract deliberately
supports them. Finally, canonical hash input and exposed compiled metadata must
come from the same parsed representation; otherwise equivalent same-hash
artifacts can still expose formatting-dependent metadata.

The proof must also be computationally bounded. A walker that handles
`properties` structurally and then revisits it through a generic map traversal
does exponential work as object depth increases, even when the schema is only a
few kilobytes. A request deadline is ineffective if the walker does not inspect
the context. Treat these as one load-bearing invariant: each structural edge is
visited once, traversal checks cancellation, successful local-ref proofs may be
memoized only after cycle-safe validation, and a total visit budget stops future
schema keywords from creating unbounded work. Keep a depth-cost regression and
a context-classification regression, not only correctness fixtures.

**Candidate rule:** an untrusted artifact validator must prove both downstream
persistence compatibility and worst-case runtime resource bounds, including the
cost of the proof itself. Test every schema form that can widen the accepted
value space, and reject unsupported forms rather than interpreting them as
harmless.
