package sink

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type PostgresStore struct {
	db *sql.DB
}

func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) Write(ctx context.Context, event Event) (WriteResult, error) {
	if err := event.Validate(); err != nil {
		return WriteResult{}, err
	}

	objectJSON, err := json.Marshal(event.Object)
	if err != nil {
		return WriteResult{}, err
	}
	if len(event.Object) == 0 {
		objectJSON = nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WriteResult{}, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
INSERT INTO dap_events_detail (
  event_id, dedupe_key, event_name, event_time, received_at, source, quality,
  actor_type, actor_id, auth_kind, space_id, identity_quality, identity_error,
  request_method, request_path_template, request_status, request_latency_ms,
  request_trace_id, request_id, request_error_class, object_json, flow_id,
  client_event_id, related_event_id, mapping_rule_id, schema_version
) VALUES (
  $1, $2, $3, $4, $5, $6, $7,
  $8, $9, $10, $11, $12, $13,
  $14, $15, $16, $17,
  $18, $19, $20, $21, $22,
  $23, $24, $25, $26
) ON CONFLICT (dedupe_key) DO NOTHING`,
		event.EventID, event.idempotencyKey(), event.EventName, event.EventTime, event.ReceivedAt,
		event.Source, event.Quality, event.Actor.Type, event.Actor.ID, event.Actor.AuthKind,
		event.Actor.SpaceID, event.Actor.IdentityQuality, event.Actor.IdentityError,
		event.Request.Method, event.Request.PathTemplate, nullableInt(event.Request.Status),
		nullableInt64(event.Request.LatencyMS), event.Request.TraceID, event.Request.RequestID,
		event.Request.ErrorClass, objectJSON, event.FlowID, event.ClientEventID,
		event.RelatedEventID, event.MappingRuleID, event.SchemaVersion)
	if err != nil {
		return WriteResult{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return WriteResult{}, err
	}
	if rows == 0 {
		if err := tx.Commit(); err != nil {
			return WriteResult{}, err
		}
		return WriteResult{Inserted: false, EventID: event.EventID}, nil
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO dap_events_daily_quality (event_date, event_name, source, quality, event_count, updated_at)
VALUES ($1, $2, $3, $4, 1, now())
ON CONFLICT (event_date, event_name, source, quality)
DO UPDATE SET event_count = dap_events_daily_quality.event_count + 1, updated_at = now()`,
		dayUTC(event.EventTime), event.EventName, event.Source, event.Quality)
	if err != nil {
		return WriteResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Inserted: true, EventID: event.EventID}, nil
}

