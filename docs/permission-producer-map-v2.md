# Bounded permission producer and consumer map

This is the XIN-2793 response to PjM directive
`83e1136b-f7af-4b23-8e1f-cf8168bb48e8`. It supersedes
`remaining-producers-v1.json` for dependency disposition. The approved v8 backend
appendix §§7, 9, 10 and shared contract `/selected_contract_sections/5` remain
the requirements; no permission, authority or repository scope is relaxed.
Exact source byte hashes and revisions are in
[`remaining-producers-v2.json`](../pkg/permissionbundle/contracts/remaining-producers-v2.json).
This is source evidence, with no deployment inspection or native acceptance.

## Work possible in the three authorized repositories

| Producer / consumer | Delivered or exact remaining output | Dependency disposition |
| --- | --- | --- |
| XIN-2793, `pkg/permissionbundle/releases.go` → `Store` and startup | Concrete `NewArtifactReleases([]SupportedCatalog, []CatalogTransition)` implements `Load`, `Compatible`, `SameIntent`. Startup verifies whole canonical bytes/pin, closed structural fragment, 827 unique identities, ten ordered profiles, content hashes/base64, source/content/profile/action references. Loads return defensive copies. No default/latest or built-in supported pin. | Independent adapter code and synthetic tests delivered here. XIN-2794 must supply the reviewed support list and directed compatible migration pairs. Structural validation is not source/profile reconciliation or support approval. The rejecting template root is unchanged. |
| XIN-2793, association owners → `CommitAssociations` | Existing creation/join/rejoin commit owners use the same native transaction and final policy suffix; current SQL configuration guards, CAS, preparation, route correlation and startup installation seam are delivered. | Real current-authority/native bindings remain owned by this task. Calling `Install` in production before those bindings and a supported release exist would create false authority. No such call or enabling flag is added. |
| XIN-2794, Fullstack-mini, Ken Draft234 at `3b07d4d2f6c4637f0c389a71fab056ddf91c3965` → server and endpoint | One final `permission-release-manifest-v1`, exact immutable version/digest, source/program reconciliation and finite compatible bounds; explicit supported pins and directed same-semantics migration pairs. | Exact registered [component contract](https://github.com/ken-anderson2006/octo-cli/blob/3b07d4d2f6c4637f0c389a71fab056ddf91c3965/docs/permission/cli-component-contract-v1.json) says `supported_catalog=NOT_AVAILABLE`, digest null, production MCP enforcement `NOT_WIRED`. This fixed consumed head is not a claim about its active execution's later output. No competing artifact or candidate digest is supplied here. |
| XIN-2795, Ada-mini2, Ploy Draft102 at `e4601041bd9bab186e4122fb8c76a3444bd1da62` → native delivery / CLI host | Managed `startBot`, mTLS control client and private worker launch are delivered as components. Bind real header/stream identity to pre-ACK replay/context handling; complete main factory, all content/publication/reuse/renewal paths and actual OS containment. | Verified [handoff v2](https://github.com/ploy-elison/codex-channel-octo/blob/e4601041bd9bab186e4122fb8c76a3444bd1da62/docs/permission-endpoint-handoff-v2.json), 5,414 bytes / SHA-256 `3499115d71c48cfd9680d3e465ce7325aae342c67820b44528285fe34a709b02`. It keeps actual native/checkpoint/host composition incomplete. Preserve the registered length-prefixed HMAC delivery identity; no message-ID/delimiter/uint32 substitute. |

`SameIntent` is deliberately conservative: an approved migration pair is
necessary, and any change to the complete entry/program graph disables retained
deny equivalence. That does not prohibit the migration; it requires current
reference validation for that deny. It never silently drops or converts a deny.
Support inputs are trusted startup data from the single catalog owner, never
request data or a manifest's self-declared format/status. The adapter validates
defensive invariants, not facade rendering, branch totality or source equivalence.

The refreshed endpoint handoff retains all eight control methods
(`hello/register_ingress/admit/bind_context/open_relay/renew/close/checkpoint`),
canonical JSON with uint32 length framing ≤65,536 bytes, and an expected
`--turn-relay <handle>` worker ABI. CLI/endpoint must reconcile that actual relay
producer and a bounded carrier preserving the complete 1 MiB/256-original
manifest; a 64 KiB frame is not permission to truncate it. These are sibling
integration obligations, not new server policy/fact wire fields. Server native
outputs remain required at the real `inspect`/content/checkpoint boundaries.

## Actual native dependency and minimum producer changes

The server [go.mod](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/go.mod#L9)
pins `Mininglamp-OSS/octo-lib v0.0.0-20260811160929-233dd6fcdda6`, full commit
`233dd6fcdda60ccf347811d1e7a069f1e9703bf8`. Its
[`config/msg.go:422–490`](https://github.com/Mininglamp-OSS/octo-lib/blob/233dd6fcdda60ccf347811d1e7a069f1e9703bf8/config/msg.go#L422)
calls `/messages`, `/channel/messagesync`, `/message/sync` and `/message/syncack`
at configured `WuKongIM.APIURL`. The content-first
[`MessageResp:946–970`](https://github.com/Mininglamp-OSS/octo-lib/blob/233dd6fcdda60ccf347811d1e7a069f1e9703bf8/config/msg.go#L946)
has an int64 message ID, **uint32** message sequence and payload bytes. Its
`MsgHeader` contains delivery flags, not the approved immutable Header. There is
no original Space/version/read cut or attested delivery incarnation in that DTO.
The ACK method returns an error/status only; it supplies no durable horizon.

Server [`bot_api/sync.go:25–158`](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/modules/bot_api/sync.go#L25)
checks current friend/creator/group/parent membership and then returns content
from that sync endpoint. Those existing native ACL checks stay independent.
They do not prove an original recipient, historical visibility or reply right.
[`webhook/api.go:252–335`](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/modules/webhook/api.go#L252)
persists a mirror, skips no-persist/sync-once messages, optionally parses payload
Space for DM presence, then notifies listeners after commit. Its
[`MsgResp/toConfigMessageResp:849–914`](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/modules/webhook/api.go#L849)
retains the same uint32/content-first representation. Adding columns or tagging
that webhook cannot establish missing original facts. The existing
[card trust model](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/docs/card-protocol.md#L173)
also records that direct IM connections bypass server HTTP ingress and the
post-storage webhook cannot veto them.

| Required native producer work | Smallest additive change and consumer | Compatibility / scope |
| --- | --- | --- |
| Original Header and actual access | In the authoritative append/delivery path, persist and issue trusted original source/Space/author/recipient/version/payload hash and stable recipient stream ID/incarnation/u64 sequence. Add metadata-only inspection and actual original/current read and independent send facts at recognized native cuts. Existing rows without provable original metadata remain unavailable; no guessed backfill. | Preserve old message/sync routes and DTOs. New private versioned operations must not release body bytes during inspection. Server owns SQL classification/member/directed relation atoms and the correlated `NativeFactSource` adapter once native identities/cuts are defined; the native authority owns historical/native read/send atoms. |
| History and disclosure | Add immutable-cut, recipient/Space-bound metadata pagination and opaque context/epoch/Header/version/hash-bound permits, ≤30 seconds, consumed once before exact body/quote/media bytes. Check actual current/original read at issue and consumption. | The approved three fact request arms and ten profiles remain unchanged. Do not emulate with ordinary sync, payload parsing, present membership or the SQL mirror. Server owns authenticated fact projection and provider composition; endpoint owns gated content use. Native route/auth/error mapping must be supplied with the producer contract; no guessed URL is installed. |
| Replay checkpoint and recovery | Native delivery owner must persist recipient stream identity and a monotonic consumed/no-redelivery horizon, bind ACK to the endpoint's committed reservation, and expose checkpoint/fence-through operations with restore/migration/key-rotation guarantees. | Keep ordinary ACK behavior for ordinary consumers. An ACK success or channel high watermark alone cannot authorize replay-journal compaction. Endpoint retains terminal records and stops at its bound until an actual horizon is established. |

An available **candidate implementation repository** is
`Mininglamp-OSS/octo-im` at read-only checkout
`613fc2b2b3a4c23054385ee3f8238fc44a8f68e7` (module
`github.com/WuKongIM/WuKongIM`). Its
[`internal/access/api/routes.go:69–72`](https://github.com/Mininglamp-OSS/octo-im/blob/613fc2b2b3a4c23054385ee3f8238fc44a8f68e7/internal/access/api/routes.go#L69),
[`internal/usecase/message/sync.go`](https://github.com/Mininglamp-OSS/octo-im/blob/613fc2b2b3a4c23054385ee3f8238fc44a8f68e7/internal/usecase/message/sync.go)
and [`pkg/channel/types.go:48–68`](https://github.com/Mininglamp-OSS/octo-im/blob/613fc2b2b3a4c23054385ee3f8238fc44a8f68e7/pkg/channel/types.go#L48)
are concrete producer locations. That Message already uses uint64 sequences;
the pinned octo-lib carrier is narrower. `Message.StreamID` is not evidence of
a recipient delivery-stream incarnation or of the complete original Header.
[`cmdsync/types.go:24–39`](https://github.com/Mininglamp-OSS/octo-im/blob/613fc2b2b3a4c23054385ee3f8238fc44a8f68e7/internal/usecase/cmdsync/types.go#L24)
says legacy request sequences do not select sync/ACK state.
[`cmdsync/app.go:160–224`](https://github.com/Mininglamp-OSS/octo-im/blob/613fc2b2b3a4c23054385ee3f8238fc44a8f68e7/internal/usecase/cmdsync/app.go#L160)
advances the latest CMD conversation read generation, not a recipient replay
horizon surviving restore. Gateway
[`RecvAck`](https://github.com/Mininglamp-OSS/octo-im/blob/613fc2b2b3a4c23054385ee3f8238fc44a8f68e7/internal/usecase/message/recvack.go#L5)
forwards a route ACK. Existing
[`FenceAndDrain/DrainResult:119–162`](https://github.com/Mininglamp-OSS/octo-im/blob/613fc2b2b3a4c23054385ee3f8238fc44a8f68e7/pkg/channel/types.go#L119)
is a channel migration append-drain proof with leader epoch/HW/checkpoint. It
does not declare the recipient consumption/no-redelivery/restore contract above;
reuse would require that additional native proof, not a field rename.

**Exact unresolved deployment fact:** nothing in the pinned server dependency
identifies the deployed IM image's source revision. The server's
[CI fixture](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/.github/workflows/ci.yml#L163)
pins `wukongim/wukongim:v2.2.4-20260313`; this is not proof that the prepared
octo-im head or that image serves the product. PjM must bind the native IM source
maintainer to the actual deployed repository/revision, using the candidate paths
above if that repository is selected, and obtain authority for those bounded
native changes. No fourth repository was edited or tested. An octo-lib release
is optional: a typed private transport adapter can live in authorized
octo-server, leaving the old DTOs intact. A library-wide uint32→uint64 change is
not a prerequisite and is not proposed as a silent compatibility break.

## Database authority: code gap versus deployment facts

The pinned library's
[`config/context.go:81–102`](https://github.com/Mininglamp-OSS/octo-lib/blob/233dd6fcdda60ccf347811d1e7a069f1e9703bf8/config/context.go#L81)
returns one lazily initialized DBR session. Its
[`pkg/db/mysql.go:32–51`](https://github.com/Mininglamp-OSS/octo-lib/blob/233dd6fcdda60ccf347811d1e7a069f1e9703bf8/pkg/db/mysql.go#L32)
wraps a MySQL connector for timing and pool limits. Configuration supplies a DSN
and pool settings, not an attested writer/session/position/recovery capability.
No actual DSN, environment value, endpoint credential or database was inspected.

Existing facilities were checked instead of assuming none existed:

| Existing source facility | What it establishes | Why it does not implement `FencedAuthority` |
| --- | --- | --- |
| [`pkg/cutover`](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/pkg/cutover/cutover.go#L1), [`Flip`](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/pkg/cutover/flip.go#L94) and [runbook](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/docs/cutover-framework.md) | A domain's rollbackable MySQL singleton, one-way allocator-mode CAS/floor and configured expected-mode guard; domain writers can drain on its row lock. | It neither elects a SQL primary nor proves a native commit/read position or surviving minimum acknowledged state. A coherently restored singleton cannot attest a later lost acknowledgement. |
| [`auth.WriterRegistry`](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/pkg/auth/writer_registry.go#L3), [`UseWriterLease`](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/pkg/auth/session_store.go#L796) | Redis live leases fence new credential issuance; the rollout has a MySQL authority floor and Redis-instance-bound scan checkpoints. | Validation readers are explicitly unaffected. It does not fence all policy/native SQL readers, writers or in-flight commits, and a scan restart is not recovery through every externally acknowledged position. |
| [`InitializeSessionRollout`](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/pkg/auth/runtime.go#L98), [control store](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/pkg/auth/session_rollout_control_store.go#L84), [operations runbook](https://github.com/Mininglamp-OSS/octo-server/blob/79701d4912eba79a8bcb02c58ed86609c38335e2/docs/token-session-rollout-runbook.md#L15) | Boot/control ordering before serving, stricter credential floor, evidence-gated transitions and reconciliation. | The authority record is in the same MySQL recovery domain. It exposes no elected-primary session assertion/native position comparator/independent acknowledgement fence for permission reads and commits. |

**Demonstrated missing code:** no approved source adapter binds the policy
transaction to elected-current SQL sessions, records its native durable read or
commit position before client acknowledgement, or checks an independent surviving
minimum position on promotion/restore. `Store.Authority` remains deliberately
unbound. Its post-COMMIT acknowledgement failure requires GET reconciliation.

**Facts not supplied by the source:** actual SQL routing/election mechanism and
generation, old-session read/write fencing, native position domain/comparator,
backup/import/restore ownership, and a minimum-acknowledged-position durability
domain that survives coherent rollback of the serving SQL state. No conclusion
that these are absent in deployment is made from a missing Go API.

**Smallest mainline prerequisite:** PjM binds the existing DB HA/recovery operator
and source adapter maintainer to those facts and its existing capability API,
including proof that old readers/writers/in-flight commits are fenced and all
acknowledged scope/catalog state is recoverable. Then XIN-2793 implements the
transaction-specific `Begin`/`Validate`/`Acknowledge` adapter and startup binding
inside octo-server. If the facility does not exist, adding a required surviving
operational guarantee needs explicit Owner disposition; a new generic fence
service is not authorized here. No DB row, pool, timestamp, process UUID or guessed
GTID is substituted. This distinguishes missing operational evidence from the
known missing adapter code.

## Delivery boundary and next action

Independent loader/compatibility work is delivered; the existing association,
wire and current configuration SQL components remain preserved. The remaining
owned work is the actual SQL/native authority adapter, actual runtime native
atom/disclosure/body composition, replay checkpoint consumption, and production
startup binding. Those cannot be completed against only content-first DTOs or
without the deployed authority contracts. The next bounded action is producer
binding and scoped native/API delivery above, plus XIN-2794's single supported
artifact. Sibling executions were not interrupted, rerun, edited or assigned.

Synthetic local tests establish byte/pin/lifecycle/correlation and transactional
component behavior only. Native freshness, original visibility, body permits,
replay/restore and complete MCP-path acceptance remain NOT_RUN. Independent
review stays deferred; no deployment, ready conversion, merge or waiver is asked.
