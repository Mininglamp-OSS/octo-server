# Permission Server — Baseline v45 Slice

Draft [#951](https://github.com/Mininglamp-OSS/octo-server/pull/951), task XIN-2793. Builds on the v44 head `a4021ad87ebc95cf995e8ca1dabbb2d331b0cee8`; the v44 document and every v1–v5 contract file stay byte-for-byte unchanged.

Scope limits (Owner v45): no octo-im / WuKongIM change of any kind, no external fence/election/recovery platform as a prerequisite, no live environment change, no production allowlist, no review/deploy/ready/merge.

## 1. Server-only message facts

`pkg/permissionbundle/server_message_facts.go` provides `ServerMessageFactProvider`, an implementation of `NativeIMFactProvider` that reads octo-server's own message storage instead of rejecting SQL message storage by name:

- sharded message tables, `message_extra` (edit/revoke/delete/version), `message_user_extra` and `channel_offset` (per-user delete / clear history), `channel_setting.offset_message_seq`, and `group` / `group_member` / `thread`;
- shard naming, the visibles whitelist, the DM payload space tag and the system-principal test come from the message module through `modules/message.NewServerMessageFactHooks`; the package has no default copy and will not construct without them (`TestServerMessageFactHooksUseModuleHelpers` checks the hooks against `DB.getTable`, `messageUserExtraDB.getTable`, `channelOffsetDB.getTable`, `visiblesAllows`, `extractPayloadSpaceID`).

Lookup uses existing identifiers: channel type, physical channel id and message id, plus the observed native version. Results are metadata only: no message, quote or media body appears in any value, and denied or unverified contexts only ever see `unavailable` with a reason. History keeps the two-step shape (headers, then a permit). The atomic substitute is not implemented.

Uncertain values are never set true. A reference whose version is missing, whose DM space is untagged, or (admit/contributor) whose group/thread original visibility cannot be proven is returned by `SQLNativeFactAdapter` as `unavailable / predicate_unsupported`. octo-server keeps no membership history, so only a visibles exclusion is provable (as false) for group and thread messages.

Outcomes: no row → `mirror_lag`; revoked, deleted, deleted thread or unreadable group → `expired_reference`; malformed or foreign reference → `predicate_unsupported`. An edit changes the native version, so older references stop being version-exact and permits are withdrawn.

Limits stated plainly: the cut is a read-time marker over ordinary SQL, not an acknowledged-state fence; permit one-use tracking is in-process; history pages are stable only against appends; the provider reads on its own session, not inside the caller's transaction. It is an isolated seam and is not constructed by startup.

## 2. Ordinary SQL versus failover/restore

Verified on ordinary SQL: Prepare, independent atomic CAS Save, complete GET readback, reopen retaining incarnation/digest/denies, concurrent CAS and ambiguous commit, and choice-B unchanged departed-member DENY.

Not claimed: failover, restore, PITR or RPO0 safety of acknowledged state. A same-DB counter or SQL writable flag is not independent proof, and manual reconciliation is not equivalent. Relative to v44, the fenced acknowledgement is optional and non-production, so a restored database can re-serve older revisions until XIN-2830 delivers the replacement contract. Startup is unchanged: opt-in without a configured authority fails closed with an explicit error; no no-op authority is installed.

The v44 fence types remain as historical interfaces (see the note at the top of `authority_fence.go`).

## 3. CLI v6 and endpoint heads consumed

- CLI Draft #234 `d39072f8182a28ce2be5884151eb03f16cd205b2` (v6 / ABI-v2). The candidate package, witness and report are unchanged since `27b387f1`. The unmodified files extracted from `d39072f8` verify on this server: 30 files, 7,823,327 stored bytes, semantic digest `15c7ca2bf6df703979df529afbca6828f116b2463b1a818456edfad80bcad799`.
- Endpoint Draft #102 `a3164ded106e06de88f98a6820da4f0d80876dc9` reports `415acc86598208cbf051c9c08cc47e33e0ec9cd8fe7f9444b48b80f11d95ba21` for the same package. Running the v44-predecessor verifier (`ff4d43c4`) on the same unmodified package and witness reproduces `415acc86...` with an identical ledger; the current verifier gives `15c7ca2b...`, which is the digest in the CLI's own report. Only difference: the earlier semantic view hashed selection/rendered maps differently. The only alteration in that reproduction was copying the witness loader, which did not exist at `ff4d43c4`. Endpoint documents should cite `15c7ca2b...`.

## 4. Handoff

`pkg/permissionbundle/contracts/handoff-v6.json` and `remaining-producers-v4.json` replace the active prerequisites: octo-im work and external HA provisioning are removed; the open items are XIN-2830's replacement contract, the version-at-delivery decision, C1–C9 and support pins (XIN-2794), and E1–E12 (XIN-2795). `contract-manifest-v6.json` pins the v45 files and the retained v44 files.
