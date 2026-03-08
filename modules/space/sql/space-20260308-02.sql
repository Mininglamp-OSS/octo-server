-- +migrate Up

-- 补录：将所有未关联任何 Space 的 Bot 加入 minglue_default
-- 覆盖 creator_uid 为空或 creator 不在任何 Space 的情况
SET NAMES utf8mb4;

INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at)
SELECT 'minglue_default', r.robot_id, 0, 1, NOW(), NOW()
FROM robot r
WHERE r.status = 1
  AND NOT EXISTS (
    SELECT 1 FROM space_member sm
    WHERE sm.uid = r.robot_id AND sm.status = 1
  );
