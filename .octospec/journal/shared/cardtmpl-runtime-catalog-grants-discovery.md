---
type: Journal
title: "Journal: card template runtime catalog grants and discovery (E3 PR-C)"
description: Record of closing the authorization loop on the dynamic card-template catalog — trusted stored provenance, MySQL producer grants, one-snapshot authorization, Bot static/dynamic merge, visibility-aware discovery and safe export, and one non-production pilot.
tags: ["card", "cardtmpl", "runtime-catalog", "grant", "discovery", "export", "provenance", "trust-boundary", "auth", "space", "bot-api", "database", "migration", "i18n", "testing"]
timestamp: 2026-08-05T00:00:00Z
# --- octospec extension fields ---
task: cardtmpl-runtime-catalog-grants-discovery
upstream: "roadmap E3 PR-C; PR-A #674@68e8134d; PR-B #675@49a66475"
source: self
---
# Journal: card template runtime catalog grants and discovery (E3 PR-C)

## What was done

PR-A and PR-B built the machinery: immutable artifacts, a runtime catalog, an
activation pointer with CAS activate/rollback/block. What they could not answer
was *who may use any of it* — the production authorizer denied every dynamic
business access because there was nothing to consult. PR-C supplies the answer
and wires it through every consumer, in six ordered slices.

1. **Trusted stored provenance.** A dynamic card now carries a server-authored
   `catalog_provenance` marker next to the existing `template_ref`. Historical
   edit and action-context stop guessing the producer from `msg.FromUID` and
   read it from the frame instead. Every raw ingress rejects both keys.
2. **Grants.** `card_template_grant` with template-ID-level
   `discover|send|edit`, exact-over-global precedence, revision CAS, and
   same-transaction audit. Revoke writes a tombstone rather than deleting, so
   revoke→recreate cannot replay an old revision.
3. **One-snapshot authorization.** Activation, block and grant are read in a
   single REPEATABLE READ transaction; the Bot's static allowlist and dynamic
   grants merge behind one resolver that both the capability manifest and the
   send path call.
4. **Discovery and export.** B1/B2 with visibility/Space-aware filtering applied
   before paging, one indistinguishable not-found, and an immutable safe export
   projection built at registration/compile time.
5. **Pilot.** The full loop against real MySQL with the existing `docs` owner,
   `docs-notify` producer and `docs/access_request.decision` RouteSpec.
6. **Reconciliation.** Contract docs, runbook and the L2b threshold statement
   updated to match what shipped rather than what was drafted.

Production gates stay closed. Merging PR-C authorizes nothing.

## Decisions worth remembering

**One authorization decision comes from one read.** The bug PR-C is really
about is not a missing check — it is a *torn* one. Reading the activation
pointer, then the block row, then the grant gives three answers from three
points in time, and a revoke landing between them produces an authorized send
against a grant that no longer exists. The fix is a store interface the caller
*cannot pull apart*: one method, one transaction, everything the decision needs.
That shape is the guarantee; any narrower API would let the split back in.

**Fail-closed means "do not fall back", not "try something else".** Once an
operator points a template ID at a dynamic version, an ungranted, blocked or
unreadable outcome must make the whole ID unsendable. Falling back to the static
card of the same ID would send materially different content under a name the
producer believes it has replaced. Same reasoning for the Bot's Space: the
legacy resolver's forgiving fallbacks are right for tagging a DM envelope and
wrong as an authorization input, so PR-C added a strict twin instead of
loosening the original.

**Discoverability is not permission, and invisibility has one shape.** Seeing
that a template exists is deliberately weaker than being allowed to use it.
Correspondingly, "no such template", "not visible to you" and "blocked" all
answer the same localized not-found — three distinguishable answers would let
any authenticated caller map the private catalog by probing IDs. The same logic
puts visibility/grant filtering *inside* the SQL predicate: filtering after
paging returns short pages, and the shrinkage is itself a count oracle.

**A permanent identity deserves a gate in code.** A version claim is permanent
and global to a catalog. "Check the version was never claimed" as a runbook step
is a step someone eventually skips; as a test-time assertion that names its own
remedy, it cannot be.

## What three review passes cost, and what they bought

The six slices were written, then reviewed three times. Each pass found defects
the previous one structurally could not, and two of the passes found regressions
introduced by the *fixes* from the pass before. That pattern is the most
transferable thing in this task.

**Pass 1 (Slices 1-4, before any database ran).** Fifteen findings, eight
confirmed correctness. All were reachable by reading; none needed a database.

**The first real MySQL run.** Two defects that only a live database could show:
a runtime-catalog integration helper asserting a hardcoded migration count, so
the grant migration broke four unrelated tests; and `classifyRuntimeStoreError`
demoting `ErrRuntimeCatalogDisabled` to "unavailable" once activation resolution
moved inside the snapshot, which would have made the Bot send path report an
outage where it should have declined quietly.

**Pass 2 (Slices 4-6).** Eleven findings, two of them regressions from pass 1's
fixes. Recording the target Space in the marker made every group and thread card
permanently uneditable, because the edit guard compared it against the
envelope's top-level `space_id`, which only DM sends write. And
`docsResultVersionFromFrame` keyed off `template_ref`, which only exists on
post-Slice-1 frames, so the two result-version callers still disagreed for every
card already delivered. Both were single-point fixes shipped without a
round-trip test; both were closed by adding one.

