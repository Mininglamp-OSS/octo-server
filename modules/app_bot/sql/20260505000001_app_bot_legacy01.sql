-- +migrate Up
-- App Bot table
CREATE TABLE IF NOT EXISTS app_bot (
  id           VARCHAR(40) PRIMARY KEY,
  uid          VARCHAR(40) UNIQUE NOT NULL,
  display_name VARCHAR(100) NOT NULL,
  description  VARCHAR(500) DEFAULT '',
  avatar       VARCHAR(200) DEFAULT '',
  scope        VARCHAR(20) NOT NULL DEFAULT 'platform' COMMENT 'platform or space',
  space_id     VARCHAR(40) DEFAULT NULL,
  status       TINYINT NOT NULL DEFAULT 0 COMMENT '0=draft 1=published 2=unpublished',
  token        VARCHAR(100) UNIQUE NOT NULL,
  created_by   VARCHAR(40) NOT NULL,
  created_at   DATETIME NOT NULL DEFAULT NOW(),
  updated_at   DATETIME NOT NULL DEFAULT NOW() ON UPDATE NOW(),
  INDEX idx_scope_status (scope, status),
  INDEX idx_space_status (space_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
