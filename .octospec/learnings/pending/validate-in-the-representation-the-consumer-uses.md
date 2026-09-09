---
type: Learning
title: "Learning: validate a value in the representation its consumer will use"
description: A bound checked as a rich type but consumed after a lossy conversion can pass validation and arrive as the exact value the validation existed to forbid; normalise into the consumer's representation, not the author's.
tags: ["validation", "configuration", "units", "lua", "redis", "guard"]
timestamp: 2026-09-06T00:00:00+08:00
# --- octospec extension fields ---
task: oidc-bearer-jwt-redemption-ledger
status: pending
---

# Validate in the representation the consumer will use

## What happened

Two configurable bounds drove an admission decision. `0` was known to be
catastrophic — it means "every request is too late", i.e. every login refused —
so the loader had an explicit guard: non-positive values fall back to the
default, with a comment explaining that a mis-configured environment must
degrade to the default policy rather than to a total outage.

The decision itself runs in a Lua script, which compares **whole seconds**. The
Go side passed `int64(d / time.Second)`.

`500ms` is positive. It passes the guard. It reaches the script as `0`.

The outage the guard was written to prevent arrived through the one spelling the
guard did not recognise, and only in the path that crosses the language boundary
— the in-process fallback path kept comparing `time.Duration` and behaved
correctly, so the two paths silently disagreed about the same configuration.

## The rule

A guard protects the value **as its consumer sees it**. If the value crosses a
boundary that changes its representation — a unit conversion, an integer
truncation, a serialisation, a narrowing cast — validate after that conversion,
or normalise the value into the consumer's representation at the point of
validation so there is only one reading of it.

Here that meant truncating both bounds to whole seconds with a one-second floor
*inside* the normaliser. That fixed the outage and, as a side effect, removed
the divergence between the two code paths: both now compare the same number.

## Smell to look for

A validation that enumerates bad values (`<= 0`, `== ""`, `nil`) rather than
asserting the property it wants (`the consumer receives at least 1`). The
enumeration is written against the spellings the author thought of; a conversion
downstream can manufacture a new one.
