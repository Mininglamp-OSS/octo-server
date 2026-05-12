-- +migrate Up
-- No-op: paired with 20260510-01 which is also now a no-op. See that file.
SELECT 1;

-- +migrate Down
SELECT 1;
