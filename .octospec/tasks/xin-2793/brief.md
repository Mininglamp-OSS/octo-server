# XIN-2793: two-stage permission server boundary

Implement the Owner-approved v8 server boundary on source
`79701d4912eba79a8bcb02c58ed86609c38335e2`, targeting
`chore/mcp-integration-20261103`. Related issue: Mininglamp-OSS/octo-server#950.

Load-bearing paths: strict policy JSON and canonical digests; whole-bundle
revision/CAS and context epochs; exact creator/Bot/Space authority; independent
reference validation and unchanged-deny retention; private projections.

Acceptance: synthetic positive, denial, corruption, stale-CAS, concurrent-edit,
epoch, restart and migration tests. Preserve existing settings, context policy,
native ACL and no-OBO. No live data, shared storage, service startup, deployment,
ready conversion or merge. Additive migration source only.

Native Header/content/replay and current-authority recovery require actual
upstream interfaces. Missing interfaces must remain unavailable and be reported;
synthetic providers prove component behavior only. A candidate catalog must never
be published as a supported release. See `docs/permission-bundle-v1.md` for the
delivery boundary and integration prerequisites.

Owner continuation `8c0bc874-400f-409b-ac37-77f2bf9d1342` adds the implemented
association commit/composition and native-wire adapter integration to the
load-bearing list. All native/project work precedes the final sorted header
suffix. A rejoin cannot seed missing security state. Default permission routes
are absent until trusted startup installs real dependencies. Exact sibling pins
and remaining producer outputs are in `remaining-producers-v2.json`; incomplete
native bindings block source completion and independent review.

PjM continuation `83e1136b-f7af-4b23-8e1f-cf8168bb48e8` adds the concrete
immutable catalog loader/compatibility adapter and bounded source-cited producer
map. Only XIN-2794 publishes a supported catalog; the server consumes its exact
support pins and directed approved migration pairs. Native source provenance and
DB HA/recovery capabilities must be established before actual adapters/startup
can bind. No fourth-repository edit or new infrastructure is authorized.
