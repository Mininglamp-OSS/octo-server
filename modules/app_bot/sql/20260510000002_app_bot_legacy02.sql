-- +migrate Up
-- Re-align app_bot collation back to utf8mb4_general_ci to match space_member.
-- The previous migration (20260510-01) flipped to utf8mb4_0900_ai_ci based on an
-- incorrect assumption about the DB default. Verified on test env (im_test):
--   space_member.space_id  -> utf8mb4_general_ci
--   space.*                -> utf8mb4_general_ci
-- Without this fix, GET /v1/app_bot/available?space_id=... fails with
-- "Error 1267: Illegal mix of collations" on the app_bot JOIN space_member ON space_id.
ALTER TABLE app_bot CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;

-- +migrate Down
ALTER TABLE app_bot CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
