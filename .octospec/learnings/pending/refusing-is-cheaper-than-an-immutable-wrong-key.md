---
type: Learning
title: "Learning: when a key is immutable, refuse the ambiguous input instead of guessing"
description: If an identifier becomes an unchangeable primary key, an unverified assumption about its shape should fail the request loudly rather than write a row; refusal is configurable, a polluted identity table is not.
tags: ["security", "trust-boundary", "identity", "fail-closed", "data-migration"]
timestamp: 2026-09-02T00:00:00+08:00
# --- octospec extension fields ---
task: oidc-oauth2-provider-abstraction
status: pending
---
# When a key is immutable, refuse the ambiguous input instead of guessing

## What happened

Onboarding an enterprise IdP, the identity row is keyed by
`(issuer, subject)` — unique, and effectively immutable: changing the value later
means every existing binding misses on the first step of login, which is
equivalent to making all those users start over as new accounts.

The vendor's documentation did not agree with itself about what `subject`
contains. The userinfo field table calls it a "sub-number" and shows an 18-digit
example; the quick-start demo comment says it is the **employee number**. A third
reading was available from structure: the employee number already has its own
field in that same response, next to a separate user id — which argues the demo
comment is simply wrong.

Two out of three readings were benign. The third was not, and not symmetrically:
**employee numbers get reused between leavers and joiners.** If `subject` were the
employee number, a new hire would match the departed employee's identity row and
log directly into their account, seeing their messages. By the time anyone noticed,
the table would already be wrong, with no rollback path.

The available verification — capture one real login response — was cheap but
needed another team's participation and was blocking a merge.

## The move

Do not resolve the ambiguity in order to proceed. **Make proceeding-while-wrong
impossible, then proceed.**

A shape guard at the trust boundary refuses a purely numeric subject shorter than
ten digits, before any row is written. The long ids the documentation shows pass;
anything containing a non-digit passes, because it cannot be an employee number;
only the specifically dangerous shape is refused.

The trade this makes is the whole point:

| | if the assumption holds | if it does not |
|---|---|---|
| guess and write | works | silent cross-person data exposure, unrecoverable |
| guard and refuse | works | first login fails loudly, zero rows written, fix is a constant |

The guard also **dissolved the blocker**. The purpose of capturing a real response
was to learn whether `subject` is an employee number. The guard answers exactly
that question at the moment of the first real login, more reliably than a sample
would, and with no data at risk. The verification stopped being a precondition
and became an observation.

## Rule of thumb

- Ask of any new identifier: *if this value turns out to be a different thing than
  I assume, can I still change my mind afterwards?* If the answer is no, the
  ambiguity must be resolved **or fenced** before the first write — not after.
- Prefer refusing a narrow, specifically dangerous shape over accepting only a
  narrow expected one. `len < 10 && all digits` misfires on far less input than
  `must be exactly 18 digits`, while excluding the same hazard.
- A guard over an immutable decision should not be an environment variable.
  Relaxing it is a judgement call about identity correctness and belongs in a diff
  that someone reviews, not in a configmap that someone edits during an incident.
- Never put the identifier itself in the refusal error. Length is enough to
  diagnose; the value is user data and the error travels into logs.
