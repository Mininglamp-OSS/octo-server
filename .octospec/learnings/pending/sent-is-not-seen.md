---
type: Learning
title: "\"The message was sent\" and \"someone can see it\" are different assertions"
description: A messaging primitive can prune recipients at more than one layer, so a message that is provably sent can still be visible to nobody but its subject. A test that asserts the send passes forever while the user-facing behaviour is absent. Assert the recipient set, not the call.
tags: ["wire-contract", "testing", "review"]
timestamp: 2026-08-23T11:00:00Z
# --- octospec extension fields ---
source: self
origin_task: space-removal-creator-handover-notice
origin_pr: none
status: pending
candidate_rule: testing
---

# "The message was sent" and "someone can see it" are different assertions

## Context

A Space member removal cascades the member out of the Space's groups. The report
was that the cleanup happened but no system message appeared in the group.

A message *was* being sent. `RemoveGroupMembers` calls octo-lib's
`SendGroupMemberBeRemove` on exactly that path, and the existing regression test
proved it:

```go
assert.True(t, payloadsContain(stub.sentPayloads(), "移除群聊"),
    "被踢出 Space 导致的退群仍要在群里告知")   // "must still tell the group"
```

That test was green. Its stated intent — telling the group — was not happening.

WuKongIM prunes recipients at **two** independent layers, and the primitive sets
both to the removed member:

```go
Subscribers: subscribers,                 // deliver only to these
Payload: {"visibles": subscribers, ...}   // render only for these
"content": "你被{0}移除群聊"                // and it is second person
```

So the message is real, delivered, and invisible to everyone the test claimed it
informed. `payloadsContain` inspects the recorded call; it cannot distinguish a
broadcast from a whisper.

## The rule

When a test's purpose is a *user-facing* outcome, assert the property that makes
it user-facing — not the API call that is supposed to produce it.

For a message that must reach a group, that means asserting the absence of every
recipient-narrowing mechanism the transport has:

```go
// group-visible ⟺ no top-level subscribers AND no payload.visibles
func payloadsContainGroupVisible(payloads []map[string]any, fragment string) bool
```

The general shape: enumerate the ways delivery can be narrowed, and assert none
of them is in play. One layer checked out of two is the same false confidence as
zero.

## Why this hides so well

The failing assertion is one an engineer writes on purpose and reads as correct.
`payloadsContain(payloads, "移除群聊")` is a reasonable-looking test of "we told
the group", and it is a *true statement about the code* — the call really is
made. Nothing about it looks like a stub or a TODO, so it survives review and
keeps passing across refactors while the behaviour it names never existed.

The tell is a mismatch between the assertion's subject and its message: the
assertion is about `sentPayloads()`, the message is about what the group sees.
When those two nouns differ, the test is measuring the wrong thing.

## Related

- `guard-branch-tests-must-reach-the-guard.md` — a test that never enters the
  branch it claims to protect. Same family: the assertion is true and the
  coverage is absent.
- `mutation-check-the-assertion-not-the-guard.md` — the follow-on failure, where
  the *replacement* assertion was also too narrow to catch a re-introduction.
