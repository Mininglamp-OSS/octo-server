---
type: Journal
title: "Journal: file-extension-policy-dynamic-config"
description: Upload extension allow/block lists and the single-file size cap move from an init()-mutated package map and three hard-coded constants into system_setting, read through an immutable snapshot; both extension keys are env ∪ DB unions with a non-revocable built-in blocklist, and the effective limits are served from appconfig.
tags: ["file", "upload", "trust-boundary", "external-content", "wire-contract", "error-response", "i18n", "bot-api", "config", "testing"]
timestamp: 2026-08-26T00:00:00Z
# --- octospec extension fields ---
task: file-extension-policy-dynamic-config
upstream: self
source: user
---
# Journal: file-extension-policy-dynamic-config

## What was done

Blocking an upload format used to mean editing a configmap and restarting every
pod. Three `system_setting` keys now drive it, read from an immutable snapshot
published through `atomic.Pointer` (`modules/file/policy.go`):

| key | direction |
|---|---|
| `file.extra_blocked_extensions` | emergency blocking |
| `file.extra_allowed_extensions` | open a format without a release |
| `file.max_size_kb` | single source for all 7 size checkpoints |

`modules/common/system_settings_file_upload.go` is the config layer (DB snapshot,
env compatibility, normalisation, hard-cap clamp); `modules/file/policy.go` owns
the policy semantics, because the built-in blocklist is `modules/file` knowledge
and `common` cannot import `file`. Two hooks bridge that direction: the appconfig
limits provider and the built-in-blocked probe, both registered from `file`'s
`init()`.

## Load-bearing decisions

- **Both extension keys are `env ∪ DB` unions.** An earlier revision had the
  allowlist override env. That is an outage waiting to happen: these deployments
  already allow a handful of formats via configmap, so an operator adding one
  more from the console types just the new extension — and silently drops the
  rest. The rule is now one sentence: *"allow" only adds, "block" only removes.*
  Revoking an env-allowed extension goes through the blocklist.
- **No code-side candidate set gating what may be opened.** The first design
  required allowlist additions to hit a reviewed in-code list. It was dropped:
  `.html`/`.htm` already ship in the built-in allowlist, so the gate would not
  have covered the real exposure, while making the feature useless for the one
  thing it exists to do — open a format without a release.
- **The built-in blocklist is non-revocable, enforced three times.** Rejected at
  write time (`ErrFileExtensionNotAllowlistable`), filtered on read so
  `effective_value` never advertises an inert entry, and excluded structurally by
  the derivation `allowed = (base ∪ extra) − blocked`.
- **Snapshot reuse compares input slices, never a joined fingerprint.**
  Normalisation does not reject separators, so any join collides.
- **Both CSVs are bounded** (64 entries / 32 chars). `system_setting.value` is
  TEXT and the list is served verbatim from the unauthenticated
  `/v1/common/appconfig`. The sticker keys get this bound for free from their
  fixed 5-entry intersection; this one had to be added explicitly.
- **`applied` in the manager response.** A failed local reload used to be
  indistinguishable from success — fatal for emergency blocking, where the
  operator reads 200 as "blocked".

## Gotchas worth remembering

- **Hand-injected tests cannot prove the wire is connected.** The first revision
  forgot `SetPolicySettings()` in `File.New()`. Every test injected a fake through
  a helper, so all of them passed while the feature was inert in production —
  manager writes landed in the DB, returned 200, and reached no upload gate.
  Fixed by driving `module.Setup → New(ctx)` end to end. See
  `learnings/pending/assembly-path-must-be-tested.md`.
- **A counting guard proves no call site was removed, not that every entry point
  has one.** The extension-parity guard counted `IsAllowedExtension` call sites,
  so `bot_api` / `robot` multipart handlers — which had *no* extension check at
  all — passed it. Replaced by handler-level tests asserting the mock's
  `UploadFile` is **not** called on rejection; the counter was narrowed to
  watching the number of entry points.
- **Fingerprint collision in a policy cache is a silent security failure.**
  `allowed=[".a"] blocked=[".b|.pdf"]` and `allowed=[".a|.b"] blocked=[".pdf"]`
  joined to the same key, so switching between them served the stale snapshot:
  `.pdf` stayed uploadable and appconfig kept advertising it, while the console
  reported `applied=true`.
- **`git stash` is a no-op on a clean tree.** A "did this fail before my change?"
  comparison run that way proves nothing. Use `git worktree` against the base
  commit.
- **The local shared `test` database makes cross-package runs unreliable.**
  `go test ./internal/...` fails with `Table … already exists` even under `-p 1`;
  each package passes when run alone after recreating the DB. Several
  dependent-package failures investigated during this task turned out to be this,
  plus a WuKongIM container answering `/health` while logging
  `context deadline exceeded` — all reproduced on the base commit.

## Behaviour change

`POST /v1/bot/file/upload` and `POST /v1/robot/file/upload` now reject extensions
outside the policy. Both previously accepted anything, including `.exe`; blocking
a format left those two paths wide open.
