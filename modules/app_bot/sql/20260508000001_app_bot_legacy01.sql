-- +migrate Up
-- Add welcome_msg column to app_bot table
ALTER TABLE app_bot ADD COLUMN welcome_msg VARCHAR(500) DEFAULT '' AFTER token;
