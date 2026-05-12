-- +migrate Up

-- 补录：将所有现存 Bot 加入其 creator 所在的 Space
-- Bot 在 PR #241 之前创建的没有 space_member 记录
SET NAMES utf8mb4;

INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at)
SELECT sm.space_id, r.robot_id, 0, 1, NOW(), NOW()
FROM robot r
INNER JOIN space_member sm ON sm.uid = r.creator_uid AND sm.status = 1
WHERE r.status = 1
  AND r.creator_uid IS NOT NULL
  AND r.creator_uid != ''
  AND NOT EXISTS (
    SELECT 1 FROM space_member sm2
    WHERE sm2.space_id = sm.space_id AND sm2.uid = r.robot_id AND sm2.status = 1
  );
