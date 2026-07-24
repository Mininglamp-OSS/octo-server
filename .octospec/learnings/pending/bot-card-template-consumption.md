---
type: Learning
title: "JSON mode XOR must use field presence, not decoded zero values"
description: For mutually-exclusive JSON request modes, empty string, null, and omission are distinct wire states; record raw key presence before enforcing the XOR.
tags: ["api", "json", "validation", "wire-contract", "trust-boundary", "go"]
timestamp: 2026-07-24T15:10:16+08:00
# --- octospec extension fields ---
source: self
origin_task: bot-card-template-consumption
origin_pr: 659
status: pending
candidate_rule: trust-boundary
---

# JSON mode XOR must use field presence, not decoded zero values

When one JSON request supports two mutually-exclusive wire modes, the XOR must
be evaluated over whether each mode's discriminator key was **present in the
JSON object**, not whether the decoded Go field looks non-empty.

For example, this is still a both-present request and must be rejected before
either mode performs reads or writes:

```json
{
  "content_edit": "",
  "template_ref": {"id": "ai.reasoning-process", "version": "0.1.0"}
}
```

A plain Go struct loses the distinction:

- omitted `content_edit` and `"content_edit":""` both decode to `string("")`;
- omitted `template_ref` and `"template_ref":null` both decode to a nil map;
- checking `strings.TrimSpace(ContentEdit) != ""` or `TemplateRef != nil`
  therefore turns an explicit both-present caller bug into one apparently valid
  mode.

**Candidate rule:** when JSON key presence participates in authorization,
dispatch mode, or a mutual-exclusion contract, record presence during
`UnmarshalJSON` (for example through a `map[string]json.RawMessage`) and enforce
the total XOR on those booleans. Validate each chosen mode's decoded value only
after the XOR. Regression tests should cover empty string and null, and assert
the invalid request performs zero downstream reads/writes.

This refines the existing `dual-mode-request-mutual-exclusion` learning: the
both-present branch is necessary, but it is only correct when “present” retains
wire-level JSON semantics.
