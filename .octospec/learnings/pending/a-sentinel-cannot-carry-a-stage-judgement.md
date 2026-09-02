---
type: Learning
title: "Learning: an error sentinel cannot carry a judgement about *where* it was raised — and a security classification built on one must be an allowlist"
description: A "is this credential ours?" decision keyed on error identity leaked locally-signed tokens upstream, because the same sentinel is returned on both sides of the signature check; mark the stage explicitly and invert the test so a forgotten mark fails closed.
tags: ["security", "credentials", "error-handling", "api-design", "testing"]
timestamp: 2026-09-02T12:00:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# An error sentinel cannot carry a judgement about where it was raised

## What happened

Two credential types arrive on the same `Authorization: Bearer` header: a JWT we
sign ourselves, and an opaque token belonging to a third-party IdP. Routing them
by *shape* was explicitly rejected — an opaque token may happen to look like a
JWT — so the split was made **cryptographically**: try local HMAC verification
first, and fall through to the upstream only if the token turns out not to be
ours.

Falling through matters because that upstream path puts the credential in a
**URL query string**. Forwarding one of our own tokens means shipping its payload
PII plus a signature valid under our shared secret into a vendor's access log.

So the whole design rests on one predicate: *did this failure happen before we
established that the token is ours?* It was implemented as:

```go
func IsForeignToken(err error) bool {
    return errors.Is(err, ErrJWTMalformed) ||
        errors.Is(err, ErrJWTBadAlg) ||
        errors.Is(err, ErrJWTInvalidSig)
}
```

with a comment asserting that those three "all fail at or before signature
verification". The comment was wrong about one of them. `ErrJWTMalformed` is
returned at three sites **after** `hmac.Equal` has already succeeded:

```
segments / header base64 / header json / payload base64 / signature base64
    ^ before the signature check — attribution genuinely unknown
hmac.Equal ─────────────────────────────────────────────── boundary
payload json / exp is not an integer / payload decode into claims
    v after the signature check — the token is unambiguously ours
```

A token bearing our own valid HMAC, whose payload merely had a wrong field type,
was therefore classified foreign and forwarded. The triggering input is mundane:
a backend writing `iat: Date.now()/1000` without flooring it emits a float, and
the client reuses that one token for its whole ~15-day life — so the leak is
continuous rather than one-shot.

## Why the sentinel could never have worked

An error value answers "what kind of thing went wrong". The decision needed
"how far did we get before it went wrong". Those are orthogonal, and the first
does not determine the second — nothing stops a malformed-input error from
occurring late. The moment one sentinel appears on both sides of the boundary,
every consumer of it is silently wrong for half its cases.

The tell was in the comment itself: it had to *enumerate* which sentinels count,
and enumerations of this kind are claims about code the enumeration cannot see.

## Why the direction of the default is the real fix

Marking the stage explicitly is necessary but not sufficient. A blacklist —
"these errors mean foreign" — makes *forgetting to classify a new error* mean
"foreign", i.e. forward the credential. The safe default is the opposite, so the
test must be an allowlist over an explicit mark:

```go
// only at sites reached before/at hmac.Equal
return fmt.Errorf("%w: %w: ...", ErrJWTForeign, ErrJWTMalformed, ...)

func IsForeignToken(err error) bool { return errors.Is(err, ErrJWTForeign) }
```

Now any check added later, anywhere in the verifier, defaults to "ours" — refuse
locally, do not forward. The stated future-proofing goal actually holds, which it
did not before.

## Why the tests had not caught it

The table enumerating "ours, and rejected on its own merits" listed expired /
zero id / far-future `iat`. All post-signature, and none of them surfacing as the
sentinel that spans the boundary. The signing helper went through
`json.Marshal`, so it **could only mint well-typed payloads** — the entire
failing class was inexpressible with the available fixture. A test table is
bounded by what its builders can construct, and that boundary is invisible when
you read the table.

## Rule of thumb

- When a decision is "at what stage did this fail", encode the **stage**, not the
  error kind. If you find yourself listing sentinels to answer a positional
  question, the list is a guess about code you are not looking at.
- For a security classification, choose the sentinel direction so that
  **omission fails closed**: mark the small, enumerable, safe set and test for
  the mark. Never mark the dangerous set.
- Check whether one sentinel is returned on both sides of your security
  boundary. Grep the constructor of every error the predicate names and note
  which are above and below the check.
- When a test table for "input we must reject" is built by a helper that
  serialises a typed struct, the malformed-payload class is missing by
  construction. Add a raw-bytes signer.
