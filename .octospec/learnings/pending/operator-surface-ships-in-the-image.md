---
type: Learning
title: "Operator surfaces mount on the server binary; tools/ does not ship"
description: The Docker image builds only the root package, so any operator command left as a standalone main under tools/ is undeliverable exactly where it is needed — production pods and private deployments. Three cutover mechanisms each rediscovered this separately.
tags: [operability, packaging, cutover, cli, delivery]
timestamp: 2026-08-13T00:00:00+08:00
# --- octospec extension fields ---
source: self
origin_task: cutover-framework
status: pending
---

# Operator surfaces mount on the server binary; tools/ does not ship

## Context

The Dockerfile builds one binary from the root package and the image contains
nothing else. `tools/msgextra-version` (#627) and `tools/botevent-seq` (#697)
were therefore never deliverable: executing a production cutover meant
cross-compiling on a laptop and `kubectl cp`ing a 43MB binary into a pod, and a
private deployment shipped runbooks pointing at commands the customer did not
have. #733 hit the same wall for the session rollout and folded its two tools
into `app session-rollout`; the cutover-framework task applied the same move to
the remaining two (`app cutover <domain>`).

The same failure was rediscovered independently three times before it became a
rule.

## Learning

When adding an operator-facing command, decide its delivery path first:

- **Operators run it against production** → mount it as a subcommand of the
  server binary (pre-flag.Parse dispatch in `main.go`, own FlagSet, validate
  the subcommand before touching config or dialing anything, print resolved
  endpoints before acting). It rides the image for free.
- **Developers run it against a source tree** (repro harnesses, linters,
  codegen) → `tools/` is fine; that is what it is for.

A directory-level guard exemption (e.g. genseq_guard's `tools/` skip) is part
of this contract: when a command moves out of `tools/`, the guard must switch
from "directory exempt" to an explicit per-file allowlist, or it either blocks
the move or silently widens.

## Applies to

Any future cutover domain, migration driver, or diagnostic command intended to
be run by an operator rather than a developer.
