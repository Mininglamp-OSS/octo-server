# Journal: card-message-internal-dispatch

## 2026-07-14 — summary sender identity amendment

Product review after PR #579 found that a dedicated `summary` User Bot creates
an unnecessary second system conversation in the user's DM list. The
`summary-notify` producer now binds to the existing `notification` User Bot;
summary cards and their plain-text fallback use the same identity as legacy
notifications.

This entry supersedes the sender choice recorded on 2026-07-13 without changing
the producer's trust boundary: the card capability remains bound to
`summary-notify`, callers still cannot choose the sender or submit arbitrary
type-17 payloads, and the pilot remains DM-only / `octo/v1` /
system-notification policy / max-in-flight 20 per process.

No destructive cleanup is performed for a `summary` identity that may already
exist in a deployed database. Such cleanup requires a separate operational
review because historic messages and conversation references may still point
to that UID.