**Pass 3.** Eleven findings, and the headline was an *altitude* observation
rather than a bug: pass 2's Space fix had been applied at the symptom. The
underlying cause was that `botCatalogPrincipal` collapses "the feature gate is
closed" and "the group row would not load" into the same zero value, and three
separate places had grown their own reading of that ambiguity. Fixing the guard
alone produced two more regressions — a multi-Space Bot locked out of editing
its own DM cards with a permanent 500, and an edit that erased the Space from a
group card's marker whenever the gate was dark.

**What actually stopped the cycle** was changing what the code *asks*:

- An edit no longer re-derives the marker's Space. It preserves the one the send
  recorded, which is already on the frame — the rule `pkg/cardtmpl`'s updater
  had followed all along. A value that is copied cannot drift from the resolver
  that produced it, because it never consults one.
- "Space could not be determined" became a state distinct from "there is no
  Space", at each of the three layers that had been conflating them, with the
  unresolvable case reported as a retryable outage rather than a client error.
- Every fix got a test that reproduces the *reported* failure with the fix
  reverted. Mutation-checking each one caught two tests that passed for the
  wrong reason.

**The efficiency finding worth generalizing.** `LoadAuthorizations` batched N
reads into one — and then propagated `ErrRuntimeCatalogDisabled` from a single
template, failing the whole call. The caller's only recourse was to re-ask per
ID, so one administratively disabled template silently reinstated the exact N
round trips the batch existed to remove, for as long as an operator left it
disabled. A batch API's error channel is part of its performance contract: if
one member's ordinary state can fail the batch, the batch is decorative.

**One review finding was wrong about its own mechanism.** Pass 2 reported that
`storedMarkersForUpdate` turns a transient Space-lookup failure into a permanent
`ErrUpdateInvalid`. The outcome is real, but `validateUpdateTarget` rejects an
empty target Space before that code runs, so the fix belonged at the click
ingress — which now refuses to create an event at all when the card's origin
Space cannot be read. The retryable error class drafted for the updater was
deleted once a test proved it unreachable. Shipping a fix at the layer a review
names, without checking the layer actually executes, is how dead code enters a
codebase wearing a justification.

## What was deliberately not done

- Production activation. Both gates keep their `false` defaults and no commit
  changes them.
- Publishing any existing static card. Every static card stays private, so
  opening one up stays a separate reviewed decision. (This bullet used to credit
  an `applyStaticCatalogVisibility` empty list in `main.go`; no such function was
  written. The privacy comes from `Register`/`RegisterJSON` fail-closing to
  private, and `Registry.SetCatalogVisibility` has no production caller — see
  D0c.)
- Deduplicating `SpaceMiddleware`/`LocalizedSpaceMiddleware` and the two Space
  resolvers. Both pairs are near-copies whose *only* difference is failure
  behaviour, and that difference is the point; collapsing them would put a
  forgiving fallback one boolean away from an authorization path.
- L2b enablement. PR-C's grants are consumption-side authorization; the L2b
  threshold asks for upload-side admission, which is untouched.

## Follow-ups

- Quality items recorded but not taken: `ReplaceView`'s extra `Snapshot` round
  trip, and the double marshal in `cardmsg.validateCatalogMarkers`.
- B1 returns `action_contract: null` for dynamic rows; filling it would mean
  compiling every listed artifact, turning one list page into N detail reads.
  Callers read it from B2.
- **Four independent encodings of "is this Space known"** now coexist:
  `botCatalogPrincipal.SpaceResolved`, `resolveBotGrantSpaceID`'s
  `(string, bool)`, `editSpaceCheck`, and `cardActionFrameOrigin.SpaceKnown`.
  A shared type is buildable — `pkg/cardmsg` has no internal imports and is
  already used by all three consumers — and was not built here because the
  refactor spans three modules and this PR is already large. It is the highest-
  value structural cleanup left.
- **`RuntimeAuthorizationBatchStore` should probably be a required interface.**
  It is optional and type-asserted, but both implementations in the repo needed
  it and had to be hand-edited; `runtime_install.go` carries a comment warning
  that forgetting to forward it silently drops callers onto the per-ID path.
  Making it required turns that warning into a compile error.
- **B1 and B2 now apply different visibility implementations.** B1 uses the SQL
  predicate in `store_discovery.go`; B2 resolves through the runtime authorizer,
  which additionally enforces the approved-owner allowlist. B2 is therefore
  strictly stricter, so nothing leaks — but an owner removed from
  `approvedRuntimeOwners` after publish would leave rows that B1 lists and B2
  404s. `LoadDiscoverable` and `selectDiscoverableExactSQL` are now reachable
  only from tests and should be deleted or reunified with B1.
- **`CapabilityFor` still issues one `MetaExact` per dynamically granted
  template**, each opening its own authorization transaction, even though
  `ListAuthorizedTemplates` already returned that snapshot. The static half is
  batched; the dynamic half is not.
- **`ai.reasoning-process` is the wrong shape for this grant model.** Its
  producers are thousands of user-created BotFather bots, and `principal_id` is
  an exact identity with no wildcard. Activating that template ID dynamically
  would make it unsendable for every bot without a grant row — and, because
  `resolveBotGrantSpaceID` refuses ambiguity, the breakage would be uneven:
  single-Space bots cut off, multi-Space bots still serving the static version.
  Either keep that family static, or extend D2 with a principal tier below the
  exact one (an `any_bot` row that an exact tombstone can still shadow, which
  preserves "revoking one bot" as a decidable, auditable fact). That is a
  contract change and belongs in its own PR, not this one.
