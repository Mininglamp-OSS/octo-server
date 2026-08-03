package sink

const PostgresDDL = `
CREATE TABLE IF NOT EXISTS dap_events_detail (
  event_id TEXT PRIMARY KEY,
  dedupe_key TEXT NOT NULL UNIQUE,
  event_name TEXT NOT NULL,
  event_time TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ NOT NULL,
  source TEXT NOT NULL,
  quality TEXT NOT NULL,
  actor_type TEXT,
  actor_id TEXT,
  auth_kind TEXT,
  space_id TEXT,
  identity_quality TEXT,
  identity_error TEXT,
  request_method TEXT,
  request_path_template TEXT,
  request_status INTEGER,
  request_latency_ms BIGINT,
  request_trace_id TEXT,
  request_id TEXT,
  request_error_class TEXT,
  object_json JSONB,
  flow_id TEXT,
  client_event_id TEXT,
  related_event_id TEXT,
  mapping_rule_id TEXT,
  schema_version TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_dap_events_detail_time
  ON dap_events_detail (event_time);
CREATE INDEX IF NOT EXISTS idx_dap_events_detail_event_day
  ON dap_events_detail (event_name, event_time, source, quality);
CREATE INDEX IF NOT EXISTS idx_dap_events_detail_flow
  ON dap_events_detail (flow_id, event_name)
  WHERE flow_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS dap_events_daily_quality (
  event_date DATE NOT NULL,
  event_name TEXT NOT NULL,
  source TEXT NOT NULL,
  quality TEXT NOT NULL,
  event_count BIGINT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (event_date, event_name, source, quality)
);

CREATE TABLE IF NOT EXISTS dap_events_funnel_daily (
  event_date DATE NOT NULL,
  event_family TEXT NOT NULL,
  click_event_name TEXT NOT NULL,
  completion_event_name TEXT NOT NULL,
  click_count BIGINT NOT NULL,
  completion_count BIGINT NOT NULL,
  converted_flow_count BIGINT NOT NULL,
  completion_only_flow_count BIGINT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (event_date, event_family, click_event_name, completion_event_name)
);
`
