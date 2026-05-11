#!/bin/bash
# -----------------------------------------------------------------------------
# Extra databases for OCTO side-services.
# Loaded ONCE on first MySQL container start via docker-entrypoint-initdb.d.
#
# This is a .sh (not .sql) so the mysql entrypoint executes it with a live
# environment — letting the passwords and schema name come from .env instead
# of being hard-coded. When the OCTO_*_DB_PASSWORD vars are unset, the
# fallback values reproduce the pre-YUJ-446 behaviour.
#
# Side services (octo-matter, octo-smart-summary) run their own embedded
# gorp migrations on boot, so we only create schemas + users here.
#
# Security model:
#   - Passwords and db names are NOT interpolated into an SQL string
#     unsafely. They are first validated against a strict allowlist so we
#     can safely inline them (MySQL CREATE USER / GRANT do not accept
#     prepared-statement parameters, so literal interpolation is the only
#     option — validation makes that interpolation safe).
#   - The allowlist is [A-Za-z0-9._-] for passwords and [A-Za-z0-9_] for
#     db names. Anything outside causes this script to abort before
#     touching MySQL. Users who need different characters should stop
#     this container, fix the .env value, and re-init (or hand-run SQL
#     against the running MySQL after quoting it themselves).
# -----------------------------------------------------------------------------

set -euo pipefail

: "${OCTO_MATTER_DB_PASSWORD:=matter}"
: "${OCTO_SUMMARY_DB_PASSWORD:=summary}"
: "${OCTO_SUMMARY_READER_PASSWORD:=summary_reader}"
: "${MYSQL_DATABASE:=octo}"

validate_password() {
  local name="$1"
  local value="$2"
  if [ -z "$value" ]; then
    echo "[init-extra-dbs] FATAL: $name is empty" >&2
    exit 1
  fi
  case "$value" in
    *[!A-Za-z0-9._-]*)
      echo "[init-extra-dbs] FATAL: $name contains characters outside [A-Za-z0-9._-]" >&2
      echo "[init-extra-dbs]        ${name} must match that regex so this script can safely" >&2
      echo "[init-extra-dbs]        inline it into the CREATE USER / GRANT statements." >&2
      exit 1
      ;;
  esac
}

validate_identifier() {
  local name="$1"
  local value="$2"
  if [ -z "$value" ]; then
    echo "[init-extra-dbs] FATAL: $name is empty" >&2
    exit 1
  fi
  case "$value" in
    *[!A-Za-z0-9_]*)
      echo "[init-extra-dbs] FATAL: $name contains characters outside [A-Za-z0-9_]" >&2
      exit 1
      ;;
  esac
}

validate_password  OCTO_MATTER_DB_PASSWORD       "$OCTO_MATTER_DB_PASSWORD"
validate_password  OCTO_SUMMARY_DB_PASSWORD      "$OCTO_SUMMARY_DB_PASSWORD"
validate_password  OCTO_SUMMARY_READER_PASSWORD  "$OCTO_SUMMARY_READER_PASSWORD"
validate_identifier MYSQL_DATABASE               "$MYSQL_DATABASE"

mysql -u root -p"${MYSQL_ROOT_PASSWORD}" <<SQL
-- Schemas ---------------------------------------------------------------------
CREATE DATABASE IF NOT EXISTS octo_matter  CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
CREATE DATABASE IF NOT EXISTS octo_summary CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;

-- Service-scoped read-write accounts -----------------------------------------
CREATE USER IF NOT EXISTS 'matter'@'%'  IDENTIFIED BY '${OCTO_MATTER_DB_PASSWORD}';
CREATE USER IF NOT EXISTS 'summary'@'%' IDENTIFIED BY '${OCTO_SUMMARY_DB_PASSWORD}';

-- Read-only account used by summary services to scan the IM schema -----------
-- Principle of least privilege: smart-summary only needs SELECT on the IM
-- schema, so we hand it a narrow account instead of the MySQL root
-- credentials.
CREATE USER IF NOT EXISTS 'summary_reader'@'%' IDENTIFIED BY '${OCTO_SUMMARY_READER_PASSWORD}';

GRANT ALL PRIVILEGES ON octo_matter.*      TO 'matter'@'%';
GRANT ALL PRIVILEGES ON octo_summary.*     TO 'summary'@'%';
GRANT SELECT         ON \`${MYSQL_DATABASE}\`.* TO 'summary_reader'@'%';
FLUSH PRIVILEGES;
SQL

echo "[init-extra-dbs] created octo_matter + octo_summary + service users (scoped to \`${MYSQL_DATABASE}\`)"
