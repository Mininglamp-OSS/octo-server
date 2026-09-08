-- +migrate Up

-- O4 — the transactional outbox that carries project lifecycle facts to the
-- Loop control plane.
--
-- WHY AN OUTBOX AND NOT THE EXISTING IN-PROCESS EVENT BUS. modules/base/event is
-- a fire-once dispatcher: a listener that fails is marked Fail and never
-- re-selected, and several listeners share one status row whose version lock is
-- never incremented, so the last writer wins. That is acceptable for work whose
-- loss is a cosmetic degradation. It is not acceptable here, because the event
-- this table carries is what stops a removed member from continuing to execute
-- on the other side of a service boundary. A dropped delivery is a revocation
-- that never happens.
--
-- The row is written in the SAME transaction as the business change it reports.
-- That is the entire point: a crash between committing a membership change and
-- recording the intent to publish it would otherwise lose the publication
-- silently, and nothing downstream would ever notice, because the consumer
-- cannot miss what it was never told about.
--
-- STRUCTURALLY COPIED from octo_project_member_removal_cleanup -- lease owner
-- plus lease_until, attempts, next_attempt_at, backoff, a terminal abandoned
-- state, the same two index shapes, retention purge. Same worker shape, same
-- hazards, and the corrections that table already earned:
--
--   * TIME IS APPLICATION-WRITTEN UTC, no DEFAULT and no ON UPDATE. The older
--     space table uses CURRENT_TIMESTAMP(3), which is the MySQL SESSION
--     timezone, and this repo has already shipped a metric broken exactly that
--     way -- an age gauge that read minus 28799 seconds under an eastern
--     timezone. One clock, in Go, in UTC.
--   * the purge scans (status, finished_at), which needs its own index: the
--     pending index leads with status too, but its second column is
--     next_attempt_at and cannot serve a finished_at range.
--
-- WHAT IS DIFFERENT FROM THAT TABLE, and why.
--
-- 1. event_id is a UNIQUE canonical UUID, generated at enqueue and sent as the
--    consumer-facing idempotency key. Retries reuse it, so a delivery that
--    succeeded but whose response was lost is recognised as a duplicate rather
--    than applied twice.
--
-- 2. payload is FROZEN AT ENQUEUE, stored here as serialized JSON, and the
--    worker sends those bytes verbatim. It is never recomputed at delivery
--    time, and that is a correctness requirement rather than an optimization:
--    the consumer fingerprints the payload it receives and answers a conflict
--    when the same event id arrives carrying different content. Recomputing
--    from live rows would produce different bytes the moment anything about the
--    project changed between the first attempt and the retry -- a rename during
--    a retry window would turn an ordinary redelivery into a permanent
--    conflict.
--
-- 3. NO CANCELLED STATE. The removal outbox needs one because re-admitting a
--    member retires an in-flight cascade. Nothing retracts a statement about
--    something that already happened: a project WAS created, a member WAS
--    revoked. A superseding event is another row, never an edit to this one.
--
-- 4. project_version carries the lifecycle version AS OF THE EMITTING
--    TRANSACTION, so the consumer can discard a statement older than what it
--    already applied. Nullable because not every event type is ordered by it --
--    a member revocation must be applied even when it arrives late, since
--    dropping it as stale would leave a removed member executing.
--
-- Deliberately NOT unique on (project_id, event_type): a project is renamed
-- many times and each rename is its own statement.
CREATE TABLE IF NOT EXISTS `octo_project_lifecycle_event` (
  `id`                BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `event_id`          VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '规范 UUID；作为消费方幂等键，重投复用同一个值',
  `event_type`        VARCHAR(64)      NOT NULL DEFAULT ''  COMMENT 'project.created / member_revoked / archived / restored / metadata_updated',
  `project_id`        VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '对齐 octo_project.project_id',
  `space_id`          VARCHAR(40)      NOT NULL DEFAULT ''  COMMENT '冗余 Space；避免投递时回表',
  `project_version`   BIGINT           NULL                 COMMENT '发出事务当时的 lifecycle_version；NULL 表示该事件类型不按版本排序',
  `payload`           JSON             NOT NULL             COMMENT '入队时冻结的载荷，投递时原样发送；绝不在投递时重算',
  `occurred_at`       DATETIME(3)      NOT NULL             COMMENT 'UTC；业务事实发生时间，应用侧写入',
  `status`            TINYINT UNSIGNED NOT NULL DEFAULT 0   COMMENT '0=pending 1=delivered 2=abandoned',
  `attempts`          INT UNSIGNED     NOT NULL DEFAULT 0,
  `next_attempt_at`   DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入，禁 CURRENT_TIMESTAMP',
  `lease_owner`       VARCHAR(64)      NOT NULL DEFAULT '',
  `lease_until`       DATETIME(3)      NULL                 COMMENT 'UTC；worker 在批内心跳续租',
  `last_error`        VARCHAR(255)     NOT NULL DEFAULT ''  COMMENT '低基数失败摘要；不得写入凭据、响应体或用户内容',
  `created_at`        DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入',
  `finished_at`       DATETIME(3)      NULL                 COMMENT 'UTC；应用侧写入',
  PRIMARY KEY (`id`),
  -- The idempotency key must be unique or a retry could be enqueued twice and
  -- the consumer would see one id for two different payloads.
  UNIQUE KEY `uk_octo_project_lifecycle_event_id` (`event_id`),
  KEY `idx_octo_project_lifecycle_event_pending` (`status`, `next_attempt_at`, `lease_until`),
  KEY `idx_octo_project_lifecycle_event_project` (`project_id`, `id`),
  KEY `idx_octo_project_lifecycle_event_finished` (`status`, `finished_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='项目生命周期事件发件箱（O4）';

-- +migrate Down
--
-- WARNING: rolling this back DISCARDS UNDELIVERED EVENTS, and an undelivered
-- revocation is a member who keeps executing on the other side. Count rows with
-- status = 0 before running it, the same warning the removal cleanup migration
-- carries.
--
-- No apostrophes in any comment in this file, on purpose. The migration test in
-- this module splits statements naively and treats a quote as a string
-- delimiter, so one possessive apostrophe makes it read the next semicolon as
-- being inside a literal. The failure PAIRS UP, so an even number can pass while
-- an odd number fails. Do not add one back and rely on counting.
DROP TABLE IF EXISTS `octo_project_lifecycle_event`;
