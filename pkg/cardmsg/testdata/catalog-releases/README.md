# Catalog release fixtures

`docs.access-request-0.3.0.handoff.zip` is the immutable Handoff asset from:

- Repository: `LLwill/octo-card-catalog`
- Release: `card/docs.access-request/v0.3.0`
- Published: `2026-08-24`
- SHA-256: `1f6720602160efdfe5f182651f4f4035821ed62443e2ad7675304a1cfdd70e93`

The adjacent `.handoff.sha256` file is the release sidecar. Tests verify the
archive before reading it and do not access the network.

This fixture proves that standard Card JSON compiled by the external Catalog
release passes octo-server's type-17 wire gate. It is not registered as a
server template: the server already has an independently authored package with
the same historical Card ID/version and a different contract. A production
migration must use a new, non-conflicting Card version.
