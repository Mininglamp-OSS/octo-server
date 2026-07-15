---
type: Learning
title: "Optional JSON slice/map: nil is not empty"
description: For optional collection fields on a request boundary, treat nil (field omitted) and non-nil-empty (explicit []) as different states. Collapsing with len()==0 silently downgrades a caller bug into a fallback.
tags: ["api", "validation", "wire-contract", "review"]
timestamp: 2026-07-16T02:00:00Z
# --- octospec extension fields ---
source: self
origin_task: card-action-internal-http-actions
origin_pr: dmwork-org/octo-server (approval_card actions follow-up)
status: pending
candidate_rule: error-handling
---
# Optional JSON slice/map: nil is not empty

When adding an optional collection field to an existing request struct
(`Actions []Foo` / `Filters map[string]string` / `Ids []int`), a natural
"caller didn't set it → use default" branch reaches for `len(field) == 0`.
That collapses two wire-level states that JSON keeps distinct:

- `field` omitted → decoded `nil` → the caller wants the default
- `"field": []` → decoded non-nil empty → the caller sent zero elements,
  usually a bug in their generation code

`len() == 0` treats both as "use default." A caller who accidentally builds
an empty array (map lookup miss, filter that removed everything, framework
serializing `Optional.empty()` as `[]`) gets a silent, incorrect fallback
instead of a 400.

**Rule candidate:** for any optional collection on a request struct, split
the branch by nilness:

```go
switch {
case field == nil:
    // caller omitted → apply default
case len(field) == 0:
    // caller sent explicit empty → 400 (caller bug, don't silently downgrade)
default:
    // validate and use
}
```

`omitempty` only affects encode; decode still preserves the distinction. Caught
in review on the approval_card actions follow-up
(`err.server.notify.card_invalid` at the ingress; `cardtmpl` layer treats
non-nil-empty as an explicit rejection error).
