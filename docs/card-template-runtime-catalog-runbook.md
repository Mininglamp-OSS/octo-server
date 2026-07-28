# Runtime card template catalog startup recovery

This runbook covers startup failures from the runtime card template catalog,
especially a static/dynamic collision on the same exact `template_id@version`.
It applies to PR-A, where dynamic artifacts can be validated and published but
cannot be activated, resolved, or sent in production.

## Safety invariant

Every exact `template_id@version` has one permanent source: `static` or
`dynamic`. Startup reconciliation must fail closed when the built-in Registry
and the database disagree. Do not add or use a switch that ignores the
collision: doing so can make replicas resolve different content for the same
identity.

The PR-A break-glass action is a **binary rollback or corrective binary**, not a
catalog bypass or database rewrite. A normal static/dynamic collision can be
recovered without changing any catalog row.

## Detection and classification

The process exits with a panic containing:

```text
card template catalog: reconcile static inventory: ...
```

Classify the nested error before acting:

| Signal | Meaning | First action |
| --- | --- | --- |
| `version claim conflicts with source` | The image contains a static exact key already reserved by a dynamic publish. | Stop the rollout and follow **Exact-key collision** below. |
| `catalog integrity failure` | Static inventory or persisted claim/artifact state violates an invariant. | Freeze catalog changes and follow **Integrity incident** below. |
| DB timeout/deadlock/connectivity error after bounded retry | Infrastructure is unavailable; no source conflict has been proven. | Restore DB connectivity/capacity, then restart. Do not edit catalog rows. |

Use the identity from the panic log for read-only diagnosis. Do not select or
export `canonical_bundle` during routine triage.

```sql
SELECT c.template_id, c.version, c.source, c.created_at,
       (a.template_id IS NOT NULL) AS has_artifact,
       a.content_sha256, a.created_at AS artifact_created_at
FROM card_template_version_claim AS c
LEFT JOIN card_template_artifact AS a
  ON a.template_id = c.template_id AND a.version = c.version
WHERE c.template_id = '<template_id>' AND c.version = '<version>';

SELECT actor_uid, operation, result, reason, change_ticket, created_at
FROM card_template_audit
WHERE template_id = '<template_id>' AND version = '<version>'
ORDER BY id DESC
LIMIT 50;
```

## Exact-key collision

This is the expected recovery path when a new image introduces a built-in
`id@version` that was previously published dynamically.

1. Stop or pause the rollout. Keep healthy replicas on the last known-good
   image serving; do not scale them down while the replacement is crash-looping.
2. Roll back to the last image that does not register the conflicting static
   exact key. If that image cannot be deployed, build a corrective image that
   removes the new registration or assigns it a new version. This is the
   break-glass recovery path.
3. Verify all serving replicas are ready and no longer emit reconciliation
   panics. Confirm that the conflicting database claim and artifact are
   unchanged.
4. Assign the built-in content a new, canonical SemVer that has never been
   claimed. Update the embedded manifest and every explicit producer/catalog
   reference in the same reviewed change. Never reuse the dynamically claimed
   version, even when the bytes happen to match.
5. Run the card template, catalog, Bot template-policy, and dispatch regression
   gates. Build and deploy the corrected image through the normal rollout.
6. Restart at least two replicas and confirm the new exact key is recorded as
   `source='static'` with no artifact row. Record the conflicting identity,
   image digests, owner, ticket, and final replacement version in the incident
   or rollout record.

Do **not** delete the dynamic artifact or claim, change its `source`, or update
its bytes/hash to make the image boot. Claims are permanent identity history;
rewriting them defeats immutability and can break historical references once
runtime activation exists.

## Integrity incident

`ErrCatalogIntegrity` is not a retryable rollout conflict. Examples include a
static claim that unexpectedly has a dynamic artifact or a dynamic claim with
no artifact during an idempotent publish.

1. Stop the rollout and disable further catalog control-plane changes at the
   operational boundary (traffic/authorization), while leaving existing static
   messaging paths available where the last known-good image permits it.
2. Capture the panic, image digest, exact identity, read-only query results, and
   relevant audit rows. Take and verify a database snapshot before any repair.
3. Escalate to the catalog owner plus SRE/DBA. Treat unexplained direct database
   mutation as a security and audit event.
4. Prepare a separate, two-person-reviewed repair plan with an explicit restore
   point, exact affected rows, post-repair invariant queries, and rollback. This
   generic runbook intentionally provides no `DELETE`, `UPDATE source`, or hash
   rewrite commands.
5. Rehearse the repair on a restored copy, execute it under a dedicated change
   ticket, then restart multiple replicas and re-run invariant checks. Preserve
   the incident record independently of the mutable database tables.

## Exit criteria

- All intended replicas run the same corrected image digest and pass startup.
- Reconciliation completes without conflict/integrity errors on multiple fresh
  starts.
- Every built-in exact key has `source='static'` and no artifact row.
- The original dynamic collision rows remain unchanged unless a separately
  approved integrity repair was required.
- PR-A remains dark: no activation, runtime overlay, dynamic discovery, or
  dynamic send capability is inferred from successful publish/reconciliation.

After PR-B/PR-C allow the first dynamic card to be sent, pre-E3 binary rollback
is no longer safe for historical exact-version rendering. Their rollout
runbooks must retain the runtime reader and use activate/rollback/block controls
instead; this PR-A procedure must not be reused past that compatibility floor.
