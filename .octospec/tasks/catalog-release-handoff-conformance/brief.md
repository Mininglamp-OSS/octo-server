---
type: Task
title: "Task: catalog-release-handoff-conformance"
description: Verify that an immutable Octo Card Catalog Handoff release is accepted by octo-server's authoritative type-17 card gate.
tags: [card, cardmsg, wire-contract, test, testing]
timestamp: 2026-08-25T13:10:00+08:00
slug: catalog-release-handoff-conformance
upstream: self
source: self
---

# Task: catalog-release-handoff-conformance

## Goal

Add a repository-local, network-free consumer fixture for the immutable
`docs.access-request@0.3.0` Handoff published by `LLwill/octo-card-catalog`.
Verify the release checksum, load its compiled golden cards, wrap them in the
existing type-17 envelope, and pass them through `cardmsg.Validate` and
`cardmsg.Finalize`.

## Background

Phase 7 of Octo Card Forge requires evidence that published Artifact/Handoff
output can enter a real backend's existing Card JSON delivery boundary. The
Catalog release is immutable and carries SHA-256
`1f6720602160efdfe5f182651f4f4035821ed62443e2ad7675304a1cfdd70e93`.

The server also has an older independently authored embedded template using the
same Card ID/version but a different contract. This task deliberately does not
register or replace that template. It verifies wire compatibility only and
makes the external release identity explicit so a later migration must mint a
new non-conflicting Card version.

## Load-bearing list

- `pkg/cardmsg.Validate` remains the authoritative server acceptance gate for
  standard Adaptive Card JSON inside type-17 envelopes.
- `pkg/cardmsg.Finalize` remains the sole authority for derived `plain` text
  and the final payload-size check.
- The release ZIP checksum and manifest view-to-wire-profile mapping are read
  from the immutable fixture rather than duplicated in test cases.
- The fixture is external Catalog output; it is not a server template registry
  entry and carries no server-authored provenance markers.

## Out of scope

- No production sender, template registry, action routing, persistence, API, or
  feature-gate behavior changes.
- No overwrite or reinterpretation of either existing `0.3.0` identity.
- No network download during tests and no dependency on another local checkout.
- No octo-web implementation change in this PR.

## Acceptance

- The checked-in Handoff ZIP matches its published SHA-256 sidecar.
- The manifest identifies `docs.access-request@0.3.0`, Adaptive Card `1.5`, and
  exact Render Profile `octo-chat@1.2.0-rc.4`.
- Every declared sample has a compiled golden and all three golden cards pass
  `cardmsg.Validate` using the view's declared wire profile.
- `cardmsg.Finalize` derives non-empty authoritative `plain` text for every
  golden without changing the Card document.
- Focused `go test ./pkg/cardmsg` and `go test ./pkg/cardmsg/...` pass.
