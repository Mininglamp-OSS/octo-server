package sink

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMemoryStoreWriteAndReadBack(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	event := baseEvent(now)

	result, err := store.Write(context.Background(), event)
	if err != nil {
		t.Fatalf("write event: %v", err)
	}
	if !result.Inserted {
		t.Fatalf("first write should insert")
	}

	events, err := store.Events(context.Background(), EventFilter{EventName: event.EventName})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].EventID != event.EventID || events[0].SchemaVersion != event.SchemaVersion {
		t.Fatalf("unexpected event read back: %+v", events[0])
	}
}

func TestMemoryStoreDedupeKeyIsIdempotent(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	first := baseEvent(now)
	duplicate := first
	duplicate.EventID = "evt-second"

	results, err := store.WriteBatch(context.Background(), []Event{first, duplicate})
	if err != nil {
		t.Fatalf("write batch: %v", err)
	}
	if len(results) != 2 || !results[0].Inserted || results[1].Inserted {
		t.Fatalf("unexpected write results: %+v", results)
	}

	events, err := store.Events(context.Background(), EventFilter{})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("idempotent write should keep 1 detail row, got %d", len(events))
	}
}

func TestDailyAggregatesKeepShadowAndExactSeparate(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	shadow := baseEvent(now)
	shadow.EventID = "evt-shadow"
	shadow.DedupeKey = "dedupe-shadow"
	shadow.Source = SourceAccessLog
	shadow.Quality = QualityShadow
	shadow.Request = Request{Method: "POST", PathTemplate: "/v1/summaries", Status: 200}
	exact := baseEvent(now)
	exact.EventID = "evt-exact"
	exact.DedupeKey = "dedupe-exact"
	exact.Source = SourceDomainEmit
	exact.Quality = QualityExact

	if _, err := store.WriteBatch(context.Background(), []Event{shadow, exact}); err != nil {
		t.Fatalf("write events: %v", err)
	}
	aggregates, err := store.DailyAggregates(context.Background(), now)
	if err != nil {
		t.Fatalf("daily aggregates: %v", err)
	}
	if len(aggregates) != 2 {
		t.Fatalf("expected shadow and exact aggregates, got %+v", aggregates)
	}
	summaries, err := store.DailySummary(context.Background(), now)
	if err != nil {
		t.Fatalf("daily summary: %v", err)
	}
	var exactSummary DailySummary
	for _, summary := range summaries {
		if summary.Source == SourceDomainEmit {
			exactSummary = summary
		}
	}
	if exactSummary.CompletionCount != 1 {
		t.Fatalf("completion facts should count exact only, got %+v", exactSummary)
	}
	if len(exactSummary.Shares) != len(allQualities) {
		t.Fatalf("daily summary should expose all quality slots, got %+v", exactSummary.Shares)
	}
}

func TestFunnelMaterializationCountsClickAndCompletionSeparately(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	click := baseEvent(now)
	click.EventID = "evt-click"
	click.EventName = "smart_summary_creation_started"
	click.DedupeKey = "dedupe-click"
	click.Source = SourceFrontendTracker
	click.Quality = QualityUIAction
	click.FlowID = "flow-1"
	completion := baseEvent(now.Add(time.Minute))
	completion.EventID = "evt-completion"
	completion.EventName = "smart_summary_completed"
	completion.DedupeKey = "dedupe-completion"
	completion.Source = SourceDomainEmit
	completion.Quality = QualityExact
	completion.FlowID = "flow-1"

	if _, err := store.WriteBatch(context.Background(), []Event{click, completion}); err != nil {
		t.Fatalf("write events: %v", err)
	}
	result, err := store.MaterializeFunnel(context.Background(), FunnelSpec{
		Date:            now,
		Family:          "smart_summary",
		ClickEvent:      "smart_summary_creation_started",
		CompletionEvent: "smart_summary_completed",
	})
	if err != nil {
		t.Fatalf("materialize funnel: %v", err)
	}
	if result.ClickCount != 1 || result.CompletionCount != 1 || result.ConvertedFlowIDs != 1 {
		t.Fatalf("unexpected funnel result: %+v", result)
	}
}

func TestPostgresDDLDoesNotDefineRestrictedColumns(t *testing.T) {
	lower := strings.ToLower(PostgresDDL)
	restricted := []string{
		"to" + "ken",
		"author" + "ization",
		"coo" + "kie",
		"api" + "_key",
		"body",
	}
	for _, marker := range restricted {
		if strings.Contains(lower, marker) {
			t.Fatalf("ddl contains restricted marker %q", marker)
		}
	}
}

func TestRejectsRestrictedFieldMaterial(t *testing.T) {
	event := baseEvent(time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC))
	event.Object = map[string]string{
		"access_" + "token": "redacted",
	}
	if _, err := NewMemoryStore().Write(context.Background(), event); err == nil {
		t.Fatalf("expected restricted material to be rejected")
	}
}

func baseEvent(t time.Time) Event {
	return Event{
		EventID:    "evt-1",
		EventName:  "smart_summary_completed",
		EventTime:  t,
		ReceivedAt: t.Add(time.Second),
		Source:     SourceDomainEmit,
		Quality:    QualityExact,
		Actor: Actor{
			Type:            "user",
			ID:              "u_alice",
			AuthKind:        "session",
			SpaceID:         "sp_1",
			IdentityQuality: "resolved",
		},
		Object: map[string]string{
			"summary_id": "sum_1",
		},
		FlowID:        "flow-1",
		DedupeKey:     "dedupe-1",
		MappingRuleID: "rule-20260803",
		SchemaVersion: "dap-event-v1",
	}
}
