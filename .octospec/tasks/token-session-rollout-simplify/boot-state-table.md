# Boot / apply state-transition table

Requested in review round 5. Every round so far found a new P0/P1 inside the code
written to fix the previous round's, which is the signature of a state machine
being patched cell by cell rather than enumerated. This is the enumeration.

Scope: every path that decides **which session mode this replica runs at** and
**what gets written to the floor key**.

---

## The inputs

Two reads, each with three outcomes that must not be collapsed:

| read | outcomes |
|---|---|
| floor (`RolloutControl`) | `error` (unreadable) · `nil,nil` (**absent**) · `present` |
| marker (`markers.Load`) | `error` (unreadable) · `1146` (table not migrated yet) · `nil,nil` (row absent) · `present` |

The recurring defect in this change has one shape, and it is visible in that
first column: **`absent` is not one state.** A floor that reads as absent is
either *never created*, *created and lost*, or *not created yet on a deployment
that will create one*. Only the marker separates them. Every finding in rounds
4 and 5 is a path that read `absent` and picked one meaning without asking.

`provisional` is the fourth input: a mode **guessed** from a failed read, as
opposed to one derived from a floor that was observed. It is what allows a
later observation to correct a guess downward, which the no-lowering rule
otherwise forbids.

---

## A. Boot — `ResolveRolloutBoot` + `runtime.go`

Runs once per process. `W?` = does this branch write the floor key.

| # | floor | marker | outcome | mode | W? | prov | pinned by |
|---|---|---|---|---|---|---|---|
| A1 | present | *(not consulted for the mode)* | `normal` / `adopted` | **= floor** | no | no | `TestWiringFirstBootBeforeMarkerMigration` |
| A2 | absent | present | `rollback-recovered` | **enforce** | **yes** | no | `TestBootResolvesMissingFloorWithoutFailing/recovered` |
| A3 | absent | absent | `fresh` | **expand** | no | no | `TestGenuinelyAbsentFloorWithNoMarkerStaysAtExpand` |
| A4 | absent | 1146 | `fresh` | **expand** | no | no | same branch as A3 |
| A5 | absent | error | `unknown` (via `StrictRolloutBoot`) | **enforce** | no | yes | — |
| A6 | error | present | `rollback-recovered` | **enforce** | **yes** | no | `TestUnreadableFloorResolvesUpwardNotToExpand` |
| A7 | error | absent | `unknown` | **enforce** | no | yes | `TestUnreadableFloorWithNoMarkerIsUnknownNotFresh` |
| A8 | error | 1146 | `unknown` | **enforce** | no | yes | `TestWiringUnreadableFloorIsNotAFreshInstall/marker table absent` |
| A9 | error | error | `unknown` (via `StrictRolloutBoot`) | **enforce** | no | yes | — |

Notes:

- **A1 consults the marker only to pick a metric label.** A floor that read
  successfully is never discarded because the marker is unreadable — that was
  the round-3 regression, and it is why the marker load in this branch is
  deliberately best-effort.
- **A2/A6 are the only boot branches that write.** They write because the marker
  *proves* a floor existed. A5/A7/A8/A9 do not, because nothing proves it, and
  writing enforce on a guess would jump a greenfield ladder irreversibly.
- A5 and A9 have no test. **Gap 1.**

### Correction to review round 5, P2-7

Round 5 states that a #725 deployment with *a floor and no marker* "resolves
downward to `expand` (`session_rollout_marker.go:267-273`)". It does not. Lines
267-273 are the `default:` branch, which requires `control == nil`; a deployment
with a floor takes the early return at :176 (row **A1**) and runs at its floor.
`TestWiringFirstBootBeforeMarkerMigration` pins exactly that, legacy session and
all. The window described in P2-7 is real — the marker row is absent until a
poller stamps it — but its consequence is the metric label reading `adopted`
rather than `normal`, not a mode change. No fix; the table is the answer.

---

## B. Poller — `pollFloor`, every 5s on a running process

