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

## What was deliberately not done

- Production activation. Both gates keep their `false` defaults and no commit
  changes them.
- Publishing any existing static card. `applyStaticCatalogVisibility` ships as
  an empty list so opening one up stays a separate reviewed decision.
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
