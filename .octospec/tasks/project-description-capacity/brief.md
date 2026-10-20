# Project description capacity

## Goal

Accept and preserve Project descriptions up to 32,768 UTF-8 bytes, matching Fleet's Project lifecycle event description limit. Store descriptions as MySQL TEXT rather than VARCHAR(500).

## Load-bearing changes

- Embedded forward schema migration for `octo_project.description`; preserve existing text and retain the widened column on downgrade.
- Apply the same byte limit to Project create and update. Oversize responses retain `err.server.project.field_too_long` and report `field=description` with `max_bytes=32768`.
- Keep name and logo character-based limits unchanged.

## Out of scope

- Production schema/configuration changes and Workspace data migration execution.
- Fleet changes and the external `octo-project-sync` tool. Its 500-character filter must be aligned before executing a later data migration.

## Acceptance

- ASCII and multibyte descriptions at 32,768 bytes survive HTTP create, update and read-back without truncation.
- A 32,769-byte multibyte description is rejected on create and update; rejected updates preserve stored data.
- Existing short descriptions survive schema widening; downgrade retains TEXT and preserves long descriptions.
- Publish a pull request with targeted integration-test and smoke evidence.
