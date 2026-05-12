
-- +migrate Up

ALTER TABLE `oidc_audit_log` MODIFY `uid` VARCHAR(64) NOT NULL DEFAULT '';

-- +migrate Down

ALTER TABLE `oidc_audit_log` MODIFY `uid` VARCHAR(40) NOT NULL DEFAULT '';
