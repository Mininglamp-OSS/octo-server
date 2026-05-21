-- +migrate Up
CREATE TABLE IF NOT EXISTS user_space_setting (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    uid VARCHAR(40) NOT NULL,
    space_id VARCHAR(40) NOT NULL,
    voice_feedback_on SMALLINT NOT NULL DEFAULT 1,
    voice_feedback_notice_acked SMALLINT NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_uid_space (uid, space_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- +migrate Down
DROP TABLE IF EXISTS user_space_setting;
