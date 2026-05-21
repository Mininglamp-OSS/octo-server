-- +migrate Up
-- YUJ-1465 / Mininglamp-OSS/octo-server#108 — Persona Clone (OBO) v2.
-- Adds a per-grant `persona_prompt` column so the grantor can attach a
-- natural-language behavioral prompt that the fan-out path appends to
-- the synthetic `obo_system_hint` string handed to the grantee bot.
--
-- NULL on pre-v2 grants preserves legacy behavior — they continue to
-- emit only the auto-generated system hint with no extra persona prompt.
-- New grants always supply an explicit value (see insertGrant /
-- createOrReactivateGrantAtomic). NULL is surfaced as "" by the read
-- paths that touch this column directly (listGrantsByGrantor uses
-- COALESCE(g.persona_prompt, '')).
--
-- PR#109 YUJ-1471 — `DEFAULT ''` was removed because MySQL < 8.0.13
-- rejects DEFAULT on TEXT/BLOB columns (ER_BLOB_CANT_HAVE_DEFAULT,
-- error 1101), breaking the migration on production servers still on
-- 5.7 / 8.0.12-. NULL is the de-facto default for these columns and
-- callers cope with NULL as documented above.
ALTER TABLE obo_grants ADD COLUMN persona_prompt TEXT;

-- +migrate Down
ALTER TABLE obo_grants DROP COLUMN persona_prompt;
