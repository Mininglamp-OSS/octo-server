-- +migrate Up

-- ① 成员维表：每日全量刷新，union user(⊎ robot)。human/agent 都有 user 行，robot=1 即 agent。
CREATE TABLE `octo_dim_member` (
  `uid`         VARCHAR(40)  NOT NULL          COMMENT '成员uid (= user.uid / robot.robot_id)',
  `name`        VARCHAR(100) NOT NULL DEFAULT '' COMMENT '展示名 (user.name)',
  `email`       VARCHAR(100) NOT NULL DEFAULT '' COMMENT '邮箱 (user.email; agent通常为空)',
  `phone`       VARCHAR(20)  NOT NULL DEFAULT '' COMMENT '手机号 (user.phone)',
  `zone`        VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '手机区号 (user.zone)',
  `member_type` TINYINT      NOT NULL DEFAULT 1  COMMENT '1=human 2=agent (源 user.robot)',
  `is_excluded` TINYINT      NOT NULL DEFAULT 0  COMMENT '1=测试/系统账号统计排除 (u_10000/botfather/fileHelper + 规则)',
  `created_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '行创建时间',
  `updated_at`  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '行更新时间',
  PRIMARY KEY (`uid`),
  KEY `idx_type` (`member_type`),
  KEY `idx_name` (`name`),
  KEY `idx_email` (`email`),
  KEY `idx_phone` (`zone`,`phone`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='看板成员维表';

-- ② 会话维表：群从 group 派生，私聊从消息流(fakeChannelID)派生；private space_id='' 不进空间维度。
CREATE TABLE `octo_dim_channel` (
  `channel_id`         VARCHAR(100) NOT NULL          COMMENT '群=group_no; 私聊=fakeChannelID(uidX@uidY)',
  `channel_type`       TINYINT      NOT NULL          COMMENT '1=私聊 2=群 (octo-lib common.ChannelType)',
  `space_id`           VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '群=group.space_id; 私聊="" (不进空间维度)',
  `conv_type`          TINYINT      NOT NULL DEFAULT 0  COMMENT '1=HH群 2=HA群 3=HH私聊 4=HA私聊 (ETL当日成员组成打标)',
  `name`               VARCHAR(255) NOT NULL DEFAULT '' COMMENT '群名(group.name); 私聊空(展示层拼"A & B")',
  `member_a_uid`       VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '私聊成员A (ETL按uid字典序规范化)',
  `member_b_uid`       VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '私聊成员B',
  `member_count`       INT          NOT NULL DEFAULT 0  COMMENT '当前在册总成员数(含外部, 群; group_member.status=1)',
  `human_member_count` INT          NOT NULL DEFAULT 0  COMMENT '当前在册human成员数',
  `agent_member_count` INT          NOT NULL DEFAULT 0  COMMENT '当前在册agent成员数',
  `status`             TINYINT      NOT NULL DEFAULT 1  COMMENT '1=正常 2=群解散 (源 group.status: 1->1, 0->2)',
  `first_msg_at`       BIGINT       NOT NULL DEFAULT 0  COMMENT '群=group.created_at(纪元秒); 私聊=首条消息时间(单调减LEAST)',
  `last_active_at`     BIGINT       NOT NULL DEFAULT 0  COMMENT '最后一条有效消息时间戳(单调增GREATEST)',
  `created_at`         TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '行创建时间',
  `updated_at`         TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '行更新时间',
  PRIMARY KEY (`channel_id`),
  KEY `idx_space` (`space_id`,`channel_type`),
  KEY `idx_conv` (`conv_type`),
  KEY `idx_last_active` (`last_active_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='看板会话维表';

-- ③ 事实表-成员×会话×日 (最细粒度，也是 ④ 的 ETL 中间产物)。
CREATE TABLE `octo_fact_member_channel_daily` (
  `stat_date`    DATE         NOT NULL          COMMENT '统计日(报告时区自然日)',
  `channel_id`   VARCHAR(100) NOT NULL          COMMENT '会话ID',
  `channel_type` TINYINT      NOT NULL          COMMENT '1=私聊 2=群',
  `space_id`     VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '冗余 (群=group.space_id; 私聊="")',
  `conv_type`    TINYINT      NOT NULL DEFAULT 0  COMMENT '冗余 (ETL当日成员组成打标)',
  `content_type` TINYINT      NOT NULL DEFAULT 0  COMMENT '保留列: P0恒0; 未来类型排除/构成扩展用',
  `sender_uid`   VARCHAR(40)  NOT NULL          COMMENT '发送者uid (message.from_uid)',
  `sender_type`  TINYINT      NOT NULL DEFAULT 1  COMMENT '1=human 2=agent (join dim_member)',
  `msg_count`    INT          NOT NULL DEFAULT 0  COMMENT '当日该成员在该会话的消息数(含撤回; 全量含系统/通话)',
  `last_msg_at`  BIGINT       NOT NULL DEFAULT 0  COMMENT '当日该成员最后一条消息时间戳',
  `created_at`   TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '行创建时间',
  PRIMARY KEY (`channel_id`,`stat_date`,`sender_uid`),
  KEY `idx_space_sender_date` (`space_id`,`sender_uid`,`stat_date`),
  KEY `idx_sender_date` (`sender_uid`,`stat_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='看板事实表-成员×会话×日';

-- ④ 事实表-会话×日 (由 ③ 上卷物化)。
CREATE TABLE `octo_fact_channel_daily` (
  `stat_date`            DATE         NOT NULL          COMMENT '统计日(报告时区自然日)',
  `channel_id`           VARCHAR(100) NOT NULL          COMMENT '会话ID',
  `channel_type`         TINYINT      NOT NULL          COMMENT '1=私聊 2=群',
  `space_id`             VARCHAR(40)  NOT NULL DEFAULT '' COMMENT '冗余 (私聊="")',
  `conv_type`            TINYINT      NOT NULL DEFAULT 0  COMMENT '冗余',
  `human_msg_count`      INT          NOT NULL DEFAULT 0  COMMENT '当日human消息总数',
  `agent_msg_count`      INT          NOT NULL DEFAULT 0  COMMENT '当日agent消息总数',
  `active_human_members` INT          NOT NULL DEFAULT 0  COMMENT '当日活跃human成员数(去重)',
  `active_agent_members` INT          NOT NULL DEFAULT 0  COMMENT '当日活跃agent成员数(去重)',
  `last_msg_at`          BIGINT       NOT NULL DEFAULT 0  COMMENT '当日最后一条消息时间戳',
  `created_at`           TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '行创建时间',
  PRIMARY KEY (`space_id`,`stat_date`,`channel_id`),
  KEY `idx_conv_date` (`conv_type`,`stat_date`),
  KEY `idx_type_date` (`channel_type`,`stat_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='看板事实表-会话×日(上卷)';

-- +migrate Down
DROP TABLE IF EXISTS `octo_fact_channel_daily`;
DROP TABLE IF EXISTS `octo_fact_member_channel_daily`;
DROP TABLE IF EXISTS `octo_dim_channel`;
DROP TABLE IF EXISTS `octo_dim_member`;
