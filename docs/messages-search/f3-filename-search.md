# S14 F3 A1: literal attachment filename search

No HTTP request/response shape, permission policy, frontend, cursor, or ordering
changes. The affected routes remain `/v1/messages/_search_files`, `_search_all`,
`_search_global_files`, `_search_global_messages`, and the grouped chat search
that reuses the global-message query. The single-channel fast paths use the same
builders as channel search.

`filename_query.go` is the common attachment keyword branch. It matches
`payload.file.nameLiteral` using case/width folding plus a zero-slop character
phrase. Query edge whitespace is ignored; internal spaces, punctuation and
version numbers are significant. `*`, `?`, `%`, `_` and quotes are literal.

The original IK `cross_fields` query, 75% minimum-match threshold, stopword
handling, analyzer-failure fallback, and field boosts remain. Existing fulltext
matches are retained if either the literal filename matches or at least one
caption/content query token matches. Thus filename + caption/body joint evidence
still works, while filename-only token matches cannot bypass T1. Old eligible
hits keep their score; newly supported filename substrings receive filename boost
2. This intentionally changes recall for filename-only noncontiguous matches.

All query branches remain inside existing channel/Space/revocation/type/virtual
filters and existing post-query visibility checks. No permission grant or scope
expansion is introduced. Empty-keyword browsing remains unchanged.

For literal filename hits, existing `name_highlight` is produced from original
text with escaped HTML, including Unicode case expansions, astral characters,
and overlapping matches. It is computed only on the returned page and never
filters/counts candidates. Existing body snippets and their handling remain.
Grouped chat previews now use the same original-text literal match mapping in a
120-code-point window around the first hit. Their existing `MessageHit.Snippet`
contract remains raw text plus `<mark>` tags; HTML entities are not introduced on
that legacy field (the frontend only decodes FileHit highlight fields). Permission
filtering still happens before preview projection. Caption-only evidence display
and body coverage states are later F3 work.

## Required rollout

Deploy together with `octo-search-indexer`'s S14 F3 mapping, live serialization and
`filename-backfill` command. Follow its `docs/f3-filename-search.md`: create a new
index with the character analyzer, reindex, backfill, refresh/verify, then switch
the single read alias and coordinated writers before enabling this reader. Never
deploy this reader first or run old writers against the new derived field.

## Verification

```sh
go test -race -shuffle=on -count=1 ./modules/messages_search
F3_OPENSEARCH_URL=http://127.0.0.1:9200 \
F3_INDEXER_MAPPING=/path/to/octo-search-indexer/internal/esindex/mapping/octo-message.json \
go test ./modules/messages_search -run TestF3FilenameOpenSearch -count=1
```

The live suite requires isolated OpenSearch with IK and creates/deletes only its
uniquely named test index. It compares channel/global file and mixed results,
two-item cursor pages, distinct same-name identities, dual-field hits, forbidden
channels, revoked documents and the existing DM Space constraint. Module tests
also retain auth/allowlist/visibility/revocation/deletion/clear-history regressions.
