# Journal: custom-sticker-management

Upstream: Mininglamp-OSS/octo-server#26 · Branch: `claude/custom-emoji-management-lv0vlr` (both repos)

## What shipped

Full-stack per-user custom stickers (flat, no packs; user-personal only).

**octo-server** (commit `75b87f6`)
- `modules/sticker/`: `GET/POST/DELETE /v1/sticker/user`. Empty list → `200 {"list":[]}` (kills the #26 404). `AuthMiddleware` + `SharedUIDRateLimiter`; ownership-checked soft delete; i18n envelope via `pkg/errcode/sticker.go` + zh-CN + regenerated en-US markers; `TestStickerNoLegacyResponseError` guard.
- Quota: `system_setting` key `sticker.user_max_count` (default 100, `Positive`, SuperAdmin-tunable, hot-reloaded) + getter `StickerUserMaxCount()`; 409 `err.server.sticker.quota_exceeded` when exceeded.
- `modules/file`: `getFilePath` for `TypeSticker` no longer hardcodes `.gif` — extension derived from the requested `filename`, restricted to gif/png/jpg/jpeg/webp (default `.gif`, back-compatible). New `StickerMaxFileSize` (1MB) cap in `uploadFile`.

**octo-web** (commit on same branch)
- datasource: dead `userStickerCategory`/`getStickers` → `userStickers`/`addSticker`/`deleteSticker` + two-step `uploadSticker`.
- `EmojiToolbar`: fixed「我的贴纸」tab, `+` upload, per-sticker delete, client-side format/size guards.
- Format-aware rendering (`tgs`→`<tgs-player>`, raster→`<img>`) in the picker and `LottieStickerCell`.

## Decisions (confirmed with maintainer)
- Stickers only (no inline custom-emoji); flat (no packs); user-scope only (no system/space packs).
- Message payload keeps `category`, pinned to constant `"user"` (send-path unchanged, old messages still decode).
- Formats gif/png/jpg/jpeg/webp; **not** TGS for user uploads. 1MB cap. List response adds `sticker_id` (for DELETE).
- Quota is the global `system_setting` default only; per-space/per-user override deferred.

## Verification
- Server: `go build` ✓, `go vet` ✓, `make i18n-extract` + `i18n-extract-check` + `i18n-lint` ✓, infra-free tests (`TestStickerNoLegacyResponseError`, `TestRespondStickerHelpers`) ✓. DB integration tests (`api_test.go`: empty-list / add+list / format-reject / quota-409 / delete-ownership) require MySQL+Redis+WuKongIM → CI.
- gofmt: two edited files (`modules/file/const.go`, `modules/common/system_settings.go`) flag pre-existing/gofmt-version-skew nits in regions I did not touch — left as-is to avoid unrelated churn; my additions are gofmt-clean.
- Web: could **not** run `pnpm install`/lint/build/vitest here — the agent proxy denies the repo's pinned `registry.npmmirror.com` (403). Changes mirror existing patterns (axios multipart upload, `WKApp.apiClient`, `@octo/base` re-exported types). **Must be lint/built in CI or locally.**

## Out of scope (future)
Inline custom emoji; packs/categories; system/space packs; favorites / recently-used / drag-sort; per-space/per-user quota override; server-side transcode; sticker-file GC; content moderation.

## Note
No PR opened (not requested). Both branches pushed.
