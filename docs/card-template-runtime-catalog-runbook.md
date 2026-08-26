# Runtime card template catalog operations and recovery

This runbook covers the PR-A immutable artifact store and the PR-B runtime
catalog overlay, including startup validation, activate/rollback/block, and a
static/dynamic collision on the same exact `template_id@version`.

PR-B deploys dark by default:

- `OCTO_CARD_RUNTIME_CATALOG_CONTROL_ENABLED=false` blocks forward activate;
- `OCTO_CARD_RUNTIME_CATALOG_NEW_SEND_ENABLED=false` blocks dynamic new-send;
- rollback and emergency block remain available after startup validation so a
  kill switch cannot remove the recovery path.

PR-B does not add producer grants or discovery. A published or active artifact
is not, by itself, permission for a Bot or internal producer to use it.

## Safety invariant

Every exact `template_id@version` has one permanent source: `static` or
`dynamic`. Startup reconciliation must fail closed when the built-in Registry
and the database disagree. Do not add or use a switch that ignores the
collision: doing so can make replicas resolve different content for the same
identity.

### Built-in static versions and binary rollback

During the pre-PR-C dark phase, **do not use Activate or Rollback to switch a
built-in-only template between static versions**. A built-in default is selected
by the image's frozen Registry `SetDefault`; change it by deploying a reviewed
image, not by persisting a catalog pointer. In particular, do not Activate or
Rollback `ai.reasoning-process` to `0.3.0` as a shortcut for the image cutover.

An activation row outlives the image that created it and overrides the
built-in default. If it points to a static version that a rollback image does
not contain, that image sees a permanent `source=static` claim outside its
frozen Registry, classifies the active target as catalog integrity failure,
marks readiness sticky-down, and rejects all catalog resolution. This can take
every replica out of readiness even though the artifact bytes themselves are
valid.

Before any binary rollback, inspect the authoritative activation target and
prove that it is absent or resolvable by the rollback image. If an operator has
already persisted an incompatible static target, stop the rollout and use the
current compatible image under an approved change ticket to Activate, with the
current revision CAS, a target present in both images. Verify the audit and
readiness, and only then roll back the binary. Do not repair this condition by
editing the activation/claim tables directly; Block is irreversible and is not
a shortcut for clearing an accidental static activation pointer.

A future dynamic-catalog pilot may need a reviewed rollback from a dynamic
artifact to a known built-in fallback. That exception must name a static
version present in every allowed rollback image and establishes the same binary
compatibility floor; it is not permission to use the control plane for routine
static-to-static version selection.

Before the first dynamic card is sent, a static-key collision is recovered with
a **binary rollback or corrective binary**, not a catalog bypass or database
rewrite. After a dynamic card is sent, rollback binaries must retain the PR-B
historical exact reader; a pre-PR-B binary is no longer safe.

## Detection and classification

Startup reconciliation and active-target validation run outside module
construction. A transient MySQL failure no longer crashes the whole
`octo-server`: DB-backed default/dynamic/control paths stay unavailable and the
process retries. Explicit static exact-version reads remain available during
that transient outage. Until the first validation succeeds, and after a proven
integrity failure, `/v1/ready` returns `503` with
`dependencies.card_template_catalog=down`; the load balancer must not route
normal traffic to that replica. This deliberately keeps static default
resolution fail-closed instead of serving a fallback whose active-pointer state
has not been established. `/v1/health` remains dependency-free for liveness.

Startup work is bounded by a 30-second static-reconciliation query budget, a
30-second active-target-list query budget, at most 128 active targets, and an
independent 10-second validation budget per target. More than 128 active rows is
an integrity/capacity failure. Normal activate and disabled-to-active rollback
transactions serialize through a single database capacity guard and reject the
129th target; active-to-disabled block uses the same guard so a concurrent
writer cannot race the last slot. The startup `LIMIT 129` check remains the
fail-closed backstop for out-of-band SQL or migration drift. Targets are re-read
and revalidated on a retry; partial progress is not retained because activation
state may have changed between attempts.

