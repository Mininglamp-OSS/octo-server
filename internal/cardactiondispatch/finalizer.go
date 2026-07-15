package cardactiondispatch

import (
	"context"
	"errors"
)

// FinalizerKey binds a route to an optional specialized terminal renderer.
// Routes without a binding use the standard approval fallback.
type FinalizerKey struct {
	Owner      string
	ActionType string
}

// FinalizerRegistry keeps custom terminal visuals explicit while preserving
// config-only onboarding for standard approval consumers.
type FinalizerRegistry struct {
	fallback Finalizer
	routes   map[FinalizerKey]Finalizer
}

func NewFinalizerRegistry(fallback Finalizer, bindings map[FinalizerKey]Finalizer) (*FinalizerRegistry, error) {
	if fallback == nil {
		return nil, errors.New("cardactiondispatch: standard finalizer is required")
	}
	routes := make(map[FinalizerKey]Finalizer, len(bindings))
	for key, finalizer := range bindings {
		if !ownerPattern.MatchString(key.Owner) || !actionTypePattern.MatchString(key.ActionType) {
			return nil, errors.New("cardactiondispatch: invalid finalizer binding")
		}
		if finalizer == nil {
			return nil, errors.New("cardactiondispatch: finalizer binding is required")
		}
		routes[key] = finalizer
	}
	return &FinalizerRegistry{fallback: fallback, routes: routes}, nil
}

func (r *FinalizerRegistry) Finalize(ctx context.Context, event Event, result DecisionResult) error {
	if r == nil || r.fallback == nil {
		return errors.New("cardactiondispatch: finalizer registry unavailable")
	}
	finalizer := r.fallback
	if specialized, ok := r.routes[FinalizerKey{Owner: event.Owner, ActionType: event.ActionType}]; ok {
		finalizer = specialized
	}
	return finalizer.Finalize(ctx, event, result)
}
