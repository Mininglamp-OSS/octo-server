-- -----------------------------------------------------------------------------
-- Extra databases for OCTO side-services.
-- Loaded ONCE on first MySQL container start via docker-entrypoint-initdb.d.
--
-- The side-services (octo-matter, octo-smart-summary) run their own embedded
-- gorp migrations on boot, so we only need to create the target schemas +
-- least-privilege users here. See docker-compose.yaml for how each service
-- receives its MYSQL_DSN.
-- -----------------------------------------------------------------------------

CREATE DATABASE IF NOT EXISTS octo_matter  CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
CREATE DATABASE IF NOT EXISTS octo_summary CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;

-- Per-service users. Production deployments SHOULD rotate these passwords
-- (hand-edit this file or set OCTO_MATTER_DB_PASSWORD / OCTO_SUMMARY_DB_PASSWORD
-- in .env and update the DSN strings in docker-compose.yaml accordingly).
CREATE USER IF NOT EXISTS 'matter'@'%'  IDENTIFIED BY 'matter';
CREATE USER IF NOT EXISTS 'summary'@'%' IDENTIFIED BY 'summary';

GRANT ALL PRIVILEGES ON octo_matter.*  TO 'matter'@'%';
GRANT ALL PRIVILEGES ON octo_summary.* TO 'summary'@'%';
FLUSH PRIVILEGES;
