---
type: Learning
title: "When two predicates for one concept coexist, enumerate the states where they disagree"
description: "Picking the stricter of two membership predicates is not a decision until you name the states that separate them. There is usually exactly one reachable state, it is usually a product decision wearing a SQL costume, and a comment that argues in the abstract ('a read must not over-select') hides it from every later reader."
tags: ["acl", "isolation", "wire-contract", "documentation", "testing", "mysql"]
timestamp: 2026-09-08T10:00:00Z
source: self
status: pending
---

# When two predicates for one concept coexist, enumerate the states where they disagree

## The rule

A codebase that has more than one way to ask "is this uid a member of this thing"
has a decision to make on every new query. Choosing "the stricter one" or "the
canonical one" is **not** that decision — it is a way of not making it.

Make it by enumerating the rows the two predicates classify differently:

1. Write down both predicates as sets.
2. Compute the difference and name every state in it that is **reachable** —
   which write path produces it, in which module.
3. For each such state, answer the product question directly: should this row
   appear on this surface?
4. Put that answer, not the abstract argument, in the comment. Pin it with a test
   that asserts **both** sides of the divergence, so the day the other surface
   changes, the comment cannot quietly become wrong.

Step 2 usually terminates with exactly one state. When it does, the choice of
predicate is that single product question and nothing else.

## Why

Abstract framing conceals the case. In octo-server the two predicates are
`is_deleted = 0` and `is_deleted = 0 AND status = GroupMemberStatusNormal`, and
the reasoning offered for the second — *"the canonical predicate; a read must not
over-select"* — is true, unfalsifiable, and tells a reader nothing about what it
does.

Enumerating: removal sets `is_deleted = 1`, so the two agree there. The only
reachable divergence is the **group blacklist**, which sets `status` and
deliberately leaves `is_deleted = 0`. So the whole choice is one question: *does
a group that blacklisted you still appear in your list?* That question has an
obvious answer (the access gates already refuse that uid, so listing it
advertises a room they cannot open) — but nobody had asked it. The comment
justifying the predicate never mentioned blacklisting, the surfaces using the
looser predicate never mentioned that they show such groups, and the two
`member_count` implementations silently returned different numbers for the same
group.

The cost of not enumerating is not usually a bug on the day. It is that the
divergence is undocumented, so the next person to touch either side has no way to
know one exists — and a divergence nobody recorded gets "fixed" in whichever
direction the next reader happens to prefer.

## Smells that this rule applies

- Two functions whose names differ only by a qualifier (`ExistMember` /
  `ExistMemberActive`, `queryXWithMember` / `queryXWithActiveMember`).
- A comment justifying a predicate by its provenance ("the canonical one", "same
  as the reconcile scan") rather than by its effect.
- A status column where one value is written by a path that deliberately does
  **not** touch the tombstone column — blacklists, mutes, suspensions, "pending
  removal" flags. That path is where the two sets separate.
- The same count exposed from two endpoints via two different helpers.

## Counter-case

This is not an argument for unifying the predicates. The looser one is correct
where it is used — a cascade whose job is to delete rows can over-select
harmlessly — and forcing one predicate everywhere would put the blacklist
decision on paths that have no business making it. The rule is to **know and
record** the divergence, not to remove it.
