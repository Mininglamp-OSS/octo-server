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

## The version is claimed once, forever

A version claim is permanent and global to a catalog database. Before adding a
new pilot directory, the exact `(template_id, version)` must be proven to have
never been claimed in the target catalog. `requirePilotVersionUnclaimed` in
`pilot_mysql_integration_test.go` enforces this at test time so the check cannot
be skipped, and it fails with the remedy spelled out.

If the version is already claimed — most likely because a shared non-production
database has seen an earlier run — **do not overwrite or reuse the directory**.
Pick a new reviewed prerelease (the convention is `<next>.0-pilot.<yyyymmdd>`),
rename the directory to match, and update `pilotVersion`.

## Content rules

Everything in a pilot bundle is synthetic. No real tenant, Space, user, bot,
document, token or callback data, and no production samples — the bundle is
committed and readable by anyone with repository access, and its allowlisted
samples are additionally served over B2 to any caller that can discover the
template.
