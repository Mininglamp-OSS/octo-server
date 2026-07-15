package cardactiondispatch

import (
	"errors"
	"testing"
	"time"
)

type captureEventQueue struct {
	event Event
	due   time.Time
	err   error
}

func (q *captureEventQueue) Enqueue(event Event, due time.Time) error {
	q.event = event
	q.due = due
	return q.err
}

type fixedSequence struct {
	value int64
	err   error
}

func (s fixedSequence) GenSeq(string) (int64, error) { return s.value, s.err }

type valueStore map[string]interface{}

func (s valueStore) SetValue(value interface{}, key string) { s[key] = value }
func (s valueStore) Value(key string) interface{}           { return s[key] }

func TestServiceAllocatesEventIDAndInstallsPerContext(t *testing.T) {
	registry, err := NewRegistry([]RouteSpec{validRouteSpec()}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	queue := &captureEventQueue{}
	service, err := NewService(registry, queue, fixedSequence{value: 991})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	event := testDispatchEvent()
	event.EventID = 0
	eventID, err := service.Enqueue(event)
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if eventID != 991 || queue.event.EventID != 991 || queue.due.IsZero() {
		t.Fatalf("Enqueue() = id %d event %+v due %v", eventID, queue.event, queue.due)
	}

	ctx := valueStore{}
	if err := Install(ctx, service); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if got, ok := FromContext(ctx); !ok || got != service {
		t.Fatalf("FromContext() = (%p, %v), want (%p, true)", got, ok, service)
	}
	if err := Install(ctx, service); !errors.Is(err, ErrServiceAlreadyInstalled) {
		t.Fatalf("second Install() error = %v, want ErrServiceAlreadyInstalled", err)
	}
}

func TestServiceRejectsUnregisteredOrCallerAssignedEvents(t *testing.T) {
	registry, err := NewRegistry([]RouteSpec{validRouteSpec()}, testGetenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	service, err := NewService(registry, &captureEventQueue{}, fixedSequence{value: 1})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	event := testDispatchEvent()
	if _, err := service.Enqueue(event); err == nil {
		t.Fatal("Enqueue(caller event_id) error = nil")
	}
	event.EventID = 0
	event.ActionType = "unknown"
	if _, err := service.Enqueue(event); err == nil {
		t.Fatal("Enqueue(unregistered route) error = nil")
	}

	queue := &captureEventQueue{err: errors.New("redis down")}
	service, _ = NewService(registry, queue, fixedSequence{value: 2})
	event.ActionType = "access_request.decision"
	if _, err := service.Enqueue(event); err == nil {
		t.Fatal("Enqueue(queue failure) error = nil")
	}
}
