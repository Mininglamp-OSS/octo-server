package cardactiondispatch

import (
	"errors"
	"fmt"
	"time"
)

const (
	serviceContextKey = "octo-server.internal.cardactiondispatch.service.v1"
	eventSequenceKey  = "cardActionDispatchEvent"
)

var ErrServiceAlreadyInstalled = errors.New("cardactiondispatch: service already installed")

type eventQueue interface {
	Enqueue(event Event, due time.Time) error
}

type sequenceGenerator interface {
	GenSeq(key string) (int64, error)
}

type ValueStore interface {
	SetValue(value interface{}, key string)
	Value(key string) interface{}
}

type Service struct {
	registry *Registry
	queue    eventQueue
	sequence sequenceGenerator
	clock    func() time.Time
}

func NewService(registry *Registry, queue eventQueue, sequence sequenceGenerator) (*Service, error) {
	if registry == nil || queue == nil || sequence == nil {
		return nil, errors.New("cardactiondispatch: service dependencies are required")
	}
	return &Service{registry: registry, queue: queue, sequence: sequence, clock: time.Now}, nil
}

func (s *Service) Resolve(senderUID, owner, actionType string) Resolution {
	if s == nil {
		return Resolution{Kind: ResolutionBotPull}
	}
	return s.registry.Resolve(senderUID, owner, actionType)
}

func (s *Service) ResolveNotifyToken(token string) (NotifyCapability, bool) {
	if s == nil {
		return NotifyCapability{}, false
	}
	return s.registry.ResolveNotifyToken(token)
}

func (s *Service) CanNotify(capability NotifyCapability, actionType string) bool {
	return s != nil && s.registry.CanNotify(capability, actionType)
}

func (s *Service) NotifyProducers() []NotifyCapability {
	if s == nil {
		return nil
	}
	return s.registry.NotifyProducers()
}

func (s *Service) Enqueue(event Event) (int64, error) {
	if s == nil {
		return 0, errors.New("cardactiondispatch: service unavailable")
	}
	if event.EventID != 0 {
		return 0, errors.New("cardactiondispatch: caller must not assign event_id")
	}
	if resolution := s.registry.Resolve(event.SenderUID, event.Owner, event.ActionType); resolution.Kind != ResolutionCallback {
		return 0, errors.New("cardactiondispatch: event has no registered callback route")
	}
	eventID, err := s.sequence.GenSeq(eventSequenceKey)
	if err != nil {
		return 0, fmt.Errorf("cardactiondispatch: allocate event_id: %w", err)
	}
	event.EventID = eventID
	if err := s.queue.Enqueue(event, s.clock()); err != nil {
		return 0, err
	}
	return eventID, nil
}

func Install(ctx ValueStore, service *Service) error {
	if ctx == nil || service == nil {
		return errors.New("cardactiondispatch: install requires context and service")
	}
	if ctx.Value(serviceContextKey) != nil {
		return ErrServiceAlreadyInstalled
	}
	ctx.SetValue(service, serviceContextKey)
	return nil
}

func FromContext(ctx ValueStore) (*Service, bool) {
	if ctx == nil {
		return nil, false
	}
	service, ok := ctx.Value(serviceContextKey).(*Service)
	return service, ok && service != nil
}
