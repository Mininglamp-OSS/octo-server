-- +migrate Up
CREATE TABLE user_folded_chat (
 uid VARCHAR(40) NOT NULL,
 space_id VARCHAR(40) NOT NULL,
 channel_type SMALLINT NOT NULL,
 channel_id VARCHAR(100) NOT NULL,
 PRIMARY KEY (uid, space_id, channel_type, channel_id)
);
-- +migrate Down
DROP TABLE user_folded_chat;
