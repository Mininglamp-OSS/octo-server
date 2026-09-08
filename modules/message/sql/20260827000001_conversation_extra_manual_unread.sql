-- +migrate Up

-- 为用户会话扩展增加独立的手动未读标识。
-- 该字段只表达侧边栏展示状态，不改变 browse_to 等真实阅读位置。
ALTER TABLE `conversation_extra`
    ADD COLUMN `manual_unread` TINYINT NOT NULL DEFAULT 0
    COMMENT '手动未读标识：0=正常，1=手动标为未读';

-- +migrate Down

ALTER TABLE `conversation_extra`
    DROP COLUMN `manual_unread`;
