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
-- 5.7 / 8.0.12-. NULL is the de-facto default for these columns.
--
-- PR#109 R3 — backfill NULL to '' immediately after the ALTER. Multiple
-- read paths in obo_db.go use `SELECT * FROM obo_grants` and scan into
-- oboGrantModel.PersonaPrompt (declared as `string`, non-pointer); a
-- NULL value would either fail the dbr/database/sql scan with
-- "converting NULL to string is unsupported" or coerce to "" silently
-- depending on driver build. Eliminating NULL at the storage layer
-- removes that ambiguity for every read path (with or without
-- COALESCE) and matches the post-v2 invariant that all new inserts
-- supply an explicit value. Safe: only touches rows where the column
-- is currently NULL, and writes the same empty string that the
-- COALESCE-protected read paths already surface.
ALTER TABLE obo_grants ADD COLUMN persona_prompt TEXT;
UPDATE obo_grants SET persona_prompt = '' WHERE persona_prompt IS NULL;

-- +migrate Down
ALTER TABLE obo_grants DROP COLUMN persona_prompt;