Watch these bounded signals:

```text
card template catalog startup validation unavailable; retrying
card template catalog startup integrity failure
dmwork_card_catalog_db_total{operation="startup_validate",result="error|integrity_error|ok"}
dmwork_card_catalog_active_targets
```

An integrity failure poisons catalog readiness and fails all catalog resolution
closed, including explicit static reads, because a source collision has been
proved. The rest of `octo-server` stays up so operators retain diagnostics and
unrelated service capacity.

Classify the logged cause before acting:

| Signal | Meaning | First action |
| --- | --- | --- |
| `version claim conflicts with source` | The image contains a static exact key already reserved by a dynamic publish. | Stop the rollout and follow **Exact-key collision** below. |
| `catalog integrity failure` | Static inventory or persisted claim/artifact state violates an invariant. | Freeze catalog changes and follow **Integrity incident** below. |
| DB timeout/deadlock/connectivity error after bounded retry | Infrastructure is unavailable; no source conflict has been proven. | Restore DB connectivity/capacity and wait for retry; restart only if required by the wider incident. Do not edit catalog rows. |
| Active target missing, blocked, hash/metadata mismatch, unsupported engine/owner, or missing interactive route | The persisted active pointer cannot be served safely by this image/config. | Freeze forward activation, inspect detail/audit, and use the last compatible image/config or the separately reviewed integrity procedure. |

Use the identity from the startup integrity log for read-only diagnosis. Do not
select or export `canonical_bundle` during routine triage.

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

Rejected overwrite attempts are retained with `result='immutable_conflict'`;
attempts to reuse a static claim are retained with `result='source_conflict'`.
These are audit-only commits: they do not change claim or artifact state. If the
audit insert or commit fails, the API returns unavailable rather than an
unaudited conflict response.

## Safe control procedure

The procedures below operate the **dynamic** catalog state machine. In the
current dark phase, a built-in-only template ID is not a control-plane drill
target. The fact that the API can validate a static target is a recovery
mechanism for a future dynamic pilot, not an operational recommendation to pin
ordinary static defaults in MySQL.

Use the manager read endpoints to obtain the authoritative version and
`revision`; never guess a revision or infer rollback history from deployment
notes:

```text
GET /v1/manager/card-templates/{id}
GET /v1/manager/card-templates/{id}/audit?limit=50
PUT /v1/manager/card-templates/{id}/active
POST /v1/manager/card-templates/{id}/rollback
POST /v1/manager/card-templates/{id}@{version}/block
```

The endpoints intentionally omit canonical bundles, schemas, samples, tokens,
and callback secrets.

### Activate

1. Keep dynamic new-send disabled. Enable the forward control gate only in the
   approved environment and only after all serving replicas run the PR-B image.
   For routine forward control, Activate only a reviewed dynamic artifact;
   never use this endpoint to select a built-in static version already
   controlled by the image Registry. The approved compatibility repair in
   "Built-in static versions and binary rollback" is the narrow exception.
2. Validate/publish the immutable artifact and review its hash, owner, protocol,
   interaction contract, and route configuration. Publish accepts only the
   reviewed runtime-owner allowlist and requires startup catalog readiness;
   validate remains available for offline diagnostics.

   There are **two owner allowlists and they are deliberately different**.
   `l2aOwnerAllowlist` (`pkg/cardtmpl`) governs which owners may be *registered*
   as built-in static templates — `docs`, `summary`, `notify`, `action`, `ai`.
   `approvedRuntimeOwners` (`modules/card_template_catalog`) governs which owners
   may be *published and authorized at runtime*, and is narrower: `ai` and `docs`
   only. A template whose owner passes registration will still be refused by
   publish if it is outside the runtime list. Widening the runtime list is a
   reviewed change, not a consequence of adding a static card.
3. Read the current revision, then send an explicit target version and
   `expected_revision`. A stale revision returns a conflict and must be
   refreshed; do not blindly retry the old body.
