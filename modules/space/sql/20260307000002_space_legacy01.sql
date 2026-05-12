-- +migrate Up

-- 团队空间
create table `space`
(
  id          integer       not null primary key AUTO_INCREMENT,
  space_id    VARCHAR(40)   not null default '',                             -- 空间唯一ID
  name        VARCHAR(100)  not null default '',                             -- 空间名称
  description VARCHAR(500)  not null default '',                             -- 空间描述
  logo        VARCHAR(200)  not null default '',                             -- 空间Logo
  creator     VARCHAR(40)   not null default '',                             -- 创建者uid
  status      smallint      not null DEFAULT 1,                              -- 状态 1.正常 0.已解散
  `version`   bigint        not null DEFAULT 0,                              -- 数据版本
  created_at  timeStamp     not null DEFAULT CURRENT_TIMESTAMP,              -- 创建时间
  updated_at  timeStamp     not null DEFAULT CURRENT_TIMESTAMP               -- 更新时间
);
CREATE UNIQUE INDEX space_spaceid on `space` (space_id);
CREATE INDEX space_creator on `space` (creator);

-- 团队空间成员
create table `space_member`
(
  id         integer     not null primary key AUTO_INCREMENT,
  space_id   VARCHAR(40) not null default '',                             -- 空间ID
  uid        VARCHAR(40) not null default '',                             -- 成员uid
  role       smallint    not null DEFAULT 0,                              -- 成员角色 0.普通成员 1.管理员 2.拥有者
  status     smallint    not null DEFAULT 1,                              -- 状态 1.正常 0.已移除
  `version`  bigint      not null DEFAULT 0,                              -- 数据版本
  created_at timeStamp   not null DEFAULT CURRENT_TIMESTAMP,              -- 创建时间
  updated_at timeStamp   not null DEFAULT CURRENT_TIMESTAMP               -- 更新时间
);
CREATE UNIQUE INDEX spacemember_spaceid_uid on `space_member` (space_id, uid);
CREATE INDEX spacemember_uid on `space_member` (uid);

-- 团队空间邀请
create table `space_invitation`
(
  id          integer      not null primary key AUTO_INCREMENT,
  space_id    VARCHAR(40)  not null default '',                             -- 空间ID
  invite_code VARCHAR(40)  not null default '',                             -- 邀请码
  creator     VARCHAR(40)  not null default '',                             -- 创建者uid
  max_uses    int          not null DEFAULT 0,                              -- 最大使用次数 0.不限
  used_count  int          not null DEFAULT 0,                              -- 已使用次数
  expires_at  timeStamp    null,                                            -- 过期时间
  status      smallint     not null DEFAULT 1,                              -- 状态 1.有效 0.无效
  created_at  timeStamp    not null DEFAULT CURRENT_TIMESTAMP,              -- 创建时间
  updated_at  timeStamp    not null DEFAULT CURRENT_TIMESTAMP               -- 更新时间
);
CREATE UNIQUE INDEX spaceinvite_code on `space_invitation` (invite_code);
CREATE INDEX spaceinvite_spaceid on `space_invitation` (space_id);