func (s *PostgresStore) WriteBatch(ctx context.Context, events []Event) ([]WriteResult, error) {
	results := make([]WriteResult, 0, len(events))
	for _, event := range events {
		result, err := s.Write(ctx, event)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *PostgresStore) Events(ctx context.Context, filter EventFilter) ([]Event, error) {
	clauses := []string{"1=1"}
	args := make([]any, 0, 5)
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if filter.EventName != "" {
		add("event_name = $%d", filter.EventName)
	}
	if filter.Source != "" {
		add("source = $%d", filter.Source)
	}
	if filter.Quality != "" {
		add("quality = $%d", filter.Quality)
	}
	if !filter.From.IsZero() {
		add("event_time >= $%d", filter.From)
	}
	if !filter.To.IsZero() {
		add("event_time < $%d", filter.To)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT event_id, dedupe_key, event_name, event_time, received_at, source, quality,
       actor_type, actor_id, auth_kind, space_id, identity_quality, identity_error,
       request_method, request_path_template, request_status, request_latency_ms,
       request_trace_id, request_id, request_error_class, object_json, flow_id,
       client_event_id, related_event_id, mapping_rule_id, schema_version
FROM dap_events_detail
WHERE `+strings.Join(clauses, " AND ")+`
ORDER BY event_time, event_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]Event, 0)
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *PostgresStore) DailyAggregates(ctx context.Context, day time.Time) ([]DailyAggregate, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT event_date, event_name, source, quality, event_count
FROM dap_events_daily_quality
WHERE event_date = $1
ORDER BY event_name, source, quality`, dayUTC(day))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DailyAggregate, 0)
	for rows.Next() {
		var aggregate DailyAggregate
		if err := rows.Scan(&aggregate.Date, &aggregate.EventName, &aggregate.Source, &aggregate.Quality, &aggregate.Count); err != nil {
			return nil, err
		}
		out = append(out, aggregate)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DailySummary(ctx context.Context, day time.Time) ([]DailySummary, error) {
	memory := NewMemoryStore()
	aggregates, err := s.DailyAggregates(ctx, day)
	if err != nil {
		return nil, err
	}
	for _, aggregate := range aggregates {
		memory.aggregates[aggregateKey{
			day:       dayUTC(aggregate.Date),
			eventName: aggregate.EventName,
			source:    aggregate.Source,
			quality:   aggregate.Quality,
		}] = aggregate.Count
	}
	return memory.DailySummary(ctx, day)
}

func (s *PostgresStore) MaterializeFunnel(ctx context.Context, spec FunnelSpec) (FunnelResult, error) {
	events, err := s.Events(ctx, EventFilter{
		From: dayUTC(spec.Date),
		To:   dayUTC(spec.Date).AddDate(0, 0, 1),
	})
	if err != nil {
		return FunnelResult{}, err
	}
	memory := NewMemoryStore()
	for _, event := range events {
		memory.events = append(memory.events, event)
	}
	result, err := memory.MaterializeFunnel(ctx, spec)
	if err != nil {
		return FunnelResult{}, err
	}
	if err := s.upsertFunnelResult(ctx, result); err != nil {
		return FunnelResult{}, err
	}
	return result, nil
}

func (s *PostgresStore) upsertFunnelResult(ctx context.Context, result FunnelResult) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO dap_events_funnel_daily (
  event_date, event_family, click_event_name, completion_event_name,
  click_count, completion_count, converted_flow_count, completion_only_flow_count, updated_at
) VALUES (
  $1, $2, $3, $4,
  $5, $6, $7, $8, now()
)
ON CONFLICT (event_date, event_family, click_event_name, completion_event_name)
DO UPDATE SET
  click_count = EXCLUDED.click_count,
  completion_count = EXCLUDED.completion_count,
  converted_flow_count = EXCLUDED.converted_flow_count,
  completion_only_flow_count = EXCLUDED.completion_only_flow_count,
  updated_at = now()`,
		result.Date, result.Family, result.ClickEvent, result.CompletionEvent,
		result.ClickCount, result.CompletionCount, result.ConvertedFlowIDs, result.CompletionOnlyIDs)
	return err
}

type eventScanner interface {
	Scan(dest ...any) error
}

func scanEvent(scanner eventScanner) (Event, error) {
	var event Event
	var source string
	var quality string
	var objectJSON []byte
	var requestStatus sql.NullInt64
	var latencyMS sql.NullInt64
	var actorType, actorID, authKind, spaceID, identityQuality, identityError sql.NullString
	var method, pathTemplate, traceID, requestID, errorClass sql.NullString
	var flowID, clientEventID, relatedEventID, mappingRuleID sql.NullString
	if err := scanner.Scan(
		&event.EventID, &event.DedupeKey, &event.EventName, &event.EventTime, &event.ReceivedAt,
		&source, &quality, &actorType, &actorID, &authKind,
		&spaceID, &identityQuality, &identityError,
		&method, &pathTemplate, &requestStatus, &latencyMS,
		&traceID, &requestID, &errorClass,
		&objectJSON, &flowID, &clientEventID, &relatedEventID,
		&mappingRuleID, &event.SchemaVersion,
	); err != nil {
		return Event{}, err
	}
	event.Source = Source(source)
	event.Quality = Quality(quality)
	event.Actor.Type = actorType.String
	event.Actor.ID = actorID.String
	event.Actor.AuthKind = authKind.String
	event.Actor.SpaceID = spaceID.String
	event.Actor.IdentityQuality = identityQuality.String
	event.Actor.IdentityError = identityError.String
	event.Request.Method = method.String
	event.Request.PathTemplate = pathTemplate.String
	event.Request.TraceID = traceID.String
	event.Request.RequestID = requestID.String
	event.Request.ErrorClass = errorClass.String
	event.FlowID = flowID.String
	event.ClientEventID = clientEventID.String
	event.RelatedEventID = relatedEventID.String
	event.MappingRuleID = mappingRuleID.String
	if requestStatus.Valid {
		event.Request.Status = int(requestStatus.Int64)
	}
	if latencyMS.Valid {
		event.Request.LatencyMS = latencyMS.Int64
	}
	if len(objectJSON) > 0 {
		if err := json.Unmarshal(objectJSON, &event.Object); err != nil {
			return Event{}, err
		}
	}
	return event, nil
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
