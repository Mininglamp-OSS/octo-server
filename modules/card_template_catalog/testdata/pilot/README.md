# Non-production card-template pilot fixtures

`bundle.json` under `<template-id>@<version>/` is **publish input for the D7
pilot test**. It is not a released template and its presence in this repository
carries no runtime meaning.

Specifically, a bundle here is:

- **not** registered in `DefaultRegistry` and **not** `go:embed`-ed into startup;
- **not** copied into `pkg/cardtmpl/*/handoff/` (those are frozen L1 handoffs);
- **not** an activation and **not** an authorization — publish, activate and
  grant are three separate operations, and merging this file changes none of
  them;
- **not** permission to enable the production gates. Both
  `OCTO_CARD_RUNTIME_CATALOG_CONTROL_ENABLED` and
  `OCTO_CARD_RUNTIME_CATALOG_NEW_SEND_ENABLED` stay false.

## A pilot never claims a production template ID

The bundle declares its own `additionalProperties:false` data contract, which
does not match any live producer's field shape. Activating it under a template
ID a real producer sends would make every one of that producer's cards fail
preflight with a 400 and zero delivery. Pilots therefore use a dedicated ID
(`docs.pilot-access-request`, not `docs.access-request`); the fixture test
asserts this rather than trusting the constant's comment.

## The version is claimed once, forever

A version claim is permanent and global to a catalog database, so the exact
`(template_id, version)` must be proven never claimed **in the catalog you are
publishing into**.

`requirePilotVersionUnclaimed` interrogates the database named by
`OCTO_PILOT_CATALOG_DSN`, and it is armed by a separate switch:

| `OCTO_PILOT_CATALOG_ENABLED` | `OCTO_PILOT_CATALOG_DSN` | outcome |
|---|---|---|
| unset / `false` | anything | not armed; reports what it did **not** check (and says so again if a DSN is set but ignored) |
| `true` | set | queries that catalog for real |
| `true` | empty | **hard failure** — arming without naming a catalog cannot be satisfied |
| malformed (e.g. `yes`) | anything | **hard failure** — a typo in a safety switch reads as neither on nor off |

Two settings rather than one because a lone DSN cannot distinguish "this
deployment runs no pilot" from "somebody meant to configure one and the variable
did not reach the process" — and the earlier shape answered both by logging and
passing. "No shared catalog configured" and "the version is free" are different
answers and only one of them is evidence, so the gate never silently approves a
version.

The per-test database cannot answer this question: `newCatalogStoreIntegrationDB`
drops and recreates it moments before the check runs, so it is always empty. An
earlier revision queried exactly that database and could therefore never fail.

If the version is already claimed, **do not overwrite or reuse the directory**.
Pick a new reviewed prerelease (the convention is `<next>.0-pilot.<yyyymmdd>`),
rename the directory to match, and update `pilotVersion`.

## Content rules

Everything in a pilot bundle is synthetic. No real tenant, Space, user, bot,
document, token or callback data, and no production samples — the bundle is
committed and readable by anyone with repository access, and its allowlisted
samples are additionally served over B2 to any caller that can discover the
template.
