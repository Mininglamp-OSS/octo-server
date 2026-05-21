-- +migrate Up
-- YUJ-1465 / Mininglamp-OSS/octo-server#108 — Persona Clone (OBO) v2.
-- Adds a per-grant `persona_prompt` column so the grantor can attach a
-- natural-language behavioral prompt that the fan-out path appends to
-- the synthetic `obo_system_hint` string handed to the grantee bot.
--
-- Default '' (empty string) preserves v0 / v1 grants — they continue to
-- emit only the auto-generated system hint with no extra persona prompt.
ALTER TABLE obo_grants ADD COLUMN persona_prompt TEXT DEFAULT '';

-- +migrate Down
ALTER TABLE obo_grants DROP COLUMN persona_prompt;
