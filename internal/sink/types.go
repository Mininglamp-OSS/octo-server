package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Source string

const (
	SourceAccessLog       Source = "access_log"
	SourceDomainEmit      Source = "domain_emit"
	SourceFrontendTracker Source = "frontend_tracker"
	SourceWorkerCallback  Source = "worker_callback"
)

type Quality string

const (
	QualityExact       Quality = "exact"
	QualityShadow      Quality = "shadow"
	QualitySubmitted   Quality = "submitted"
	QualityUIAction    Quality = "ui_action"
	QualityUnavailable Quality = "unavailable"
)

var allQualities = []Quality{
	QualityExact,
	QualityShadow,
	QualitySubmitted,
	QualityUIAction,
	QualityUnavailable,
}

type Actor struct {
	Type            string `json:"actor_type,omitempty"`
	ID              string `json:"actor_id,omitempty"`
	AuthKind        string `json:"auth_kind,omitempty"`
	SpaceID         string `json:"space_id,omitempty"`
	IdentityQuality string `json:"identity_quality,omitempty"`
	IdentityError   string `json:"identity_error,omitempty"`
}

type Request struct {
	Method       string `json:"method,omitempty"`
	PathTemplate string `json:"path_template,omitempty"`
	Status       int    `json:"status,omitempty"`
	LatencyMS    int64  `json:"latency_ms,omitempty"`
	TraceID      string `json:"trace_id,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	ErrorClass   string `json:"error_class,omitempty"`
}

type Event struct {
	EventID        string            `json:"event_id"`
	EventName      string            `json:"event_name"`
	EventTime      time.Time         `json:"event_time"`
	ReceivedAt     time.Time         `json:"received_at"`
	Source         Source            `json:"source"`
	Quality        Quality           `json:"quality"`
	Actor          Actor             `json:"actor,omitempty"`
	Request        Request           `json:"request,omitempty"`
	Object         map[string]string `json:"object,omitempty"`
	FlowID         string            `json:"flow_id,omitempty"`
	ClientEventID  string            `json:"client_event_id,omitempty"`
	DedupeKey      string            `json:"dedupe_key,omitempty"`
	RelatedEventID string            `json:"related_event_id,omitempty"`
	MappingRuleID  string            `json:"mapping_rule_id,omitempty"`
	SchemaVersion  string            `json:"schema_version"`
}

type WriteResult struct {
	Inserted bool
	EventID  string
}

type Store interface {
	Write(ctx context.Context, event Event) (WriteResult, error)
	WriteBatch(ctx context.Context, events []Event) ([]WriteResult, error)
	Events(ctx context.Context, filter EventFilter) ([]Event, error)
	DailyAggregates(ctx context.Context, day time.Time) ([]DailyAggregate, error)
	DailySummary(ctx context.Context, day time.Time) ([]DailySummary, error)
	MaterializeFunnel(ctx context.Context, spec FunnelSpec) (FunnelResult, error)
}

type EventFilter struct {
	EventName string
	Source    Source
	Quality   Quality
	From      time.Time
	To        time.Time
}

type DailyAggregate struct {
	Date      time.Time
	EventName string
	Source    Source
	Quality   Quality
	Count     int64
}

type QualityShare struct {
	Quality Quality
	Count   int64
	Ratio   float64
}

type DailySummary struct {
	Date            time.Time
	EventName       string
	Source          Source
	TotalCount      int64
	CompletionCount int64
	Shares          []QualityShare
}

type FunnelSpec struct {
	Date            time.Time
	Family          string
	ClickEvent      string
	CompletionEvent string
}

type FunnelResult struct {
	Date              time.Time
	Family            string
	ClickEvent        string
	CompletionEvent   string
	ClickCount        int64
	CompletionCount   int64
	ConvertedFlowIDs  int64
	CompletionOnlyIDs int64
}

func (e Event) Validate() error {
	if e.EventID == "" {
		return errors.New("event_id is required")
	}
	if e.EventName == "" {
		return errors.New("event_name is required")
	}
	if e.EventTime.IsZero() {
		return errors.New("event_time is required")
	}
	if e.ReceivedAt.IsZero() {
		return errors.New("received_at is required")
	}
	if !e.Source.valid() {
		return fmt.Errorf("unsupported source %q", e.Source)
	}
	if !e.Quality.valid() {
		return fmt.Errorf("unsupported quality %q", e.Quality)
	}
	if e.SchemaVersion == "" {
		return errors.New("schema_version is required")
	}
	if e.Source == SourceAccessLog {
		if e.Request.Method == "" || e.Request.PathTemplate == "" || e.Request.Status == 0 {
			return errors.New("access_log events require request method, path_template, and status")
		}
	}
	if containsRestrictedSignal(e) {
		return errors.New("event contains restricted credential or payload material")
	}
	return nil
}

func (e Event) idempotencyKey() string {
	if e.DedupeKey != "" {
		return e.DedupeKey
	}
	return e.EventID
}

func (s Source) valid() bool {
	switch s {
	case SourceAccessLog, SourceDomainEmit, SourceFrontendTracker, SourceWorkerCallback:
		return true
	default:
		return false
	}
}

func (q Quality) valid() bool {
	switch q {
	case QualityExact, QualityShadow, QualitySubmitted, QualityUIAction, QualityUnavailable:
		return true
	default:
		return false
	}
}

func dayUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func containsRestrictedSignal(event Event) bool {
	raw, err := json.Marshal(event)
	if err != nil {
		return true
	}
	normalized := strings.ToLower(string(raw))
	restricted := []string{
		"to" + "ken",
		"author" + "ization",
		"coo" + "kie",
		"api" + "_key",
		"bot" + "_token",
		"secret",
		"bearer ",
		"eyj",
		"body",
		"content",
	}
	for _, marker := range restricted {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}
