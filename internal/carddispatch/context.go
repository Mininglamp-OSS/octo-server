package carddispatch

import "errors"

const registryContextKey = "octo-server.internal.carddispatch.registry.v1"

type ValueStore interface {
	SetValue(value interface{}, key string)
	Value(key string) interface{}
}

// Install publishes one immutable registry into one application context. It is
// called only by the single-threaded bootstrap path before module construction.
func Install(ctx ValueStore, registry *Registry) error {
	if ctx == nil || registry == nil {
		return errors.New("carddispatch: registry install requires context and registry")
	}
	if ctx.Value(registryContextKey) != nil {
		return ErrRegistryAlreadyInstalled
	}
	ctx.SetValue(registry, registryContextKey)
	return nil
}

func SenderFromContext(ctx ValueStore, id ProducerID) (Sender, error) {
	if ctx == nil {
		return nil, categorized(CategoryProducerDisabled, errors.New("application context unavailable"))
	}
	registry, ok := ctx.Value(registryContextKey).(*Registry)
	if !ok || registry == nil {
		return nil, categorized(CategoryProducerDisabled, errors.New("registry unavailable"))
	}
	return registry.Sender(id)
}
