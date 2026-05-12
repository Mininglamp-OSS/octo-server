-- +migrate Up

-- 补录：BotFather 加入所有现存 Space
SET NAMES utf8mb4;

INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at)
SELECT s.space_id, 'botfather', 0, 1, NOW(), NOW()
FROM space s
WHERE s.status = 1
  AND NOT EXISTS (
    SELECT 1 FROM space_member sm
    WHERE sm.space_id = s.space_id AND sm.uid = 'botfather' AND sm.status = 1
  );
