package sink

import (
	"context"
	"sort"
	"sync"
	"time"
)

type MemoryStore struct {
	mu         sync.RWMutex
	events     []Event
	byDedupe   map[string]int
	aggregates map[aggregateKey]int64
}

type aggregateKey struct {
	day       time.Time
	eventName string
	source    Source
	quality   Quality
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byDedupe:   make(map[string]int),
		aggregates: make(map[aggregateKey]int64),
	}
}

func (s *MemoryStore) Write(ctx context.Context, event Event) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	if err := event.Validate(); err != nil {
		return WriteResult{}, err
	}

	key := event.idempotencyKey()
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx, ok := s.byDedupe[key]; ok {
		return WriteResult{Inserted: false, EventID: s.events[idx].EventID}, nil
	}
	s.events = append(s.events, cloneEvent(event))
	s.byDedupe[key] = len(s.events) - 1
	s.aggregates[aggregateKey{
		day:       dayUTC(event.EventTime),
		eventName: event.EventName,
		source:    event.Source,
		quality:   event.Quality,
	}]++
	return WriteResult{Inserted: true, EventID: event.EventID}, nil
}

func (s *MemoryStore) WriteBatch(ctx context.Context, events []Event) ([]WriteResult, error) {
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

func (s *MemoryStore) Events(ctx context.Context, filter EventFilter) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Event, 0, len(s.events))
	for _, event := range s.events {
		if !matchesFilter(event, filter) {
			continue
		}
		out = append(out, cloneEvent(event))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EventTime.Equal(out[j].EventTime) {
			return out[i].EventID < out[j].EventID
		}
		return out[i].EventTime.Before(out[j].EventTime)
	})
	return out, nil
}

func (s *MemoryStore) DailyAggregates(ctx context.Context, day time.Time) ([]DailyAggregate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target := dayUTC(day)
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]DailyAggregate, 0)
	for key, count := range s.aggregates {
		if !key.day.Equal(target) {
			continue
		}
		out = append(out, DailyAggregate{
			Date:      key.day,
			EventName: key.eventName,
			Source:    key.source,
			Quality:   key.quality,
			Count:     count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EventName != out[j].EventName {
			return out[i].EventName < out[j].EventName
		}
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Quality < out[j].Quality
	})
	return out, nil
}

func (s *MemoryStore) DailySummary(ctx context.Context, day time.Time) ([]DailySummary, error) {
	aggregates, err := s.DailyAggregates(ctx, day)
	if err != nil {
		return nil, err
	}
	type groupKey struct {
		eventName string
		source    Source
	}
	groups := make(map[groupKey]*DailySummary)
	for _, aggregate := range aggregates {
		key := groupKey{eventName: aggregate.EventName, source: aggregate.Source}
		summary := groups[key]
		if summary == nil {
			summary = &DailySummary{
				Date:      aggregate.Date,
				EventName: aggregate.EventName,
				Source:    aggregate.Source,
			}
			groups[key] = summary
		}
		summary.TotalCount += aggregate.Count
		if aggregate.Quality == QualityExact {
			summary.CompletionCount += aggregate.Count
		}
		summary.Shares = append(summary.Shares, QualityShare{
			Quality: aggregate.Quality,
			Count:   aggregate.Count,
		})
	}

	out := make([]DailySummary, 0, len(groups))
	for _, summary := range groups {
		seen := make(map[Quality]struct{}, len(summary.Shares))
		for _, share := range summary.Shares {
			seen[share.Quality] = struct{}{}
		}
		for _, quality := range allQualities {
			if _, ok := seen[quality]; ok {
				continue
			}
			summary.Shares = append(summary.Shares, QualityShare{Quality: quality})
		}
		sort.Slice(summary.Shares, func(i, j int) bool {
			return summary.Shares[i].Quality < summary.Shares[j].Quality
		})
		for i := range summary.Shares {
			if summary.TotalCount > 0 {
				summary.Shares[i].Ratio = float64(summary.Shares[i].Count) / float64(summary.TotalCount)
			}
		}
		out = append(out, *summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EventName != out[j].EventName {
			return out[i].EventName < out[j].EventName
		}
		return out[i].Source < out[j].Source
	})
	return out, nil
}

func (s *MemoryStore) MaterializeFunnel(ctx context.Context, spec FunnelSpec) (FunnelResult, error) {
	if err := ctx.Err(); err != nil {
		return FunnelResult{}, err
	}
	target := dayUTC(spec.Date)
	result := FunnelResult{
		Date:            target,
		Family:          spec.Family,
		ClickEvent:      spec.ClickEvent,
		CompletionEvent: spec.CompletionEvent,
	}
	clickFlows := make(map[string]struct{})
	completionFlows := make(map[string]struct{})

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, event := range s.events {
		if !dayUTC(event.EventTime).Equal(target) {
			continue
		}
		switch event.EventName {
		case spec.ClickEvent:
			result.ClickCount++
			if event.FlowID != "" {
				clickFlows[event.FlowID] = struct{}{}
			}
		case spec.CompletionEvent:
			result.CompletionCount++
			if event.FlowID != "" {
				completionFlows[event.FlowID] = struct{}{}
			}
		}
	}
	for flowID := range completionFlows {
		if _, ok := clickFlows[flowID]; ok {
			result.ConvertedFlowIDs++
		} else {
			result.CompletionOnlyIDs++
		}
	}
	return result, nil
}

func matchesFilter(event Event, filter EventFilter) bool {
	if filter.EventName != "" && event.EventName != filter.EventName {
		return false
	}
	if filter.Source != "" && event.Source != filter.Source {
		return false
	}
	if filter.Quality != "" && event.Quality != filter.Quality {
		return false
	}
	if !filter.From.IsZero() && event.EventTime.Before(filter.From) {
		return false
	}
	if !filter.To.IsZero() && !event.EventTime.Before(filter.To) {
		return false
	}
	return true
}

func cloneEvent(event Event) Event {
	if event.Object != nil {
		cloned := make(map[string]string, len(event.Object))
		for k, v := range event.Object {
			cloned[k] = v
		}
		event.Object = cloned
	}
	return event
}
