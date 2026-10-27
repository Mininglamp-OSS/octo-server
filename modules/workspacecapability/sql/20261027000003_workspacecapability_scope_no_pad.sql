-- +migrate Up
-- utf8mb4_bin is PAD SPACE in MySQL 8; a legacy NO PAD Space ID ending in a
-- space could then match another Space. Use the NO PAD binary collation and
-- reject whitespace-padded scope identifiers at the HTTP boundary.
ALTER TABLE `workspace_capability`
  MODIFY `space_id` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
  MODIFY `workspace_id` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL;

ALTER TABLE `workspace_capability_binding`
  MODIFY `space_id` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,
  MODIFY `workspace_id` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL;

-- +migrate Down
ALTER TABLE `workspace_capability`
  MODIFY `space_id` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  MODIFY `workspace_id` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL;

ALTER TABLE `workspace_capability_binding`
  MODIFY `space_id` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  MODIFY `workspace_id` VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL;
