# Internal identity resolution

## Goal

Expose `POST /v1/internal/auth/identity` (`identity.resolve`) so internal callers can resolve current user, Space and personal Bot identity facts. Reuse `OCTO_MEMBERSHIP_INTERNAL_TOKEN` through `X-Internal-Token`.

## Load-bearing behavior

- Validate active user, Space and user membership; return membership role separately from Space.
- Read active user/Space membership first. Only when `bot_id` is supplied, read one active BF Bot owned by that user and its profile name. Bot creation/association guarantees the same Space; do not query a second membership or app_bot.
- Use fixed sequential SELECTs in one read-only repeatable-read snapshot bounded to three seconds, including connection-pool wait. Keep indexable predicates and byte-exact ID comparisons. The provider reads current facts; callers own their result-cache policy.
- Return `bot: null` only when no Bot is requested. An unavailable user/Space/member or supplied Bot (missing, wrong owner or inactive) returns the existing not-found error without a partial identity. A resolved Bot has `is_active=true`.
- Reuse the module's existing strict per-IP limiter before Secret authentication, sharing its bucket and configuration with membership/project routes.
- Reuse the original internalAuthMiddleware and internalToken, including existing enablement checks. Do not maintain a separate identity Secret or auth middleware.
- Reuse the existing registered error codes, compatibility envelope and shared renderer. Preserve existing routes and their peer-cache release gate.

## Scope

Octo Server provider, tests and provider documentation. No Token issuance, permission evaluation, binding management, schema changes or user interface. Consumer changes belong to separate repositories and are not included here.

## Acceptance and verification

HTTP contract and Secret isolation tests; real MySQL lifecycle, relationship and collation tests; cancellation/pool deadline tests; production-route Secret isolation tests; existing error-envelope compatibility tests; build, vet, race and i18n checks. See [verification.md](verification.md) for results and the legacy test-environment limitation.

## Delivery

The approved identity changes are ported from the GitLab feature branch onto the latest GitHub main without GitLab synchronization history. The pull request targets `main`; no merge or deployment is part of this task.
