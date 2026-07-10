
-- +migrate Up

-- doc_binding：slug ↔ 挂载点（group / thread / space）最小映射
-- OCT-130 方案 A §3.2；octo-server 只存映射，doc 正文与渲染在 octo-docs-html。
create table `doc_binding` (
    id                bigint       not null primary key AUTO_INCREMENT,
    slug              VARCHAR(120) not null default '',                             -- doc 唯一 slug；本表以此为业务主键
    mount_type        VARCHAR(16)  not null default '',                             -- 'group' | 'thread' | 'space'
    group_no          VARCHAR(40)  not null default '',                             -- group / thread 挂载时的群号；space 挂载时为空
    thread_id         VARCHAR(40)  not null default '',                             -- thread 挂载时的 thread short_id；其它类型为空
    space_id          VARCHAR(40)  not null default '',                             -- space 挂载时的 space_id；其它类型为空
    creator_uid       VARCHAR(40)  not null default '',                             -- 建 binding 的用户；用于写权限兜底与审计
    allow_share_code  smallint     not null default 0,                              -- 分享码开关：0=仅成员可见（默认），1=允许持码人访问
    created_at        timeStamp    not null default CURRENT_TIMESTAMP,
    updated_at        timeStamp    not null default CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX doc_binding_slug on `doc_binding` (slug);
-- 按挂载点反查（“这个群 / 子区 / space 有哪些绑定”）；slug 唯一索引已覆盖单点查询。
CREATE INDEX doc_binding_group_no  on `doc_binding` (group_no);
CREATE INDEX doc_binding_thread_id on `doc_binding` (thread_id);
CREATE INDEX doc_binding_space_id  on `doc_binding` (space_id);
