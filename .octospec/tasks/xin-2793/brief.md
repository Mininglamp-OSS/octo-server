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
