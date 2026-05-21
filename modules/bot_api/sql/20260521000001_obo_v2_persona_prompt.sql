-- +migrate Up
ALTER TABLE obo_grants ADD COLUMN persona_prompt TEXT;
UPDATE obo_grants SET persona_prompt = '' WHERE persona_prompt IS NULL;

-- +migrate Down
ALTER TABLE obo_grants DROP COLUMN persona_prompt;
