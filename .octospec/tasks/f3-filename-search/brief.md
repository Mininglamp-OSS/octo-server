# F3 attachment filename search

## Goal
Apply S14 T1 literal case/width-insensitive filename substrings consistently to channel/global file and mixed searches, while retaining T2 fulltext and cross-field matches.

## Load-bearing behavior
- Shared attachment query and indexer normalized filename contract.
- Only filename matches must preserve punctuation and internal whitespace; fulltext branch requires caption/content evidence and keeps existing cross_fields/MSM/stopwords.
- Query-time authorization, visibility revalidation, channel/Space scoping, revoked/virtual filters, sorting and cursor semantics remain unchanged.
- Existing filename highlight uses original-text positions, safely escaped HTML.

## Out of scope
Frontend, permission rules, body extraction/statuses, document/drive search, relevance algorithm redesign, changes to sorting/count policies.

## Acceptance
- Identical eligible file identities in channel/global file and mixed feeds for case/width/substring variants.
- v1.2 does not become v12; internal spaces and literal wildcard syntax preserved; empty query retains browsing.
- Caption/content-only and name+body cross-field matches retained; dual hits appear once.
- Existing visibility/permission/cursor regression passes.
- Index mapping, live writes and bounded restartable historical backfill agree; no application-side candidate scan during search.

## Review remediation
- Grouped previews use the same literal filename highlights with a 120-code-point window around the first hit. Existing visibility gates remain before projection.
- Companion indexer validates target mapping/analyzer and derived values, and bounds bulk backfill by bytes/count/rate.
