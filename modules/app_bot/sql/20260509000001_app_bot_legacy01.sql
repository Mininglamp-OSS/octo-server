-- +migrate Up
-- No-op: the original 20260505 CREATE TABLE now pins app_bot to
-- utf8mb4_general_ci explicitly, so this collation realignment is
-- redundant on clean installs. Kept as a recorded migration so older
-- environments that already ran this row stay consistent.
SELECT 1;

-- +migrate Down
SELECT 1;
