-- +migrate Up
-- Align app_bot collation with project default (utf8mb4_general_ci) to avoid
-- "Illegal mix of collations" when JOIN-ing with space_member (general_ci).
ALTER TABLE app_bot CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;

-- +migrate Down
ALTER TABLE app_bot CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