4. Confirm the success audit and resulting revision. Exercise exact resolution,
   rollback, and block while new-send remains disabled.

   **Changed by E3 PR-C.** PR-B had no producer grants, so drills had to avoid
   any consumed template ID. Grants now exist, so a dynamic version for a
   consumed ID is a supported (and intended) operation — but it fails closed
   until the consuming producer holds a `send` grant in the Space it sends to.
   Two consequences for an operator:

   - `publish`, `grant` and `activate` are three separate operations and none
     implies the next. A published artifact is inert; a granted producer still
     sends the previously active version; an activated version without a grant
     makes the whole template ID unsendable rather than falling back to the
     static card of the same ID.
   - A Bot's advertised catalog is now **request-scoped**. `GET /v1/bot/card/profile`
     answers for the authenticated Bot in its authoritative Space, so two Bots in
     one deployment legitimately see different manifests. When diagnosing "the
     Bot cannot send X", read the profile *as that Bot*, not as the deployment.

### Rollback

- Always name the target version. The server accepts only a version that the
  audit history proves was previously active and that still passes artifact,
  owner, block, compiler, and route validation.
- Do not use rollback for routine static-to-static version selection. A static
  target is allowed operationally only as an explicitly reviewed fallback from
  a dynamic pilot, and only when that exact version exists in every image still
  eligible for binary rollback.
- Rollback remains available when the forward control gate is off. It performs
  a revision CAS and writes the state change plus success audit in one
  transaction.
- A stale CAS is not retryable without first reading the new revision.

### Emergency block

- Block is dynamic-only, one-way, and remains available when forward activate
  is disabled. There is no unblock or artifact rewrite operation.
- If the blocked version is current active, provide an explicit known-good
  fallback that was previously active, or omit fallback to atomically set the
  template to `disabled`. The block, pointer transition, revision, and audit
  commit together.
- If the blocked version is not active, its block audit commits without changing
  the active revision. Every replica re-reads authoritative block metadata even
  on a compiled-cache hit, so a post-commit request cannot use a hot cache to
  bypass the block.
- Preserve the last successfully stored card payload for display; subsequent
  render/edit/action-context operations for the blocked exact version fail.

## Exact-key collision

This is the expected recovery path when a new image introduces a built-in
`id@version` that was previously published dynamically.

1. Stop or pause the rollout. Keep healthy replicas on the last known-good
   image serving; do not enable dynamic control/new-send while the replacement
   reports catalog integrity failure.
2. Roll back to the last image that does not register the conflicting static
   exact key. If that image cannot be deployed, build a corrective image that
   removes the new registration or assigns it a new version. This is the
   break-glass recovery path.
3. Verify all serving replicas report `startup_validate=ok` and no longer emit
   catalog integrity logs. Confirm that the conflicting database claim and
   artifact are unchanged.
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
2. Capture the integrity log, image digest, exact identity, read-only query
   results, and relevant audit rows. Take and verify a database snapshot before
   any repair.
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

- All intended replicas run the same corrected image digest and emit
  `startup_validate=ok`; `/v1/ready` reports
  `dependencies.card_template_catalog=up`.
- Reconciliation and active-target validation complete without
  conflict/integrity errors on multiple fresh starts.
- Every built-in exact key has `source='static'` and no artifact row.
- The original dynamic collision rows remain unchanged unless a separately
  approved integrity repair was required.
- Forward control/new-send gates are in the approved posture; successful
  publish, reconciliation, or activation is not treated as a producer grant.
- No activation row points to a built-in static version absent from any image
  still eligible for rollback. Built-in-only version changes use image
  `SetDefault`, not a persistent Activate/Rollback pointer.
- Multi-replica default resolution, explicit rollback, hot-cache block, process
  restart, and DB-outage behavior have been exercised in the target environment.

After PR-C allows the first dynamic card to be sent, pre-PR-B binary rollback is
not safe for historical exact-version rendering. Keep the runtime reader and use
activate/rollback/block controls; do not reuse the PR-A-only rollback procedure
past that compatibility floor.
