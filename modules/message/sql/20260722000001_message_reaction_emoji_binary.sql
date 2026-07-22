-- +migrate Up

-- Reaction identity is the exact emoji string. utf8mb4_general_ci treats some
-- distinct supplementary-plane emoji (for example 👍 and 🎉) as equal, so the
-- five-column unique key can route a new emoji into another emoji's upsert row.
-- Change only this identity column to a byte-exact collation; the existing
-- reaction_users_channel_msg_uid_emoji_uidx index is rebuilt with that comparison
-- semantics automatically.
ALTER TABLE `reaction_users`
    MODIFY COLUMN `emoji` VARCHAR(20)
    CHARACTER SET utf8mb4 COLLATE utf8mb4_bin
    NOT NULL DEFAULT '';

-- +migrate Down

-- Forward-only: after this migration, byte-distinct emoji rows may coexist while
-- comparing equal under utf8mb4_general_ci. Reverting the collation could fail the
-- unique index or require destructive deduplication, so application rollback keeps
-- the safer binary collation.
SELECT 1;
