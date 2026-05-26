-- +migrate Up
ALTER TABLE user_space_setting
    ADD COLUMN voice_input_enabled SMALLINT NOT NULL DEFAULT 0
    AFTER space_id;

-- +migrate Down
ALTER TABLE user_space_setting
    DROP COLUMN voice_input_enabled;
