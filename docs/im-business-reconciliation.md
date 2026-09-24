# Durable business-to-IM reconciliation

Group and child-channel membership changes commit a dirty group revision in the
same MySQL transaction as the business mutation. Foreground calls wait for that
revision, while background workers recover requests that never reached IM and
requests whose successful replies were lost. A timeout remains an error and
leaves durable pending work. Retrying does not manufacture a new remove delta.

The receiver must support subscriber protocol 4. Each channel retains its latest
business revision after ordinary receipts expire. A later revision fences every
page of earlier snapshots. A retained completed receipt can return success for
an exact retry without applying it again. A never-applied obsolete snapshot is
rejected. SQL checkpoint updates compare revision and lease owner so an old
worker cannot clear a new mutation.

## Rollout and rollback

1. Upgrade all IM replicas to the companion implementation and verify ordinary
   membership operations. Protocol-3 and protocol-4 binaries can coexist before
   authority activation; new revision commands require all current identities
   to have a replicated protocol-4 capability proof.
2. Apply the embedded group migration and deploy this backend to every writer.
   Keep `DM_IM_RECONCILE_ENABLED` disabled during this preparation.
3. Enable `DM_IM_RECONCILE_ENABLED=true` for all writers together. Stop or drain
   old writers first: old backend binaries cannot record the new SQL intent.
   Drain their in-flight IM requests and legacy cleanup before activation;
   do not mix legacy cleanup with the new authority stream.
   Existing groups are adopted in bounded batches, without resetting already
   recorded revisions. New changes capture their own intent immediately.
4. Check pending age and errors using the queries below. Foreground HTTP success
   continues to mean reconciliation completed, not merely that SQL committed.

After activation, reverting to a legacy writer is unsupported. Managed IM
channels reject legacy subscriber, denylist, disband and metadata mutations.
This backend refuses to start with the flag disabled when authority rows exist.
The migration does not drop queues or authority tombstones on rollback. IM
protocol-3 binaries cannot safely reopen protocol-4 data. Back up SQL and IM
consistently; restoring only one side can roll back the revision sequence.

Adoption covers groups and child ownership present in SQL. It cannot infer the
identity of an orphan that was physically deleted before this change recorded
its ownership. Existing orphan discovery/repair is a separate data audit.

## Work bounds and semantics

- Four active groups per process, two background workers, four concurrent IM
  requests per group. The same permits cover foreground and background work.
- Each claim handles at most 16 children plus the parent, with 128-UID pages.
  Both child and member cursors persist across attempts. Full canonical member
  state is read to compute a stable snapshot digest; this is not constant-memory
  reconciliation for arbitrarily large groups.
- Claims commit before network I/O. Mutation lock order is Space-seat guards,
  parent group, intent, then member/child locks. Workers use consistent reads
  after locking the intent, without holding locks across HTTP.
- Failures retry within the existing five-second foreground budget. Remaining
  work backs off in SQL and is never abandoned. A new business mutation resets
  the backoff and supersedes obsolete work.
- Blacklisted members lose parent and child subscription/access. A per-member
  mute retains subscriptions and follows the existing parent-only mute policy.
  Group disband and soft child deletion retain historical memberships/messages;
  physical deletion records a terminal empty snapshot.
- Thread membership preferences, pins, message counters and native auth/Space
  checks retain their existing roles. A reconciliation queue is not a new
  authorization source: the committed business tables are the source.

## Operations

```sql
SELECT pending, COUNT(*) FROM im_group_reconcile GROUP BY pending;
SELECT group_no, revision, completed_revision, attempts, next_attempt_at,
       lease_until, last_error, updated_at
FROM im_group_reconcile WHERE pending = 1
ORDER BY updated_at LIMIT 100;
```

A pending row means SQL and IM may temporarily differ. Do not delete it to
clear an alert. Investigate IM availability/capability, queue backpressure and
`last_error`; allow the current revision to reconcile. Expected injected-fault
HTTP errors must be counted separately from normal-load latency acceptance.

## Validation status

