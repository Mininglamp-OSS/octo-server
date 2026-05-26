-- +migrate Up
ALTER TABLE user_space_setting ALTER COLUMN voice_feedback_on SET DEFAULT 0;

-- +migrate Down
ALTER TABLE user_space_setting ALTER COLUMN voice_feedback_on SET DEFAULT 1;
