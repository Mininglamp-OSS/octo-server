---
type: Task
title: "Dedicated MCP Bot policy"
slug: mcp-bot-policy
source: self
upstream: https://github.com/Mininglamp-OSS/octo-server/issues/940
---

## Goal

Implement XIN-2692 under Charter XIN-2323 v11/v10 and approved design v2
SHA-256 `d2405ece492b3faa96f974dfbb5cb9d18dbf35fc629e312eda7e350887b4aee7`.
Target `Mininglamp-OSS/octo-server:main`, baseline
`adb7c385386c558d78be8b6fbf7881392fab4475`.

## Load-bearing list

- Bot token verification: identity remains authoritative, policy is opt-in.
- Dedicated mandatory default and explicit overrides, no general settings keys.
- Read-only repeatable-read snapshots, two-second context budget.
- Singleton → active robot → override lock order, global generation CAS.
- Creator-only Bot management and superadmin-only default management using
  existing login and shared UID rate limiting. Bot Bearer credentials do not
  become human sessions. Resource UIDs never become actors.
- Existing i18n fixed-400 envelope; semantic conflict 409 and unavailable 503.
- Migration seed preservation and protected Down after any policy edit.

## Out of scope

IM/UI, CLI implementation, OBO changes, Fleet, deployment, live DB operations,
automatic stage transitions, formal review, ready conversion, merge and release.

## Acceptance

- Missing/false opt-in never touches policy tables; malformed opt-in rejected.
- Explicit compatible survives strict default; absent overrides follow default.
- Invalid/missing default, corrupt override and DB/timeout/commit failures reject.
- Closed 2 KiB management JSON; lossless positive uint64 generations.
- Owner/nonowner/admin guards, transaction owner recheck, CAS conflict, no-op,
  delete, rollback and consistent snapshots covered by tests.
- Up/repeated Up/Down/CHECK/protected data verified only on disposable MySQL.
- Contract specimens, executed tests and limitations accompany an own-fork Draft PR.