Real MySQL tests cover atomic rollback, released locks during blocked HTTP,
stale checkpoint rejection, restart/child paging, retry revival, member paging,
canonical identity/adoption, historical disband behavior, concurrent mutations
and foreground retry/final timeout. Source admission guards also pass.

A source-built isolated backend and IM passed 437 core business assertions with
three actual Chromium/WKSDK clients: group/child creation, concurrent messaging
and membership churn, mute/blacklist restoration, voluntary/repeated exit,
rejoin, historical disband behavior and 96 persisted message identities.
Four actual business fault scenarios passed on the earlier companion binary: lost successful create replies
through the foreground deadline; never-forwarded child requests; backend
SIGKILL after SQL commit; and failed removal followed by rejoin and late old
snapshots. The earlier single-creator larger run also passed: 25 real SDK clients, 126 groups, 252 children,
6,379 core assertions and 917 persisted message identities. All 709 business
HTTP requests returned 200 (p99 120.673 ms, maximum 156.195 ms), and every SQL
revision completed. IM sampled memory peaked at 186,806,272 bytes with no OOM
or CPU throttling. The create/churn duration is reported separately from setup;
this business run is not a one-hour saturation test. Final validation adds
16 concurrent creators while keeping the normal per-account rate limit, and
uses the final IM binary with the adoption deletion-boundary fix. Its separate
acceptance record includes exact hashes and measured results.

Final-binary business acceptance (`988a5a80` IM + `fea9536` backend) passed:
25 fresh actual SDK clients, 126 groups, 252 children, 16 parallel creator workers,
three active groups with ten kick/rejoin rounds each, 4,886 core assertions and
419 persisted message identities. All 710 business HTTP requests returned 200;
p99 was 1,691.190 ms and maximum 1,969.815 ms. The creation/churn phase lasted
44.174 seconds; registration, friendship setup and audits were separate.
The normal 550 ms per-account pacing and registration limit were retained.
All 265 authority rows completed, with zero pending/incomplete revisions.
Sampled IM memory peaked at 200,306,688 bytes and MySQL at 518,787,072 bytes;
no container hit memory limits, OOM or CPU throttling. This stronger concurrency
case has different latency and message counts from the earlier single-creator
run; the earlier 120 ms p99 is not substituted for it.

A pre-existing removal-tip defect remains: the pinned octo-lib sends both
`channel_id` and `subscribers`, which the existing IM message API rejects. This
change does not claim to fix that separate message protocol. A failed tip
check and server errors are retained; membership/access assertions are separate.

Run the SQL tests against an isolated MySQL 8 instance with an account allowed
to create/drop temporary test databases:

```sh
IM_RECONCILE_TEST_DSN='test-user:test-password@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4' \
  go test -race -count=1 -timeout=120s ./pkg/imreconcile
```

The tests use fresh random database names. Existing module TestMain helpers
hard-code `root:demo@tcp(127.0.0.1:3306)/test` and can clear that database; run
those only in their dedicated test environment. Source-built business tests
use a separately configured stack and do not use those helpers.

Final-binary business fault validation also passed all four cases with the new
25-user fixture: lost successful replies through the foreground deadline;
never-forwarded child creation; backend SIGKILL after SQL commit; failed kick,
rejoin and five delayed old snapshots (all 409, latest membership preserved).
All 266 authority rows completed afterward. Business failure responses remain
the existing HTTP400 envelope; injected IM503 responses are not reported as
normal-load successes. Configuration was restored after the fault run.

Final IM binary `988a5a80` (`60931515d80a7f7c5ce800f9c317870d6199d4e1153e5e680fcd4a389f246c88`) additionally passed
603.452 seconds of the same controlled mixed workload:
8,121,536 HTTP attempts, zero failures and zero Raft
timeouts. Churn sustained 112.650/s,
maximum 1.591 s. Quiet observation, all membership and
witness checks, 448 sampled message identities,
all 378 persisted message tails (7,977,210 messages)
and restart checks passed. This final-binary short regression complements the
explicitly identified earlier hour; it does not relabel that hour's binary.

Machine-readable evidence: [validation record](im-reconciliation-validation-20260925.json).