| # | floor | marker | action | effect on a **provisional** state |
|---|---|---|---|---|
| B1 | present | — | `ApplyRolloutState(floor)` | corrected, in either direction; `provisional` cleared |
| B2 | absent | present | **recover at enforce** | corrected upward |
| B3 | absent | absent / 1146 | `ApplyRolloutState(expand)` | guess cleared → **expand** |
| B4 | absent | error | nothing | guess retained (cannot decide) |
| B5 | error | — | nothing | guess retained (transient) |

**Before this round the whole `absent` row did not exist**: `pollFloor` returned
immediately on `err != nil || control == nil`, so B2/B3/B4 were one dead branch.
That is review round 5's P1-1 — a provisional `enforce` on a deployment with no
floor record was never revisited, so a two-second Redis error inside the 5s boot
window pinned that pod at `enforce` for its whole life and denied every existing
v1/v2 session on it.

B2 matters as much as B3 and is not in the review's suggested fix: applying
`expand` unconditionally on an absent floor would be a **fail-open** on exactly
the RDB-rollback case the marker exists to catch. The poller needs the same
three-way distinction boot uses.

---

## C. Predicate — `EvaluateRolloutAdvance`, decides whether to advance

| # | floor | marker | target | allowed? |
|---|---|---|---|---|
| C1 | present, `< enforce` | — | `floor.next()` | per gates |
| C2 | present, `enforce` | — | — | terminal, no scan |
| C3 | absent | absent / 1146 | `v3-write` (first floor) | per gates |
| C4 | absent | present | **must refuse** | floor was lost, not never-created — recovery restores it, an advance must not re-create it | 
| C5 | absent | error / no store | **must refuse** | cannot tell C3 from C4 |
| C6 | error | — | — | returns the error |

**C4 is review round 5's second P1** (found only by the automated reviewer).
The predicate has no marker at all — `RolloutAdvanceInput` never carried one —
so an in-flight floor loss reads as greenfield and the reconciler re-creates the
ladder at `v3-write` **over a stamped marker**. Any replica that restarts in that
window boots at `v3-write` (row A1), and `v3-write` accepts precisely the
persistent and over-max legacy bearers `bounded` had begun rejecting.

Note this is the *same cell* as B2 seen from the other path: both are "floor
reads absent while a marker says one existed". Boot got it right in round 4; the
poller and the predicate did not. That is the interaction the review asked to
have written down, and it is why patching one of them would have left the other.

---

## D. Who may change the mode after boot

| writer | may lower? | condition |
|---|---|---|
| `ApplyRolloutState` | only when current is **provisional** | otherwise monotonic |
| `ApplyProvisionalRolloutState` | never | raises only, marks provisional |
| `advanceFloor` | n/a — writes the floor, not the mode | marker → snapshot → CAS, single path |
| `RecoverRolloutControlAtEnforce` | n/a | replaces only an absent or genuinely unparseable record |

The monotonic rule protects **observed** floors. Applying it to a guess is what
produced P1-1; exempting an observed floor from it would re-open round 4's
downgrade. `provisional` is the one bit that separates the two, and rows B1–B3
are the only places it is cleared.

---

## Gaps this table exposes

1. **A5 / A9 untested** — boot with an unreadable marker. Both route through
   `StrictRolloutBoot`, which round 4 changed from `Recovered` to `Unknown`; no
   test pins that it no longer writes a floor.
2. **B2 / B3 / B4 unimplemented** — the poller's entire absent-floor row.
3. **C4 / C5 unimplemented** — the predicate's marker consultation.
4. **`unknown` is not a metric label** — `sessionRolloutBootOutcomes` lists four
   values, so rows A5 and A7–A9 set every series to 0 and the gauge's "exactly
   one outcome is 1" contract is false for the four rows an operator most needs
   to see.

Gaps 2 and 3 are the two P1s. Gaps 1 and 4 are what would have made them
visible: the first as a test, the second as an alert.
