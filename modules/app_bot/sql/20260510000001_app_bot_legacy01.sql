-- +migrate Up
-- Re-align app_bot collation with actual DB default (utf8mb4_0900_ai_ci).
-- The previous migration (20260509-01) targeted general_ci based on an incorrect
-- assumption about project default; actual production/test DB default is 0900_ai_ci.
-- This ensures app_bot JOINs with space_member (also 0900_ai_ci) work without
-- COLLATE casts. Requires MySQL 8.0+.
ALTER TABLE app_bot CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

-- +migrate Down
ALTER TABLE app_bot CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
