---
type: Learning
title: "Learning: a guard that asks 'did we sign this?' answers a smaller question than 'is this ours?' — enumerate what you issue, not what you can verify"
description: A credential-provenance split built on HMAC verification leaked session tokens and API keys upstream, because those are ours but are not the kind of credential the verifier understands.
tags: ["security", "credentials", "taxonomy", "architecture"]
timestamp: 2026-09-02T18:00:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# "Ours" is larger than the subset you happen to be able to verify

## What happened

An endpoint accepts two credential types on one `Authorization: Bearer` header:
one we sign (HS256 JWT), one the upstream IdP issues (opaque). Routing them by
*shape* was rejected on good grounds, so the split was made cryptographically:
verify the HMAC; if it doesn't verify, the credential isn't ours, fall through to
the upstream — which puts it in a **URL query**.

The predicate implemented was "is this an HS256 JWT signed with our secret". The
predicate needed was "is this a credential our service issued". The first is a
strict subset of the second, and the gap is not small: session tokens and `uk_`
API keys are unambiguously ours, are not JWTs at all, and therefore failed at
`strings.Split(token, ".")` — landing in the "not ours, forward it" branch.

Both arrive on that header for mundane reasons. The same change had introduced a
global middleware making `Authorization: Bearer <session token>` the convention
for every endpoint in the service, and the sibling routes in the *same route
group* read the same header for `uk_` keys. A session token in a third party's
access log is directly replayable.

The verifier was not wrong. It answered its question correctly. It was wired to a
decision that needed a different question.

## Why the taxonomy is the artefact that failed

After an earlier round of the same defect class, a matrix had been written
enumerating identity paths × credential types × guards. That matrix listed three
credential types — and all three were credentials the **upstream** issues. There
was no row for the ones we issue ourselves, so the cell where this defect lives
did not exist to be checked.

The taxonomy inherited its axis from the feature under review ("which upstream
credential can a client present?") rather than from the harm being guarded against
("what can arrive on this header?"). Enumerations built from the feature's own
vocabulary reproduce the feature's blind spots.

## Rule of thumb

- Write the predicate you need, then check whether the mechanism you have answers
  it. "Can I verify it?" and "did we issue it?" are different questions; a
  verifier answers the first and it is easy to read it as answering the second.
- Enumerate credentials by **who mints them**, not by which subsystem knows how to
  check them. The set of things your service issues is finite, knowable, and
  usually written down as prefixes and stores — grep for them.
- When building a taxonomy to prevent recurrence, choose its axis from the harm,
  not from the feature. "Everything that can arrive on this header" is an axis;
  "the credential types this integration supports" is a feature description that
  will omit whatever else shows up.
- Detection should use deterministic local tests (a canonical prefix, a lookup in
  your own store), never shape heuristics — and it must fail **closed**: if the
  store is unavailable the answer is "undecided", and undecided must not forward,
  or a transient infrastructure error becomes indistinguishable from a leak.
- Separately: a guard placed where a value is *produced* does not cover a value
  produced by the previous binary. Anywhere you deliberately consume state written
  by an older version, the guard has to run on the read side too.
