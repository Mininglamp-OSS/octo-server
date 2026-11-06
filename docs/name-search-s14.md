# S14 name search contract

This implements the A-owned F1/F2 core. Client page migrations and Web, Electron,
iOS and Android acceptance remain separate work; adding these endpoints does not
make the old pages use S14 automatically. Content and summary-task search retain
their own rules.

## Endpoints

`POST /v1/search/contacts`, `POST /v1/search/groups` and `POST /v1/search/threads`, authenticated as the
current user, behind the existing Space and UID-rate-limit middleware. They do
not depend on the message-search backend being enabled.

```json
{
  "keyword": "向",
  "space_id": "A",
  "entry": "global",
  "scope": "directory",
  "contact_type": "all",
  "page_size": 50,
  "cursor": ""
}
```

The Space header/query verified by SpaceMiddleware must agree with `space_id`.
For explicit no-Space queries send JSON null and no Space header/query. The Web
Service uses suppressSpaceId to prevent automatic global-header injection.
The server derives UID from authentication, never the request body.

Entry is `global` (64 Unicode codepoints), `address_book` or `name_selector`
(100). Empty/overlong queries fail; no truncation. Entry is not an authorization
credential. Per-page size is 1–100, default 50, with no total-result cap.

## Eligibility and scopes

- Directory contacts: active current-Space humans, global active human friends,
  current-Space active ordinary Bots, published visible platform App Bots, and
  published visible current-Space App Bots. No self. No-Space excludes all
  ordinary and Space App Bots. App identity is checked even when also obtained
  through friendship or Space membership.
- Directory groups: active readable joined product groups, independently of
  group/my's saved/Space list modes. No default subzone results.
- `member`/`mention` contacts additionally require an active readable group
  (`channel_id`, `channel_type=2`) and use its complete active member roster,
  including external humans. Bots still pass R1
  publication/Space checks. This R6 roster never grants mentioning/sending rights.
- Other scopes are rejected rather than falling back to directory.

For build-group/forward/sender/source selectors, A also provides an optional
`allowed_ids` narrowing set. The entry's existing operation eligibility source
must supply a complete, current allowed set. Server R1/R2 checks still run,
then filtering occurs before paging. Omitted/null means no narrowing; `[]`
means no selectable results. `allowed_ids_complete=false` requires an explicit `allowed_ids` array and
reports partial coverage with `total=null` and `selection_pending`. This is a filter, NOT
proof of operation permission; final actions retain existing authorization.
Request bodies beyond 1 MiB fail explicitly, never silently truncate IDs.

The operation scopes below are server-owned providers; `allowed_ids` can only
narrow their already-authorized set, never grant eligibility. Directory scopes
remain R1/R2. Operation rosters use R6, including external human group members
and existing readable DM sources which need not be directory contacts.

| Scope | Kind | Authorized candidates |
| --- | --- | --- |
| `forward` | thread | Active threads in active joined groups; inherited group/individual mute enforced. Archived threads excluded, matching the forward picker. |
| `thread_filter` | thread | Active threads of the supplied authorized group; `include_archived=true` also includes archived. |
| `summary_member` | contact | Current Space human roster; optional group/thread channel narrows to its authorized human roster (including external members). Requires active current Space. |
| `summary_source` | contact | Current user's existing DM conversations within the summary source boundary, with original/remark separately matched. |
| `summary_source` | group | Active joined product groups in current Space, or all joined groups without Space. |
| `summary_source` | thread | Active joined-parent threads with messages, in current Space when present; optional archived threads. |

Thread results have kind `thread`, full channel identity `group____short`,
`parent_id` and optional `archived:true`. There is no per-user thread remark in
the current schema. Parent names do not participate in child matching. UI can
retain parent context separately but must not label that context as a hit.

Summary providers were traced to `octo-smart-summary` candidates.go at
c153bf0fdd9132ce39d1c1f6a78452965b6d84e0. The name-only query authority now lives
beside the shared matcher in octo-server; the summary Web adapter calls these
scopes directly. No summary-service release or duplicate matcher is required.
Existing empty-keyword browse endpoints and final source/action checks remain.
Historical summary roster/DM eligibility excludes system identities (the
existing 32-character human roster constraint); it does not set contact type.
Bot identity, publication and Space checks still apply in every scope.

Contact remarks come from the current user's `user_setting.remark`, exactly as
the remark-update endpoint writes them; `friend.remark` is not a fallback.
All static name-provider SQL binds identities by ASCII-only folding plus binary
comparison, including owner and Space joins. This tolerates migrated mixed
collations without equating distinct IDs (for example s/ß) or changing display
names. Unrestricted legacy IDs retain non-ASCII bytes and embedded ASCII case
compatibility. Source queries have no keyword SQL interpolation.

## Response

Each item includes `id`, `kind`, `contact_type` for contacts, optional `bot_kind`,
`original_name`, `remark_name`, `display_name`, and `matches.original/remark`.
Matches are arrays of `{start,end}` half-open ranges in unchanged display-text
Unicode codepoints. No HTML and no pinyin replacement text is returned. The
page keeps the authoritative order; clients must not re-sort it.

Pages include `items`, `next_cursor`, `has_more`, `complete`, `total`,
`incomplete_reasons`, `rules_version` and `result_revision`. Total is only an
integer when coverage is complete. Missing contact type creates partial data;
it is never assumed human. DB/source failure is an error, not a successful
empty list. Empty successful complete results may display no-results feedback.

## Paging and changes

Candidates come from one repeatable-read snapshot, without per-source top-N.
Ordering follows S14 type/score/position/length/codepoint-name/codepoint-ID.
Filtering and paging occur after complete ranking. Each request recomputes the
result revision and checks the cursor against authenticated user, query,
Space, type, scope, allowed set and rules version. Page-size changes retain
the same ordered sequence. Cursors are not authorization claims.

`err.server.search.query_changed` means clear the old pages and restart at the
first page. The existing error envelope keeps HTTP 400 compatibility and
reports semantic HTTP 409 in `error.http_status`. Invalid cursors use the
request-invalid code. Permission failures are explicit and contain no names.

Client adapters must invalidate on keyword, login, Space, scope/filter, remark
and permission/type changes. A's Web bridge additionally revalidates active
queries every 30 seconds, pauses while hidden and rechecks on focus. This is
an engineering default, not instant cross-device invalidation. The C-owned
page adapters must connect domain invalidation events and current operation
candidate-set refresh; the hook cannot infer permissions held by another service.

## Regression evidence

- `pkg/namesearch/testdata/s14-v1.json` is the golden nine-score, seven-remark
  and ten-type-order fixture for all client integrations.
- `go test ./pkg/namesearch ./modules/search -count=1` runs pure, provider and
  HTTP tests. The MySQL test is skipped unless explicitly configured.
- `S14_MYSQL_DSN='root@tcp(127.0.0.1:PORT)/s14_test?charset=utf8mb4&parseTime=true' go test ./modules/search -run TestNameMySQLIntegration -count=1`
  runs actual SQL against disposable schema fixtures. It refuses non-s14_ DBs
  and is not a full production-migration rehearsal.
