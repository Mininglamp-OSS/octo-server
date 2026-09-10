---
type: Learning
title: "Learning: a green `go build ./...` says nothing about your test files"
description: go build skips _test.go, so a mechanical rename can leave the whole test binary uncompilable while the build gate stays green; a rename is not done until go vet or go test has run.
tags: ["testing", "refactoring", "rename", "tooling", "verification"]
timestamp: 2026-09-02T00:00:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# A green `go build ./...` says nothing about your test files

## What happened

A de-identification pass over `modules/oidc` replaced a vendor keyword with a
neutral one using a literal string substitution across the package. One of the
matches was inside a Go identifier, and the replacement contained a hyphen:

```go
const bearer-jwtTestSecret = "..."   // not a valid identifier
```

The **entire `modules/oidc` test binary stopped compiling**. The previous session
reported the change as verified on the strength of `go build ./...`, which was
genuinely green — because `go build` does not compile `_test.go` files. Roughly
seventy test cases, including every guard for the security-critical paths that
same session had just written, had silently stopped running.

The breakage survived a session boundary and was only found when the next session
happened to run `go vet`.

## Why the usual reasoning fails here

"It compiles" feels like the strongest possible evidence that a rename was
mechanical and safe — stronger than a test pass, because a rename should not be
able to change behaviour. That intuition is right about *behaviour* and wrong
about *coverage*: the failure mode of a bad rename is not a wrong answer, it is
no answer at all, and the tool that would have told you is the one that was not
run.

The trap is sharper for a rename than for a normal edit, precisely because a
rename feels too trivial to warrant running the suite.

## Rule of thumb

- A rename, a de-identification sweep, or any mechanical substitution is not
  finished until `go vet ./...` or `go test` has compiled the test files. `go
  build` is not a substitute.
- Prefer a tool that understands identifiers (`gofmt -r`, `gorename`, an IDE
  rename) over `sed` when the target string can appear inside an identifier. If
  `sed` is the only practical option, grep the result for the replacement string
  adjacent to identifier characters.
- When reporting a mechanical change as verified, name the command that was run.
  "Build is green" and "tests compile" are different claims.

## Corollary: mechanical substitution damages more than syntax

The same sweep also left, in files that still compiled:

- CJK and Latin words run together where the replacement removed a needed space
- comments naming a file or function that no longer had that name
- four files whose alignment blocks were no longer `gofmt`-clean, because
  identifier widths had changed
- a replacement word that was semantically wrong in its new context (a credential
  *type* substituted where the text meant a *party*)

None of these fail a build. All of them mislead the next reader, which is the
thing the comments existed to prevent. Budget a read-through of the diff, not
just a compile.
