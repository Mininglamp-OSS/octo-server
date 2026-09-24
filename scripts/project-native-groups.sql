-- Run once after the compatible Project release is serving all traffic and old
-- instances plus in-flight Project/group tasks have drained. The Project module
-- automatically applies embedded migrations on startup, so this post-drain
-- contraction must stay outside modules/project/sql during a rolling deployment.
-- A fresh installation has no old instances or in-flight work; run this SQL
-- after its first startup once the compatible application is serving traffic.
--
-- This removes only the obsolete Project group pointer and provisioning lease.
-- Historical groups retain group.project_id, members, owners, messages and IM
-- subscriptions. Back up the schema before applying this irreversible change:
-- adding the columns back would not restore their historical values, and old
-- binaries cannot be used as a rollback after the drop.
--
-- INSTANT fails closed if MySQL cannot drop online; do not remove the algorithm
-- constraint and silently allow a table rebuild. Verify the old columns exist
-- before running; a second run fails rather than masking an unexpected schema.
ALTER TABLE `octo_project`
  DROP COLUMN `all_member_group_lease_until`,
  DROP COLUMN `all_member_group_no`,
  ALGORITHM=INSTANT;
