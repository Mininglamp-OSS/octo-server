---
type: Learning
title: "A template engine binds data literally; escaping belongs at the whitelist validator, not the binder"
description: When a server-side templating engine must reproduce an authoring tool's compiled output byte-for-byte, the engine must substitute data verbatim — escaping in the binder breaks byte-equivalence. Put the injection defense at the structural validator (cardmsg.Validate's URL allowlist + element whitelist), which the caller cannot bypass, and keep escaping an opt-in seam.
tags: ["cardtmpl", "template-engine", "trust-boundary", "escape", "wire-contract"]
timestamp: 2026-07-23T12:58:36Z
# --- octospec extension fields ---
source: self
origin_task: cardtmpl-json-template-engine
origin_pr: dmwork-org/octo-server (roadmap E1)
status: pending
candidate_rule: trust-boundary
---
# A template engine binds data literally; escaping belongs at the whitelist validator, not the binder

When a runtime engine compiles authored templates (`.template.json`) against
caller data and must match a golden produced by the authoring tool **byte-for-
byte**, the engine has to substitute `${...}` **literally** — the authoring tool
does not markdown-escape (`run_sql`, `funnel_definition.sql` keep raw
underscores), so escaping in the binder would diverge from the golden and fail
conformance.

That does **not** mean "no defense". Place the injection defense at the
**structural validator the caller cannot bypass** — here `cardmsg.Validate`,
whose positive URL allowlist covers TextBlock markdown links and whose element
whitelist + node/size caps reject anything dangerous. renderCore runs it on the
compiled card, so a literal-bound `[x](javascript:…)` is rejected
(`ErrRenderFailed`) even though the binder never escaped it. Cosmetic markdown
(`*`/`_`) passes through, which is acceptable for first-party/agent-authored
content.

Keep escaping an **injected, opt-in seam** (`EscapeFunc`, identity by default) so
a specific card can turn it on without changing the engine — but the default and
the security guarantee both live at the validator, not the binder.

**Rule of thumb**: byte-equivalence with an external compiler and boundary
escaping are in tension; resolve it by moving the escape to the last validator
the caller can't cross, not by escaping at substitution time.
