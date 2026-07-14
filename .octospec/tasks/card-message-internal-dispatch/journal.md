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

## 2026-07-14 — user share target and authorship correction

User-initiated sharing and automatic origin delivery are separate capabilities.
Origin delivery is tied to the task's immutable source group/thread and is sent
by `notification`; user sharing is an explicit action to a selected DM, group,
or thread and must appear as authored by the sharing user in that conversation.

The previous Bot-card/group-thread-only proposal is superseded. A Bot cannot
inject a message into an existing human-to-human DM: it would create a separate
Bot conversation. The user-share brief now requires a separate authenticated,
server-minted card path with narrow verifiable provenance. Existing generic
user type-17, Bot OBO, arbitrary sender, and arbitrary card payload paths remain
forbidden.

## 2026-07-14 — resource-share platform generalization

The trusted user-share path is a platform capability, not a smart-summary-only
transport. `user-resource-share-card` is the single authority for provider
registration, authenticated sender/target/provenance/rate/idempotency/audit
contracts. The superseded `smart-summary-user-share-card` brief was removed to
avoid two competing share contracts.

Future docs, task, or approval user shares onboard with provider-specific
briefs and adapters. They must not duplicate the human-card trust boundary or
create resource-specific message endpoints. A resource's automated Bot
notification or interactive action remains a separate capability.
