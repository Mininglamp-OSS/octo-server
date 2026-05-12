-- +migrate Up
-- No-op: this was a mistaken flip to 0900_ai_ci which 20260510-02 reverted.
-- The 20260505 CREATE TABLE now pins app_bot to utf8mb4_general_ci from the
-- start, so neither this nor 20260510-02 need to ALTER anything. The
-- migration row is preserved so already-applied environments stay in sync.
SELECT 1;

-- +migrate Down
SELECT 1;
