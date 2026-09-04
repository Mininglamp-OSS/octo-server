-- +migrate Up
CREATE TABLE IF NOT EXISTS bot_instance_binding (
  token_fingerprint BINARY(32)   NOT NULL COMMENT 'SHA-256 of the Bot API token.',
  bot_kind          VARCHAR(16)  NOT NULL COMMENT 'user or app.',
  robot_id          VARCHAR(64)  NOT NULL,
  instance_id       VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  im_token          VARCHAR(200) NOT NULL,
  created_at        DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at        DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (token_fingerprint),
  KEY idx_bot_instance_binding_robot (robot_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- +migrate Down
DROP TABLE IF EXISTS bot_instance_binding;
