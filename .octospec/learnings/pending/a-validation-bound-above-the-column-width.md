---
type: Learning
title: "Learning: a validation bound above the column width is a delayed failure"
description: Sizing an input-length check with "headroom" above the storage column turns a rejected value into a strict-mode write error — and when the column shares an UPDATE with siblings, that error silently discards their values too.
tags: ["validation", "migration", "mysql", "strict-mode", "input-bound"]
timestamp: 2026-09-03T00:00:00+08:00
# --- octospec extension fields ---
task: bot-agent-hosting
status: pending
---
# A validation bound above the column width is a delayed failure

## What happened

A new self-reported column got an input-length check:

```go
const maxAgentHostingLen = 64   // "room for future shapes"
```

```sql
ADD COLUMN `agent_hosting` VARCHAR(20) NOT NULL DEFAULT ''
```

The bound was picked as a round number with headroom — the reflex of someone
sizing a buffer. But the check is not protecting a buffer, it is gating a write
into a fixed-width column, and this DB runs `STRICT_TRANS_TABLES` (verified:
`INSERT REPEAT('x',200)` into `VARCHAR(50)` → `Error 1406 Data too long`, not a
silent truncation).

So a 25-byte value would **pass validation and then fail the write**. On its own
that is merely a confusing 500-less failure. What made it worse: the column
shares **one** UPDATE statement with three sibling columns —

```go
d.session.Update("robot").SetMap(map[string]interface{}{
    "agent_platform": ..., "agent_version": ..., "plugin_version": ...,
    "agent_hosting": ...,          // the 25-byte value
}).Where(...)
```

— so the 1406 discards the caller's three *valid* sibling values as well, and
the endpoint still answers 200 because the write error is only logged. One
oversized field on a field nobody thought was important silently drops an entire
group of reports.

## The rule

**Pin an input-length bound to the storage width, and assert the equality.** Not
`<=`: too large fails as above; too small pointlessly rejects values the column
could hold, which is invisible until someone needs those bytes.

```go
// maxFooLen MUST equal the foo column width.
const maxFooLen = 64
```

```go
func TestFooBoundMatchesColumnWidth(t *testing.T) {
    raw, _ := os.ReadFile("../<module>/sql/<migration>.sql")
    m := regexp.MustCompile("`foo`\\s+VARCHAR\\((\\d+)\\)").FindStringSubmatch(string(raw))
    width, _ := strconv.Atoi(m[1])
    assert.Equal(t, width, maxFooLen)
}
```

The test matters because the two halves live in different files and different
languages. A comment saying "keep these in sync" is a comment; extracting the
width from the migration is a gate.

## Generalisation

Two things travel with this:

- **When several columns share one UPDATE, they share one failure.** Adding a
  column to an existing multi-column write means the new column's validation gaps
  become the *existing* columns' problem. Either validate everything the
  statement touches, or split the statement — but know which one you chose.
- **Bound the input before any operation that allocates proportionally to it.**
  In the same function, the length check had to move *above* `strings.ToLower`:
  the field is caller-controlled and the endpoint takes an unbounded JSON body,
  so a 10MB value must be rejected without first allocating a 10MB lowercase
  copy. `strings.TrimSpace` is safe to run first — it returns a sub-slice.
