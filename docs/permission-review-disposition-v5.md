# Prior-head automated findings — server continuation

This disposition covers the automated findings on Draft #951 head
`d5ef4b38269b379bac4aa471a6cad06073c563c8`, including
[review 5821661272](https://github.com/Mininglamp-OSS/octo-server/pull/951#pullrequestreview-5821661272)
and the companion startup-composition finding. It records implementation and
limits; independent exact-head review remains deferred.

## Commit followed by failed acknowledgement

The finding is valid. The previous shared unavailable error could send the
conversational Bot creation path into destructive compensation after the native
membership and permanent permission header had committed.

`complete` now returns `ErrReconcile` after an uncertain COMMIT result or failed
post-COMMIT authority acknowledgement. This marker wraps the original public
unavailable class, preserving the closed error envelope. Native callers must
reconcile the exact Bot/Space/header before retry or compensation. Conversational
creation preserves the exact Bot artifacts and propagates the marker. API
creation withholds its success/credential response on that outcome. Other native
association paths propagate the marker without a transient-SQL retry or cleanup;
policy retries remain bounded by the unchanged exact CAS tuple.

A disposable SQLite regression commits a real new native membership/header
pair, rejects acknowledgement, invokes the usual deferred rollback, then reads
both committed records through a fresh successful authority. The existing
mutation regression also asserts the distinct marker and reads the higher
committed revision. This establishes local reconciliation behavior; the real
surviving acknowledgement-position/recovery contract is still a producer gap.

## Original association deadline

The suffix now retrieves the original bounded context registered by
`BeginAssociation` instead of starting from Background. A per-runtime,
transaction-keyed context binding is deleted by the returned cancel function;
all native callers already defer it. An installed composition rejects an
unregistered transaction. `BeginAssociationContext` accepts a request parent for
native callers that have one. Authority acquisition, SQL scope/header queries,
commit and acknowledgement inherit the same deadline. Legacy uninstalled
composition retains its original transaction path.

The regression verifies the exact parent value/deadline observed by the actual
authority suffix and cancellation before commit. No timer sleeps or shared
database are required.

## Native lock order and concurrency

The reviewer identifies the existing difference between parent-first creation
and member-first batch prefixes. Those native owners retain their original
transaction order and seat-transition funnel. The added suffix's classification,
parent-row and permanent-header locks use `FOR SHARE NOWAIT` / `FOR UPDATE
NOWAIT`; they cannot add a waiting edge back to a parent held by the opposing
prefix. Parent contention stops and rolls back the whole unit. The suffix takes
all native scope locks before any permission-header lock. Native absence-range
checks retain their required locking predicates.

This is a static disposition of the reported added lock cycle, not an InnoDB
concurrency acceptance result. Real MySQL deadlock/recovery testing is NOT_RUN.
No whole-operation automatic retry is introduced for ambiguous commit or failed
acknowledgement, because it could repeat a durable native change.

## Production startup

The absence of real startup installation remains an acknowledged incomplete
integration. New v2 registry/Store installation is exercised positively in local
tests. The product binding still requires the actual serving IM source, elected
SQL/native-position/fence/recovery capabilities and supported production catalog
described in `remaining-producers-v2.json` and handoff v4. A pool, local SQL row,
fixture authority or unsupported catalog cannot supply those guarantees.
Production adapters/startup remain owned by XIN-2793 after concrete producer
contracts arrive. Source delivery, native acceptance and independent review are
still open; the blocked boundary is not presented as a completed feature.
