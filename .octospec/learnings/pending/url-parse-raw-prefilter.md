---
type: Learning
title: "URL boundary checks must prefilter the raw string, not the parsed struct"
description: Go's net/url silently drops empty fragments and folds ports into Host. Validating only url.URL fields lets malformed raw URLs through. Reject on the raw string before parsing.
tags: ["ssrf", "url", "validation", "trust-boundary", "review"]
timestamp: 2026-07-16T02:00:00Z
# --- octospec extension fields ---
source: self
origin_task: card-action-internal-http-actions
origin_pr: dmwork-org/octo-server (approval_card actions follow-up)
status: pending
candidate_rule: security
---
# URL boundary checks must prefilter the raw string, not the parsed struct

Validating callback / webhook URLs by inspecting `url.URL` fields alone is
insufficient. `net/url` is deliberately lenient in three places that matter
for a trust-boundary check:

1. **Empty fragment is discarded.** `url.Parse("https://host/path#")`
   produces `Fragment == ""` — indistinguishable from a URL that had no `#`
   at all. Checking `u.Fragment != ""` accepts both. `u.ForceQuery` provides
   the equivalent for a trailing `?`, but there is no `ForceFragment` — you
   must reject `#` on the raw string.

2. **Host contains the port.** `url.Parse("http://:8080/x").Host == ":8080"`
   is non-empty and looks valid. `u.Hostname()` strips the port and returns
   `""`, which is what a hostname predicate actually wants.

3. **Scheme-only oddities.** `url.Parse("https:opaque")` leaves `u.Opaque`
   set and `u.Host == ""`. Some strict scheme checks miss this because the
   scheme is right; the shape isn't.

**Rule candidate:** at any external-URL trust boundary (callback allowlist,
webhook target, image proxy, redirect destination), the validator should:

- trim-then-reject-if-mutated (`strings.TrimSpace(raw) != raw`),
- prefilter the raw string for `#` (and for `?` if you don't rely on
  `ForceQuery`),
- parse, then check `u.Scheme` against a small allowlist, `u.Hostname() != ""`
  (not `u.Host`), `u.Opaque == ""`, `u.User == nil`,
- reject `u.Fragment != ""`, `u.RawQuery != ""`, `u.ForceQuery`.

Caught in review on the approval_card HTTP-scheme follow-up
(`internal/cardactiondispatch.validateCallbackURL`). Both misses had test
vectors: `https://host/path#` and `http://:8080/path`.
