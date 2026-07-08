-- +migrate Up

-- card-message-interaction P2 D10：卡片消息修订历史侧表。
-- 由 bot 编辑路径(botMessageEdit 卡片分支)在 content_edit 覆盖之外追加非 transient
-- 帧(每帧一整份可渲染信封);cap 20 非墓碑帧/消息(应用层裁剪);清除写墓碑行(审计);
-- 消息撤回时删除。查询端 GET /v1/message/card/revisions(成员可见)。octo_ 前缀(新表规约)。
CREATE TABLE `octo_message_card_revision` (
  `id`           BIGINT       NOT NULL AUTO_INCREMENT,
  `message_id`   VARCHAR(20)  NOT NULL DEFAULT '' COMMENT '卡片消息ID',
  `channel_id`   VARCHAR(100) NOT NULL DEFAULT '' COMMENT '存储行频道ID(person=fakeChannelID)',
  `channel_type` TINYINT      NOT NULL DEFAULT 0  COMMENT '频道类型',
  `card_seq`     BIGINT       DEFAULT NULL         COMMENT '该帧 card_seq(D9); NULL=无序号',
  `content`      TEXT                              COMMENT '完整帧信封(可渲染); 墓碑行为 NULL',
  `plain`        VARCHAR(512) NOT NULL DEFAULT '' COMMENT '列表摘要(server 权威 plain 截断)',
  `is_tombstone` TINYINT      NOT NULL DEFAULT 0  COMMENT '1=清除墓碑行(审计, 无 content)',
  `cleared_count` INT         NOT NULL DEFAULT 0  COMMENT '墓碑行清除的帧数',
  `editor_uid`   VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '编辑/清除者uid(bot)',
  `edited_at`    BIGINT       NOT NULL DEFAULT 0  COMMENT '编辑/清除时间(纪元秒)',
  `created_at`   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '行创建时间',
  `updated_at`   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '行更新时间',
  PRIMARY KEY (`id`),
  KEY `idx_message` (`message_id`,`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='卡片消息修订历史(D10)';
