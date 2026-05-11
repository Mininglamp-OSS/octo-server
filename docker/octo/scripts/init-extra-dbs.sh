#!/bin/bash
# -----------------------------------------------------------------------------
# Extra databases for OCTO side-services.
# Loaded ONCE on first MySQL container start via docker-entrypoint-initdb.d.
#
# This is a .sh (not .sql) so the mysql entrypoint executes it with a live
# environment — letting the passwords come from .env instead of being hard-
# coded. When the OCTO_*_DB_PASSWORD vars are unset, the fallback values
# reproduce the pre-YUJ-446 behaviour.
#
# The side services (octo-matter, octo-smart-summary) run their own embedded
# gorp migrations on boot, so we only create schemas + users here.
# -----------------------------------------------------------------------------

set -euo pipefail

: "${OCTO_MATTER_DB_PASSWORD:=matter}"
: "${OCTO_SUMMARY_DB_PASSWORD:=summary}"
: "${OCTO_SUMMARY_READER_PASSWORD:=summary_reader}"

mysql -u root -p"${MYSQL_ROOT_PASSWORD}" <<SQL
-- Schemas ---------------------------------------------------------------------
CREATE DATABASE IF NOT EXISTS octo_matter  CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
CREATE DATABASE IF NOT EXISTS octo_summary CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;

-- Service-scoped read-write accounts -----------------------------------------
CREATE USER IF NOT EXISTS 'matter'@'%'  IDENTIFIED BY '${OCTO_MATTER_DB_PASSWORD}';
CREATE USER IF NOT EXISTS 'summary'@'%' IDENTIFIED BY '${OCTO_SUMMARY_DB_PASSWORD}';

-- Read-only account used by summary services to scan the IM schema -----------
-- Principle of least privilege: smart-summary only needs SELECT on octo.*,
-- so we hand it a narrow account instead of the MySQL root credentials.
CREATE USER IF NOT EXISTS 'summary_reader'@'%' IDENTIFIED BY '${OCTO_SUMMARY_READER_PASSWORD}';

GRANT ALL PRIVILEGES ON octo_matter.*  TO 'matter'@'%';
GRANT ALL PRIVILEGES ON octo_summary.* TO 'summary'@'%';
GRANT SELECT         ON \`${MYSQL_DATABASE:-octo}\`.* TO 'summary_reader'@'%';
FLUSH PRIVILEGES;
SQL

echo "[init-extra-dbs] created octo_matter + octo_summary + service users"
